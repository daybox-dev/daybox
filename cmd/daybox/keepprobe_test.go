package main

import (
	"strings"
	"testing"
	"time"
)

// keepprobe_test.go — tests the pure emitter + the runKeepProbe orchestration
// with a fake env. The shell-running half (realKeepProbeEnv) is covered by
// the live reaper verification (W11), not the unit suite.

// fakeKeepProbeEnv scripts the substrate: fixed conns/load, and a fresh()
// that consults a map of "path -> fresh bool" so tests can stage any
// filesystem shape without touching disk.
type fakeKeepProbeEnv struct {
	connsN  int
	loadF   float64
	freshBy map[string]bool // path -> fresh; absent path -> not fresh
}

func (f fakeKeepProbeEnv) conns(planeIP string) (int, error)      { return f.connsN, nil }
func (f fakeKeepProbeEnv) load() (float64, error)                 { return f.loadF, nil }
func (f fakeKeepProbeEnv) fresh(path string, _ int) (bool, error) { return f.freshBy[path], nil }

func mustKeepSignal(path, within string) keepSignal {
	d, err := time.ParseDuration(within)
	if err != nil {
		panic(err)
	}
	return keepSignal{path: path, within: d}
}

func TestEvalKeepEmpty(t *testing.T) {
	// zero signals: ssh+load only — no file= lines (the safe baseline).
	got := evalKeep(0, 0.01, nil)
	want := "conns=0\nload=0.01\n"
	if got != want {
		t.Errorf("evalKeep empty keep:\n got %q\nwant %q", got, want)
	}
}

func TestRunKeepProbeEmpty(t *testing.T) {
	got := runKeepProbe(nil, "10.0.0.1", fakeKeepProbeEnv{connsN: 2, loadF: 0.5})
	want := "conns=2\nload=0.5\n"
	if got != want {
		t.Errorf("empty keep -> %q, want %q", got, want)
	}
}

func TestRunKeepProbeOneFresh(t *testing.T) {
	keep := []keepSignal{mustKeepSignal("/work/state/claude/projects", "10m")}
	got := runKeepProbe(keep, "10.0.0.1", fakeKeepProbeEnv{
		connsN: 0, loadF: 0.01, freshBy: map[string]bool{"/work/state/claude/projects": true},
	})
	want := "conns=0\nload=0.01\nfile=/work/state/claude/projects=1\n"
	if got != want {
		t.Errorf("one fresh:\n got %q\nwant %q", got, want)
	}
	// the fresh path is in the output, parseable by the plane to log WHICH
	// signal kept the box.
	if !strings.Contains(got, "/work/state/claude/projects=1") {
		t.Error("fresh path must appear path-bearing in output")
	}
}

func TestRunKeepProbeOneStale(t *testing.T) {
	keep := []keepSignal{mustKeepSignal("/tmp/watched", "3m")}
	got := runKeepProbe(keep, "10.0.0.1", fakeKeepProbeEnv{
		connsN: 0, loadF: 0.01, freshBy: map[string]bool{}, // path absent -> not fresh
	})
	want := "conns=0\nload=0.01\nfile=/tmp/watched=0\n"
	if got != want {
		t.Errorf("one stale:\n got %q\nwant %q", got, want)
	}
}

func TestRunKeepProbeMultipleMixed(t *testing.T) {
	keep := []keepSignal{
		mustKeepSignal("/work/state/claude/projects", "10m"),
		mustKeepSignal("/tmp/build.log", "2m"),
	}
	got := runKeepProbe(keep, "10.0.0.1", fakeKeepProbeEnv{
		connsN: 1, loadF: 0.2,
		freshBy: map[string]bool{"/work/state/claude/projects": false, "/tmp/build.log": true},
	})
	// order follows declared order; one fresh, one stale.
	want := "conns=1\nload=0.2\nfile=/work/state/claude/projects=0\nfile=/tmp/build.log=1\n"
	if got != want {
		t.Errorf("mixed:\n got %q\nwant %q", got, want)
	}
}

func TestWithinMinutes(t *testing.T) {
	cases := []struct {
		d    string
		want int
	}{
		{"10m", 10},
		{"3m", 3},
		{"90s", 2}, // ceil(1.5) = 2
		{"30s", 1}, // floored at 1
		{"0s", 1},  // floored at 1
		{"1h", 60},
	}
	for _, c := range cases {
		d, _ := time.ParseDuration(c.d)
		if got := withinMinutes(d); got != c.want {
			t.Errorf("withinMinutes(%s) = %d, want %d", c.d, got, c.want)
		}
	}
}
