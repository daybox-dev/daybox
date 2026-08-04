package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	opts := []string{
		"-o", "ConnectTimeout=10",
		// detect a wedged-but-established connection faster than TCP's
		// ~2h default: 3 unanswered 15s keepalives => ssh exits (255), which
		// sshRetry then rides out. Bounds long-lived delegate sessions
		// (attach) so they don't hang on a dead control plane.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
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

// sshCapture runs a command on host, returning stdout (stderr streams
// through). Retries on a transport-level failure (see sshRetry).
func sshCapture(host, cmd string) (string, error) {
	var out []byte
	err := sshRetry("control plane", func() error {
		c := exec.Command("ssh", append(sshOpts(true), host, cmd)...)
		c.Stderr = os.Stderr
		var e error
		out, e = c.Output()
		return e
	})
	return string(out), err
}

// sshRun streams everything through — for delegated verbs and setup steps.
// Retries on a transport-level failure (see sshRetry).
func sshRun(host string, cmd string) error {
	return sshRetry("control plane", func() error {
		c := exec.Command("ssh", append(sshOpts(true), host, cmd)...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.Stdin = os.Stdin
		return c.Run()
	})
}

// exitCoder is implemented by *exec.ExitError (and by test fakes): ssh's
// process-exit code distinguishes a transport failure from a remote command
// failure without parsing locale-dependent stderr.
type exitCoder interface{ ExitCode() int }

// sshTransient reports whether an ssh failure is transport-level — the
// connection never came up — vs. a remote command that ran and exited
// non-zero. OpenSSH exits 255 for connect/auth/host-key failures and
// propagates the remote command's own exit code otherwise, so 255 is the
// one a retry can fix (a momentary cloud-network blip); any other code
// means the command itself failed and will fail identically next time.
func sshTransient(err error) bool {
	var ec exitCoder
	return errors.As(err, &ec) && ec.ExitCode() == 255
}

// sshRetry runs an ssh/scp op, retrying it when it fails at the transport
// layer (ssh exit 255), backing off between attempts. A freshly-created
// control plane — like any cloud box — can drop a SYN now and then, and the
// default ConnectTimeout=10 with no retry has aborted `daybox init` at its
// very last step: the plane provisioned and configured fine, then a single
// ~10s blip on the final `daybox setup` ssh left it provisioned but
// unsummonable (no volume, no registered keys) and the laptop unconfigured.
// Every remote op daybox runs is idempotent, so a retry is always safe; a
// non-transient failure (the remote command itself exited non-255) is
// returned immediately and never retried.
func sshRetry(label string, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = fn()
		if err == nil || !sshTransient(err) {
			return err
		}
		if attempt >= sshMaxAttempts {
			say("  (%s unreachable after %d attempts — giving up)", label, attempt)
			return err
		}
		backoff := time.Duration(1<<attempt) * time.Second // 2s, then 4s
		say("  (%s unreachable — retry %d/%d in %s)", label, attempt, sshMaxAttempts-1, backoff)
		sshRetrySleep(backoff)
	}
}

// sshMaxAttempts is the total attempt count for sshRetry (1 initial + 2
// retries). Bounds the worst case for a dead host at ~3× ConnectTimeout plus
// backoff, so init never hangs for many minutes on a plane that is gone for
// good — while still riding out the ordinary few-seconds blip.
const sshMaxAttempts = 3

// sshRetrySleep is the backoff sleeper, overridable in tests.
var sshRetrySleep = time.Sleep

// delegate runs a command on the control plane with this terminal wired
// through; tty gives interactive verbs a remote pty. Retries on a
// transport-level failure (see sshRetry) — safe because a 255 means the
// connection never came up, so no prior shell exists to duplicate.
func delegate(cmd string, tty bool) {
	host := mustControl()
	err := sshRetry("control plane", func() error {
		opts := sshOpts(!tty)
		if tty {
			opts = append(opts, "-t")
		}
		c := exec.Command("ssh", append(opts, host, cmd)...)
		c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
		return c.Run()
	})
	if err != nil {
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

// cmdProfile forwards the `profile` command group (add|ls|use|rename|rm|seed)
// straight to the control plane, quoting each token — except `edit`, which
// runs $EDITOR here on the laptop (profilecmd.go): the plane stores the
// seed, the laptop authors it.
func cmdProfile(args []string) {
	if len(args) > 0 {
		// the laptop-authority subverbs (profilecmd.go, proposalcmd.go):
		// editing and proposal review never delegate — approval is a
		// laptop-side action by design (§1e).
		switch args[0] {
		case "edit":
			cmdProfileEdit(args[1:])
			return
		case "proposals":
			cmdProfileProposals(args[1:])
			return
		case "accept":
			cmdProfileAccept(args[1:])
			return
		case "reject":
			cmdProfileReject(args[1:])
			return
		case "propose":
			// box-side: reads the local seed, talks to the relay — the
			// only profile subverb that runs on a summoned box
			cmdProfilePropose(args[1:])
			return
		}
	}
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
