package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// summon.go — the plane-side `up` (bash cmd_up + wait_ready + net_join_box),
// the spine of the single-binary port. Fixes two bugs from the bash CLI:
//
//   #2 — `up` must NEVER destroy an existing box. The bash "fail closed"
//        branch (bin/daybox:676) tore a running, billing server down when it
//        couldn't read a clean provisioning verdict. Here an existing box
//        with a bad verdict is LEFT RUNNING and up exits non-zero with
//        inspection help — never downed. The only thing that deletes a box
//        is `down`/`reap`.
//
//   #3 — no `IP`/`HOSTKEY` stdout contract. The bash emit_conn printed both
//        so the laptop could parse the IP as a success signal; the hostkeys
//        leaked to stdout. Here the plane says the IP on STDERR (for the
//        user) and signals success by exit code only — stdout stays clean.
//        The laptop hops via the plane to ssh in under ingress lockdown
//        anyway; it never needed the IP to connect, only to know "ok".
//
// The testable core is summonUp, which takes a Provider (U1) and a
// summonOps (the ssh/net/headscale steps). The real planeSshOps shells out;
// tests use fakeSummonOps to assert the bug regressions without a cloud.

// summonOps is the set of ssh/net/headscale operations summonUp needs. They
// are an interface so the decision logic — "existing box, bad verdict:
// do NOT down" — is exercised in tests with fakes. The real implementation
// (planeSshOps) shells out exactly as the bash CLI did.
type summonOps interface {
	// seedVerdict reads firstboot's verdict on an existing box: "ok",
	// "FAILED…", or "" when unreadable. Bounded (bash: timeout 20).
	seedVerdict(ip string) string

	// netNodeOnline: is this profile's box registered AND online on the net?
	netNodeOnline() bool

	// netJoinBox pushes the agent + a single-use ephemeral preauth key to
	// the box at ip and waits for it to come online. Fatal-on-failure
	// semantics: a daybox exists ONLY on the net.
	netJoinBox(ip string) error

	// waitReady waits for sshd to open + firstboot's verdict (bash:
	// wait_ready). Returns nil only on verdict "ok".
	waitReady(ip string) error

	// pinHostkey rescans the box's host key into the profile's known_hosts
	// (Hetzner recycles IPs — a stale global entry would hard-fail later).
	pinHostkey(ip string) error

	// sshIntoBox opens a fresh shell on the box (bash cmd_ssh: over the
	// public IP, the control plane is allowlisted even in strict mode).
	sshIntoBox() error
}

// summonUp is the plane-side `up`. Returns nil on success. On a non-nil
// error the caller exits non-zero (the laptop's delegate surfaces it).
// stdout is never written — bug #3: no IP/HOSTKEY contract.
func summonUp(p *profile, prov Provider, detach bool, ops summonOps) error {
	existing, err := prov.Probe(p.serverName)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if existing != nil {
		return reconnectBox(p, prov, existing, detach, ops)
	}
	return summonFresh(p, prov, detach, ops)
}

// reconnectBox hands back an EXISTING box. Bug #2: it NEVER tears the box
// down — the bash "fail closed" path is gone. A bad verdict or a failed net
// re-enroll leaves the box running with an actionable error.
func reconnectBox(p *profile, prov Provider, s *Server, detach bool, ops summonOps) error {
	say("big box already up at %s (%s)", s.IP, s.Type)
	if err := ops.pinHostkey(s.IP); err != nil {
		// non-fatal: a pin failure shouldn't block reconnect (bash tolerated it)
		say("WARNING: could not pin hostkey: %v", err)
	}
	verdict := ops.seedVerdict(s.IP)
	if verdict != "ok" {
		// bug #2 fix: leave the box RUNNING. Do NOT call down/reap. Report
		// + tell the user how to inspect or tear it down themselves.
		v := verdict
		if v == "" {
			v = "unreadable"
		}
		return fmt.Errorf("existing box has no clean provisioning verdict (%s) — left running so you can inspect; run: daybox -p %s ssh   (then: sudo cat /var/log/cloud-init-output.log), or: daybox -p %s down", v, p.name, p.name)
	}
	if !ops.netNodeOnline() {
		say("existing box is not on the net — re-enrolling it")
		if err := ops.netJoinBox(s.IP); err != nil {
			// bug #2: the box stays. The bash path tore it down here too.
			return fmt.Errorf("net join failed — box left running; fix and re-run, or: daybox -p %s down: %w", p.name, err)
		}
	}
	resetIdle(p)
	if detach {
		say("box is up at %s — connect with: daybox ssh   (tmux: daybox attach)", s.IP)
		return nil
	}
	return ops.sshIntoBox()
}

// summonFresh creates a new box: detach the volume if it's stuck attached,
// size-check the rendered user_data, create + wait-for-running, wait for
// firstboot's verdict, enroll on the net. No emit_conn (bug #3): the IP is
// said on stderr only.
func summonFresh(p *profile, prov Provider, detach bool, ops summonOps) error {
	if err := p.dep.requireSeed(p); err != nil {
		return err
	}
	// pending proposals get one last look BEFORE the render (the profile is
	// frozen into user_data at render time). Non-blocking note only — the
	// laptop's own `up` offers the interactive review before delegating.
	pending := countPendingProposals(p)
	if pending > 0 {
		say("NOTE: %d pending proposal(s) for '%s' — summoning with the CURRENT profile (review on your laptop: daybox profile proposals)", pending, p.name)
	}

	vid, err := p.volumeID()
	if err != nil {
		return err
	}
	// a volume can only attach to one server; free it if it's stuck
	if attached, _ := prov.VolumeAttachedTo(vid); attached != "" {
		say("volume still attached to server %s — detaching", attached)
		if err := prov.VolumeDetach(vid); err != nil {
			return fmt.Errorf("detach volume: %w", err)
		}
		time.Sleep(3 * time.Second)
	}

	userData, err := renderUserData(p, vid)
	if err != nil {
		return err
	}
	// provider caps user_data size (Hetzner: 32KiB); catch here so the error
	// names an oversized render (usually a grown profile seed) not an opaque
	// API validation error.
	if cap := prov.UserDataMaxBytes(); len(userData) > cap {
		return fmt.Errorf("rendered cloud-init is %d bytes — over the %d-byte user_data cap; the usual cause is a large profile seed (daybox profile seed path %s)", len(userData), cap, p.name)
	}

	price := prov.PriceHourly(p.serverType, p.location)
	say("creating %s in %s (~€%s/h gross) ...", p.serverType, p.location, priceOrQ(price))
	srv, err := prov.Summon(p.serverName, p.serverType, p.image, p.location, vid, userData)
	if err != nil {
		return fmt.Errorf("summon: %w", err)
	}
	say("server %s running at %s — waiting for provisioning", srv.ID, srv.IP)
	if err := ops.pinHostkey(srv.IP); err != nil {
		return fmt.Errorf("pin hostkey: %w", err)
	}
	if err := ops.waitReady(srv.IP); err != nil {
		return err
	}
	say("net: enrolling box on the daybox net")
	if err := ops.netJoinBox(srv.IP); err != nil {
		return fmt.Errorf("net join failed — box left running; run: daybox -p %s down: %w", p.name, err)
	}
	resetIdle(p)
	say("ready. ~€%s/h until 'daybox down' (idle reaper: %dmin; hard cap: %dh)", priceOrQ(price), p.reapAfterIdleMin, p.maxLifetimeHours)
	if detach {
		say("box is up at %s — connect with: daybox ssh   (tmux: daybox attach)", srv.IP)
		return nil
	}
	return ops.sshIntoBox()
}

// requireSeed dies with setup help when the profile has no seed (bash:
// need_seed). A box carries what its profile declares; there is no default
// at summon time — inventing one would mean a box silently carrying
// something its profile never declared (the exact drift this design removes).
func (d *deployment) requireSeed(p *profile) error {
	if !fileExists(p.seedFile()) {
		return fmt.Errorf("profile '%s' has no seed at %s\n  A box carries what its profile declares; there is no default at summon time.\n  create one with:  daybox profile seed init %s", p.name, p.seedFile(), p.name)
	}
	return nil
}

// countPendingProposals: an empty proposals store is steady state (bash:
// the glob's || true). Returns 0 when none or unreadable.
func countPendingProposals(p *profile) int {
	dir := filepath.Join(p.dep.confDir, "profiles", p.name, "proposals")
	entries, err := readDirGlob(dir, "*.toml")
	if err != nil {
		return 0
	}
	return len(entries)
}

func resetIdle(p *profile) {
	writeFile(p.idleTicksFile(), "0")
	writeFile(p.unreachTicksFile(), "0")
}

func priceOrQ(price string) string {
	if price == "" {
		return "?"
	}
	return price
}

// ---- real plane-side ops (shell out) ----

// planeSshOps implements summonOps with real ssh/scp/headscale calls. These
// are the faithful ports of bash ssh_retry/scp_retry + headscale CLI use;
// they need a real plane (not unit-tested — covered by manual + conformance
// testing). Constructed with the profile whose paths + identity it uses.
type planeSshOps struct {
	p   *profile
	out io.Writer
}

func newPlaneSshOps(p *profile) *planeSshOps { return &planeSshOps{p: p} }

func (o *planeSshOps) seedVerdict(ip string) string {
	// bash: timeout 20 ssh ... 'cat /var/lib/daybox/seed.status 2>/dev/null'
	args := []string{"timeout", "20", "ssh"}
	args = append(args, sshBoxOpts(o.p)...)
	args = append(args, o.p.remoteUser+"@"+ip, "cat /var/lib/daybox/seed.status 2>/dev/null")
	cmd := exec.Command(args[0], args[1:]...)
	b, _ := cmd.Output()
	return strings.TrimSpace(string(b))
}

func (o *planeSshOps) netNodeOnline() bool {
	out, err := o.p.dep.headscaleNodesJSON()
	if err != nil {
		return false
	}
	return nodeIsOnline(out, o.p.serverName)
}

func (o *planeSshOps) netJoinBox(ip string) error {
	return netJoinBox(o.p, ip)
}

func (o *planeSshOps) waitReady(ip string) error {
	return waitReady(o.p, ip)
}

func (o *planeSshOps) pinHostkey(ip string) error {
	return pinHostkey(o.p, ip)
}

func (o *planeSshOps) sshIntoBox() error {
	// bash cmd_ssh: over the public IP (the control plane is allowlisted
	// even in strict mode). tty for an interactive shell. The summon/reconnect
	// paths already said the IP on stderr; re-probe here so the ops interface
	// stays IP-free (the laptop's delegate doesn't carry the IP either).
	ip := o.currentBoxIP()
	if ip == "" {
		return fmt.Errorf("box is up but its IP could not be re-probed — connect with: daybox ssh")
	}
	args := append([]string{"ssh"}, sshBoxOptsTty(o.p)...)
	args = append(args, o.p.remoteUser+"@"+ip)
	c := exec.Command(args[0], args[1:]...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// propagate the ssh exit code (a remote command failure, not a
			// transport failure) so `daybox ssh false` exits 1, not 255.
			osExitWithCode(ee.ExitCode())
		}
		return err
	}
	return nil
}

// currentBoxIP re-probes the box to get its IP for the ssh follow-up.
func (o *planeSshOps) currentBoxIP() string {
	s, err := o.p.dep.providerFor(o.p).Probe(o.p.serverName)
	if err != nil || s == nil {
		return ""
	}
	return s.IP
}

// sshBoxOpts: the known_hosts + batch options for sshing to a box from the
// plane (bash: -o UserKnownHostsFile=$KNOWN_HOSTS -o BatchMode=yes -o ...).
func sshBoxOpts(p *profile) []string {
	return []string{
		"-o", "UserKnownHostsFile=" + p.knownHosts,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
}

func sshBoxOptsTty(p *profile) []string {
	return append(sshBoxOpts(p), "-t")
}

// osExitWithCode propagates an ssh exit code without pulling os into the
// testable surface; thin wrapper so tests can swap it. The real path calls
// syscall.Exit; tests stub it to assert the code path without dying.
var osExitWithCode = syscall.Exit

// readFile/writeFile/readDirGlob/stdout/stderr/stdin/fileExists are the
// os-touching helpers centralised so the testable core can be faked later;
// here they delegate to the os.
func writeFile(path, content string) { _ = writeFileImpl(path, content) }
