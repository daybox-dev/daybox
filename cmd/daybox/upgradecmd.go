package main

// upgradecmd.go — `daybox upgrade`: bring the laptop and the control
// plane to the live release in one command. init answers "make me a
// deployment"; upgrade answers "my deployment is fine, its code is old" —
// so it runs with NO interview, learns everything from config.local, and
// never touches what *is* the deployment (identity, net, token, volumes,
// profiles).
//
// In order:
//   1. self-update the laptop if the live release is ahead (no -version
//      only): run the installer, then syscall.Exec the new binary so the
//      plane gets the new version, not the old one this process still is.
//      Skipped for dev builds and explicit -version pins. (selfupdate.go)
//   2. resolve the payload exactly like init (a signed, checksum-verified
//      release download — same trust anchor, payload.go)
//   3. REPLACE ~/daybox on the control plane (fresh unpack + swap, previous
//      tree kept at ~/daybox.prev) — init's first-push untars into an empty
//      dir, but untarring over a live tree would leave files the new
//      release retired sitting there, shadowing reality
//   4. refresh the agent binary the plane pushes to every summoned box
//   5. re-run the same idempotent controlplane-setup.sh init runs
//
// Run ON the control plane (no CONTROL_HOST — it IS the host), the same
// steps act on this machine instead of over ssh, so the plane can move
// itself to a new release without the laptop. There is no step 1 there:
// the plane's daybox binary lives inside ~/daybox, so the tree swap is its
// self-update. With no -version it targets the live release (not this
// binary's pinned one) and refuses to downgrade.
//
// Boxes pick the new version up at their next summon; a running box keeps
// the version it was summoned with until it is reaped or downed.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func cmdUpgrade(p Parsed) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	var versionFlag string
	fs.StringVar(&versionFlag, "version", "", "release payload to upgrade to (default: the binary's pinned version; latest for dev builds and on the control plane itself)")
	fs.Parse(p.Rest())

	cfg := loadConfig()
	control := cfg.controlHost()
	plane := planeTarget{host: control}
	if plane.local() {
		if !isInitializedPlane() {
			log.Fatal("no CONTROL_HOST in config.local — this device has no deployment to upgrade; 'daybox init' sets one up")
		}
		latest, ok := latestRelease()
		ver, err := planeUpgradeVersion(versionFlag, version, latest, ok)
		if err != nil {
			log.Fatal(err)
		}
		versionFlag = ver
	} else if versionFlag == "" && version != "dev" {
		// Self-update the laptop (no explicit -version only): if the live
		// release is ahead of this binary, install it and re-exec, so the
		// plane gets the new version instead of the old one this process
		// still is. Skipped for dev builds (a local build is changes under
		// test — don't clobber it) and when -version pins one explicitly (a
		// targeted push, not "bring me current"). Unreachable installer =>
		// non-fatal: proceed with the binary's own pinned version (the
		// pre-self-update behavior).
		if latest, ok := latestRelease(); ok {
			if isNewer(latest, version) {
				say("• self-updating the laptop: %s → %s (so the plane gets it too)", version, latest)
				if err := runSelfUpdate(); err != nil {
					log.Fatalf("self-update failed: %v — the control plane was not touched", err)
				}
				exe, err := os.Executable()
				if err != nil {
					log.Fatalf("self-updated but could not re-exec: %v — re-run 'daybox upgrade'", err)
				}
				say("  re-launching %s", exe)
				if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
					log.Fatalf("re-exec failed: %v — re-run 'daybox upgrade'", err)
				}
				return // unreachable: syscall.Exec replaced the process
			}
		} else {
			say("• could not check for a newer release (installer unreachable) — continuing")
		}
	}

	repo, cleanupPayload := resolvePayload(versionFlag)
	defer cleanupPayload()

	if !plane.local() {
		say("• checking ssh access to %s", plane)
		if _, err := plane.capture("true"); err != nil {
			log.Fatalf("cannot ssh to %s: %v", plane, err)
		}
	}
	if _, err := plane.capture("test -d ~/daybox"); err != nil {
		log.Fatalf("%s has no ~/daybox — not an initialized control plane; run 'daybox init'", plane)
	}
	before := remoteAgentVersion(plane)
	if plane.local() {
		say("  this control plane is on %s", before)
	} else {
		say("  reachable; control plane is on %s", before)
	}

	// The setup script anchors headscale's server_url to the public IP, so
	// learn it fresh (as init does) — an upgrade then also heals an IP move.
	// But everything enrolled points at LITTLEBOX_IP, so drift is worth a
	// loud note: healing it is init's job, not upgrade's.
	publicIP, err := plane.capture("curl -4fsS --max-time 10 https://api.ipify.org")
	if err != nil || strings.TrimSpace(publicIP) == "" {
		log.Fatalf("could not learn %s's public IP: %v", plane, err)
	}
	publicIP = strings.TrimSpace(publicIP)
	if want := cfg.get("LITTLEBOX_IP", ""); want != "" && want != publicIP {
		say("  NOTE: %s reports public IP %s but this device's LITTLEBOX_IP is %s", plane, publicIP, want)
		say("        — an IP move needs 'daybox init'; continuing with %s", publicIP)
	}

	say("• replacing ~/daybox on %s (previous tree kept at ~/daybox.prev)", plane)
	if err := replaceTree(repo, plane); err != nil {
		log.Fatalf("pushing repo: %v", err)
	}

	say("• refreshing the net agent (what every summoned box runs)")
	agentBin := filepath.Join(repo, "dist", "daybox-agent-linux-amd64")
	if _, err := os.Stat(agentBin); err != nil {
		log.Fatalf("missing %s in the release payload — report it (the payload is incomplete)", agentBin)
	}
	if err := plane.copy(agentBin, ".config/daybox/agent/daybox-agent"); err != nil {
		log.Fatal(err)
	}

	say("• re-running the control-plane setup (idempotent: heals, never clobbers)")
	if err := plane.copy(filepath.Join(repo, "remote", "controlplane-setup.sh"),
		".daybox-controlplane-setup.sh"); err != nil {
		log.Fatal(err)
	}
	setup := fmt.Sprintf("PUBLIC_IP=%s GIT_NAME=%s GIT_EMAIL=%s NET_USER=%s bash ~/.daybox-controlplane-setup.sh",
		shQuote(publicIP), shQuote(cfg.get("GIT_NAME", "")), shQuote(cfg.get("GIT_EMAIL", "")),
		shQuote(cfg.get("NET_USER", "dev")))
	if err := plane.run(setup); err != nil {
		log.Fatalf("control-plane setup failed (output above) — the previous tree is intact at ~/daybox.prev on %s", plane)
	}

	// Same tail as init: let the new release heal whatever it now expects of
	// a deployment (keys/volume registration, profile seeding). Idempotent.
	if _, err := plane.capture("test -f ~/.config/daybox/token"); err == nil {
		say("• re-running 'daybox setup' on %s", plane)
		if err := plane.run(remoteDaybox + " setup"); err != nil {
			if plane.local() {
				log.Fatalf("daybox setup failed (output above) — re-run 'daybox upgrade', or by hand:\n    %s setup", remoteDaybox)
			}
			log.Fatalf("daybox setup failed (output above) — re-run 'daybox upgrade', or by hand:\n    ssh %s '%s setup'", plane, remoteDaybox)
		}
	}

	say("")
	say("✓ control plane upgraded: %s → %s", before, remoteAgentVersion(plane))
	say("  new boxes summon at the new version; running boxes keep the version")
	say("  they were summoned with until reaped ('daybox down' + 'up' rotates one now)")
}

// isInitializedPlane reports whether this machine is a control plane `daybox
// init` set up: it has the deployed tree and the agent binary it pushes to
// boxes. A laptop with no CONTROL_HOST (never init'ed) has neither, and keeps
// getting the "run daybox init" error rather than upgrading itself.
func isInitializedPlane() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return fileExists(filepath.Join(home, "daybox")) &&
		fileExists(filepath.Join(confDir(), "agent", "daybox-agent"))
}

// planeUpgradeVersion picks the release a control plane upgrades ITSELF to:
// an explicit -version always wins; otherwise the live release, because the
// tree swap is the plane's only self-update (the binary's own pinned version
// would just re-install what it already runs). It refuses to move a release
// build backwards — a live installer lagging this binary means "nothing to
// do", not "downgrade" — and fails rather than guessing when the live
// release can't be learned. Pure, so it's testable.
func planeUpgradeVersion(flagVersion, current, latest string, latestOK bool) (string, error) {
	if flagVersion != "" {
		return flagVersion, nil
	}
	if !latestOK {
		return "", errors.New("could not learn the live release (installer unreachable) — pass -version vX.Y.Z to pick one")
	}
	if strings.HasPrefix(current, "v") && compareVersions(latest, current) < 0 {
		return "", fmt.Errorf("the live release %s is older than this plane's %s — refusing to downgrade; pass -version %s to force it", latest, current, latest)
	}
	return latest, nil
}

// planeTarget is where upgrade does its work: a control plane reached over
// ssh from a laptop (host set), or this machine itself when upgrade runs ON
// the plane (host "" — no CONTROL_HOST, it IS the host). Local commands go
// through bash -c so "~" and shell syntax mean exactly what they mean over
// ssh; the laptop path keeps using the retrying ssh helpers unchanged.
type planeTarget struct{ host string }

func (t planeTarget) local() bool { return t.host == "" }

func (t planeTarget) String() string {
	if t.local() {
		return "this control plane"
	}
	return t.host
}

// cmd builds the process that runs script on the plane (not yet started).
func (t planeTarget) cmd(script string) *exec.Cmd {
	if t.local() {
		return exec.Command("bash", "-c", script)
	}
	return exec.Command("ssh", append(sshOpts(true), t.host, script)...)
}

// capture runs script on the plane and returns its stdout (stderr streams
// through), like sshCapture.
func (t planeTarget) capture(script string) (string, error) {
	if !t.local() {
		return sshCapture(t.host, script)
	}
	c := t.cmd(script)
	c.Stderr = os.Stderr
	out, err := c.Output()
	return string(out), err
}

// run streams everything through, like sshRun.
func (t planeTarget) run(script string) error {
	if !t.local() {
		return sshRun(t.host, script)
	}
	c := t.cmd(script)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// copy puts src at dst, a path relative to the plane's home (scp's
// convention). Locally it goes through copyFileAtomic: dst may be a running
// executable — the net relay runs the agent binary — and opening one for
// writing fails with ETXTBSY.
func (t planeTarget) copy(src, dst string) error {
	if !t.local() {
		return scpTo(src, t.host, dst)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return copyFileAtomic(src, filepath.Join(home, dst))
}

// copyFileAtomic writes src's bytes and permission bits to dst+".new", then
// renames it over dst. The rename swaps the directory entry, so a process
// still executing the old dst keeps its inode and never sees a torn file.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// OpenFile's mode is filtered by the umask; make the bits exact.
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// remoteAgentVersion reads the version of the agent binary the control plane
// pushes to boxes — the one stamped artifact a deployment carries.
func remoteAgentVersion(t planeTarget) string {
	out, err := t.capture("~/.config/daybox/agent/daybox-agent version 2>/dev/null")
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
// whole like pushTree: every attempt starts from a fresh ~/daybox.new. On
// the plane itself the same script runs locally — moving the tree out from
// under the running daybox binary is fine (it keeps its inode).
func replaceTree(repo string, t planeTarget) error {
	return sshRetry("control plane", func() error {
		tar := exec.Command("tar", "-C", repo, "--no-xattrs",
			"--exclude=./.git", "--exclude=./cmd/daybox/daybox",
			"-czf", "-", ".")
		unpack := t.cmd("rm -rf ~/daybox.new && mkdir -p ~/daybox.new && tar -xzf - -C ~/daybox.new && " +
			"rm -rf ~/daybox.prev && mv ~/daybox ~/daybox.prev && mv ~/daybox.new ~/daybox")
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
