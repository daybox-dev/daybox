package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profileargs_test.go — regressions for two bugs found on a live plane:
//
//  1. `daybox profile add --help` created a profile literally named
//     "--help" (config, state, and a billed 50GB volume): the help check
//     only looked at the subverb slot, the flag fell through as the
//     positional name, and validProfileName accepted a dash-first name.
//     The same shape made `profile rm --help --purge` act on whatever
//     "--help" resolved to. Fix: help flags anywhere print usage, names
//     can't start with '-', and the lifecycle args are parsed strictly —
//     no unknown flags, no -p-plus-positional guesses, no default fallback.
//
//  2. `daybox profile edit <name>` on the control plane died with "no
//     control plane configured — run: daybox init" while `profile add`
//     worked: edit called mustControl unconditionally (it was written as a
//     laptop-only verb), and the plane has no CONTROL_HOST — it IS the
//     host. add goes through the amPlane split; edit didn't. Fix: the
//     seed-authority verbs go through seedStore (ssh or local files).

// captureStderr runs fn with os.Stderr redirected, returning what it wrote
// (cmdProfile prints usage via os.Stderr, like say()).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

// TestProfileSubverbHelpPrintsUsage: a help flag after a profile subverb
// prints the group usage, exits 0, and acts on nothing — no profile dir,
// no volume, no delete. On a plane (no CONTROL_HOST) with a fake provider,
// so a regression would really create/delete rather than fail on ssh.
func TestProfileSubverbHelpPrintsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"profile", "add", "--help"},
		{"profile", "add", "-h"},
		{"profile", "add", "work", "--help"},
		{"-p", "work", "profile", "add", "--help"},
		{"profile", "rm", "--help", "--purge"},
		{"profile", "rm", "gone", "--purge", "-h"},
		{"profile", "use", "--help"},
		{"profile", "rename", "gone", "--help"},
		{"profile", "seed", "init", "--help"},
		{"profile", "edit", "--help"},
		{"profile", "proposals", "-h"},
		{"profile", "accept", "--help"},
		{"profile", "reject", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d, prov := newU8Deployment(t)
			// a profile rm could wrongly hit, plus the 'default' whose
			// volume a fallback would purge
			for name, vid := range map[string]string{"gone": "100", "default": "200"} {
				os.MkdirAll(filepath.Join(d.stateDir, "profiles", name), 0o755)
				os.WriteFile(filepath.Join(d.stateDir, "profiles", name, "volume_id"), []byte(vid), 0o644)
			}
			var stdout bytes.Buffer
			code := -1
			got := captureStderr(t, func() { code = run(args, &stdout, io.Discard) })
			if code != 0 {
				t.Errorf("exit %d, want 0", code)
			}
			if !strings.Contains(got, "usage: daybox profile") {
				t.Errorf("did not print profile usage: %q", got)
			}
			if len(prov.ensureCalls) != 0 || len(prov.deleteCalls) != 0 || len(prov.renameCalls) != 0 {
				t.Errorf("help acted: ensure=%v delete=%v rename=%v", prov.ensureCalls, prov.deleteCalls, prov.renameCalls)
			}
			for _, dir := range []string{
				filepath.Join(d.confDir, "profiles", "--help"),
				filepath.Join(d.stateDir, "profiles", "--help"),
				filepath.Join(d.confDir, "profiles", "-h"),
				filepath.Join(d.confDir, "profiles", "work"),
			} {
				if fileExists(dir) {
					t.Errorf("help created %s", dir)
				}
			}
			for _, name := range []string{"gone", "default"} {
				if !fileExists(filepath.Join(d.stateDir, "profiles", name)) {
					t.Errorf("help removed profile %q", name)
				}
			}
		})
	}
}

// TestParseProfileArgs: the lifecycle grammar. Every refusal here is a
// case that used to be guessed at — most importantly, rm never resolves a
// missing or ambiguous name to anything (least of all 'default').
func TestParseProfileArgs(t *testing.T) {
	ok := []struct {
		args []string
		want profileCmd
	}{
		{[]string{"profile"}, profileCmd{sub: "ls"}},
		{[]string{"profile", "ls"}, profileCmd{sub: "ls"}},
		{[]string{"profile", "list"}, profileCmd{sub: "ls"}},
		{[]string{"-p", "work", "profile", "ls"}, profileCmd{sub: "ls"}},
		{[]string{"profile", "add", "work"}, profileCmd{sub: "add", name: "work"}},
		{[]string{"profile", "add", "work", "ccx43"}, profileCmd{sub: "add", name: "work", arg: "ccx43"}},
		{[]string{"profile", "add", "-p", "work", "ccx43"}, profileCmd{sub: "add", name: "work", arg: "ccx43"}},
		{[]string{"profile", "use", "work"}, profileCmd{sub: "use", name: "work"}},
		{[]string{"-p", "work", "profile", "use"}, profileCmd{sub: "use", name: "work"}},
		{[]string{"profile", "rename", "old", "new"}, profileCmd{sub: "rename", name: "old", arg: "new"}},
		{[]string{"profile", "rename", "-p", "old", "new"}, profileCmd{sub: "rename", name: "old", arg: "new"}},
		{[]string{"profile", "mv", "old", "new"}, profileCmd{sub: "rename", name: "old", arg: "new"}},
		{[]string{"profile", "rm", "gone"}, profileCmd{sub: "rm", name: "gone"}},
		{[]string{"profile", "rm", "gone", "--purge"}, profileCmd{sub: "rm", name: "gone", purge: true}},
		{[]string{"profile", "rm", "--purge", "gone"}, profileCmd{sub: "rm", name: "gone", purge: true}},
		{[]string{"profile", "rm", "-p", "gone", "--purge"}, profileCmd{sub: "rm", name: "gone", purge: true}},
		{[]string{"profile", "remove", "gone"}, profileCmd{sub: "rm", name: "gone"}},
		{[]string{"profile", "seed"}, profileCmd{sub: "seed", arg: "show"}},
		{[]string{"profile", "seed", "show", "work"}, profileCmd{sub: "seed", name: "work", arg: "show"}},
		{[]string{"profile", "seed", "-p", "work", "path"}, profileCmd{sub: "seed", name: "work", arg: "path"}},
	}
	for _, c := range ok {
		p, err := Parse(c.args, globalFlags)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseProfileArgs(p)
		if err != nil {
			t.Errorf("%v: unexpected error: %v", c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("%v: got %+v, want %+v", c.args, got, c.want)
		}
	}

	bad := []struct {
		args []string
		why  string // substring the error must carry
	}{
		// help flags never become names, even if wantsProfileHelp is bypassed
		{[]string{"profile", "add", "--help"}, "unknown flag '--help'"},
		{[]string{"profile", "rm", "--help", "--purge"}, "unknown flag '--help'"},
		{[]string{"profile", "rename", "a", "-h"}, "unknown flag '-h'"},
		// ...nor does a flag-shaped -p value
		{[]string{"profile", "add", "-p", "--help"}, "invalid profile '--help'"},
		{[]string{"profile", "rm", "-p", "-x"}, "invalid profile '-x'"},
		// rm: no target is a usage error, never the default profile
		{[]string{"profile", "rm"}, "usage: daybox profile rm"},
		{[]string{"profile", "rm", "--purge"}, "usage: daybox profile rm"},
		// rm: two candidate targets is refused, not guessed
		{[]string{"profile", "rm", "-p", "gone", "other"}, "already -p gone"},
		{[]string{"profile", "rm", "gone", "other"}, "unexpected 'other'"},
		{[]string{"profile", "rm", "gone", "--prge"}, "unknown flag '--prge'"},
		{[]string{"profile", "rm", "../state"}, "invalid profile '../state'"},
		// rename: both names validated, extras refused
		{[]string{"profile", "rename", "old"}, "usage: daybox profile rename"},
		{[]string{"profile", "rename", "old", "New"}, "invalid profile 'New'"},
		{[]string{"profile", "rename", "-p", "old", "new", "extra"}, "already -p old"},
		{[]string{"profile", "use", "-p", "a", "b"}, "already -p a"},
		{[]string{"profile", "add"}, "usage: daybox profile add"},
		{[]string{"profile", "add", "a", "b", "c"}, "unexpected 'c'"},
		{[]string{"profile", "ls", "extra"}, "usage: daybox profile ls"},
		{[]string{"profile", "seed", "show", "a", "-p", "b"}, "name one profile"},
		{[]string{"profile", "seed", "show", "-bad"}, "unknown flag '-bad'"},
		{[]string{"profile", "bogus"}, "unknown: profile bogus"},
	}
	for _, c := range bad {
		p, err := Parse(c.args, globalFlags)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseProfileArgs(p)
		if err == nil {
			t.Errorf("%v: accepted as %+v, want error (%s)", c.args, got, c.why)
			continue
		}
		if !strings.Contains(err.Error(), c.why) {
			t.Errorf("%v: error %q, want it to mention %q", c.args, err, c.why)
		}
	}
}

// TestProfileRmRefusesStrayFlagNamedProfile: even with a "--help" profile
// already on disk (what the old bug left behind), profileRm won't act on
// a dash-first name — the stray is removed by hand, by exact volume id.
func TestProfileRmRefusesStrayFlagNamedProfile(t *testing.T) {
	d, prov := newU8Deployment(t)
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "--help"), 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "--help", "volume_id"), []byte("107"), 0o644)
	if err := profileRm(d, "--help", true); err == nil {
		t.Fatal("profileRm(--help) should refuse a flag-shaped name")
	}
	if len(prov.deleteCalls) != 0 {
		t.Errorf("refused rm still deleted volumes: %v", prov.deleteCalls)
	}
	if err := profileRename(d, "--help", "ok"); err == nil {
		t.Error("profileRename should refuse a flag-shaped old name")
	}
	if len(prov.renameCalls) != 0 {
		t.Errorf("refused rename still renamed: %v", prov.renameCalls)
	}
}

// TestProfilePlaneRmPurgeAnyOrder: --purge is a flag, not a positional
// slot, so `rm --purge <name>` purges the named profile too.
func TestProfilePlaneRmPurgeAnyOrder(t *testing.T) {
	d, prov := newU8Deployment(t)
	unmountWorkFn = func(p *profile, ip string) string { return "" }
	os.MkdirAll(filepath.Join(d.stateDir, "profiles", "gone"), 0o755)
	os.WriteFile(filepath.Join(d.stateDir, "profiles", "gone", "volume_id"), []byte("100"), 0o644)
	c, err := Parse([]string{"profile", "rm", "--purge", "gone"}, globalFlags)
	if err != nil {
		t.Fatal(err)
	}
	cmdProfilePlane(c)
	if len(prov.deleteCalls) != 1 || prov.deleteCalls[0] != "100" {
		t.Errorf("deleteCalls = %v, want [100]", prov.deleteCalls)
	}
	if fileExists(filepath.Join(d.stateDir, "profiles", "gone")) {
		t.Error("state dir should be removed")
	}
}

func TestProfileEditTarget(t *testing.T) {
	for _, c := range []struct {
		flag string
		toks []string
		want string
	}{
		{"", nil, ""}, // current profile, resolved by the store
		{"", []string{"work"}, "work"},
		{"work", nil, "work"},
	} {
		got, err := profileEditTarget(c.flag, c.toks)
		if err != nil || got != c.want {
			t.Errorf("profileEditTarget(%q, %v) = %q, %v; want %q", c.flag, c.toks, got, err, c.want)
		}
	}
	for _, c := range []struct {
		flag string
		toks []string
	}{
		{"a", []string{"b"}},
		{"", []string{"a", "b"}},
		{"", []string{"--nope"}},
	} {
		if got, err := profileEditTarget(c.flag, c.toks); err == nil {
			t.Errorf("profileEditTarget(%q, %v) = %q, want error", c.flag, c.toks, got)
		}
	}
}

// newPlaneSeedHome is a plane-role HOME (config.local without CONTROL_HOST)
// holding profile 'work' with seed, and an $EDITOR script that replaces
// whatever it's given with newSeed.
func newPlaneSeedHome(t *testing.T, seed, newSeed string) (store string) {
	t.Helper()
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\n")
	t.Setenv("DAYBOX_CONTROL_HOST", "")
	if !amPlane() {
		t.Fatal("test HOME should be in the plane role")
	}
	store = filepath.Join(d.confDir, "profiles")
	setupProfile(t, store, "work", seed)
	bin := t.TempDir()
	repl := filepath.Join(bin, "new.toml")
	os.WriteFile(repl, []byte(newSeed), 0o644)
	editor := filepath.Join(bin, "editor.sh")
	os.WriteFile(editor, []byte("#!/bin/sh\ncat "+shq(repl)+` > "$1"`+"\n"), 0o755)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", editor)
	t.Setenv("TMPDIR", t.TempDir())
	return store
}

// TestProfileEditOnPlane: `daybox profile edit work` (and the -p form) on
// the plane edits the local seed — validated, backed up, replaced —
// instead of dying in mustControl. A regression log.Fatals the test binary.
func TestProfileEditOnPlane(t *testing.T) {
	for _, args := range [][]string{
		{"profile", "edit", "work"},
		{"-p", "work", "profile", "edit"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			const before = "packages = [\"git\"]\n"
			const after = "packages = [\"git\", \"jq\"]\n"
			store := newPlaneSeedHome(t, before, after)
			if code := run(args, io.Discard, io.Discard); code != 0 {
				t.Fatalf("exit %d", code)
			}
			b, err := os.ReadFile(filepath.Join(store, "work", "profile.toml"))
			if err != nil || string(b) != after {
				t.Fatalf("seed = %q, %v; want the edited seed", b, err)
			}
			baks, _ := filepath.Glob(filepath.Join(store, "work", "profile.toml.bak.*"))
			if len(baks) != 1 {
				t.Fatalf("backups = %v, want exactly one", baks)
			}
			if old, _ := os.ReadFile(baks[0]); string(old) != before {
				t.Errorf("backup = %q, want the pre-edit seed", old)
			}
		})
	}
}

// TestProfileEditOnPlaneDefaultsToCurrent: bare `profile edit` on the plane
// resolves the current profile from the plane's own state.
func TestProfileEditOnPlaneDefaultsToCurrent(t *testing.T) {
	const after = "packages = [\"jq\"]\n"
	store := newPlaneSeedHome(t, "packages = []\n", after)
	setupProfile(t, store, "default", "packages = []\n")
	stateDir := filepath.Join(filepath.Dir(store), "state")
	os.MkdirAll(stateDir, 0o755)
	os.WriteFile(filepath.Join(stateDir, "current_profile"), []byte("work\n"), 0o644)
	if code := run([]string{"profile", "edit"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(store, "work", "profile.toml")); string(b) != after {
		t.Errorf("work seed = %q, want edited (current_profile=work)", b)
	}
	if b, _ := os.ReadFile(filepath.Join(store, "default", "profile.toml")); string(b) != "packages = []\n" {
		t.Errorf("default seed was edited instead: %q", b)
	}
}

// TestProposalVerbsOnPlane: proposals/accept/reject share edit's root cause
// (mustControl) and its fix. accept on a non-tty never applies (the
// confirmation defaults to no); reject drops the proposal; the live seed
// is untouched by both.
func TestProposalVerbsOnPlane(t *testing.T) {
	const live = "packages = [\"git\"]\n"
	store := newPlaneSeedHome(t, live, live)
	setupProposal(t, store, "work", "20261008-1", "packages = [\"git\", \"jq\"]\n")
	ppath := filepath.Join(store, "work", "proposals", "20261008-1.toml")

	// accept's confirmation reads os.Stdin; make it a non-tty even when the
	// tests run from a terminal, so the prompt takes its default (no).
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	origStdin := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = origStdin }()

	if code := run([]string{"profile", "proposals"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("proposals: exit %d", code)
	}
	if code := run([]string{"profile", "accept", "20261008-1"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("accept: exit %d", code)
	}
	if !fileExists(ppath) {
		t.Error("unconfirmed accept dropped the proposal")
	}
	if code := run([]string{"profile", "reject", "20261008-1"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("reject: exit %d", code)
	}
	if fileExists(ppath) {
		t.Error("reject left the proposal pending")
	}
	if b, _ := os.ReadFile(filepath.Join(store, "work", "profile.toml")); string(b) != live {
		t.Errorf("live seed changed: %q", b)
	}
}

// TestLocalSeedStore: the plane store round-trips a seed with a backup and
// lists/reads/drops proposals (drop is rm -f: a second drop is fine).
func TestLocalSeedStore(t *testing.T) {
	dir := t.TempDir()
	setupProfile(t, dir, "work", "packages = []\n")
	setupProposal(t, dir, "work", "p1", "packages = [\"jq\"]\n")
	s := localSeedStore{dir: dir}

	if got, err := s.fetch("work"); err != nil || got != "packages = []\n" {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if _, err := s.fetch("nope"); err == nil || !strings.Contains(err.Error(), "seed init nope") {
		t.Errorf("fetch(nope) error = %v, want seed-init help", err)
	}
	if err := s.push("work", "packages = [\"x\"]\n", "20261008-000000"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "work", "profile.toml.bak.20261008-000000")); string(b) != "packages = []\n" {
		t.Errorf("backup = %q", b)
	}
	ps, err := s.proposals()
	if err != nil || len(ps) != 1 || ps[0] != (proposal{profile: "work", id: "p1"}) {
		t.Fatalf("proposals = %v, %v", ps, err)
	}
	if got, err := s.readProposal(ps[0]); err != nil || got != "packages = [\"jq\"]\n" {
		t.Errorf("readProposal = %q, %v", got, err)
	}
	if err := s.dropProposal(ps[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.dropProposal(ps[0]); err != nil {
		t.Errorf("second drop should be a no-op: %v", err)
	}
}
