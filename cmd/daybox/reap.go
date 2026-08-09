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
// Three busy signals (bash reap_one): ssh sessions, load, and RECENT CLAUDE
// TRANSCRIPT WRITES — a detached claude on an API-bound task shows load
// ~0.2 and zero connections, and the reaper once killed one mid-task one
// minute before its last transcript write. Misconfigured knobs degrade
// toward KEEPING the box, never toward a surprise reap (a non-numeric
// LOAD_BUSY would compare as 0 = always busy = never reaped, meter runs;
// an IDLE_MIN under one tick would reap on the first quiet probe).

// reapOps is the injectable surface for the reaper's on-box busy probe.
// Tests fake it to assert the cap / idle / unreachable decisions without
// a box or headscale.
type reapOps interface {
	// busyProbe ssh's the box for the three busy signals, returning
	// (sshConnections, loadAvg1min, recentTranscriptWrite, error). A
	// non-nil error means the box was unreachable this tick.
	busyProbe(ip string) (conns int, load float64, working bool, err error)
	// down reaps the box (volume detach + server delete + net node drop).
	down(p *profile) error
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

	conns, load, working, probeErr := ops.busyProbe(s.IP)
	if probeErr != nil {
		// unreachable: tick the unreachable counter; an hour = zombie reaped
		u := readInt(p.unreachTicksFile()) + 1
		writeFile(p.unreachTicksFile(), strconv.Itoa(u))
		say("[%s] big box unreachable (tick %d/12)", p.name, u)
		if u >= 12 {
			say("[%s] unreachable for 1h — force reaping", p.name)
			return ops.down(p)
		}
		return nil
	}

	// reachable: reset unreachable, evaluate the three busy signals
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
	if conns > 0 || working || load >= loadBusy {
		writeFile(p.idleTicksFile(), "0")
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

// busyProbe runs the on-box probe (bash: the timeout 30 ssh). Three lines:
// ssh session count (excluding the control plane), the 1min load, and a
// recent claude transcript write. A timeout/wedge returns an error
// (unreachable this tick) — the reaper must never hang on a wedged box.
func (o *planeReapOps) busyProbe(ip string) (int, float64, bool, error) {
	args := []string{"timeout", "30", "ssh"}
	args = append(args, sshBoxOpts(o.p)...)
	args = append(args, o.p.remoteUser+"@"+ip,
		"ss -Htn state established '( sport = :22 )' | grep -vc '"+
			o.p.littleboxIP+"' ; cut -d' ' -f1 /proc/loadavg ; find /work/state/claude/projects -type f -newermt '-10 minutes' 2>/dev/null | head -1")
	c := exec.Command(args[0], args[1:]...)
	out, err := c.Output()
	if err != nil {
		return 0, 0, false, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return 0, 0, false, fmt.Errorf("short probe output")
	}
	conns, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
	load, _ := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	working := strings.TrimSpace(lines[2]) != ""
	return conns, load, working, nil
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
