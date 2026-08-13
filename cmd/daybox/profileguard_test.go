package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profileguard_test.go — regressions for two bugs reported against acting
// on a profile:
//
//  1. `daybox up -p <nonexistent>` SILEENTLY created the profile: cmdUp's
//     plane path called deriveProfile, which mkdirs the state dir as a side
//     effect, BEFORE any existence check. `profile ls` then listed the
//     phantom as a real profile. The fix gates every acting verb on
//     requireProfile, which checks existence WITHOUT creating anything.
//
//  2. `daybox profile --help` printed "unknown: profile --help": the
//     profile group had no help path, so --help fell through to the plane's
//     "unknown subverb" fatal.

// TestRequireProfileRejectsNonexistent: acting on a profile that was never
// created must error AND must not leave a phantom state dir behind — the
// exact side effect that made `up -p ghost` look like it created a profile.
func TestRequireProfileRejectsNonexistent(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	// 'default' is set up (has a state dir, as `daybox setup` would leave).
	if _, err := d.deriveProfile("default"); err != nil {
		t.Fatal(err)
	}
	// 'ghost' was never created.
	_, err := d.requireProfile("ghost")
	if err == nil {
		t.Fatal("requireProfile(ghost) should error, not silently derive")
	}
	if fileExists(filepath.Join(d.stateDir, "profiles", "ghost")) {
		t.Error("requireProfile created a state dir for a nonexistent profile — the exact bug")
	}
	if !strings.Contains(err.Error(), "no such profile") {
		t.Errorf("error should name the missing profile: %v", err)
	}
}

// TestRequireProfileResolvesCurrent: with no -p, requireProfile falls back
// to the current_profile file (then default) — and lets an existing one
// through, so the bare-verb path still works after the guard.
func TestRequireProfileResolvesCurrent(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	// set up 'work' and make it the current profile
	if _, err := d.deriveProfile("work"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(d.stateDir, 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "current_profile"), []byte("work\n"), 0o644)
	p, err := d.requireProfile("") // bare verb -> current_profile -> work
	if err != nil {
		t.Fatalf("requireProfile('') with current=work: %v", err)
	}
	if p.name != "work" {
		t.Errorf("resolved profile = %q, want work", p.name)
	}
}

// TestRequireProfileRejectsBareDefaultWhenUnset: bare `daybox up` on a
// fresh plane (no -p, no current_profile, no setup) must refuse — NOT
// invent a 'default' profile by side effect.
func TestRequireProfileRejectsBareDefaultWhenUnset(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	// nothing set up — not even 'default'
	if _, err := d.requireProfile(""); err == nil {
		t.Fatal("bare verb with no profile set up should error, not create 'default'")
	}
	if fileExists(filepath.Join(d.stateDir, "profiles", "default")) {
		t.Error("requireProfile created a 'default' state dir by side effect")
	}
}

// TestProfileHelpPrintsUsage: `daybox profile --help` (and -h, help) must
// print the profile subcommand usage and exit 0 — not "unknown: profile
// --help". The bug: profile had no help path, so --help hit the plane's
// "unknown subverb" fatal.
func TestProfileHelpPrintsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"profile", "--help"},
		{"profile", "-h"},
		{"profile", "help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			// cmdProfile writes usage to os.Stderr (like say()); capture it.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			origStderr := os.Stderr
			os.Stderr = w
			code := run(args, &out, io.Discard)
			w.Close()
			os.Stderr = origStderr
			var stderrBuf bytes.Buffer
			io.Copy(&stderrBuf, r)
			got := stderrBuf.String()
			if code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if strings.Contains(got, "unknown") {
				t.Errorf("profile --help printed 'unknown ...': %q", got)
			}
			if !strings.Contains(got, "profile") || !strings.Contains(got, "add") {
				t.Errorf("profile --help did not print profile usage: %q", got)
			}
			if out.Len() != 0 {
				t.Errorf("profile --help wrote to stdout: %q", out.String())
			}
		})
	}
}
