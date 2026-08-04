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
	"log"
	"os"
)

// version is stamped at release time via -ldflags "-X main.version=vX.Y.Z".
// Unstamped local builds report "dev".
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `usage: daybox <command> [flags]

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

setup:
  init             set up (or adopt) a control plane + enroll this device
  upgrade          move the control plane to a newer release (no interview)
  enroll           (re-)enroll this device on your private net

plumbing (used by machines more than people):
  dial HOST PORT   ssh ProxyCommand over the net
  join             devbox-side net node (runs under systemd on boxes)
  relay            control-plane proposal intake (runs under systemd there)
  ip               bring the net node up, print its address
  version          print the binary version

Deployment config lives in ~/.config/daybox/config.local — written by
'daybox init', documented in the README (Configuration).
`)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("daybox: ")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "init":
		cmdInit(args)
	case "upgrade":
		cmdUpgrade(args)
	case "enroll":
		cmdEnroll(args)
	case "up":
		cmdUp(args)
	case "ssh":
		cmdSSH(args)
	case "attach":
		cmdAttach(args)
	case "status":
		cmdDelegateP("status", args)
	case "down":
		cmdDelegateP("down", args)
	case "net": // deprecated spelling — folded into status; kept for muscle memory
		cmdDelegate("net")
	case "profile":
		cmdProfile(args)
	case "join":
		cmdJoin(args)
	case "relay":
		cmdRelay(args)
	case "dial":
		cmdDial(args)
	case "ip":
		cmdIP(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

// say narrates progress to stderr — the CLI explains what it's doing and
// why, because onboarding IS the product.
func say(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}
