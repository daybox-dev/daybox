package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// summon_impl.go — the shell-out implementations of the summonOps steps
// (waitReady, netJoinBox, pinHostkey, headscale nodes), ported from bash.
// The decision logic in summon.go is unit-tested with fakes; these are the
// real plane-side operations, covered by manual + conformance testing.

// waitReadyImpl waits for sshd to open, then for firstboot's verdict.
// bash: wait_ready. There is deliberately no "connect anyway" fallback —
// handing back a box that is missing what its profile declared, while
// reporting success, is the exact failure this design exists to prevent.
func waitReady(p *profile, ip string) error {
	// 1. wait for sshd (bash: nc -z -w 2; 60 attempts @ 2s)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		c := exec.Command("nc", "-z", "-w", "2", ip, "22")
		if c.Run() == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if exec.Command("nc", "-z", "-w", "2", ip, "22").Run() != nil {
		return fmt.Errorf("ssh port never opened on %s", ip)
	}
	if err := pinHostkey(p, ip); err != nil {
		return err
	}
	// 2. wait for the seed verdict (bash: 300 attempts @ 2s = 10min)
	for i := 0; i < 300; i++ {
		args := append([]string{"ssh"}, sshBoxOpts(p)...)
		args = append(args, p.remoteUser+"@"+ip, "cat /var/lib/daybox/seed.status 2>/dev/null")
		c := exec.Command(args[0], args[1:]...)
		out, _ := c.Output()
		st := strings.TrimSpace(string(out))
		switch {
		case st == "ok":
			return nil
		case strings.HasPrefix(st, "FAILED"):
			say("provisioning FAILED on %s:", ip)
			fmt.Fprintln(os.Stderr, st)
			return fmt.Errorf("the box is left RUNNING so you can inspect it:\n    daybox -p %s ssh    then: sudo cat /var/log/cloud-init-output.log\n  fix the seed and re-summon, or: daybox -p %s down", p.name, p.name)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("provisioning never finished on %s (no verdict in /var/lib/daybox/seed.status after 10min)", ip)
}

// pinHostkeyImpl rescans the box's host key into the profile's known_hosts.
// bash: pin_hostkey. Hetzner recycles IPs, and a stale global entry turns
// into a hard verification failure on the next unlucky box — so the host
// key is pinned per-profile, fresh every time.
func pinHostkey(p *profile, ip string) error {
	// ssh-keygen -R clears any stale entry for this host in OUR file first
	if exec.Command("ssh-keygen", "-R", ip, "-f", p.knownHosts).Run() != nil {
		// non-fatal: a missing file or no entry is fine
	}
	// sshd opens :22 before it reliably serves its host-key banner — the
	// port listens while cloud-init is still hammering the fresh box, so a
	// single ssh-keyscan the instant nc -z succeeds races sshd and can come
	// back empty (exit 1, no keys). Bash pin_hostkey used -T 5 and checked
	// output SIZE, not the exit code, for the same reason; retry until it
	// returns at least one key, the way waitReady's own loops ride out the
	// same startup window.
	deadline := time.Now().Add(30 * time.Second)
	var out []byte
	for time.Now().Before(deadline) {
		o, err := exec.Command("ssh-keyscan", "-T", "5", "-t", "ed25519,rsa,ecdsa", ip).Output()
		if err == nil && len(o) > 0 {
			out = o
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(out) == 0 {
		return fmt.Errorf("ssh-keyscan %s: no host keys (sshd never served them)", ip)
	}
	// ensure the file exists + is owned right (mkdir done in deriveProfile)
	if err := os.WriteFile(p.knownHosts, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", p.knownHosts, err)
	}
	return nil
}

// netJoinBoxImpl pushes the agent + a single-use ephemeral preauth key to
// the box and waits for it to come online. bash: net_join_box. A daybox
// exists ONLY on the net, so every step here is fatal: on any failure the
// box is left running and the caller reports it (summonUp no longer downs).
func netJoinBox(p *profile, ip string) error {
	uid, err := p.dep.headscaleUserID(p.netUser)
	if err != nil {
		return fmt.Errorf("net: no headscale user '%s': %w", p.netUser, err)
	}
	// purge any stale node holding this box's name (a leftover claims the
	// name, headscale dedupes to daybox-<profile>-1, and the online poll
	// goes blind). bash: the for-loop over net_nodes_json.
	if err := p.dep.purgeStaleNodes(p.serverName); err != nil {
		say("net: WARNING: could not purge stale nodes: %v", err)
	}
	key, err := p.dep.mintPreauthKey(uid)
	if err != nil {
		return fmt.Errorf("net: could not mint pre-auth key: %w", err)
	}
	say("net: enrolling box on the daybox net")
	sshOpts := []string{"-o", "UserKnownHostsFile=" + p.knownHosts, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	agentBin := p.dep.agentBin()
	serviceFile := filepath.Join(p.dep.repoDir, "remote", "daybox-agent.service")
	// copy the agent binary + service file (bash: scp_retry)
	scpArgs := append([]string{"-q"}, sshOpts...)
	scpArgs = append(scpArgs, agentBin, serviceFile, p.remoteUser+"@"+ip+":/tmp/")
	if c := exec.Command("scp", scpArgs...); c.Run() != nil {
		return fmt.Errorf("net: could not copy agent to box")
	}
	// install the agent + key (the key travels via stdin, never argv/ps)
	installCmd := fmt.Sprintf(`set -e
sudo install -m 755 /tmp/daybox-agent /usr/local/bin/daybox-agent
sudo install -d -m 700 /var/lib/daybox-agent
sudo install -m 600 /dev/stdin /var/lib/daybox-agent/authkey
printf 'DAYBOX_CONTROL=%s\n' '%s' | sudo tee /etc/default/daybox-agent >/dev/null
sudo cp /tmp/daybox-agent.service /etc/systemd/system/daybox-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now daybox-agent
rm -f /tmp/daybox-agent /tmp/daybox-agent.service`, p.netControlURL, p.netControlURL)
	sshArgs := append([]string{"ssh"}, sshOpts...)
	sshArgs = append(sshArgs, p.remoteUser+"@"+ip, installCmd)
	c := exec.Command(sshArgs[0], sshArgs[1:]...)
	c.Stdin = strings.NewReader(key + "\n")
	if c.Run() != nil {
		return fmt.Errorf("net: agent install failed on box")
	}
	// poll for the box to come online (bash: 30 attempts @ 2s)
	for i := 0; i < 30; i++ {
		nodes, _ := p.dep.headscaleNodesJSON()
		if addr := onlineNodeAddr(nodes, p.serverName); addr != "" {
			say("net: box online at %s (from an enrolled device: ssh over 'daybox-agent dial')", addr)
			// record the agent version this box was summoned with (status
			// shows it so a mixed-version fleet is visible after an upgrade)
			if b, err := exec.Command(p.dep.agentBin(), "version").Output(); err == nil {
				os.WriteFile(p.agentVersionFile(), bytesTrimSpace(b), 0o644)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("net: box never came online — check on box: systemctl status daybox-agent")
}

// headscaleNodesJSONImpl lists all net nodes as JSON. bash: net_nodes_json.
func (d *deployment) headscaleNodesJSON() (string, error) {
	out, err := exec.Command("headscale", "nodes", "list", "-o", "json").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// headscaleUserID resolves a net user's id. bash: net_user_id.
func (d *deployment) headscaleUserID(name string) (string, error) {
	out, err := exec.Command("headscale", "users", "list", "-o", "json").Output()
	if err != nil {
		return "", fmt.Errorf("headscale not reachable: %w", err)
	}
	var users []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Name == name {
			return fmt.Sprintf("%d", u.ID), nil
		}
	}
	return "", fmt.Errorf("no headscale user %q (run: daybox init)", name)
}

// mintPreauthKey creates a single-use ephemeral key. bash: headscale
// preauthkeys create --user $uid --ephemeral --expiration 15m.
func (d *deployment) mintPreauthKey(uid string) (string, error) {
	out, err := exec.Command("headscale", "preauthkeys", "create", "--user", uid, "--ephemeral", "--expiration", "15m").Output()
	if err != nil {
		return "", err
	}
	// headscale prints human CLI output; the key is the last whitespace token
	return strings.TrimSpace(strings.Fields(string(out))[len(strings.Fields(string(out)))-1]), nil
}

// purgeStaleNodes removes any node holding the box's name (leftover claims
// the name, headscale dedupes, and the online poll goes blind).
func (d *deployment) purgeStaleNodes(serverName string) error {
	nodes, err := d.headscaleNodesJSON()
	if err != nil {
		return err
	}
	var list []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(nodes), &list); err != nil {
		return err
	}
	for _, n := range list {
		if n.Name == serverName {
			if exec.Command("headscale", "nodes", "delete", "-i", fmt.Sprintf("%d", n.ID), "--force").Run() == nil {
				say("net: purged stale node '%s' (id %d)", serverName, n.ID)
			}
		}
	}
	return nil
}

// nodeIsOnlineImpl: is this profile's box registered AND online?
func nodeIsOnline(nodesJSON, hostname string) bool {
	return onlineNodeAddr(nodesJSON, hostname) != ""
}

// onlineNodeAddr returns the box's net IP if it's online, "" otherwise.
func onlineNodeAddr(nodesJSON, hostname string) string {
	var list []struct {
		Name        string   `json:"name"`
		Online      bool     `json:"online"`
		Connected   bool     `json:"connected"`
		IPAddresses []string `json:"ip_addresses"`
	}
	if err := json.Unmarshal([]byte(nodesJSON), &list); err != nil {
		return ""
	}
	for _, n := range list {
		if n.Name == hostname && (n.Online || n.Connected) && len(n.IPAddresses) > 0 {
			return n.IPAddresses[0]
		}
	}
	return ""
}

// providerFor builds the Provider for this profile's PROVIDER knob. Used by
// the real planeSshOps to re-probe the box's IP for the ssh follow-up.
func (d *deployment) providerFor(p *profile) Provider {
	prov, err := d.loadProvider(p.provider)
	if err != nil {
		return nil
	}
	return prov
}

// --- os-touching helpers (centralised so the testable core can be faked) ---

func writeFileImpl(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func readDirGlob(dir, pattern string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, pattern))
}

func bytesTrimSpace(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }
