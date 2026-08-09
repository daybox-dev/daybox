package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestDeployment builds a sandboxed deployment under t.TempDir() so tests
// never touch (or depend on) a real ~/.config/daybox. DAYBOX_REPO_DIR is
// pointed at a temp repo dir with keys/ for the fallback path.
func newTestDeployment(t *testing.T) *deployment {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "daybox")
	t.Setenv("DAYBOX_REPO_DIR", repo)
	if err := os.MkdirAll(filepath.Join(repo, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := loadDeployment()
	if d.confDir != filepath.Join(home, ".config", "daybox") {
		t.Fatalf("confDir = %s", d.confDir)
	}
	return d
}

// writeConfig writes a KEY=VALUE config.local (deployment-wide baseline).
func writeConfig(t *testing.T, d *deployment, body string) {
	t.Helper()
	if err := os.MkdirAll(d.confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeProfileOverlay writes a profile's own KEY=VALUE config overlay.
func writeProfileOverlay(t *testing.T, d *deployment, name, body string) {
	t.Helper()
	dir := filepath.Join(d.confDir, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileDeriveDefaults(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\nGIT_NAME=Alice\nGIT_EMAIL=alice@example.com\n")
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	if p.serverName != "daybox-default" || p.volumeName != "daybox-default-vol" {
		t.Errorf("derived names = %s/%s", p.serverName, p.volumeName)
	}
	if p.provider != "hetzner" || p.serverType != "ccx33" || p.location != "hil" {
		t.Errorf("defaults not applied: %+v", p)
	}
	if p.remoteUser != "dev" {
		t.Errorf("remoteUser = %q, want dev", p.remoteUser)
	}
	if p.gitName != "Alice" || p.gitEmail != "alice@example.com" {
		t.Errorf("git identity not layered: %s/%s", p.gitName, p.gitEmail)
	}
	if p.netControlURL != "http://10.0.0.1:8080" {
		t.Errorf("netControlURL = %q, want http://10.0.0.1:8080", p.netControlURL)
	}
}

// TestProfileKnobsDoNotLeakBetweenProfiles is the regression test for the
// test-provider-select.sh bug: deriving profile A (which overrides knobs)
// then profile B (which does not) must give B the DEPLOYMENT defaults, not
// A's leaked values. A leaked REMOTE_USER once made the next profile's probe
// fail as the wrong user until it was force-reaped.
func TestProfileKnobsDoNotLeakBetweenProfiles(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\nSERVER_TYPE=cpx11\nREMOTE_USER=dev\nMAX_LIFETIME_HOURS=24\n")
	// profile A overrides provider, server type, remote user, lifetime
	writeProfileOverlay(t, d, "alpha", "PROVIDER=hetzner\nSERVER_TYPE=ccx33\nREMOTE_USER=alice\nMAX_LIFETIME_HOURS=6\n")
	// profile B has NO overlay — must get deployment baselines, NOT alpha's
	if _, err := d.deriveProfile("alpha"); err != nil {
		t.Fatal(err)
	}
	b, err := d.deriveProfile("beta")
	if err != nil {
		t.Fatal(err)
	}
	if b.serverType != "cpx11" {
		t.Errorf("beta.serverType = %q (leaked from alpha?), want cpx11 (deployment)", b.serverType)
	}
	if b.remoteUser != "dev" {
		t.Errorf("beta.remoteUser = %q (leaked from alpha?), want dev", b.remoteUser)
	}
	if b.maxLifetimeHours != 24 {
		t.Errorf("beta.maxLifetimeHours = %d (leaked from alpha?), want 24", b.maxLifetimeHours)
	}
}

// TestProfileOverlayWinsOverBaseline: a profile's own value must win over
// the deployment-wide config (e.g. one profile wants a bigger box).
func TestProfileOverlayWinsOverBaseline(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\nSERVER_TYPE=cpx11\n")
	writeProfileOverlay(t, d, "big", "SERVER_TYPE=ccx33\nREMOTE_USER=dev\n")
	p, err := d.deriveProfile("big")
	if err != nil {
		t.Fatal(err)
	}
	if p.serverType != "ccx33" {
		t.Errorf("overlay lost: serverType = %q, want ccx33", p.serverType)
	}
}

func TestProfileInvalidName(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	for _, bad := range []string{"", "UPPER", "with space", "under_score", "dot.dot"} {
		if _, err := d.deriveProfile(bad); err == nil {
			t.Errorf("deriveProfile(%q) should error", bad)
		}
	}
}

func TestValidateIdentity(t *testing.T) {
	for _, bad := range []string{"", "10.0.0.1"} {
		if err := validateIdentity(bad, "dev"); err != nil {
			t.Errorf("validateIdentity(%q, dev) should pass for IP/empty, got %v", bad, err)
		}
	}
	if err := validateIdentity("", "dev"); err != nil {
		t.Errorf("empty LITTLEBOX_IP (plane not set) should pass: %v", err)
	}
	for _, bad := range []string{"not-an-ip", "10.0.0", "10.0.0.1.5", "example.com"} {
		if err := validateIdentity(bad, "dev"); err == nil {
			t.Errorf("validateIdentity(%q, dev) should reject bad IP", bad)
		}
	}
	for _, bad := range []string{"", "Dev", "root!", "a b", "1leading"} {
		if err := validateIdentity("10.0.0.1", bad); err == nil {
			t.Errorf("validateIdentity(10.0.0.1, %q) should reject bad user", bad)
		}
	}
}

func TestCurrentProfileResolution(t *testing.T) {
	d := newTestDeployment(t)
	// no -p, no current_profile file -> default, not explicit
	if name, explicit := d.currentProfile(""); name != "default" || explicit {
		t.Errorf("bare verb -> %q explicit=%v, want default/not-explicit", name, explicit)
	}
	// current_profile file -> that name, not explicit
	os.MkdirAll(d.stateDir, 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "current_profile"), []byte("work\n"), 0o644)
	if name, explicit := d.currentProfile(""); name != "work" || explicit {
		t.Errorf("current_profile file -> %q explicit=%v, want work/not-explicit", name, explicit)
	}
	// -p given -> that name, explicit (status uses this to scope vs show-all)
	if name, explicit := d.currentProfile("staging"); name != "staging" || !explicit {
		t.Errorf("-p staging -> %q explicit=%v, want staging/explicit", name, explicit)
	}
}

func TestVolumeIDMissingGivesSetupHelp(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.volumeID()
	if err == nil || !strings.Contains(err.Error(), "no volume yet") || !strings.Contains(err.Error(), "daybox setup") {
		t.Errorf("missing volumeID err = %v, want setup help mentioning 'no volume yet' + 'daybox setup'", err)
	}
	// present -> returns the cached id
	os.WriteFile(p.volumeIDFile(), []byte("123456\n"), 0o644)
	id, err := p.volumeID()
	if err != nil || id != "123456" {
		t.Errorf("volumeID = %q/%v, want 123456/nil", id, err)
	}
}

func TestKeysDirFallback(t *testing.T) {
	d := newTestDeployment(t)
	// no local keys -> repo keys/ fallback
	if got := d.keysDir(); got != filepath.Join(d.repoDir, "keys") {
		t.Errorf("keysDir = %s, want repo fallback", got)
	}
	// local keys present -> local wins
	local := filepath.Join(d.confDir, "keys")
	os.MkdirAll(local, 0o755)
	os.WriteFile(filepath.Join(local, "laptop.pub"), []byte("ssh-ed25519 AAAA x\n"), 0o644)
	if got := d.keysDir(); got != local {
		t.Errorf("keysDir = %s, want local", got)
	}
}

func TestListProfiles(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	for _, n := range []string{"alpha", "beta", "default"} {
		d.deriveProfile(n) // creates the state dir
	}
	got := d.listProfiles()
	if len(got) != 3 {
		t.Fatalf("listProfiles = %v, want 3", got)
	}
}

// TestAmPlaneRoleGate: no CONTROL_HOST -> plane (do work locally);
// CONTROL_HOST set -> laptop (delegate). This gates every verb's two paths.
func TestAmPlaneRoleGate(t *testing.T) {
	d := newTestDeployment(t)
	// no config -> no CONTROL_HOST -> plane
	if !amPlane() {
		t.Error("no CONTROL_HOST -> should be plane role")
	}
	writeConfig(t, d, "CONTROL_HOST=alice@10.0.0.1\n")
	if amPlane() {
		t.Error("CONTROL_HOST set -> should be laptop role (not plane)")
	}
}

// TestLoadProviderUnknown is a config error, not a silent skip.
func TestLoadProviderUnknown(t *testing.T) {
	d := newTestDeployment(t)
	if _, err := d.loadProvider("aws"); err == nil {
		t.Error("unknown provider should error")
	}
	p, err := d.loadProvider("hetzner")
	if err != nil || p.Name() != "hetzner" {
		t.Errorf("loadProvider(hetzner) = %v/%v", p, err)
	}
}
