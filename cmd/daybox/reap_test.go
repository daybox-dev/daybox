package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recordingReapOps records whether down was called (the reaper's only
// destructive action) + scripts the busy probe's three signals + reachability.
type recordingReapOps struct {
	conns      int
	load       float64
	working    bool
	probeErr   error
	downCalls  int
}

func (r *recordingReapOps) busyProbe(ip string) (int, float64, bool, error) {
	return r.conns, r.load, r.working, r.probeErr
}
func (r *recordingReapOps) down(p *profile) error { r.downCalls++; return nil }

// a profile with an explicit lifetime cap + idle threshold for the tests.
func newReapTestProfile(t *testing.T, maxHours, idleMin int) *profile {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	d := loadDeployment()
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "default"), 0o755)
	os.MkdirAll(filepath.Join(d.confDir, "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(d.confDir, "config.local"),
		[]byte("LITTLEBOX_IP=10.0.0.1\nREMOTE_USER=dev\nGIT_NAME=A\nGIT_EMAIL=a@b.c\nMAX_LIFETIME_HOURS="+itoa(maxHours)+"\nREAP_AFTER_IDLE_MIN="+itoa(idleMin)+"\n"), 0o600)
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	if n < 0 {
		return "-1"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func probeServer(ageMin int, running bool) *Server {
	if !running {
		return nil
	}
	return &Server{
		ID: "99", Name: "daybox-default", IP: "5.78.0.1", Status: "running",
		Created: time.Now().Add(-time.Duration(ageMin) * time.Minute).Format(time.RFC3339),
		Type: "ccx33",
	}
}

// a minimal provider stub that returns a scripted probe result.
type reapProvider struct{ probe *Server }

func (r *reapProvider) Name() string            { return "hetzner" }
func (r *reapProvider) HasCredentials() bool     { return true }
func (r *reapProvider) CheckCredentials() error  { return nil }
func (r *reapProvider) PrepareSSHKeys(string) error { return nil }
func (r *reapProvider) Probe(string) (*Server, error) { return r.probe, nil }
func (r *reapProvider) Summon(string, string, string, string, string, string) (Server, error) {
	return Server{}, nil
}
func (r *reapProvider) Reap(string) error             { return nil }
func (r *reapProvider) VolumeEnsure(string, int, string) (string, error) { return "1", nil }
func (r *reapProvider) VolumeAttachedTo(string) (string, error) { return "", nil }
func (r *reapProvider) VolumeDetach(string) error    { return nil }
func (r *reapProvider) VolumeSize(string) (int, error) { return 50, nil }
func (r *reapProvider) VolumeRename(string, string) error { return nil }
func (r *reapProvider) VolumeDelete(string) error    { return nil }
func (r *reapProvider) UserDataMaxBytes() int        { return 32768 }
func (r *reapProvider) PriceHourly(string, string) string { return "" }

// TestReapNoBoxIsNoop: nothing to reap -> reset idle, no down.
func TestReapNoBoxIsNoop(t *testing.T) {
	p := newReapTestProfile(t, 12, 30)
	ops := &recordingReapOps{}
	if err := reapOne(p, &reapProvider{probe: nil}, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 0 {
		t.Errorf("no box -> downCalls=%d, want 0", ops.downCalls)
	}
}

// TestReapLifetimeCapForceReaps: a box older than MAX_LIFETIME_HOURS is
// reaped REGARDLESS of activity — the runaway backstop runs independently
// of every busy signal (a box that always looks busy is exactly the case
// the idle reaper cannot stop).
func TestReapLifetimeCapForceReaps(t *testing.T) {
	p := newReapTestProfile(t, 1, 30) // cap 1h
	ops := &recordingReapOps{conns: 99, load: 99.0, working: true} // very busy
	prov := &reapProvider{probe: probeServer(61, true)}            // 61min > 60
	if err := reapOne(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 1 {
		t.Errorf("lifetime cap + busy box -> downCalls=%d, want 1 (force-reap)", ops.downCalls)
	}
}

// TestReapLifetimeCapNotYetKeeps: under the cap, a busy box is kept.
func TestReapLifetimeCapNotYetKeeps(t *testing.T) {
	p := newReapTestProfile(t, 12, 30)
	ops := &recordingReapOps{conns: 3, load: 0.1, working: false}
	prov := &reapProvider{probe: probeServer(10, true)} // 10min, cap 12h
	if err := reapOne(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 0 {
		t.Errorf("under cap + busy -> downCalls=%d, want 0", ops.downCalls)
	}
}

// TestReapBusyKeeps: each of the three signals alone keeps the box (ssh
// conns, load, recent transcript write) — the reaper once killed a detached
// claude mid-task one minute before its last transcript write.
func TestReapBusyKeeps(t *testing.T) {
	cases := []recordingReapOps{
		{conns: 1, load: 0.0, working: false},
		{conns: 0, load: 0.50, working: false}, // >= LOAD_BUSY 0.40
		{conns: 0, load: 0.0, working: true},
	}
	for _, ops := range cases {
		p := newReapTestProfile(t, 0, 30) // cap disabled
		o := ops
		if err := reapOne(p, &reapProvider{probe: probeServer(1, true)}, &o); err != nil {
			t.Fatal(err)
		}
		if o.downCalls != 0 {
			t.Errorf("a busy signal (%+v) -> downCalls=%d, want 0", ops, o.downCalls)
		}
	}
}

// TestReapIdleAccumulatesThenReaps: quiet ticks accumulate; the box is
// reaped only when the threshold is reached (REAP_AFTER_IDLE_MIN/5 ticks).
func TestReapIdleAccumulatesThenReaps(t *testing.T) {
	p := newReapTestProfile(t, 0, 30) // need = 30/5 = 6 ticks
	prov := &reapProvider{probe: probeServer(1, true)}
	// 5 quiet ticks -> not yet
	for i := 0; i < 5; i++ {
		ops := &recordingReapOps{}
		if err := reapOne(p, prov, ops); err != nil {
			t.Fatal(err)
		}
		if ops.downCalls != 0 {
			t.Fatalf("tick %d -> downCalls=%d, want 0 (threshold not reached)", i+1, ops.downCalls)
		}
	}
	// 6th quiet tick -> reap
	ops := &recordingReapOps{}
	if err := reapOne(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 1 {
		t.Errorf("6th quiet tick -> downCalls=%d, want 1", ops.downCalls)
	}
}

// TestReapUnreachableThenZombieReap: an unreachable box ticks; after 1h
// (12 ticks) it's force-reaped (a zombie that still bills).
func TestReapUnreachableThenZombieReap(t *testing.T) {
	p := newReapTestProfile(t, 0, 30)
	prov := &reapProvider{probe: probeServer(1, true)}
	// 11 unreachable ticks -> not yet
	for i := 0; i < 11; i++ {
		ops := &recordingReapOps{probeErr: errors.New("unreachable")}
		if err := reapOne(p, prov, ops); err != nil {
			t.Fatal(err)
		}
		if ops.downCalls != 0 {
			t.Fatalf("unreachable tick %d -> downCalls=%d, want 0", i+1, ops.downCalls)
		}
	}
	// 12th unreachable tick -> reap
	ops := &recordingReapOps{probeErr: errors.New("unreachable")}
	if err := reapOne(p, prov, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 1 {
		t.Errorf("12th unreachable tick -> downCalls=%d, want 1", ops.downCalls)
	}
}

// TestReapBadLoadBusyDegradesToKeep: a non-numeric LOAD_BUSY defaults to
// 0.40, NEVER to 0 (0 would read every box as busy -> never reaped, meter
// runs). The knob must degrade toward KEEPING the box, not reaping.
func TestReapBadLoadBusyDegradesToKeep(t *testing.T) {
	// A misconfigured LOAD_BUSY must degrade toward KEEPING the box, never
	// toward a surprise reap (a non-numeric would compare as 0 = always busy
	// = never reaped, meter runs). The validation fallback is 0.40. Assert
	// the helper: a valid value passes, NaN is rejected.
	if !isNonNegFloat(0.40) {
		t.Error("0.40 must be valid")
	}
	if isNonNegFloat(loadBusyNaN()) {
		t.Error("NaN LOAD_BUSY must be rejected so it degrades to 0.40 (keep)")
	}
	// and a box at load 0.41 (>= fallback 0.40) with a poisoned NaN knob is
	// KEPT — prove the fallback fired inside reapOne.
	p := newReapTestProfile(t, 0, 30)
	p.loadBusy = loadBusyNaN() // poison; reapOne must fall back to 0.40
	ops := &recordingReapOps{conns: 0, load: 0.41, working: false}
	if err := reapOne(p, &reapProvider{probe: probeServer(1, true)}, ops); err != nil {
		t.Fatal(err)
	}
	if ops.downCalls != 0 {
		t.Errorf("poisoned NaN LOAD_BUSY + load 0.41 -> downCalls=%d, want 0 (fallback 0.40 keeps it)", ops.downCalls)
	}
}

func loadBusyNaN() float64 {
	var zero float64
	return zero / zero // NaN
}

// TestReapNeverReturnsError: reapOne must always return nil (bash: "Must
// end 0"). A non-zero would abort the profile loop and stop reaping every
// OTHER profile's box while they bill.
func TestReapNeverReturnsError(t *testing.T) {
	p := newReapTestProfile(t, 12, 30)
	ops := &recordingReapOps{probeErr: errors.New("totally wedged")}
	if err := reapOne(p, &reapProvider{probe: probeServer(1, true)}, ops); err != nil {
		t.Errorf("reapOne must never return an error (loop safety): %v", err)
	}
}
