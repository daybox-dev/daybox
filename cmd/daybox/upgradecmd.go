package main

// upgradecmd.go — `daybox upgrade`: move an existing deployment's control
// plane to a newer release. init answers "make me a deployment"; upgrade
// answers "my deployment is fine, its code is old" — so it runs with NO
// interview, learns everything from config.local, and never touches what
// *is* the deployment (identity, net, token, volumes, profiles).
//
// In order:
//   1. resolve the payload exactly like init (a signed, checksum-verified
//      release download — same trust anchor, payload.go)
//   2. REPLACE ~/daybox on the control plane (fresh unpack + swap, previous
//      tree kept at ~/daybox.prev) — init's first-push untars into an empty
//      dir, but untarring over a live tree would leave files the new
//      release retired sitting there, shadowing reality
//   3. refresh the agent binary the plane pushes to every summoned box
//   4. re-run the same idempotent controlplane-setup.sh init runs
//
// Boxes pick the new version up at their next summon; a running box keeps
// the version it was summoned with until it is reaped or downed.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func cmdUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	var versionFlag string
	fs.StringVar(&versionFlag, "version", "", "release payload to upgrade to (default: the binary's pinned version; latest for dev builds)")
	fs.Parse(args)

	cfg := loadConfig()
	control := cfg.controlHost()
	if control == "" {
		log.Fatal("no CONTROL_HOST in config.local — this device has no deployment to upgrade; 'daybox init' sets one up")
	}

	repo, cleanupPayload := resolvePayload(versionFlag)
	defer cleanupPayload()

	say("• checking ssh access to %s", control)
	if _, err := sshCapture(control, "true"); err != nil {
		log.Fatalf("cannot ssh to %s: %v", control, err)
	}
	if _, err := sshCapture(control, "test -d ~/daybox"); err != nil {
		log.Fatalf("%s has no ~/daybox — not an initialized control plane; run 'daybox init'", control)
	}
	before := remoteAgentVersion(control)
	say("  reachable; control plane is on %s", before)

	// The setup script anchors headscale's server_url to the public IP, so
	// learn it fresh (as init does) — an upgrade then also heals an IP move.
	// But everything enrolled points at LITTLEBOX_IP, so drift is worth a
	// loud note: healing it is init's job, not upgrade's.
	publicIP, err := sshCapture(control, "curl -4fsS --max-time 10 https://api.ipify.org")
	if err != nil || strings.TrimSpace(publicIP) == "" {
		log.Fatalf("could not learn %s's public IP: %v", control, err)
	}
	publicIP = strings.TrimSpace(publicIP)
	if want := cfg.get("LITTLEBOX_IP", ""); want != "" && want != publicIP {
		say("  NOTE: %s reports public IP %s but this device's LITTLEBOX_IP is %s", control, publicIP, want)
		say("        — an IP move needs 'daybox init'; continuing with %s", publicIP)
	}

	say("• replacing ~/daybox on %s (previous tree kept at ~/daybox.prev)", control)
	if err := replaceTree(repo, control); err != nil {
		log.Fatalf("pushing repo: %v", err)
	}

	say("• refreshing the net agent (what every summoned box runs)")
	agentBin := filepath.Join(repo, "dist", "daybox-agent-linux-amd64")
	if _, err := os.Stat(agentBin); err != nil {
		log.Fatalf("missing %s in the release payload — report it (the payload is incomplete)", agentBin)
	}
	if err := scpTo(agentBin, control, ".config/daybox/agent/daybox-agent"); err != nil {
		log.Fatal(err)
	}

	say("• re-running the control-plane setup (idempotent: heals, never clobbers)")
	if err := scpTo(filepath.Join(repo, "remote", "controlplane-setup.sh"),
		control, ".daybox-controlplane-setup.sh"); err != nil {
		log.Fatal(err)
	}
	setup := fmt.Sprintf("PUBLIC_IP=%s GIT_NAME=%s GIT_EMAIL=%s NET_USER=%s bash ~/.daybox-controlplane-setup.sh",
		shQuote(publicIP), shQuote(cfg.get("GIT_NAME", "")), shQuote(cfg.get("GIT_EMAIL", "")),
		shQuote(cfg.get("NET_USER", "dev")))
	if err := sshRun(control, setup); err != nil {
		log.Fatalf("control-plane setup failed (output above) — the previous tree is intact at ~/daybox.prev on %s", control)
	}

	// Same tail as init: let the new release heal whatever it now expects of
	// a deployment (keys/volume registration, profile seeding). Idempotent.
	if _, err := sshCapture(control, "test -f ~/.config/daybox/token"); err == nil {
		say("• re-running 'daybox setup' on the control plane")
		if err := sshRun(control, remoteDaybox+" setup"); err != nil {
			log.Fatalf("daybox setup failed (output above) — re-run 'daybox upgrade', or by hand:\n    ssh %s '%s setup'", control, remoteDaybox)
		}
	}

	say("")
	say("✓ control plane upgraded: %s → %s", before, remoteAgentVersion(control))
	say("  new boxes summon at the new version; running boxes keep the version")
	say("  they were summoned with until reaped ('daybox down' + 'up' rotates one now)")
}

// remoteAgentVersion reads the version of the agent binary the control plane
// pushes to boxes — the one stamped artifact a deployment carries.
func remoteAgentVersion(control string) string {
	out, err := sshCapture(control, "~/.config/daybox/agent/daybox-agent version 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return "unknown"
	}
	return strings.TrimSpace(out)
}

// replaceTree is pushTree for a control plane that already has one: unpack
// to ~/daybox.new, then swap. ~/.local/bin/daybox is a symlink into
// ~/daybox, so the previous tree at ~/daybox.prev makes rollback one mv;
// the symlink dangles only for the instant between the two mvs (a reaper
// tick landing exactly there fails once and self-heals next tick). Retried
// whole like pushTree: every attempt starts from a fresh ~/daybox.new.
func replaceTree(repo, control string) error {
	return sshRetry("control plane", func() error {
		tar := exec.Command("tar", "-C", repo, "--no-xattrs",
			"--exclude=./.git", "--exclude=./dist", "--exclude=./cmd/daybox/daybox",
			"-czf", "-", ".")
		unpack := exec.Command("ssh", append(sshOpts(true), control,
			"rm -rf ~/daybox.new && mkdir -p ~/daybox.new && tar -xzf - -C ~/daybox.new && "+
				"rm -rf ~/daybox.prev && mv ~/daybox ~/daybox.prev && mv ~/daybox.new ~/daybox")...)
		var err error
		unpack.Stdin, err = tar.StdoutPipe()
		if err != nil {
			return err
		}
		tar.Stderr, unpack.Stdout, unpack.Stderr = os.Stderr, os.Stderr, os.Stderr
		if err := tar.Start(); err != nil {
			return err
		}
		uerr := unpack.Run()
		twerr := tar.Wait()
		if uerr != nil {
			return uerr
		}
		return twerr
	})
}
