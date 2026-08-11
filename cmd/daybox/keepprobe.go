package main

// keepprobe.go — `daybox keep-probe <planeIP>`, the on-box half of the
// reaper's busy probe. Runs ON a summoned box (as `daybox-agent keep-probe`);
// the plane's reaper ssh-runs it instead of shipping a hand-built probe
// string. See docs/keep-and-proposals.md (W2).
//
// Why the box does it: keep.toml is box-owned now (it lives on the
// persistent /work volume), so the box is the authority on its own
// file-freshness signals. The plane no longer loads keep.toml or knows the
// entry count — it ssh-runs this, parses the key=value output, and decides
// keep/reap. The 30s `timeout 30 ssh` the caller wraps this in is the
// keep-DoS bound: a box that bombs its own keep makes its own probe slow →
// unreachable tick → self-reaps (see REAP_AFTER_UNREACHABLE_MIN).
//
// Output (what parseProbeOutput expects, one line per signal):
//
//	conns=N
//	load=F
//	file=<path>=0|1   (one per [[files]] entry, in declared order)
//
// conns/load are the substrate signals (ssh sessions excluding the plane,
// 1-min loadavg) — unchanged from the old plane-built probe. The plane
// passes its own IP as the arg so the self-exclusion (this probe's ssh
// session counts as a :22 conn) stays exact without the box mirroring
// plane-side config.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// keepTomlPath is where a box's keep.toml lives — on the persistent /work
// volume (survives reaps + down/up), so the box owns its liveness
// declaration. $HOME is on the ephemeral root disk, so keep MUST be under
// /work to persist. See docs/keep-and-proposals.md D1.
const keepTomlPath = "/work/state/daybox/keep.toml"

// cmdKeepProbe is `daybox keep-probe <planeIP>` — runs ON a summoned box.
func cmdKeepProbe(p Parsed) {
	fs := flag.NewFlagSet("keep-probe", flag.ExitOnError)
	keepPath := fs.String("keep", keepTomlPath, "path to the box's keep.toml")
	fs.Parse(p.Rest())
	if fs.NArg() < 1 {
		log.Fatal("usage: daybox keep-probe <planeIP>  (the reaper runs this on the box)")
	}
	planeIP := fs.Arg(0)
	keep := loadKeepToml(*keepPath)
	fmt.Print(runKeepProbe(keep, planeIP, realKeepProbeEnv{}))
}

// keepProbeEnv is the on-box substrate the probe reads: ssh conns excluding
// the plane, 1-min load, and per-path file freshness. An interface so the
// emitter is unit-testable without a real box (the live reaper owns the
// shell-running half; the suite owns the logic).
type keepProbeEnv interface {
	conns(planeIP string) (int, error)
	load() (float64, error)
	fresh(path string, withinMin int) (bool, error)
}

// runKeepProbe loads nothing (the caller passes the parsed keep set), runs
// the substrate + each file signal via env, and returns the key=value
// output. A signal whose fresh() errors degrades to not-fresh (file=path=0)
// — never to keeping the box forever; the lifetime cap is the hard bound
// either way. conns/load errors degrade to 0 (the caller's short-read guard
// treats a missing required key as unreachable, not a silent zero-idle).
func runKeepProbe(keep []keepSignal, planeIP string, env keepProbeEnv) string {
	conns, _ := env.conns(planeIP) // degrade to 0 on error
	load, _ := env.load()
	files := make([]fileSignalResult, 0, len(keep))
	for _, k := range keep {
		fresh := false
		if v, err := env.fresh(k.path, withinMinutes(k.within)); err == nil {
			fresh = v
		}
		files = append(files, fileSignalResult{path: k.path, fresh: fresh})
	}
	return evalKeep(conns, load, files)
}

// evalKeep is the pure emitter: assembles the key=value output from the
// signal results. One line per signal so parseProbeOutput can log WHICH
// path kept the box (and status can surface the active set). load uses
// shortest-round-trip formatting so it re-parses cleanly.
func evalKeep(conns int, load float64, files []fileSignalResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "conns=%d\n", conns)
	fmt.Fprintf(&b, "load=%s\n", strconv.FormatFloat(load, 'g', -1, 64))
	for _, f := range files {
		v := 0
		if f.fresh {
			v = 1
		}
		fmt.Fprintf(&b, "file=%s=%d\n", f.path, v)
	}
	return b.String()
}

// withinMinutes is the ceil(within/1min) floored at 1 the old plane-built
// probe used. The reaper ticks every 5min, so sub-5min windows are
// misleading but not forbidden — the lifetime cap is the hard bound either
// way (design lean: document, don't enforce).
func withinMinutes(within time.Duration) int {
	mins := int(within / time.Minute)
	if within%time.Minute != 0 {
		mins++
	}
	if mins < 1 {
		mins = 1
	}
	return mins
}

// realKeepProbeEnv runs the same shell the old plane-built probe did:
// ss for established :22 conns, /proc/loadavg for load, GNU find -newermt
// for file freshness. The plane's IP is matched in Go (never interpolated
// into a shell string) so an untrusted arg can't inject. keep paths were
// validated by keepPathRe at load (absolute, [A-Za-z0-9._/-] only), so shq
// single-quoting is defense-in-depth, not the primary guard.
type realKeepProbeEnv struct{}

func (realKeepProbeEnv) conns(planeIP string) (int, error) {
	out, err := exec.Command("sh", "-c", "ss -Htn state established '( sport = :22 )'").Output()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.Contains(line, planeIP) {
			continue
		}
		n++
	}
	return n, nil
}

func (realKeepProbeEnv) load() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, fmt.Errorf("short /proc/loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

func (realKeepProbeEnv) fresh(path string, withinMin int) (bool, error) {
	// head -1 + grep -c . = "any fresh file?" and SIGPIPEs find after the
	// first match (mirrors the old probe; avoids reading a whole tree).
	// grep -c exits 1 on zero matches — that's "not fresh", not an error.
	cmd := fmt.Sprintf("find %s -type f -newermt '-%d minutes' 2>/dev/null | head -1 | grep -c .",
		shq(path), withinMin)
	out, _ := exec.Command("sh", "-c", cmd).Output()
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n > 0, nil
}
