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
	prof, rest := takeProfileName(args)
	if prof == "" {
		return "", rest
	}
	return " -p " + shq(prof), rest
}

// takeProfileName is takeProfile without the remote quoting — for laptop
// verbs that need the plain profile name itself.
func takeProfileName(args []string) (string, []string) {
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
	return prof, rest
}

// cmdDelegateP delegates a no-arg verb, forwarding a -p profile selection.
func cmdDelegateP(verb string, args []string) {
	prof, _ := takeProfile(args)
	delegate(remoteDaybox+prof+" "+verb, false)
}

// cmdProfile routes the `profile` group. The laptop-authority subverbs
// (profilecmd.go, proposalcmd.go — edit/proposals/accept/reject/propose)
// NEVER delegate — approval is a laptop-side action by design (§1e). The
// box/volume lifecycle subverbs (add/ls/use/rename/rm/seed) run on the
// plane when amPlane, else delegate.
func cmdProfile(args []string) {
	if len(args) > 0 {
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
			cmdProfilePropose(args[1:])
			return
		}
	}
	if amPlane() {
		cmdProfilePlane(args)
		return
	}
	cmd := remoteDaybox + " profile"
	for _, a := range args {
		cmd += " " + shq(a)
	}
	delegate(cmd, false)
}

// cmdProfilePlane runs the box/volume lifecycle subverbs locally on the
// plane (add/ls/use/rename/rm/seed). Unknown subverb -> usage.
func cmdProfilePlane(args []string) {
	dep := loadDeployment()
	sub := "ls"
	rest := []string{}
	if len(args) > 0 {
		sub = args[0]
		rest = args[1:]
	}
	name, rest := takeProfileName(rest)
	switch sub {
	case "add":
		stype := ""
		if len(rest) > 0 {
			stype = rest[0]
		}
		if name == "" && len(rest) > 0 {
			name = rest[0]
		}
		if name == "" {
			log.Fatal("usage: daybox profile add <name> [server-type]")
		}
		if err := profileAdd(dep, name, stype); err != nil {
			log.Fatal(err)
	}
	case "ls", "list", "":
		profileLs(dep, os.Stdout)
	case "use":
		if name == "" && len(rest) > 0 {
			name = rest[0]
		}
		if name == "" {
			log.Fatal("usage: daybox profile use <name>")
		}
		if err := profileUse(dep, name); err != nil {
			log.Fatal(err)
		}
	case "rename", "mv":
		new := ""
		if len(rest) > 0 {
			new = rest[0]
		}
		if name == "" || new == "" {
			log.Fatal("usage: daybox profile rename <old> <new>")
		}
		if err := profileRename(dep, name, new); err != nil {
			log.Fatal(err)
		}
	case "rm", "remove":
		purge := ""
		if len(rest) > 0 {
			purge = rest[0]
		}
		if name == "" && len(rest) > 0 {
			name = rest[0]
		}
		if name == "" {
			log.Fatal("usage: daybox profile rm <name> [--purge]")
		}
		if err := profileRm(dep, name, purge); err != nil {
			log.Fatal(err)
		}
	case "seed":
		s := "show"
		n := name
		if len(rest) > 0 {
			s = rest[0]
		}
		if len(rest) > 1 {
			n = rest[1]
		}
		if err := profileSeed(dep, s, n, os.Stdout); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown: profile %s  (add|ls|use|rename|rm|seed)", sub)
	}
}

// cmdUp: summon the big box, then ssh in — a fresh shell, any terminal.
// tmux is opt-in via `daybox attach`.
//
// Two roles, one binary (the unify-single-binary port): on the LAPTOP it
// delegates `up` to the control plane over ssh (the Hetzner token never
// leaves the plane); on the PLANE it does the summon locally via summonUp.
// Under ingress lockdown the box's public :22 is dark to everyone but the
// plane, so the laptop's follow-up shell also hops via the plane (delegate).
//
// Bug #3: the plane no longer prints `IP`/`HOSTKEY` to stdout. The laptop
// relies on the plane's exit code (0 = success) + the IP the plane says on
// stderr (the user sees it either way); no stdout contract to parse.
func cmdUp(args []string) {
	name, rest := takeProfileName(args)
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	detach := fs.Bool("detach", false, "summon but don't connect")
	fs.Parse(rest)
	serverType := ""
	if fs.NArg() > 0 {
		serverType = fs.Arg(0)
	}

	// PLANE role: do the summon here. No stdout IP/HOSTKEY contract (bug #3).
	if amPlane() {
		dep := loadDeployment()
		p, err := dep.deriveProfile(profileNameOrCurrent(dep, name))
		if err != nil {
			log.Fatal(err)
		}
		if serverType != "" {
			p.serverType = serverType
		}
		prov, err := dep.loadProvider(p.provider)
		if err != nil {
			log.Fatal(err)
		}
		if err := prov.CheckCredentials(); err != nil {
			log.Fatal(err)
		}
		if err := summonUp(p, prov, *detach, newPlaneSshOps(p)); err != nil {
			log.Fatal(err)
		}
		return
	}

	// LAPTOP role: delegate to the plane. The profile is frozen into
	// user_data at render time on the plane, so pending proposals get one
	// last non-blocking look here before the summon.
	host := mustControl()
	maybeOfferProposalReview(host, name)
	prof := ""
	if name != "" {
		prof = " -p " + shq(name)
	}
	cmd := remoteDaybox + prof + " up"
	if *detach {
		cmd += " --detach"
	}
	if serverType != "" {
		cmd += " " + shq(serverType)
	}
	say("summoning via %s (fresh boot takes ~60-90s)…", host)
	// delegate runs the remote cmd with a pty; the plane's `up` ssh'es
	// in itself when not --detach (bug #3: the plane owns the follow-up
	// shell). delegate exits with the remote's code on failure.
	// the plane ssh'd in already (bug #3: the plane owns the follow-up
	// shell when not --detach); --detach just reports. Non-detach delegates
	// (interactive pty).
	if *detach {
		delegate(cmd, false) // non-tty: the plane just reports
		say("box is up — connect with: daybox ssh   (tmux: daybox attach)")
		return
	}
	delegate(cmd, true) // tty: the plane ssh'es in interactively
}

// profileNameOrCurrent resolves the -p flag to a profile name, falling back
// to the current_profile file, then 'default'. Returns the resolved name.
func profileNameOrCurrent(dep *deployment, explicit string) string {
	name, _ := dep.currentProfile(explicit)
	return name
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

// cmdSetup: one-time bootstrap of the current profile (register keys,
// seed profile.toml, create the volume). Plane-only; the laptop delegates.
func cmdSetup(args []string) {
	if amPlane() {
		if err := setup(loadDeployment()); err != nil {
			log.Fatal(err)
		}
		return
	}
	prof, _ := takeProfile(args)
	delegate(remoteDaybox+prof+" setup", false)
}

// cmdStatus: the whole deployment in one command. Role-gated: the plane
// prints every profile's box + the net table; the laptop delegates.
func cmdStatus(args []string) {
	name, _ := takeProfileName(args)
	if amPlane() {
		dep := loadDeployment()
		explicit := ""
		if name != "" {
			explicit = name
		}
		statusRun(dep, os.Stdout, explicit)
		return
	}
	prof := ""
	if name != "" {
		prof = " -p " + shq(name)
	}
	delegate(remoteDaybox+prof+" status", false)
}

// cmdReap: idle-reaper entry (run by the systemd timer every 5min on the
// plane). The plane loops every profile + reapOne; the laptop delegates.
func cmdReap(args []string) {
	if amPlane() {
		reapRun(loadDeployment())
		return
	}
	prof, _ := takeProfile(args)
	delegate(remoteDaybox+prof+" reap", false)
}

// cmdDown: delete the box now (billing stops). Role-gated like up: the plane
// detaches the volume + reaps + drops the net node; the laptop delegates
// (the Hetzner token lives on the plane).
func cmdDown(args []string) {
	name, _ := takeProfileName(args)
	if amPlane() {
		dep := loadDeployment()
		p, err := dep.deriveProfile(profileNameOrCurrent(dep, name))
		if err != nil {
			log.Fatal(err)
		}
		prov, err := dep.loadProvider(p.provider)
		if err != nil {
			log.Fatal(err)
		}
		if err := downBox(p, prov, newPlaneDownOps(p)); err != nil {
			log.Fatal(err)
		}
		return
	}
	prof := ""
	if name != "" {
		prof = " -p " + shq(name)
	}
	delegate(remoteDaybox+prof+" down", false)
}
