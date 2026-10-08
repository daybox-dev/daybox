package main

// The control plane owns summon/reap/state; the laptop delegates to its
// daybox over ssh ("the CLI lives where the user is", the control logic
// lives where the credentials are). Remote path is ~-relative because
// non-interactive ssh shells don't have ~/.local/bin on PATH.
//
// Every remote command the laptop hands the plane is produced by
// Parsed.String (grammar.go) and re-parsed there by Parse — never
// hand-concatenated. That round-trip is what makes `daybox <verb>
// -p <profile>` work from the laptop regardless of where -p sits, the
// class of bug that broke every laptop-side profile-flagged verb in
// v0.3.0/v0.3.1 (the plane's args[0] dispatch rejected a leading -p).

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

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

// delegate runs a parsed command on the control plane with this terminal
// wired through; tty gives interactive verbs a remote pty. The command is
// the grammar's canonical form (Parsed.String), never a hand-built string,
// so the plane re-parses exactly what the laptop intended. Retries on a
// transport-level failure (see sshRetry) — safe because a 255 means the
// connection never came up, so no prior shell exists to duplicate.
func delegate(p Parsed, tty bool) {
	host := mustControl()
	cmd := remoteDaybox + " " + p.String()
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

// sshRunningBox is the plane-side half of `daybox ssh`/`attach`: probe the
// running box for profile p, then ssh -t into it (the control plane is
// allowlisted even under ingress lockdown), forwarding cmd as the remote
// command — empty for an interactive shell. The laptop delegates these
// verbs here, so the box's public :22 never needs to face the laptop.
// Mirrors bash cmd_ssh/cmd_attach (cmd_attach was cmd_ssh with the tmux
// wrapper as the command); the Go port had dropped this branch, so the
// plane-side `daybox ssh`/`attach` delegated back into a fatal mustControl.
func sshRunningBox(dep *deployment, p *profile, cmd []string) error {
	prov, err := dep.loadProvider(p.provider)
	if err != nil {
		return err
	}
	s, err := prov.Probe(p.serverName)
	if err != nil || s == nil {
		return fmt.Errorf("no big box running — summon with: daybox up")
	}
	args := append([]string{"ssh"}, sshBoxOptsTty(p)...)
	args = append(args, p.remoteUser+"@"+s.IP)
	args = append(args, cmd...)
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

// profileUsageText is the `daybox profile` group's own usage, printed for
// `profile --help`/`-h`/`help` and for an unknown subverb. The top-level
// usage only sketches the profile group; this lists every subverb routed
// here (edit/proposals/accept/reject/propose are laptop-authority) and in
// cmdProfilePlane (add/ls/use/rename/rm/seed).
const profileUsageText = `usage: daybox profile <subcommand> [args]

A profile is a whole daybox (own server + volume + creds). Add -p <name> to
the everyday verbs (up/ssh/attach/down/status) to target one (default:
'default').

subcommands:
  add <name> [type]              create a profile (writes config + creates its volume)
  ls                             list every profile: box state + volume size
  use <name>                     set the profile bare commands resolve to
  rename <old> <new>             rename (box must be down)
  rm <name> [--purge]            reap box; keep the volume unless --purge
  seed [show|init|path] [<name>] manage the profile's seed (what a box carries)
  edit [name]                    edit a profile's seed in $EDITOR (applied next up)
  proposals                      review box-proposed seed changes
  accept <id>                    accept a proposal
  reject <id>                    reject a proposal
  propose                        (on a box) propose detected drift to the profile
`

func profileUsage() { fmt.Fprint(os.Stderr, profileUsageText) }

// isProfileHelp reports whether tok is a help spelling the profile group
// honours at its top level (--help/-h/help). The grammar does not hoist
// these (only -p/--profile is global), so they arrive in Rest.
func isProfileHelp(tok string) bool {
	return tok == "--help" || tok == "-h" || tok == "help"
}

// isHelpFlag reports whether tok is a help FLAG (-h/--help). After a
// subverb only the flag spellings mean help — a bare "help" there is a
// positional like any other.
func isHelpFlag(tok string) bool { return tok == "--help" || tok == "-h" }

// wantsProfileHelp reports whether a `profile` invocation is a help request:
// a help spelling as the subverb, or a help flag anywhere after it. The
// second case is the `daybox profile add --help` bug — the flag used to
// become the profile name, and add created (and billed) a volume for it.
// propose is exempt: it parses its own flags and prints its own usage.
func wantsProfileHelp(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	if isProfileHelp(rest[0]) {
		return true
	}
	return rest[0] != "propose" && slices.ContainsFunc(rest[1:], isHelpFlag)
}

// cmdProfile routes the `profile` group. The seed-authority subverbs
// (profilecmd.go, proposalcmd.go — edit/proposals/accept/reject/propose)
// never delegate: they run where the user is, against the seed store
// (over ssh from the laptop, local files on the plane) — a box can only
// propose (§1e). The box/volume lifecycle subverbs (add/ls/use/rename/rm/
// seed) run on the plane when amPlane, else delegate. Help and argument
// checks run before any of that, so both roles agree and a laptop refuses
// a bad invocation before it reaches the plane (whatever binary it runs).
func cmdProfile(p Parsed) {
	rest := p.Rest()
	// `daybox profile --help`, `profile <sub> --help` (or -h): print the
	// group usage — never treat the flag as an argument.
	if wantsProfileHelp(rest) {
		profileUsage()
		return
	}
	if len(rest) > 0 {
		switch rest[0] {
		case "edit":
			name, err := profileEditTarget(p.Global("profile"), rest[1:])
			if err != nil {
				log.Fatal(err)
			}
			cmdProfileEdit(name)
			return
		case "proposals":
			cmdProfileProposals(rest[1:])
			return
		case "accept":
			cmdProfileAccept(rest[1:])
			return
		case "reject":
			cmdProfileReject(rest[1:])
			return
		case "propose":
			cmdProfilePropose(rest[1:])
			return
		}
	}
	pc, err := parseProfileArgs(p)
	if err != nil {
		log.Fatal(err)
	}
	if amPlane() {
		runProfilePlane(loadDeployment(), pc)
		return
	}
	delegate(p, false)
}

// profileCmd is a validated lifecycle invocation (add/ls/use/rename/rm/
// seed), produced only by parseProfileArgs.
type profileCmd struct {
	sub   string // canonical subverb: add|ls|use|rename|rm|seed
	name  string // the profile acted on; "" only for ls and a bare seed
	arg   string // add: server type; rename: the new name; seed: show|init|path
	purge bool   // rm --purge
}

// profileSubUsage is each lifecycle subverb's one-line usage, quoted in the
// errors parseProfileArgs returns.
var profileSubUsage = map[string]string{
	"add":    "daybox profile add <name> [server-type]",
	"ls":     "daybox profile ls",
	"use":    "daybox profile use <name>",
	"rename": "daybox profile rename <old> <new>",
	"rm":     "daybox profile rm <name> [--purge]",
	"seed":   "daybox profile seed [show|init|path] [<name>]",
}

// parseProfileArgs turns `profile <sub> ...` into a profileCmd, refusing
// anything it can't read unambiguously. Role-independent (no I/O): the
// laptop runs it before delegating and the plane before acting.
//
// The profile acted on comes from -p (hoisted by the grammar) OR the first
// positional — never both, and never the current_profile/'default'
// fallback the everyday verbs use: a destructive verb with no explicit
// target, or with two candidate targets, is a usage error. Flag-shaped
// tokens are never names: --purge is rm's one flag (any position), and any
// other flag is an error — so a typo like --prge, or a --help that slipped
// past wantsProfileHelp, can't become the profile acted on.
func parseProfileArgs(p Parsed) (profileCmd, error) {
	sub := "ls"
	toks := p.Rest()
	if len(toks) > 0 {
		sub, toks = toks[0], toks[1:]
	}
	switch sub {
	case "", "list":
		sub = "ls"
	case "mv":
		sub = "rename"
	case "remove":
		sub = "rm"
	}
	usage, known := profileSubUsage[sub]
	if !known {
		return profileCmd{}, fmt.Errorf("unknown: profile %s  (add|ls|use|rename|rm|seed|edit|proposals|accept|reject|propose)", sub)
	}
	pc := profileCmd{sub: sub}
	var pos []string
	for _, t := range toks {
		switch {
		case sub == "rm" && t == "--purge":
			pc.purge = true
		case isFlagTok(t):
			return profileCmd{}, fmt.Errorf("profile %s: unknown flag '%s'\nusage: %s", sub, t, usage)
		default:
			pos = append(pos, t)
		}
	}
	flagName := p.Global("profile")
	// target splits off the acted-on profile; tail is what remains for the
	// subverb's own operands. maxTail bounds tail, so -p plus a positional
	// name (or any stray extra) is refused rather than guessed at.
	target := func(minTail, maxTail int) ([]string, error) {
		tail := pos
		if flagName != "" {
			pc.name = flagName
		} else if len(pos) > 0 {
			pc.name, tail = pos[0], pos[1:]
		}
		if len(tail) > maxTail {
			if flagName != "" {
				return nil, fmt.Errorf("profile %s: unexpected '%s' — the profile is already -p %s\nusage: %s", sub, tail[maxTail], flagName, usage)
			}
			return nil, fmt.Errorf("profile %s: unexpected '%s'\nusage: %s", sub, tail[maxTail], usage)
		}
		if pc.name == "" || len(tail) < minTail {
			return nil, fmt.Errorf("usage: %s", usage)
		}
		if !validProfileName(pc.name) {
			return nil, fmt.Errorf("invalid profile '%s' (%s)", pc.name, profileNameRule)
		}
		return tail, nil
	}
	switch sub {
	case "ls":
		if len(pos) > 0 {
			return profileCmd{}, fmt.Errorf("usage: %s", usage)
		}
	case "add":
		tail, err := target(0, 1)
		if err != nil {
			return profileCmd{}, err
		}
		if len(tail) == 1 {
			pc.arg = tail[0]
		}
	case "use", "rm":
		if _, err := target(0, 0); err != nil {
			return profileCmd{}, err
		}
	case "rename":
		tail, err := target(1, 1)
		if err != nil {
			return profileCmd{}, err
		}
		pc.arg = tail[0]
		if !validProfileName(pc.arg) {
			return profileCmd{}, fmt.Errorf("invalid profile '%s' (%s)", pc.arg, profileNameRule)
		}
	case "seed":
		// seed [show|init|path] [<name>]: the name is the second positional
		// or -p, not both; profileSeed defaults a missing one to 'default'.
		pc.arg = "show"
		if len(pos) > 0 {
			pc.arg = pos[0]
		}
		pc.name = flagName
		if len(pos) > 1 {
			if flagName != "" {
				return profileCmd{}, fmt.Errorf("profile seed: got -p %s AND '%s' — name one profile\nusage: %s", flagName, pos[1], usage)
			}
			pc.name = pos[1]
		}
		if len(pos) > 2 {
			return profileCmd{}, fmt.Errorf("usage: %s", usage)
		}
		if pc.name != "" && !validProfileName(pc.name) {
			return profileCmd{}, fmt.Errorf("invalid profile '%s' (%s)", pc.name, profileNameRule)
		}
	}
	return pc, nil
}

// profileEditTarget resolves `profile edit [name]`'s profile: -p or the
// positional, not both; neither means the current profile (""), which is
// fine for edit — it's validated and backed up, not destructive.
func profileEditTarget(flagName string, toks []string) (string, error) {
	const usage = "usage: daybox profile edit [name]"
	var pos []string
	for _, t := range toks {
		if isFlagTok(t) {
			return "", fmt.Errorf("profile edit: unknown flag '%s'\n%s", t, usage)
		}
		pos = append(pos, t)
	}
	switch {
	case len(pos) > 1, len(pos) == 1 && flagName != "":
		return "", fmt.Errorf("profile edit takes one profile name\n%s", usage)
	case len(pos) == 1:
		return pos[0], nil
	}
	return flagName, nil
}

// cmdProfilePlane runs a lifecycle subverb locally on the plane: the same
// parse cmdProfile applies, then runProfilePlane.
func cmdProfilePlane(p Parsed) {
	pc, err := parseProfileArgs(p)
	if err != nil {
		log.Fatal(err)
	}
	runProfilePlane(loadDeployment(), pc)
}

// runProfilePlane executes a parsed lifecycle subverb (add/ls/use/rename/
// rm/seed) on the plane.
func runProfilePlane(dep *deployment, pc profileCmd) {
	var err error
	switch pc.sub {
	case "add":
		err = profileAdd(dep, pc.name, pc.arg)
	case "ls":
		profileLs(dep, os.Stdout)
	case "use":
		err = profileUse(dep, pc.name)
	case "rename":
		err = profileRename(dep, pc.name, pc.arg)
	case "rm":
		err = profileRm(dep, pc.name, pc.purge)
	case "seed":
		err = profileSeed(dep, pc.arg, pc.name, os.Stdout)
	}
	if err != nil {
		log.Fatal(err)
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
func cmdUp(p Parsed) {
	name := p.Global("profile")
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	detach := fs.Bool("detach", false, "summon but don't connect")
	fs.Parse(p.Rest())
	serverType := ""
	if fs.NArg() > 0 {
		serverType = fs.Arg(0)
	}

	// PLANE role: do the summon here. No stdout IP/HOSTKEY contract (bug #3).
	if amPlane() {
		dep := loadDeployment()
		prof, err := dep.requireProfile(name)
		if err != nil {
			log.Fatal(err)
		}
		if serverType != "" {
			prof.serverType = serverType
		}
		prov, err := dep.loadProvider(prof.provider)
		if err != nil {
			log.Fatal(err)
		}
		if err := prov.CheckCredentials(); err != nil {
			log.Fatal(err)
		}
		if err := summonUp(prof, prov, *detach, newPlaneSshOps(prof)); err != nil {
			log.Fatal(err)
		}
		return
	}

	// LAPTOP role: delegate to the plane. The profile is frozen into
	// user_data at render time on the plane, so pending proposals get one
	// last non-blocking look here before the summon. The verb's own tokens
	// (--detach, a server type) ride in p.String; the laptop only needs to
	// know --detach to choose a pty for the plane's follow-up shell.
	host := mustControl()
	maybeOfferProposalReview(host, name)
	say("summoning via %s (fresh boot takes ~60-90s)…", host)
	if *detach {
		delegate(p, false) // non-tty: the plane just reports
		say("box is up — connect with: daybox ssh   (tmux: daybox attach)")
		return
	}
	delegate(p, true) // tty: the plane ssh'es in interactively
}

// cmdSSH: a fresh shell (or one-off command) on the box, hopping via the
// control plane — works even when the box only allows the control plane in.
// tmux is a user choice: `daybox attach` is the persistent session.
func cmdSSH(p Parsed) {
	// PLANE role: ssh into the running box (bash cmd_ssh ran here). The
	// laptop delegates `daybox ssh` here, so this is the box-facing half.
	if amPlane() {
		dep := loadDeployment()
		prof, err := dep.requireProfile(p.Global("profile"))
		if err != nil {
			log.Fatal(err)
		}
		if err := sshRunningBox(dep, prof, p.Rest()); err != nil {
			log.Fatal(err)
		}
		return
	}
	delegate(p, true)
}

// cmdAttach: the shared persistent tmux session — the only verb that
// starts tmux; everything else lands in a plain shell.
func cmdAttach(p Parsed) {
	// PLANE role: ssh in running the tmux wrapper (bash cmd_attach was
	// cmd_ssh with /home/$USER/.local/bin/devbox-tmux as the command).
	if amPlane() {
		dep := loadDeployment()
		prof, err := dep.requireProfile(p.Global("profile"))
		if err != nil {
			log.Fatal(err)
		}
		cmd := []string{"/home/" + prof.remoteUser + "/.local/bin/devbox-tmux"}
		if err := sshRunningBox(dep, prof, cmd); err != nil {
			log.Fatal(err)
		}
		return
	}
	delegate(p, true)
}

// cmdSetup: one-time bootstrap of the current profile (register keys,
// seed profile.toml, create the volume). Plane-only; the laptop delegates.
func cmdSetup(p Parsed) {
	if amPlane() {
		if err := setup(loadDeployment()); err != nil {
			log.Fatal(err)
		}
		return
	}
	delegate(p, false)
}

// cmdStatus: the whole deployment in one command. Role-gated: the plane
// prints every profile's box + the net table; the laptop delegates.
func cmdStatus(p Parsed) {
	name := p.Global("profile")
	if amPlane() {
		dep := loadDeployment()
		explicit := ""
		if name != "" {
			explicit = name
		}
		statusRun(dep, os.Stdout, explicit)
		return
	}
	delegate(p, false)
}

// cmdReap: idle-reaper entry (run by the systemd timer every 5min on the
// plane). The plane loops every profile + reapOne; the laptop delegates.
func cmdReap(p Parsed) {
	if amPlane() {
		reapRun(loadDeployment())
		return
	}
	delegate(p, false)
}

// cmdDown: delete the box now (billing stops). Role-gated like up: the plane
// detaches the volume + reaps + drops the net node; the laptop delegates
// (the Hetzner token lives on the plane).
func cmdDown(p Parsed) {
	name := p.Global("profile")
	if amPlane() {
		dep := loadDeployment()
		prof, err := dep.requireProfile(name)
		if err != nil {
			log.Fatal(err)
		}
		prov, err := dep.loadProvider(prof.provider)
		if err != nil {
			log.Fatal(err)
		}
		if err := downBox(prof, prov, newPlaneDownOps(prof)); err != nil {
			log.Fatal(err)
		}
		return
	}
	delegate(p, false)
}
