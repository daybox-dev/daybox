package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// u8Provider is a fake provider that records VolumeEnsure/Rename/Delete +
// PrepareSSHKeys so the profile verbs are exercised without a cloud.
type u8Provider struct {
	ensureCalls  []ensureCall
	renameCalls   []renameCall
	deleteCalls   []string
	prepareCalls  []string
	probeResult   *Server
	volumeSize     int
}
type ensureCall struct{ name string; size int; loc string }
type renameCall struct{ id, name string }

func (p *u8Provider) Name() string             { return "hetzner" }
func (p *u8Provider) HasCredentials() bool      { return true }
func (p *u8Provider) CheckCredentials() error   { return nil }
func (p *u8Provider) PrepareSSHKeys(dir string) error { p.prepareCalls = append(p.prepareCalls, dir); return nil }
func (p *u8Provider) Probe(string) (*Server, error) { return p.probeResult, nil }
func (p *u8Provider) Summon(string, string, string, string, string, string) (Server, error) { return Server{}, nil }
func (p *u8Provider) Reap(string) error            { return nil }
func (p *u8Provider) VolumeEnsure(name string, size int, loc string) (string, error) {
	p.ensureCalls = append(p.ensureCalls, ensureCall{name, size, loc})
	return "100", nil
}
func (p *u8Provider) VolumeAttachedTo(string) (string, error) { return "", nil }
func (p *u8Provider) VolumeDetach(string) error              { return nil }
func (p *u8Provider) VolumeSize(string) (int, error)         { return p.volumeSize, nil }
func (p *u8Provider) VolumeRename(id, name string) error      { p.renameCalls = append(p.renameCalls, renameCall{id, name}); return nil }
func (p *u8Provider) VolumeDelete(id string) error           { p.deleteCalls = append(p.deleteCalls, id); return nil }
func (p *u8Provider) UserDataMaxBytes() int                  { return 32768 }
func (p *u8Provider) PriceHourly(string, string) string       { return "" }

func newU8Deployment(t *testing.T) (*deployment, *u8Provider) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAYBOX_REPO_DIR", repoRoot(t))
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.confDir, "keys"), 0o755)
	os.WriteFile(filepath.Join(d.confDir, "keys", "x.pub"), []byte("ssh-ed25519 AAAA x\n"), 0o644)
	os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte("LITTLEBOX_IP=10.0.0.1\nREMOTE_USER=dev\nGIT_NAME=A\nGIT_EMAIL=a@b.c\n"), 0o600)
	// swap in our fake provider by overriding loadProvider: monkey-patch via a
	// package var.
	prov := &u8Provider{volumeSize: 50}
	providerFactory = func(d *deployment, name string) (Provider, error) { return prov, nil }
	return d, prov
}

// TestProfileAddCreatesVolumeSeedConfig: add writes a config, seeds
// profile.toml, prepares ssh keys, ensures the volume, caches the id.
func TestProfileAddCreatesVolumeSeedConfig(t *testing.T) {
	d, prov := newU8Deployment(t)
	if err := profileAdd(d, "work", ""); err != nil {
		t.Fatal(err)
	}
	if len(prov.ensureCalls) != 1 || prov.ensureCalls[0].name != "daybox-work-vol" {
		t.Errorf("VolumeEnsure = %+v, want daybox-work-vol", prov.ensureCalls)
	}
	if len(prov.prepareCalls) != 1 {
		t.Errorf("PrepareSSHKeys = %v, want 1 call", prov.prepareCalls)
	}
	if !fileExists(filepath.Join(d.confDir, "profiles", "work", "config")) {
		t.Error("profile config not written")
	}
	if !fileExists(filepath.Join(d.confDir, "profiles", "work", "profile.toml")) {
		t.Error("seed profile.toml not written")
	}
	vid := readFileTrim(filepath.Join(d.stateDir, "profiles", "work", "volume_id"))
	if vid != "100" {
		t.Errorf("cached volume id = %q, want 100", vid)
	}
}

// TestProfileAddRefusesExisting: never overwrite an existing profile.
func TestProfileAddRefusesExisting(t *testing.T) {
	d, _ := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "work", "config"), 0o755)
	// pretend a config exists
	os.WriteFile(filepath.Join(d.confDir, "profiles", "work", "config"), []byte("x"), 0o600)
	if err := profileAdd(d, "work", ""); err == nil {
		t.Error("add must refuse an existing profile")
	}
}

// TestProfileUseSetsCurrent: use writes the current_profile file + rejects
// unknown profiles.
func TestProfileUseSetsCurrent(t *testing.T) {
	d, _ := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "work"), 0o755)
	if err := profileUse(d, "work"); err != nil {
		t.Fatal(err)
	}
	if got := readFileTrim(filepath.Join(d.stateDir, "current_profile")); got != "work" {
		t.Errorf("current_profile = %q, want work", got)
	}
	if err := profileUse(d, "nope"); err == nil {
		t.Error("use must reject an unknown profile")
	}
}

// TestProfileRenameRefusesLiveBox: a live box would orphan the reaper
// counters + leave a net ghost, so rename requires the box down first.
func TestProfileRenameRefusesLiveBox(t *testing.T) {
	d, prov := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "old"), 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "old", "volume_id"), []byte("100"), 0o644)
	prov.probeResult = &Server{ID: "1", Name: "daybox-old", IP: "1.2.3.4", Type: "ccx33"}
	if err := profileRename(d, "old", "new"); err == nil {
		t.Error("rename must refuse a live box")
	}
	if len(prov.renameCalls) != 0 {
		t.Errorf("rename should not touch the volume on a live box: %v", prov.renameCalls)
	}
	// box down -> rename proceeds + renames the volume
	prov.probeResult = nil
	if err := profileRename(d, "old", "new"); err != nil {
		t.Fatal(err)
	}
	if len(prov.renameCalls) != 1 || prov.renameCalls[0].name != "daybox-new-vol" {
		t.Errorf("volume rename = %+v, want daybox-new-vol", prov.renameCalls)
	}
	if !fileExists(filepath.Join(d.stateDir, "profiles", "new")) {
		t.Error("state dir not moved")
	}
}

// TestProfileRmProtectsDefault: 'default' cannot be removed.
func TestProfileRmProtectsDefault(t *testing.T) {
	d, _ := newU8Deployment(t)
	if err := profileRm(d, "default", ""); err == nil {
		t.Error("rm must refuse the 'default' profile")
	}
}

// TestProfileRmReapsLiveBoxThenDeletesState: rm of an up box reaps it
// (billing stops, frees the volume) before removing state.
func TestProfileRmReapsLiveBoxThenDeletesState(t *testing.T) {
	d, prov := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "gone"), 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "gone", "volume_id"), []byte("100"), 0o644)
	prov.probeResult = &Server{ID: "9", Name: "daybox-gone", IP: "1.2.3.4", Type: "ccx33"} // live
	// reuse a real-ish down: downBox will probe + reap; stub the unmount so it's fast
	unmountWorkFn = func(p *profile, ip string) string { return "" }
	if err := profileRm(d, "gone", ""); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(d.stateDir, "profiles", "gone")) {
		t.Error("state dir should be removed")
	}
	// volume kept (no --purge)
	if len(prov.deleteCalls) != 0 {
		t.Errorf("rm without --purge should KEEP the volume: %v", prov.deleteCalls)
	}
}

// TestProfileRmPurgeDeletesVolume: --purge deletes the volume.
func TestProfileRmPurgeDeletesVolume(t *testing.T) {
	d, prov := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "gone"), 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "gone", "volume_id"), []byte("100"), 0o644)
	unmountWorkFn = func(p *profile, ip string) string { return "" }
	if err := profileRm(d, "gone", "--purge"); err != nil {
		t.Fatal(err)
	}
	if len(prov.deleteCalls) != 1 || prov.deleteCalls[0] != "100" {
		t.Errorf("purge -> deleteCalls=%v, want [100]", prov.deleteCalls)
	}
}

// TestProfileSeedInitShowPath: init writes the default seed, show prints it,
// path prints the path; init refuses an existing seed.
func TestProfileSeedInitShowPath(t *testing.T) {
	d, _ := newU8Deployment(t)
	var out bytes.Buffer
	if err := profileSeed(d, "path", "work", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), "profiles/work/profile.toml") {
		t.Errorf("path = %q", out.String())
	}
	// init
	out.Reset()
	if err := profileSeed(d, "init", "work", &out); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(d.confDir, "profiles", "work", "profile.toml")) {
		t.Error("seed not written")
	}
	// show
	out.Reset()
	if err := profileSeed(d, "show", "work", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "# ") || !strings.Contains(out.String(), "profile.toml") {
		t.Errorf("show should print the path header: %q", out.String())
	}
	// init refuses existing
	if err := profileSeed(d, "init", "work", &out); err == nil {
		t.Error("init must refuse an existing seed")
	}
}

// TestSetupSeedsAndCreatesVolume: setup seeds the profile.toml (if absent)
// + creates the volume; never overwrites an edited seed.
func TestSetupSeedsAndCreatesVolume(t *testing.T) {
	d, prov := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	if err := setup(d); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(d.confDir, "profiles", "default", "profile.toml")) {
		t.Error("setup should seed profile.toml")
	}
	if len(prov.ensureCalls) != 1 {
		t.Errorf("setup -> VolumeEnsure=%v, want 1", prov.ensureCalls)
	}
	// rerun with an edited seed: the seed is NOT overwritten
	seed := filepath.Join(d.confDir, "profiles", "default", "profile.toml")
	os.WriteFile(seed, []byte("# my edits\n[meta]\nowner=\"me\"\n"), 0o644)
	prov.ensureCalls = nil
	if err := setup(d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(seed)
	if !strings.Contains(string(b), "my edits") {
		t.Error("setup must not overwrite an edited seed")
	}
}
