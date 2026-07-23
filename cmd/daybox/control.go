package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The control plane owns summon/reap/state; the laptop delegates to its
// daybox over ssh ("the CLI lives where the user is", the control logic
// lives where the credentials are). Remote path is ~-relative because
// non-interactive ssh shells don't have ~/.local/bin on PATH.
const remoteDaybox = "~/.local/bin/daybox"

func mustControl() string {
	c := loadConfig().controlHost()
	if c == "" {
		log.Fatal("no control plane configured — run: daybox init")
	}
	return c
}

// controlKnownHosts holds host keys of provisioned control planes, pinned
// by explicit keyscan at provision time. Kept out of ~/.ssh/known_hosts:
// Hetzner recycles IPs aggressively, and a stale global entry turns into a
// hard verification failure on the next unlucky box.
func controlKnownHosts() string { return filepath.Join(confDir(), "control_known_hosts") }

// sshIdentity, when set, pins every control-plane ssh to exactly this
// private key (with IdentitiesOnly, so ssh-agent keys and default-identity
// resolution can't shoulder in). init --provision sets it to the key it
// registered on the fresh box, so root user-setup doesn't fail
// Permission-denied when an agent offers unrelated keys first or when $HOME
// diverges from the passwd home. Empty for adopt mode + everyday verbs,
// which keep the user's own ssh auth untouched.
var sshIdentity string

// sshOpts: when the dedicated file exists, consult it FIRST, then fall back
// to the user's normal known_hosts — so adopt-mode ssh aliases keep working
// off the global file untouched.
func sshOpts(batch bool) []string {
	opts := []string{"-o", "ConnectTimeout=10"}
	if batch {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	if sshIdentity != "" {
		opts = append(opts, "-i", sshIdentity, "-o", "IdentitiesOnly=yes")
	}
	if kh := controlKnownHosts(); fileExists(kh) {
		opts = append(opts, "-o",
			"UserKnownHostsFile="+kh+" "+filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts"))
	}
	return opts
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// sshCapture runs a command on host, returning stdout (stderr streams through).
func sshCapture(host, cmd string) (string, error) {
	c := exec.Command("ssh", append(sshOpts(true), host, cmd)...)
	c.Stderr = os.Stderr
	out, err := c.Output()
	return string(out), err
}

// sshRun streams everything through — for delegated verbs and setup steps.
func sshRun(host string, cmd string) error {
	c := exec.Command("ssh", append(sshOpts(true), host, cmd)...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	return c.Run()
}

// delegate runs a command on the control plane with this terminal wired
// through; tty gives interactive verbs a remote pty.
func delegate(cmd string, tty bool) {
	host := mustControl()
	opts := sshOpts(!tty)
	if tty {
		opts = append(opts, "-t")
	}
	c := exec.Command("ssh", append(opts, host, cmd)...)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		log.Fatalf("control plane unreachable (%s): %v", host, err)
	}
}

func cmdDelegate(verb string) { delegate(remoteDaybox+" "+verb, false) }

// takeProfile pulls a -p/--profile <name> flag out of args (it may appear
// anywhere) and returns the remote flag fragment (" -p name", already quoted)
// plus the remaining args. The control plane's daybox accepts -p ahead of any
// verb, so the laptop just forwards it — a profile is a whole daybox
// (README: Profiles).
func takeProfile(args []string) (string, []string) {
	prof := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-p" || a == "--profile":
			if i+1 < len(args) {
				prof = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-p="):
			prof = strings.TrimPrefix(a, "-p=")
		case strings.HasPrefix(a, "--profile="):
			prof = strings.TrimPrefix(a, "--profile=")
		default:
			rest = append(rest, a)
		}
	}
	if prof == "" {
		return "", rest
	}
	return " -p " + shq(prof), rest
}

// cmdDelegateP delegates a no-arg verb, forwarding a -p profile selection.
func cmdDelegateP(verb string, args []string) {
	prof, _ := takeProfile(args)
	delegate(remoteDaybox+prof+" "+verb, false)
}

// cmdProfile forwards the `profile` command group (add|ls|use|rename|rm)
// straight to the control plane, quoting each token.
func cmdProfile(args []string) {
	cmd := remoteDaybox + " profile"
	for _, a := range args {
		cmd += " " + shq(a)
	}
	delegate(cmd, false)
}

// cmdUp: summon via the control plane, then ssh in — a fresh shell, any
// terminal. tmux is opt-in via `daybox attach`.
// The shell hops via the control plane like `ssh`/`attach` do: under
// ingress lockdown the box's public :22 is dark to everyone else, so a
// direct connect only ever worked in the pre-ufw window of a fresh boot.
func cmdUp(args []string) {
	prof, rest := takeProfile(args)
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	detach := fs.Bool("detach", false, "summon but don't connect")
	fs.Parse(rest)
	host := mustControl()

	cmd := remoteDaybox + prof + " up"
	if fs.NArg() > 0 {
		cmd += " " + shq(fs.Arg(0)) // server type override
	}
	say("summoning via %s (fresh boot takes ~60-90s)…", host)
	out, err := sshCapture(host, cmd)
	if err != nil {
		log.Fatal("summon failed (control plane output above)")
	}

	var ip string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "IP ") {
			ip = strings.TrimSpace(strings.TrimPrefix(line, "IP "))
		}
	}
	if ip == "" {
		log.Fatalf("could not parse the control plane's answer:\n%s", out)
	}

	if *detach {
		say("box is up at %s — connect with: daybox ssh   (tmux: daybox attach)", ip)
		return
	}
	// the follow-up shell must target the SAME profile's box
	delegate(remoteDaybox+prof+" ssh", true)
}

// shq single-quotes s for a remote POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// cmdSSH: a fresh shell (or one-off command) on the box, hopping via the
// control plane — works even when the box only allows the control plane in.
// tmux is a user choice: `daybox attach` is the persistent session.
func cmdSSH(args []string) {
	prof, rest := takeProfile(args)
	cmd := remoteDaybox + prof + " ssh"
	for _, a := range rest {
		// quote for the control plane's shell so the whole command reaches
		// the box intact (unquoted, `; x` would run x on the control plane)
		cmd += " " + shq(a)
	}
	delegate(cmd, true)
}

// cmdAttach: the shared persistent tmux session — the only verb that
// starts tmux; everything else lands in a plain shell.
func cmdAttach(args []string) {
	prof, _ := takeProfile(args)
	delegate(remoteDaybox+prof+" attach", true)
}
