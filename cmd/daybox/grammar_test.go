package main

// grammar_test.go — tests strictly about the CLI grammar: how Parse splits
// argv into a Command and how String serializes one back. No daybox verb
// knowledge belongs here — no summon, no provider, no config — only the
// mechanics of verb resolution, global hoisting, rest preservation,
// error cases, and the round-trip between Parse and String.

import (
	"reflect"
	"strings"
	"testing"
)

// profileGlobal is the one global daybox registers; the grammar itself is
// agnostic to it, but the tests need a concrete global to exercise the
// value-taking short/long forms.
var profileGlobal = []Global{{Short: "p", Long: "profile", TakesValue: true}}

// boolGlobal exercises a value-less (boolean) global alongside a
// value-taking one, so both code paths are covered.
var boolGlobal = []Global{{Short: "d", Long: "detach", TakesValue: false}}

// asMap flattens a Command's ordered globals into a name→value map for
// order-independent comparison (the grammar only promises a global is
// present with its last value, not the order it arrived in).
func asMap(c Command) map[string]string {
	m := map[string]string{}
	for _, g := range c.globals {
		m[g.name] = g.value
	}
	return m
}

// equiv reports whether two Commands mean the same thing: same verb, same
// globals (by name+value, order-agnostic), same rest in order.
func equiv(a, b Command) bool {
	return a.verb == b.verb &&
		reflect.DeepEqual(asMap(a), asMap(b)) &&
		reflect.DeepEqual(a.rest, b.rest)
}

func TestParseVerbOnly(t *testing.T) {
	c, err := Parse([]string{"up"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" || len(c.Rest()) != 0 || c.Global("profile") != "" {
		t.Fatalf("want verb=up no rest no profile, got %+v", c)
	}
}

func TestParseGlobalBeforeVerb(t *testing.T) {
	// the v0.3.0/v0.3.1 bug: a leading -p made the plane's args[0]
	// dispatch reject the verb. Parse hoists -p first, so the verb
	// resolves regardless of where -p sits.
	c, err := Parse([]string{"-p", "daybox", "up"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" {
		t.Fatalf("verb: want up, got %q", c.Verb())
	}
	if c.Global("profile") != "daybox" {
		t.Fatalf("profile: want daybox, got %q", c.Global("profile"))
	}
}

func TestParseGlobalAfterVerb(t *testing.T) {
	c, err := Parse([]string{"up", "-p", "daybox"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" || c.Global("profile") != "daybox" {
		t.Fatalf("got verb=%q profile=%q", c.Verb(), c.Global("profile"))
	}
}

func TestParseGlobalEqualsForms(t *testing.T) {
	cases := [][]string{
		{"-p=daybox", "up"},
		{"--profile=daybox", "up"},
		{"--profile", "daybox", "up"},
		{"up", "--profile=daybox"},
	}
	for _, args := range cases {
		c, err := Parse(args, profileGlobal)
		if err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}
		if c.Verb() != "up" || c.Global("profile") != "daybox" {
			t.Fatalf("Parse(%v): want verb=up profile=daybox, got verb=%q profile=%q", args, c.Verb(), c.Global("profile"))
		}
	}
}

func TestParseRestPreservedInOrder(t *testing.T) {
	c, err := Parse([]string{"up", "--detach", "ccx33"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" {
		t.Fatalf("verb: want up, got %q", c.Verb())
	}
	if rest := c.Rest(); len(rest) != 2 || rest[0] != "--detach" || rest[1] != "ccx33" {
		t.Fatalf("rest: want [--detach ccx33], got %v", rest)
	}
}

func TestParseGlobalHoistedFromMiddle(t *testing.T) {
	// -p may sit between a verb flag and a positional; it is lifted out and
	// the rest keeps its order.
	c, err := Parse([]string{"up", "--detach", "-p", "x", "ccx33"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" || c.Global("profile") != "x" {
		t.Fatalf("got verb=%q profile=%q", c.Verb(), c.Global("profile"))
	}
	if rest := c.Rest(); len(rest) != 2 || rest[0] != "--detach" || rest[1] != "ccx33" {
		t.Fatalf("rest: want [--detach ccx33], got %v", rest)
	}
}

func TestParseNoVerb(t *testing.T) {
	c, err := Parse([]string{"-p", "x"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "" {
		t.Fatalf("want empty verb, got %q", c.Verb())
	}
	if c.Global("profile") != "x" {
		t.Fatalf("profile: want x, got %q", c.Global("profile"))
	}
}

func TestParseEmpty(t *testing.T) {
	c, err := Parse([]string{}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "" || len(c.Rest()) != 0 || len(c.globals) != 0 {
		t.Fatalf("want empty command, got %+v", c)
	}
}

func TestParseGlobalNoValueErrors(t *testing.T) {
	cases := [][]string{
		{"-p"},          // -p with nothing after it
		{"up", "-p"},    // -p at the end, no value
		{"--profile"},   // long form, no value
	}
	for _, args := range cases {
		_, err := Parse(args, profileGlobal)
		if err == nil {
			t.Fatalf("Parse(%v): want error for value-less -p, got nil", args)
		}
		if !strings.Contains(err.Error(), "requires a value") {
			t.Fatalf("Parse(%v): error %q does not mention a missing value", args, err)
		}
	}
}

func TestParseGlobalValueMayLookLikeFlag(t *testing.T) {
	// POSIX: a value-taking flag consumes the next token verbatim, even
	// one that looks like a flag. The grammar matches that so `-p --detach`
	// is profile=--detach, not a detached parse.
	c, err := Parse([]string{"-p", "--detach", "up"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global("profile") != "--detach" {
		t.Fatalf("profile: want --detach, got %q", c.Global("profile"))
	}
	if c.Verb() != "up" {
		t.Fatalf("verb: want up, got %q", c.Verb())
	}
}

func TestParseUnknownFlagBeforeVerbGoesToRest(t *testing.T) {
	// an unregistered flag before the verb is not hoisted; it lands in rest
	// and the verb still resolves, so `daybox --detach up` round-trips to
	// `up --detach` (verb-first canonical form).
	c, err := Parse([]string{"--detach", "up"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verb() != "up" {
		t.Fatalf("verb: want up, got %q", c.Verb())
	}
	if rest := c.Rest(); len(rest) != 1 || rest[0] != "--detach" {
		t.Fatalf("rest: want [--detach], got %v", rest)
	}
}

func TestParseGlobalLastValueWins(t *testing.T) {
	// repeating a global is legal; the last value is kept (and the first
	// arrival's slot is reused, so String stays stable).
	c, err := Parse([]string{"-p", "a", "up", "-p", "b"}, profileGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global("profile") != "b" {
		t.Fatalf("profile: want b (last wins), got %q", c.Global("profile"))
	}
}

func TestParseBooleanGlobal(t *testing.T) {
	globals := append([]Global{}, profileGlobal...)
	globals = append(globals, boolGlobal...)
	c, err := Parse([]string{"-d", "-p", "x", "up"}, globals)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global("detach") != "true" {
		t.Fatalf("detach: want true, got %q", c.Global("detach"))
	}
	if c.Global("profile") != "x" || c.Verb() != "up" {
		t.Fatalf("got profile=%q verb=%q", c.Global("profile"), c.Verb())
	}
}

func TestStringVerbFirstThenGlobalsThenRest(t *testing.T) {
	c, _ := Parse([]string{"-p", "x", "up", "--detach", "ccx33"}, profileGlobal)
	want := "up --profile 'x' '--detach' 'ccx33'"
	if got := c.String(); got != want {
		t.Fatalf("String: want %q, got %q", want, got)
	}
}

func TestStringQuotesUnsafeRest(t *testing.T) {
	// a rest token with shell metacharacters is single-quoted so it
	// survives the ssh→remote-shell hop intact.
	c, _ := Parse([]string{"ssh", "echo", "a;b", "rm -rf"}, profileGlobal)
	s := c.String()
	if !strings.Contains(s, "'a;b'") || !strings.Contains(s, "'rm -rf'") {
		t.Fatalf("String %q does not safely quote the unsafe rest tokens", s)
	}
}

func TestStringEmptyCommand(t *testing.T) {
	c, _ := Parse([]string{}, profileGlobal)
	if c.String() != "" {
		t.Fatalf("empty command String: want %q, got %q", "", c.String())
	}
}

func TestRoundTrip(t *testing.T) {
	// the invariant the grammar exists to uphold: parsing a serialized
	// command yields an equivalent command. The laptop emits String, the
	// plane parses it, and the two agree on verb + globals + rest.
	cases := [][]string{
		{"up"},
		{"-p", "daybox", "up"},
		{"up", "-p", "daybox"},
		{"up", "--detach", "ccx33"},
		{"-p", "daybox", "up", "--detach", "ccx33"},
		{"ssh", "echo", "hello world"},
		{"ssh", "a;b", "|", "cat"},
		{"--profile=daybox", "up", "-p", "other"},
		{"status"},
		{"-p", "x"}, // no verb
	}
	for _, args := range cases {
		c, err := Parse(args, profileGlobal)
		if err != nil {
			t.Fatalf("Parse(%v): %v", args, err)
		}
		// tokenize the canonical form the way a shell would, then re-parse
		reparsed, err := Parse(tokenize(c.String()), profileGlobal)
		if err != nil {
			t.Fatalf("Parse(String(Parse(%v))): %v", args, err)
		}
		if !equiv(c, reparsed) {
			t.Fatalf("round-trip %v:\n  orig    = verb=%q globals=%v rest=%v\n  reparsed= verb=%q globals=%v rest=%v",
				args, c.verb, asMap(c), c.rest, reparsed.verb, asMap(reparsed), reparsed.rest)
		}
	}
}

func TestRestIsACopy(t *testing.T) {
	// a handler must not mutate the grammar's internal slice through Rest.
	c, _ := Parse([]string{"up", "--detach"}, profileGlobal)
	rest := c.Rest()
	rest[0] = "MUTATED"
	again := c.Rest()
	if again[0] == "MUTATED" {
		t.Fatalf("Rest returned an alias: mutating it changed the command")
	}
}

func TestCommandSatisfiesParsed(t *testing.T) {
	// the compile-time `var _ Parsed = Command{}` guards the accessor set;
	// this runtime check documents the same intent for readers and keeps
	// the interface from quietly drifting.
	var _ Parsed = Command{}
	c, _ := Parse([]string{"up", "-p", "x"}, profileGlobal)
	var p Parsed = c
	if p.Verb() != "up" || p.Global("profile") != "x" {
		t.Fatalf("Parsed view mismatch: verb=%q profile=%q", p.Verb(), p.Global("profile"))
	}
}

// tokenize splits a Command.String() the way a POSIX shell would for these
// tests' inputs: whitespace separation, single-quoted spans kept literal.
// It is NOT a general shell word-splitter — only enough to round-trip the
// canonical form String produces (single-quoted tokens, no nested quotes
// in values beyond the one quoting rule shq uses).
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			inQ = !inQ
		case ch == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
