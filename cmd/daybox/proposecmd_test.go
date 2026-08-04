package main

import (
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func decodeSeed(t *testing.T, src string) map[string]any {
	t.Helper()
	var m map[string]any
	if _, err := toml.Decode(src, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const proposeSeed = `# what this box carries
packages = [
  "ripgrep",  # keep
  "jq",
]

[persist]
".claude/" = "claude"

[setup]
once = ["echo hi"]

[tools]
node = "24.18.0"
"npm:@anthropic-ai/claude-code" = "2.1.220"

[tools.settings]
locked = true
`

func TestDetectDrift(t *testing.T) {
	seed := decodeSeed(t, proposeSeed)
	d := detectDrift(seed,
		map[string]string{
			"node":                          "24.18.5", // stale pin
			"npm:@anthropic-ai/claude-code": "2.1.220", // matches
			"age":                           "1.3.1",   // undeclared
		},
		[]string{"jq", "htop", "tmux"}) // declared, new, substrate

	if d.toolBumps["node"] != "24.18.5" || len(d.toolBumps) != 1 {
		t.Errorf("bumps: %v", d.toolBumps)
	}
	if d.toolAdds["age"] != "1.3.1" || len(d.toolAdds) != 1 {
		t.Errorf("adds: %v", d.toolAdds)
	}
	if len(d.pkgAdds) != 1 || d.pkgAdds[0] != "htop" {
		t.Errorf("pkgAdds: %v", d.pkgAdds)
	}
}

func TestDetectDriftNothing(t *testing.T) {
	seed := decodeSeed(t, proposeSeed)
	d := detectDrift(seed,
		map[string]string{"node": "24.18.0"}, []string{"ripgrep", "git"})
	if !d.empty() {
		t.Errorf("expected no drift, got %+v", d)
	}
}

func TestParseAptHistory(t *testing.T) {
	hist := `Start-Date: 2026-08-04  10:00:00
Commandline: apt-get install -y ripgrep jq
Install: ripgrep:amd64 (13.0.0), jq:amd64 (1.7)
End-Date: 2026-08-04  10:00:05

Start-Date: 2026-08-04  11:00:00
Commandline: apt install htop
End-Date: 2026-08-04  11:00:02

Start-Date: 2026-08-04  12:00:00
Commandline: apt-get remove nano
End-Date: 2026-08-04  12:00:01
`
	got := parseAptHistory(hist)
	want := []string{"ripgrep", "jq", "htop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestApplyDriftFullRewrite(t *testing.T) {
	d := drift{
		toolAdds:  map[string]string{"age": "1.3.1"},
		toolBumps: map[string]string{"node": "24.18.5", "npm:@anthropic-ai/claude-code": "2.2.0"},
		pkgAdds:   []string{"htop"},
	}
	out, err := applyDrift(proposeSeed, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDriftOnly(proposeSeed, out, d); err != nil {
		t.Fatal(err)
	}
	// text-level guarantees: comments survive, quoted keys stay quoted
	for _, want := range []string{"# what this box carries", `"ripgrep",  # keep`,
		`node = "24.18.5"`, `"npm:@anthropic-ai/claude-code" = "2.2.0"`, `age = "1.3.1"`, `"htop",`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestApplyDriftSingleLinePackages(t *testing.T) {
	src := "packages = [\"jq\"]\n\n[tools]\n"
	out, err := applyDrift(src, drift{pkgAdds: []string{"htop"}})
	if err != nil {
		t.Fatal(err)
	}
	after := decodeSeed(t, out)
	pkgs, _ := after["packages"].([]any)
	if len(pkgs) != 2 {
		t.Fatalf("packages after: %v\n%s", pkgs, out)
	}
}

func TestApplyDriftMultilineNoTrailingComma(t *testing.T) {
	src := "packages = [\n  \"jq\"  # comment, no trailing comma\n]\n"
	out, err := applyDrift(src, drift{pkgAdds: []string{"htop"}})
	if err != nil {
		t.Fatal(err)
	}
	after := decodeSeed(t, out)
	pkgs, _ := after["packages"].([]any)
	if len(pkgs) != 2 {
		t.Fatalf("packages after: %v\n%s", pkgs, out)
	}
}

// A bump for a pin the seed doesn't carry as its own line must refuse, not
// guess — and verifyDriftOnly must catch a rewrite that touched [setup].
func TestApplyDriftRefusals(t *testing.T) {
	if _, err := applyDrift("packages = []\n", drift{toolBumps: map[string]string{"node": "1"}}); err == nil {
		t.Error("bump without a pin line should refuse")
	}
	if _, err := applyDrift("packages = []\n", drift{toolAdds: map[string]string{"node": "1"}}); err == nil {
		t.Error("tool add without a [tools] section should refuse")
	}
	tampered := strings.Replace(proposeSeed, `once = ["echo hi"]`, `once = ["curl evil"]`, 1)
	if err := verifyDriftOnly(proposeSeed, tampered, drift{}); err == nil {
		t.Error("a [setup] change must fail verification")
	}
}

func TestMiseToolsAbsentIsQuiet(t *testing.T) {
	t.Setenv("PATH", os.TempDir()) // no mise on PATH
	if got := miseTools(); got != nil {
		t.Errorf("expected nil without mise, got %v", got)
	}
}
