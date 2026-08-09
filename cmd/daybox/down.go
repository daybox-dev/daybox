package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// downSleep is the detach-poll sleeper, overridable in tests so the
// 120s wait doesn't slow the suite. The real path is time.Sleep.
var downSleep = time.Sleep

// unmountWorkFn runs the on-box unmount fallback. Overridable in tests
// (the real path ssh's to the box; tests stub it to avoid a 10s
// ConnectTimeout against a fake IP).
var unmountWorkFn = unmountWork

// down.go — the plane-side `down` (bash cmd_down). Detaches the volume,
// deletes the box (billing stops), drops the net node, resets idle.
//
// Deliberately does NOT require identity config (no GIT_NAME/GIT_EMAIL): the
// reaper calls this to stop billing, and it must never be blocked by a
// half-edited config.local — a commented-out GIT_NAME once made this die and
// every reap path silently defeated while the box billed (bash cmd_down
// comment). It needs only provider credentials + ssh state.
//
// The volume-teardown sequence is load-bearing (comments cite real
// incidents): sync before unmount so a failed unmount still detaches with
// data flushed, kill holders, lazy-unmount as last resort, then WAIT for
// the detach to finish before deleting the server (deleting while a detach
// is in flight wedges the volume locked — seen 2026-07-21).

// downOps is the injectable ssh/net surface for the teardown. Tests fake
// it to assert the detach-poll + net-node-drop decisions without a cloud.
type downOps interface {
	// netEnabled reports headscale + the agent binary present (bash:
	// net_enabled). down drops the net node only when the net is up here.
	netEnabled() bool
	// dropNetNode removes the box's headscale node immediately (ephemeral
	// GC would take ~30min; the net view must never show ghosts).
	dropNetNode(serverName string)
}

// downBox is the plane-side `down`. Returns nil when there was no box
// (idempotent). On the volume-detach path it waits up to 120s for the
// detach to settle before deleting the server.
func downBox(p *profile, prov Provider, ops downOps) error {
	if err := prov.CheckCredentials(); err != nil {
		return err
	}
	s, err := prov.Probe(p.serverName)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if s == nil {
		say("no big box running")
		resetIdle(p)
		return nil
	}
	vid, _ := p.volumeID() // missing volume_id is non-fatal (bash: || true)

	if vid != "" {
		attached, _ := prov.VolumeAttachedTo(vid)
		if attached == s.ID {
			say("unmounting /work + detaching volume")
				uerr := unmountWorkFn(p, s.IP)
			if uerr != "" {
				say("WARNING: clean unmount failed (%s) — detaching anyway; unflushed writes may be lost", uerr)
			}
			if err := prov.VolumeDetach(vid); err != nil {
				return fmt.Errorf("detach volume: %w", err)
			}
			// wait for the detach to settle (bash: 60 @ 2s). Deleting the
			// server while our detach is in flight makes Hetzner fire its
			// own auto-detach on top and the two wedge the volume locked.
			detached := false
			for i := 0; i < 60; i++ {
				if a, _ := prov.VolumeAttachedTo(vid); a == "" {
					detached = true
					break
				}
				downSleep(2 * time.Second)
			}
			if !detached {
				say("WARNING: volume still detaching after 120s — deleting the server anyway (billing stops).\n  The provider may briefly lock the volume; if the next summon fails with 'volume is locked', wait and re-run.")
			}
		}
	}

	say("deleting server %s (billing stops now)", s.ID)
	if err := prov.Reap(s.ID); err != nil {
		return fmt.Errorf("reap: %w", err)
	}
	resetIdle(p)

	if ops.netEnabled() {
		ops.dropNetNode(p.serverName)
	}
	say("done. workspace persists on volume '%s'.", p.volumeName)
	return nil
}

// unmountWork runs the sync -> umount -> fuser-kill -> lazy-umount fallback
// on the box. Returns the stderr output when the whole thing failed (so the
// caller can warn), "" when it succeeded or the box was unreachable. bash:
// the timeout 60 ssh block.
func unmountWork(p *profile, ip string) string {
	args := []string{"timeout", "60", "ssh"}
	args = append(args, sshBoxOpts(p)...)
	args = append(args, p.remoteUser+"@"+ip,
		"sync; sudo umount /work || { sudo fuser -km /work; sleep 1; sudo umount /work; } || { sync; sudo umount -l /work; }")
	c := exec.Command(args[0], args[1:]...)
	var stderr strings.Builder
	c.Stderr = &stderr
	if c.Run() != nil {
		out := strings.TrimSpace(stderr.String())
		if out == "" {
			return "no output — box unreachable?"
		}
		return out
	}
	return ""
}

// ---- real downOps ----

type planeDownOps struct{ p *profile }

func newPlaneDownOps(p *profile) *planeDownOps { return &planeDownOps{p: p} }

func (o *planeDownOps) netEnabled() bool {
	// bash: command -v headscale && [ -x "$NET_AGENT_BIN" ]
	if _, err := exec.LookPath("headscale"); err != nil {
		return false
	}
	if info, err := os.Stat(o.p.dep.agentBin()); err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	return true
}

func (o *planeDownOps) dropNetNode(serverName string) {
	nodes, err := o.p.dep.headscaleNodesJSON()
	if err != nil {
		say("net: WARNING: could not list nodes: %v", err)
		return
	}
	for _, id := range nodeIDsForName(nodes, serverName) {
		if exec.Command("headscale", "nodes", "delete", "-i", id, "--force").Run() == nil {
			say("net: removed node '%s' (id %s)", serverName, id)
		} else {
			say("net: WARNING: node id %s was not removed — a stale entry is left on the net", id)
		}
	}
}

// nodeIDsForName returns the headscale node ids whose name OR given_name
// matches the box (bash: .name==$h or .given_name==$h).
func nodeIDsForName(nodesJSON, hostname string) []string {
	var list []struct {
		ID        any    `json:"id"`
		Name      string `json:"name"`
		GivenName string `json:"given_name"`
	}
	if err := json.Unmarshal([]byte(nodesJSON), &list); err != nil {
		return nil
	}
	var out []string
	for _, n := range list {
		if n.Name == hostname || n.GivenName == hostname {
			out = append(out, fmt.Sprintf("%v", n.ID))
		}
	}
	return out
}
