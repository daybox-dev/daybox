package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// downRecordingProvider records every destructive call so a test asserts
// the order + that the server is reaped (billing stops) and the volume
// detached.
type downRecordingProvider struct {
	probeResult       *Server
	attachedToResults []string // sequence returned by VolumeAttachedTo (polled)
	attachedIdx        int
	reapCalls          []string
	detachCalls        []string
}

func (r *downRecordingProvider) Name() string             { return "hetzner" }
func (r *downRecordingProvider) HasCredentials() bool      { return true }
func (r *downRecordingProvider) CheckCredentials() error   { return nil }
func (r *downRecordingProvider) PrepareSSHKeys(dir string) error { return nil }
func (r *downRecordingProvider) Probe(name string) (*Server, error) { return r.probeResult, nil }
func (r *downRecordingProvider) Summon(name, st, img, loc, vid, ud string) (Server, error) {
	return Server{}, nil
}
func (r *downRecordingProvider) Reap(id string) error { r.reapCalls = append(r.reapCalls, id); return nil }
func (r *downRecordingProvider) VolumeEnsure(name string, size int, loc string) (string, error) {
	return "123456", nil
}
func (r *downRecordingProvider) VolumeAttachedTo(id string) (string, error) {
	if r.attachedIdx < len(r.attachedToResults) {
		v := r.attachedToResults[r.attachedIdx]
		r.attachedIdx++
		return v, nil
	}
	return "", nil
}
func (r *downRecordingProvider) VolumeDetach(id string) error { r.detachCalls = append(r.detachCalls, id); return nil }
func (r *downRecordingProvider) VolumeSize(id string) (int, error) { return 50, nil }
func (r *downRecordingProvider) VolumeRename(id, n string) error { return nil }
func (r *downRecordingProvider) VolumeDelete(id string) error    { return nil }
func (r *downRecordingProvider) UserDataMaxBytes() int          { return 32768 }
func (r *downRecordingProvider) PriceHourly(st, loc string) string { return "" }

type fakeDownOps struct {
	enabled     bool
	droppedNames []string
}

func (f *fakeDownOps) netEnabled() bool { return f.enabled }
func (f *fakeDownOps) dropNetNode(serverName string) {
	f.droppedNames = append(f.droppedNames, serverName)
}

func init() {
	// keep the detach-poll instant + skip the real ssh unmount in tests
	downSleep = func(time.Duration) {}
	unmountWorkFn = func(p *profile, ip string) string { return "" }
}

func newDownTestProfile(t *testing.T) *profile {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAYBOX_REPO_DIR", repoRoot(t))
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte("LITTLEBOX_IP=10.0.0.1\nGIT_NAME=A\nGIT_EMAIL=a@b.c\nREMOTE_USER=dev\n"), 0o600)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "default", "volume_id"), []byte("123456"), 0o644)
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDownNoBoxIsNoop: no box -> "no big box running", idle reset, no reap.
func TestDownNoBoxIsNoop(t *testing.T) {
	p := newDownTestProfile(t)
	prov := &downRecordingProvider{probeResult: nil}
	ops := &fakeDownOps{enabled: true}
	if err := downBox(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if len(prov.reapCalls) != 0 {
		t.Errorf("no box -> reapCalls=%v, want none", prov.reapCalls)
	}
	if len(ops.droppedNames) != 0 {
		t.Errorf("no box -> dropNetNode=%v, want none", ops.droppedNames)
	}
}

// TestDownDetachesThenReaps: a box with an attached volume -> unmount,
// detach, poll until free, THEN reap (deleting before the detach settles
// wedges the volume locked — seen 2026-07-21).
func TestDownDetachesThenReaps(t *testing.T) {
	p := newDownTestProfile(t)
	prov := &downRecordingProvider{
		probeResult:       &Server{ID: "99", Name: "daybox-default", IP: "5.78.0.1", Type: "ccx33"},
		// attached to our server, then free after detach
		attachedToResults: []string{"99", "", ""},
	}
	ops := &fakeDownOps{enabled: false}
	if err := downBox(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if len(prov.detachCalls) != 1 || prov.detachCalls[0] != "123456" {
		t.Errorf("detach calls = %v, want [123456]", prov.detachCalls)
	}
	if len(prov.reapCalls) != 1 || prov.reapCalls[0] != "99" {
		t.Errorf("reap calls = %v, want [99]", prov.reapCalls[0])
	}
}

// TestDownReapsEvenIfVolumeAlreadyFree: if the volume is not attached to
// this box, skip the detach dance and just reap (billing stops).
func TestDownReapsEvenIfVolumeAlreadyFree(t *testing.T) {
	p := newDownTestProfile(t)
	prov := &downRecordingProvider{
		probeResult:       &Server{ID: "99", Name: "daybox-default", IP: "5.78.0.1", Type: "ccx33"},
		attachedToResults: []string{""}, // free
	}
	if err := downBox(p, prov, &fakeDownOps{}); err != nil {
		t.Fatal(err)
	}
	if len(prov.detachCalls) != 0 {
		t.Errorf("free volume -> detachCalls=%v, want none", prov.detachCalls)
	}
	if len(prov.reapCalls) != 1 {
		t.Errorf("free volume -> reapCalls=%v, want [99]", prov.reapCalls)
	}
}

// TestDownDropsNetNode: the box's headscale node is dropped immediately on
// down (ephemeral GC would take ~30min; the net view must not show ghosts).
func TestDownDropsNetNode(t *testing.T) {
	p := newDownTestProfile(t)
	prov := &downRecordingProvider{
		probeResult:       &Server{ID: "99", Name: "daybox-default", IP: "5.78.0.1", Type: "ccx33"},
		attachedToResults: []string{""},
	}
	ops := &fakeDownOps{enabled: true}
	if err := downBox(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if len(ops.droppedNames) != 1 || ops.droppedNames[0] != "daybox-default" {
		t.Errorf("dropNetNode = %v, want [daybox-default]", ops.droppedNames)
	}
}

// TestDownSurvivesMissingIdentityConfig: teardown must NOT be blocked by a
// half-edited config.local — a commented-out GIT_NAME once made every reap
// path silently defeated while the box billed. down needs only creds.
func TestDownSurvivesMissingIdentityConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAYBOX_REPO_DIR", repoRoot(t))
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	// NO GIT_NAME / GIT_EMAIL — a broken config.local. Only REMOTE_USER + LITTLEBOX_IP.
	os.WriteFile(filepath.Join(d.confDir, "config.local"), []byte("LITTLEBOX_IP=10.0.0.1\nREMOTE_USER=dev\n"), 0o600)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "default", "volume_id"), []byte("123456"), 0o644)
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatalf("down must build a profile from identity-light config: %v", err)
	}
	prov := &downRecordingProvider{
		probeResult:       &Server{ID: "99", Name: "daybox-default", IP: "5.78.0.1", Type: "ccx33"},
		attachedToResults: []string{""},
	}
	if err := downBox(p, prov, &fakeDownOps{}); err != nil {
		t.Fatalf("down must reap despite missing identity: %v", err)
	}
	if len(prov.reapCalls) != 1 {
		t.Errorf("reapCalls=%v, want [99] — billing must stop", prov.reapCalls)
	}
}

// TestDownResetsIdle: down resets the idle/unreachable counters so the
// next summon starts clean (bash: reset_idle).
func TestDownResetsIdle(t *testing.T) {
	p := newDownTestProfile(t)
	// pre-seed non-zero counters
	writeFile(p.idleTicksFile(), "5")
	writeFile(p.unreachTicksFile(), "3")
	prov := &downRecordingProvider{
		probeResult:       &Server{ID: "99", Name: "daybox-default", IP: "5.78.0.1", Type: "ccx33"},
		attachedToResults: []string{""},
	}
	if err := downBox(p, prov, &fakeDownOps{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.idleTicksFile()); strings.TrimSpace(string(b)) != "0" {
		t.Errorf("idle ticks = %q, want 0", b)
	}
}
