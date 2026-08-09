// daybox — the unified CLI. One static binary, every machine.
//
// On a laptop it is the whole product surface: `init` bootstraps (or
// adopts) a control plane, `up` summons and sshes in, `enroll` joins this
// device to the private net. Control-plane verbs are delegated over ssh to
// the control plane's own daybox; net connectivity is an embedded
// userspace tsnet node (no TUN, no root, no vendor apps — the README).
//
// The same binary runs on devboxes as `daybox-agent join` (systemd),
// proxying net-side :22 to the local sshd.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
)

// version is stamped at release time via -ldflags "-X main.version=vX.Y.Z".
// Unstamped local builds report "dev".
var version = "dev"

// usageText is a constant so tests can assert against it without writing.
const usageText = `usage: daybox <command> [flags]

A profile is a whole daybox (own server + volume + creds); add -p <name> to
any everyday verb to target one (default: 'default'). See README: Profiles.

everyday:
  up [-p prof] [type]  summon the big box (or reconnect) and ssh in (fresh shell)
  ssh [-p prof] [cmd]  FRESH shell on the running box (tmux only if you start it)
  attach [-p prof]     the persistent tmux session (the only verb that starts tmux)
  status [-p prof]     everything: each profile's box + net members (-p: one profile)
  down [-p prof]       delete the box now (billing stops)
  profile ...          add|ls|use|rename|rm your daybox profiles
  profile edit [name]  edit a profile's seed in $EDITOR (validated, applied next up)
  profile proposals    review box-proposed seed changes (accept <id> | reject <id>)
  profile propose      (on a box) propose detected tool/package drift to the profile

setup:
  init             set up (or adopt) a control plane + enroll this device
  upgrade          move the control plane to a newer release (no interview)
  enroll           (re-)enroll this device on your private net

plumbing (used by machines more than people):
  dial HOST PORT   ssh ProxyCommand over the net
  join             devbox-side net node (runs under systemd on boxes)
  relay            control-plane proposal intake (runs under systemd there)
  ip               bring the net node up, print its address
  version          print the binary version (-v / --version also work)

Deployment config lives in ~/.config/daybox/config.local — written by
'daybox init', documented in the README (Configuration).
`

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("daybox: ")
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable core: it routes the verb (profile flags are parsed
// inside each command) and returns the process exit code. Streams are
// parameters so dispatch-level behavior — version flags, usage — is unit-
// testable without building a binary. Commands that delegate or do real work
// may call os.Exit/log.Fatal themselves (unchanged from the pre-refactor
// behavior); run's return value is authoritative only for the simple paths.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "init":
		cmdInit(rest)
	case "upgrade":
		cmdUpgrade(rest)
	case "enroll":
		cmdEnroll(rest)
	case "up":
		cmdUp(rest)
	case "ssh":
		cmdSSH(rest)
	case "attach":
		cmdAttach(rest)
	case "status":
		cmdStatus(rest)
	case "down":
		cmdDown(rest)
	case "net": // deprecated spelling — folded into status; kept for muscle memory
		cmdDelegate("net")
	case "reap":
		cmdReap(rest)
	case "profile":
		cmdProfile(rest)
	case "join":
		cmdJoin(rest)
	case "relay":
		cmdRelay(rest)
	case "dial":
		cmdDial(rest)
	case "ip":
		cmdIP(rest)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usage(stderr)
		return 0
	default:
		usage(stderr)
		return 2
	}
	return 0
}

// say narrates progress to stderr — the CLI explains what it's doing and
// why, because onboarding IS the product.
func say(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}
