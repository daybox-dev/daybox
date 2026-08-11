package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// reap.go — the plane-side `reap` (bash reap_one + cmd_reap), run by the
// idle-reaper systemd timer (every 5min). It force-deletes a box when:
//   - it exceeds the hard lifetime cap (MAX_LIFETIME_HOURS), the runaway
//     backstop that runs INDEPENDENTLY of every busy signal, on purpose —
//     a box that always looks busy is exactly the case the idle reaper
//     cannot stop; OR
//   - it has been idle for REAP_AFTER_IDLE_MIN (counted in 5min ticks); OR
//   - it has been unreachable for 1h (a zombie that still bills).
//
// Three busy signals (bash reap_one): ssh sessions, load, and a
// file-freshness signal — a detached agent on an API-bound task shows
// load ~0.2 and zero connections, and the reaper once killed one mid-task
// one minute before its last transcript write. The file-freshness signal
// is user-declared per profile in keep.toml (the [keep] table); the old
// hardcoded /work/state/claude/projects path is gone — a pi user, a
// build, a dev server declare their own paths. An absent keep.toml means
// ssh+load only (the safe baseline); the lifetime cap bounds spend
// regardless. Misconfigured knobs degrade toward KEEPING the box, never
// toward a surprise reap (a non-numeric LOAD_BUSY would compare as 0 =
// always busy = never reaped, meter runs; an IDLE_MIN under one tick
// would reap on the first quiet probe).

// reapOps is the injectable surface for the reaper's on-box busy probe.
// Tests fake it to assert the cap / idle / unreachable decisions without
// a box or headscale.
type reapOps interface {
	// busyProbe ssh's the box for the busy signals, returning ssh
	// connection count, 1-min load, and a per-signal file-freshness
	// result (one per declared [keep] entry, in declaration order). A
	// non-nil error means the box was unreachable this tick.
	busyProbe(ip string) (conns int, load float64, files []fileSignalResult, err error)
	// down reaps the box (volume detach + server delete + net node drop).
	down(p *profile) error
}

// fileSignalResult is one [keep] file-signal's result: whether any file
// under its path was fresh within its window. The path is carried
// alongside so reapOne can log WHICH signal kept the box.
type fileSignalResult struct {
	path  string
	fresh bool
}

// anyFresh is the OR over [keep] file-signals: the box is kept if ANY
// declared path has a fresh file. Returns the first fresh signal's path
// for logging ("kept by file-signal <path>"). Pure so the OR is
// unit-testable without a fake or ssh.
func anyFresh(files []fileSignalResult) (fresh bool, path string) {
	for _, f := range files {
		if f.fresh {
			return true, f.path
		}
	}
	return false, ""
}

// reapOne is the per-profile reaper. Returns nil always (bash: "Must end
// 0" — a non-zero would abort the loop and stop reaping every OTHER
// profile's box while they bill).
func reapOne(p *profile, prov Provider, ops reapOps) error {
	s, err := prov.Probe(p.serverName)
	if err != nil {
		say("[%s] reap probe failed: %v", p.name, err)
		return nil
	}
	if s == nil {
		resetIdle(p)
		return nil
	}

	// ---- hard lifetime cap (runaway backstop) ----
	// Checked BEFORE the busy probe and independently of every busy signal,
	// on purpose: a box that always looks busy is exactly the case the idle
	// reaper cannot stop. The workspace volume survives, so what's lost is
	// the root disk + running processes, not your repos or state.
	if p.maxLifetimeHours > 0 {
		capMin := p.maxLifetimeHours * 60
		if ageMin, ok := boxAgeMinutes(s.Created); ok {
			if ageMin >= capMin {
				say("[%s] LIFETIME CAP: box is %dmin old (cap %dh) — force reaping regardless of activity", p.name, ageMin, p.maxLifetimeHours)
				return ops.down(p)
			}
			left := capMin - ageMin
			if left <= 30 {
				say("[%s] WARNING: lifetime cap in %dmin — this box will be reaped even if busy. 'daybox down && daybox up' starts a fresh one.", p.name, left)
			}
		} else {
			say("[%s] WARNING: could not parse box creation time — lifetime cap NOT enforced this tick", p.name)
		}
	}

	conns, load, files, probeErr := ops.busyProbe(s.IP)
	if probeErr != nil {
		// unreachable: tick the unreachable counter; REAP_AFTER_UNREACHABLE_MIN
		// (default 60 = 12 ticks × 5min) = zombie reaped. A box sabotaging its
		// own keep makes its own probe time out → self-reaps on this clock.
		u := readInt(p.unreachTicksFile()) + 1
		writeFile(p.unreachTicksFile(), strconv.Itoa(u))
		need := p.reapAfterUnreachableMin / 5
		if need < 1 {
			need = 1
		}
		say("[%s] big box unreachable (tick %d/%d)", p.name, u, need)
		if u >= need {
			say("[%s] unreachable for %dmin — force reaping", p.name, p.reapAfterUnreachableMin)
			return ops.down(p)
		}
		return nil
	}

	// reachable: reset unreachable, evaluate the busy signals
	writeFile(p.unreachTicksFile(), "0")
	// degrade toward KEEP: a bad LOAD_BUSY defaults to 0.40, never to 0
	loadBusy := p.loadBusy
	if !isNonNegFloat(loadBusy) {
		say("[%s] invalid LOAD_BUSY '%v' — using 0.40", p.name, loadBusy)
		loadBusy = 0.40
	}
	need := p.reapAfterIdleMin / 5
	if need < 1 {
		need = 1 // an IDLE_MIN under one tick would reap on the first quiet probe
	}
	fileFresh, freshPath := anyFresh(files)
	if conns > 0 || fileFresh || load >= loadBusy {
		writeFile(p.idleTicksFile(), "0")
		if fileFresh {
			say("[%s] kept by file-signal %s", p.name, freshPath)
		}
		return nil
	}
	ticks := readInt(p.idleTicksFile()) + 1
	writeFile(p.idleTicksFile(), strconv.Itoa(ticks))
	say("[%s] idle tick %d/%d (conns=%d load=%v)", p.name, ticks, need, conns, load)
	if ticks >= need {
		say("[%s] idle for %dmin — reaping", p.name, p.reapAfterIdleMin)
		return ops.down(p)
	}
	return nil
}

// reapRun is the plane-side `reap` (bash cmd_reap): a quiet no-op before
// init has run (no LITTLEBOX_IP = can't exclude the probe self), then each
// profile reaped independently so one broken profile never stops reaping
// the others (they'd keep billing). Credentials checked PER PROFILE.
func reapRun(dep *deployment) {
	// Quiet no-op ONLY before init has run at all. missing_config also fires
	// on a half-edited config.local (GIT_NAME gone) — silently disabling the
	// reaper while a box bills. Reaping needs LITTLEBOX_IP (probe self-
	// exclusion) + provider creds; never the git identity.
	cfg := loadConfigFile(configPath())
	if cfg.get("LITTLEBOX_IP", "") == "" {
		return
	}
	for _, name := range dep.listProfiles() {
		p, err := dep.deriveProfile(name)
		if err != nil {
			say("[%s] reap check failed (bad config): %v — continuing with other profiles", name, err)
			continue
		}
		prov, err := dep.loadProvider(p.provider)
		if err != nil {
			say("[%s] reap check failed (bad provider): %v — continuing", name, err)
			continue
		}
		if !prov.HasCredentials() {
			say("[%s] no credentials for provider '%s' — cannot reap-check", name, p.provider)
			continue
		}
		if err := reapOne(p, prov, newPlaneReapOps(p, prov)); err != nil {
			say("[%s] reap check failed — continuing with other profiles: %v", name, err)
		}
	}
}

// ---- real reapOps ----

type planeReapOps struct {
	p    *profile
	prov Provider
}

func newPlaneReapOps(p *profile, prov Provider) *planeReapOps {
	return &planeReapOps{p: p, prov: prov}
}

// busyProbe ssh-runs `daybox-agent keep-probe <planeIP>` on the box and
// parses its key=value output. The box owns its keep.toml now (it lives
// on the /work volume), so the box is the authority on its own
// file-freshness signals — the plane no longer loads keep.toml, knows the
// entry count, or builds the probe string. A timeout/wedge returns an
// error (unreachable this tick) — the reaper must never hang on a box.
//
// The 30s timeout is the keep-DoS bound: a box that bombs its own keep
// makes its own probe slow → unreachable tick → self-reaps on
// REAP_AFTER_UNREACHABLE_MIN. The plane passes its IP as the arg so the
// ssh self-exclusion (this probe's session counts as a :22 conn) stays
// exact without the box mirroring plane-side config. See keepprobe.go.
func (o *planeReapOps) busyProbe(ip string) (int, float64, []fileSignalResult, error) {
	args := []string{"timeout", "30", "ssh"}
	args = append(args, sshBoxOpts(o.p)...)
	args = append(args, o.p.remoteUser+"@"+ip, "/usr/local/bin/daybox-agent keep-probe "+o.p.littleboxIP)
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return 0, 0, nil, err
	}
	conns, load, files, perr := parseProbeOutput(string(out))
	if perr != nil {
		return 0, 0, nil, perr
	}
	return conns, load, files, nil
}

// parseProbeOutput parses the on-box probe's key=value output into its
// signals. conns and load are required (a missing or unparseable key is a
// short read → error → unreachable tick — the reaper degrades toward the
// zombie reap, never toward an idle reap that would kill a working box).
// Each file=<path>=0|1 line becomes a fileSignalResult (path + fresh), in
// declared order, so the caller can log WHICH signal kept the box and
// status can surface the active set. A short read (ssh truncated) has no
// conns/load → error → unreachable tick. Pure so the parser is unit-tested
// without a box or ssh.
func parseProbeOutput(out string) (conns int, load float64, files []fileSignalResult, err error) {
	var haveConns, haveLoad bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue // not a key=value line (e.g. an ssh banner) — skip
		}
		key := line[:eq]
		rest := strings.TrimSpace(line[eq+1:])
		switch key {
		case "conns":
			if conns, err = strconv.Atoi(rest); err != nil {
				return 0, 0, nil, fmt.Errorf("short probe output: bad conns %q", rest)
			}
			haveConns = true
		case "load":
			if load, err = strconv.ParseFloat(rest, 64); err != nil {
				return 0, 0, nil, fmt.Errorf("short probe output: bad load %q", rest)
			}
			haveLoad = true
		case "file":
			// file=<path>=0|1 — the path is between the first and last '='
			// (keepPathRe rejects '=' in paths, so this is unambiguous).
			lastEq := strings.LastIndexByte(rest, '=')
			if lastEq < 0 {
				return 0, 0, nil, fmt.Errorf("short probe output: bad file %q", rest)
			}
			n, e := strconv.Atoi(rest[lastEq+1:])
			if e != nil {
				return 0, 0, nil, fmt.Errorf("short probe output: bad file %q", rest)
			}
			files = append(files, fileSignalResult{path: rest[:lastEq], fresh: n > 0})
		}
	}
	if !haveConns || !haveLoad {
		return 0, 0, nil, fmt.Errorf("short probe output: missing conns or load")
	}
	return conns, load, files, nil
}

func (o *planeReapOps) down(p *profile) error {
	return downBox(p, o.prov, newPlaneDownOps(p))
}

// --- helpers ---

// boxAgeMinutes parses an RFC3339 created time to whole minutes since.
// Returns (0, false) when the value can't be parsed (a provider that can't
// supply it emits null — bash box_age_min's `||` fallback).
func boxAgeMinutes(created string) (int, bool) {
	if created == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return 0, false
	}
	return int(time.Since(t).Minutes()), true
}

func readInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func isNonNegFloat(f float64) bool {
	return f >= 0 && f == f // reject NaN; allow 0
}
