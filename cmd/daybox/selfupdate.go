package main

// selfupdate.go — `daybox upgrade`'s laptop half: before pushing a release
// to the control plane, bring the laptop binary up to the live release so
// the plane gets the new version, not the old one this process still is.
//
// The mechanism is the same curl|sh installer a stranger uses, just driven
// from Go: fetch the live install.sh, read its pinned DAYBOX_RELEASE to
// decide whether a newer release exists, and if so run it (it downloads +
// verifies the binary against its own pinned checksum + signature, then
// writes ~/.local/bin/daybox). `upgrade` then syscall.Exec's the new binary
// with the same args — the process replaces itself in place and re-enters
// upgrade, now at the new version, and proceeds to the plane.
//
// Trust model: install.sh is served over TLS from daybox.dev and is its own
// anchor (it is deliberately NOT in SHA256SUMS — it pins that file's hash).
// So "what's current" is TLS-trusted, exactly like the one-liner; the binary
// the installer writes is verified against install.sh's pinned hashes. An
// attacker who can't break TLS to daybox.dev can't redirect either.
//
// Skipped for dev builds (version == "dev": a local build is changes under
// test — don't clobber it) and when -version pins a release explicitly (a
// targeted push, not "bring me current").

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// latestRelease reads DAYBOX_RELEASE from the live installer — the one place
// "what's current" is published. Returns "",false if the installer is
// unreachable or unparseable (upgrade then proceeds with the binary's own
// pinned version, the pre-self-update behavior).
func latestRelease() (string, bool) {
	script, err := fetchURL(installerURL, 1<<20)
	if err != nil {
		return "", false
	}
	// the stamped line looks like: DAYBOX_RELEASE="v0.2.11"
	for _, line := range strings.Split(string(script), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "DAYBOX_RELEASE=") {
			continue
		}
		v := strings.Trim(strings.TrimPrefix(line, "DAYBOX_RELEASE="), `"'`)
		if v != "" && v != "__DAYBOX_RELEASE__" { // __... = unstamped template
			return v, true
		}
	}
	return "", false
}

// runSelfUpdate runs the live installer to replace this binary with the
// latest release. The installer verifies its own download (pinned checksum
// + signature); a verification failure exits non-zero and the caller aborts
// before the control plane is touched.
func runSelfUpdate() error {
	script, err := fetchURL(installerURL, 1<<20)
	if err != nil {
		return fmt.Errorf("fetching the installer: %w", err)
	}
	cmd := exec.Command("sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer failed: %w", err)
	}
	return nil
}

// isNewer reports whether latest is a strictly newer release than current.
// Both are "vX.Y.Z". Unparseable => false (never self-update on a version
// we can't reason about; the user can force a specific one with -version).
// Guards against a downgrade: if the live installer lags the running binary
// (e.g. this is a build ahead of the published release), self-update stays
// off rather than silently replacing it with an older one.
func isNewer(latest, current string) bool {
	return compareVersions(latest, current) > 0
}

// compareVersions returns -1/0/1 for a<b / a==b / a>b over "vX.Y.Z".
func compareVersions(a, b string) int {
	pa := versionParts(a)
	pb := versionParts(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	var p [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		if i >= 3 {
			break
		}
		n, _ := strconv.Atoi(s)
		p[i] = n
	}
	return p
}
