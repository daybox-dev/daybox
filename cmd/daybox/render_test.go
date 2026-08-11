package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMatchesBashGolden is the U6 regression: the Go renderUserData
// must produce byte-identical output to the checked-in golden file, across
// the cases the awk port gets subtly right — @REPO:/@FILE: includes with
// indent preservation, @SSH_KEYS@ list items, @PROFILE_SEED@ with TOML
// comment stripping that is multiline-string-aware (a #-line inside a """
// block is content and survives; a #-line outside is stripped), the
// first-line shebang kept, and all @PLACEHOLDER@ substitutions.
//
// The golden was generated against the bash renderer (the original oracle)
// at port time; it is now a snapshot — if the template or renderer changes
// intentionally, regenerate it and commit the new golden.
func TestRenderMatchesBashGolden(t *testing.T) {
	goldenPath := filepath.Join("testdata", "render-golden.yaml")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("missing golden file at %s — regenerate the Go renderer's output and commit it", goldenPath)
	}
	// Point DAYBOX_REPO_DIR at the repo root (the test runs from cmd/daybox/,
	// so the repo root is ../..). The template + remote/ includes live there.
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAYBOX_REPO_DIR", abs)
	// Build the same sandboxed deployment the golden script uses.
	home := t.TempDir()
	t.Setenv("HOME", home)
	d := loadDeployment()
	confDir := d.confDir
	os.MkdirAll(filepath.Join(confDir, "keys"), 0o755)
	os.MkdirAll(filepath.Join(confDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(confDir, "keys", "laptop.pub"),
		[]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIETK/JI88OZihytMTWNWbOmQhLPVXFEKtw4sLg5XTVMx laptop\n"), 0o644)
	os.WriteFile(filepath.Join(confDir, "keys", "mac.pub"),
		[]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkey2macbookairabcdef1234567890 mac\n"), 0o644)
	os.WriteFile(filepath.Join(confDir, "config.local"), []byte(`LITTLEBOX_IP=203.0.113.10
GIT_NAME="Alice O'Brien"
GIT_EMAIL="alice@example.com"
REMOTE_USER=dev
PROVIDER=hetzner
SERVER_TYPE=ccx33
LOCATION=hil
NET_USER=dev
NET_PORT=8080
`), 0o600)
	os.WriteFile(filepath.Join(confDir, "profiles", "default", "profile.toml"), []byte(`# this is a comment that must be stripped from the seed
[meta]
owner = "alice"

[packages]
apt = ["ripgrep", "htop"]

[setup]
# another stripped comment
once = """
echo hello
# this #-line is INSIDE a multiline string — it must survive (content)
echo done
"""
`), 0o644)
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderUserData(p, "123456")
	if err != nil {
		t.Fatalf("renderUserData: %v", err)
	}
	if got != string(want) {
		if os.Getenv("DAYBOX_REGEN_GOLDEN") == "1" {
			if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
				t.Fatalf("regen: write %s: %v", goldenPath, err)
			}
			t.Logf("regenerated %s (%d bytes) — review + commit", goldenPath, len(got))
			return
		}
		// first divergence helps debug
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(string(want), "\n")
		for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
			if gotLines[i] != wantLines[i] {
				t.Errorf("first divergence at line %d:\n  got:  %q\n  want: %q\n  (got %d lines, want %d lines; got %d bytes, want %d bytes)",
					i+1, gotLines[i], wantLines[i], len(gotLines), len(wantLines), len(got), len(want))
				break
			}
		}
		// if lengths differ but all shared lines match, report that
		if len(gotLines) != len(wantLines) && !t.Failed() {
			t.Errorf("line count: got %d, want %d", len(gotLines), len(wantLines))
		}
	}
}

// TestRenderStripsSeedCommentsOutsideMultiline asserts the TOML-aware
// stripping directly: a #-line outside a string is gone, inside survives.
func TestRenderStripsSeedCommentsOutsideMultiline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAYBOX_REPO_DIR", repoRoot(t))
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.confDir, "keys"), 0o755)
	os.WriteFile(filepath.Join(d.confDir, "keys", "x.pub"),
		[]byte("ssh-ed25519 AAAA x\n"), 0o644)
	os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte("LITTLEBOX_IP=10.0.0.1\nGIT_NAME=A\nGIT_EMAIL=a@b.c\nREMOTE_USER=dev\n"), 0o600)
	os.WriteFile(p_seedFile(d, "default"), []byte("# stripped\n[meta]\n# stripped too\nowner=\"x\"\n[setup]\nkeep=\"\"\"\n# SURVIVE\nx\n\"\"\"\n"), 0o644)
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderUserData(p, "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "# stripped") {
		t.Error("a comment outside a multiline string was NOT stripped")
	}
	if !strings.Contains(got, "# SURVIVE") {
		t.Error("a comment inside a multiline string was stripped — it's content")
	}
}

// TestRenderRejectsMultilineIdentity: GIT_NAME with a newline must error
// (it reaches a heredoc where no quoting can contain a newline).
func TestRenderRejectsMultilineIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAYBOX_REPO_DIR", repoRoot(t))
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.confDir, "keys"), 0o755)
	os.WriteFile(filepath.Join(d.confDir, "keys", "x.pub"), []byte("ssh-ed25519 AAAA x\n"), 0o644)
	os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte("LITTLEBOX_IP=10.0.0.1\nGIT_NAME=Bad\nGIT_EMAIL=a@b.c\nREMOTE_USER=dev\n"), 0o600)
	os.WriteFile(p_seedFile(d, "default"), []byte("[meta]\n"), 0o644)
	p, _ := d.deriveProfile("default")
	p.gitName = "line1\nline2" // inject the newline the charset guard can't catch
	if _, err := renderUserData(p, "1"); err == nil {
		t.Error("a newline in GIT_NAME must be rejected")
	}
}

func p_seedFile(d *deployment, name string) string {
	return filepath.Join(d.confDir, "profiles", name, "profile.toml")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
