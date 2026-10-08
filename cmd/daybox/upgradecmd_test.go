package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plane upgrading itself targets the live release, never moves a release
// build backwards, and refuses to guess when the live release is unknown.
func TestPlaneUpgradeVersion(t *testing.T) {
	cases := []struct {
		name                  string
		flag, current, latest string
		latestOK              bool
		want                  string
		wantErr               string
	}{
		{"explicit -version wins", "v0.4.0", "v0.5.3", "v0.5.4", true, "v0.4.0", ""},
		{"explicit wins even offline", "v0.5.4", "v0.5.3", "", false, "v0.5.4", ""},
		{"newer live release", "", "v0.5.3", "v0.5.4", true, "v0.5.4", ""},
		{"same release re-applies", "", "v0.5.4", "v0.5.4", true, "v0.5.4", ""},
		{"live lags this binary", "", "v0.5.4", "v0.5.3", true, "", "refusing to downgrade"},
		{"dev build takes latest", "", "dev", "v0.5.3", true, "v0.5.3", ""},
		{"installer unreachable", "", "v0.5.3", "", false, "", "pass -version"},
	}
	for _, c := range cases {
		got, err := planeUpgradeVersion(c.flag, c.current, c.latest, c.latestOK)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got (%q, %v), want %q", c.name, got, err, c.want)
		}
	}
}

// copyFileAtomic replaces dst by rename (so a running executable at dst is
// never opened for writing), with src's exact bytes and permission bits, and
// leaves no .new behind.
func TestCopyFileAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old agent, longer than the new one"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(dst)

	if err := copyFileAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "new agent" {
		t.Fatalf("dst = %q, %v; want %q", b, err, "new agent")
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("dst mode = %v, want 0755", fi.Mode().Perm())
	}
	if os.SameFile(before, fi) {
		t.Error("dst was rewritten in place; want a rename (new inode)")
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("leftover %s.new (err %v)", dst, err)
	}
}

// On the plane, commands run locally through bash, so "~" expands to this
// machine's home exactly as it would in the remote shell over ssh.
func TestPlaneTargetLocalShell(t *testing.T) {
	plane := planeTarget{}
	if !plane.local() || plane.String() != "this control plane" {
		t.Fatalf("zero planeTarget should be local, got %q", plane)
	}
	out, err := plane.capture("echo ~")
	if err != nil {
		t.Fatal(err)
	}
	if home, _ := os.UserHomeDir(); strings.TrimSpace(out) != home {
		t.Errorf("echo ~ = %q, want %q", strings.TrimSpace(out), home)
	}
	if remote := (planeTarget{host: "plane.example"}); remote.local() || remote.String() != "plane.example" {
		t.Errorf("host-set planeTarget = %+v, want remote named plane.example", remote)
	}
}
