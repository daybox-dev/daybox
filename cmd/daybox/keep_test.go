package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// keep_test.go — the keep.toml sidecar: parse, path validation,
// degrade-to-ignore, OR semantics, absent-file = empty.

func writeKeepToml(t *testing.T, d *deployment, name, body string) {
	t.Helper()
	dir := filepath.Join(d.confDir, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadKeepTomlAbsentIsEmpty: no keep.toml = no file-signals (the safe
// ssh+load baseline). NOT an error — the whole point of not shipping a
// default.
func TestLoadKeepTomlAbsentIsEmpty(t *testing.T) {
	d := newTestDeployment(t)
	if got := loadKeepToml(filepath.Join(d.confDir, "profiles", "default", "keep.toml")); len(got) != 0 {
		t.Errorf("absent keep.toml -> %v, want empty", got)
	}
	// deriveProfile succeeds with an absent keep.toml (keep is no longer a
	// profile field — the box loads it on-box at probe time).
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	if _, err := d.deriveProfile("default"); err != nil {
		t.Fatalf("deriveProfile with absent keep.toml: %v", err)
	}
}

// TestLoadKeepTomlParses: valid entries decode to (path, within).
func TestLoadKeepTomlParses(t *testing.T) {
	d := newTestDeployment(t)
	writeKeepToml(t, d, "default", `[[files]]
path = "/work/state/claude/projects"
within = "10m"

[[files]]
path = "/work/state/pi/.pi/agent/sessions"
within = "5m"
`)
	got := loadKeepToml(filepath.Join(d.confDir, "profiles", "default", "keep.toml"))
	if len(got) != 2 {
		t.Fatalf("got %d signals, want 2: %+v", len(got), got)
	}
	if got[0].path != "/work/state/claude/projects" || got[0].within != 10*time.Minute {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].path != "/work/state/pi/.pi/agent/sessions" || got[1].within != 5*time.Minute {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

// TestLoadKeepTomlSkipsBadEntry: a bad entry (non-absolute path, shell
// metachar, non-positive within) is LOGGED and SKIPPED — degrade to
// ignoring that signal, never to a hard error. Good entries in the same
// file still fire.
func TestLoadKeepTomlSkipsBadEntry(t *testing.T) {
	d := newTestDeployment(t)
	writeKeepToml(t, d, "default", `[[files]]
path = "relative/path"
within = "10m"

[[files]]
path = "/work/state/claude/projects"
within = "10m"

[[files]]
path = "/has space"
within = "10m"

[[files]]
path = "/ok/path"
within = "0"

[[files]]
path = "/also/ok"
within = "not-a-duration"
`)
	got := loadKeepToml(filepath.Join(d.confDir, "profiles", "default", "keep.toml"))
	// only the one fully-valid entry survives
	if len(got) != 1 {
		t.Fatalf("got %d signals, want 1 (only the valid entry): %+v", len(got), got)
	}
	if got[0].path != "/work/state/claude/projects" || got[0].within != 10*time.Minute {
		t.Errorf("surviving entry = %+v, want the claude path @ 10m", got[0])
	}
}

// TestLoadKeepTomlBadTOMLDegrades: a structurally invalid TOML file
// degrades to empty (reap on ssh+load + cap), NOT a hard error — a hard
// error would make deriveProfile fail and reapRun SKIP the whole profile
// (box bills until the lifetime cap), the more expensive failure.
func TestLoadKeepTomlBadTOMLDegrades(t *testing.T) {
	d := newTestDeployment(t)
	writeKeepToml(t, d, "default", "this is not = = valid toml {{{")
	got := loadKeepToml(filepath.Join(d.confDir, "profiles", "default", "keep.toml"))
	if got != nil {
		t.Errorf("bad TOML -> %v, want nil (degrade to ssh+load)", got)
	}
	// deriveProfile still succeeds
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	if _, err := d.deriveProfile("default"); err != nil {
		t.Errorf("deriveProfile with bad keep.toml should not fail: %v", err)
	}
}

// TestLoadKeepTomlEmptyFilesIsSafe: an empty keep.toml (no [[files]]) is
// valid = ssh+load only, same as absent.
func TestLoadKeepTomlEmptyFilesIsSafe(t *testing.T) {
	d := newTestDeployment(t)
	writeKeepToml(t, d, "default", "# no signals\n")
	got := loadKeepToml(filepath.Join(d.confDir, "profiles", "default", "keep.toml"))
	if len(got) != 0 {
		t.Errorf("empty keep.toml -> %v, want empty", got)
	}
}

// TestKeepPathRe: the path-validation regex — absolute, letters/digits/
// dot/underscore/slash/hyphen only; rejects shell-unsafe chars.
func TestKeepPathRe(t *testing.T) {
	good := []string{
		"/work/state/claude/projects",
		"/work/state/pi/.pi/agent/sessions",
		"/tmp/x",
		"/a-b_c.d/e",
	}
	for _, p := range good {
		if !keepPathRe.MatchString(p) {
			t.Errorf("keepPathRe should accept %q", p)
		}
	}
	bad := []string{
		"relative/path", // not absolute
		"/has space",    // space
		"/has;rm",       // semicolon
		"/has$var",      // dollar
		"/has`back`",    // backtick
		"/has|pipe",     // pipe
		"/has\"quote",   // double quote
		"/has'quote",    // single quote
		"/has\\back",    // backslash
		"/has&amp",      // ampersand
		"has\tno",       // tab + not absolute
	}
	for _, p := range bad {
		if keepPathRe.MatchString(p) {
			t.Errorf("keepPathRe should reject %q", p)
		}
	}
}
