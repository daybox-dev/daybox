package main

// grammar.go — the daybox CLI grammar, parsed and serialized in one place.
//
// Parsing and serialization live HERE and nowhere else. The laptop parses
// argv into a Command and serializes it to hand to the control plane over
// ssh; the plane parses that string back into the same Command. The
// round-trip is a property, not a hope — grammar_test.go asserts that
// Parse(tokenize(String(c))) is equivalent to c, so a laptop invocation
// and the plane's re-parse of it can never disagree about verb, profile,
// or the verb's own tokens.
//
// The grammar is deliberately NOT daybox-specific. A Command is a verb,
// a set of hoisted global flags, and the verb-specific tokens left in
// order. daybox registers which flags are global (today: -p/--profile)
// and which verbs exist (main.go's dispatch table); this file names
// neither. The bash-era bug this replaces — the laptop hand-concatenating
// "daybox -p x up" that the plane's args[0] dispatch then rejected — is
// structurally impossible once every remote command is produced by
// Command.String and every entry point goes through Parse.
//
// Abstraction is enforced the same way provider.go enforces its contract:
// command handlers depend on the Parsed interface (read-only accessors),
// not on Command's concrete fields, and a compile-time assertion pins
// Command to it. The sole constructor is Parse, and the sole serializer is
// String — there is no other way in or out, so no handler can hand-build a
// Command or hand-concatenate a command string that bypasses the grammar.

import (
	"fmt"
	"strings"
)

// Global declares a flag the grammar hoists out of any position. Short is
// the single-letter form without the dash ("p" → -p); Long is the word
// form ("profile" → --profile). TakesValue is true for -p foo (consumes
// the next token) and false for a boolean global. daybox registers its
// globals once (main.go); the grammar knows nothing of what they mean.
type Global struct {
	Short      string
	Long       string
	TakesValue bool
}

// Parsed is the read-only view of a parsed command. Command handlers and
// the delegator depend on this interface, not on Command's concrete fields
// — the same property provider.go's Provider interface gives the cloud
// path: Command's representation can change without touching a handler, as
// long as these accessors hold. Reading a parsed command any other way is
// a smell the compiler can't always catch, but the interface makes the
// contract explicit and the one obvious path.
type Parsed interface {
	// Verb is the first non-flag token ("" if none — run treats that as
	// usage, never as an unknown command).
	Verb() string
	// Global returns a hoisted global's value keyed by its Long name, or
	// "" if that global was absent. A boolean global returns "true".
	Global(name string) string
	// Rest are the verb-specific tokens (verb flags like --detach and
	// positionals like a server type) in their original order, with every
	// global flag already hoisted out. The verb's own flag.NewFlagSet
	// parses these; handlers must not mutate the slice.
	Rest() []string
	// String is the canonical, shell-safe form for re-execution on the
	// control plane: verb, then --long 'value' globals, then each rest
	// token single-quoted. Parse(String(c)) is equivalent to c.
	String() string
}

// Command is the concrete parsed invocation. Fields are unexported: Parse
// is the only constructor and the accessors above are the only readers, so
// a handler can no more reach into the grammar's internals than a summon
// caller can reach into a hetznerProvider. A compile-time assertion (the
// var _ below) fails the build if Command ever drops a Parsed accessor.
type Command struct {
	verb    string
	globals []globalValue // ordered by first arrival; a repeat sets the last value
	rest    []string
}

// globalValue is a hoisted global's name (the Long form) and value.
type globalValue struct {
	name  string
	value string
}

// Parse splits args into a Command per the grammar:
//   - a registered global flag is hoisted out wherever it appears, in any
//     of its four forms (-p x, -p=x, --profile x, --profile=x);
//   - the first remaining non-flag token is the verb;
//   - everything else (verb flags, positionals) stays in Rest in order for
//     the verb's own flag.NewFlagSet.
//
// A global that TakesValue but finds no value is an error, so a typo dies
// at parse time rather than silently swallowing the verb. A Command with
// an empty verb (no non-flag token, e.g. bare `daybox` or `daybox -p x`)
// is returned without error; run() treats the empty verb as usage.
func Parse(args []string, globals []Global) (Command, error) {
	byShort, byLong := indexGlobals(globals)
	c := Command{}
	i := 0
	for i < len(args) {
		if g, val, consumed, ok := matchGlobal(args, i, byShort, byLong); ok {
			if g.TakesValue && val == "" {
				return c, fmt.Errorf("flag -%s/--%s requires a value", g.Short, g.Long)
			}
			c.setGlobal(g.Long, val)
			i += consumed
			continue
		}
		if c.verb == "" && !isFlagTok(args[i]) {
			c.verb = args[i]
			i++
			continue
		}
		c.rest = append(c.rest, args[i])
		i++
	}
	return c, nil
}

// Verb returns the parsed verb, or "" if none.
func (c Command) Verb() string { return c.verb }

// Global returns the value of the named (Long) global, or "" if absent.
func (c Command) Global(name string) string {
	for _, g := range c.globals {
		if g.name == name {
			return g.value
		}
	}
	return ""
}

// Rest returns a copy of the verb-specific tokens, globals hoisted out.
func (c Command) Rest() []string {
	out := make([]string, len(c.rest))
	copy(out, c.rest)
	return out
}

// String returns the canonical shell-safe form: the verb, then each
// hoisted global as --long 'value', then each rest token single-quoted.
// It is the sole serializer the delegator uses to hand a command to the
// control plane; re-parsing it yields an equivalent Command.
func (c Command) String() string {
	var b strings.Builder
	if c.verb != "" {
		b.WriteString(c.verb)
	}
	for _, g := range c.globals {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("--")
		b.WriteString(g.name)
		b.WriteByte(' ')
		b.WriteString(shq(g.value))
	}
	for _, r := range c.rest {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shq(r))
	}
	return b.String()
}

// setGlobal records a global value, last-wins, preserving first-arrival
// order so String is stable across re-parses of the same command.
func (c *Command) setGlobal(name, val string) {
	for i := range c.globals {
		if c.globals[i].name == name {
			c.globals[i].value = val
			return
		}
	}
	c.globals = append(c.globals, globalValue{name: name, value: val})
}

// matchGlobal tries to match args[i] (and possibly args[i+1]) as a
// registered global flag in any form. On a match it returns the Global,
// the consumed value ("" for a value-taking flag that found none), the
// number of args consumed (1 or 2), and ok=true. The caller owns the
// "value-taking flag with no value" error so Parse's message is uniform.
func matchGlobal(args []string, i int, byShort, byLong map[string]Global) (g Global, val string, consumed int, ok bool) {
	a := args[i]
	switch {
	case strings.HasPrefix(a, "--"):
		name := a[2:]
		eq := strings.IndexByte(name, '=')
		hasEq := eq >= 0
		if hasEq {
			val = name[eq+1:]
			name = name[:eq]
		}
		g, ok = byLong[name]
		if !ok {
			return
		}
		if g.TakesValue {
			if hasEq {
				return g, val, 1, true
			}
			if i+1 < len(args) {
				return g, args[i+1], 2, true
			}
			return g, "", 1, true // no value: caller errors
		}
		return g, "true", 1, true
	case isFlagTok(a):
		name := a[1:]
		eq := strings.IndexByte(name, '=')
		hasEq := eq >= 0
		if hasEq {
			val = name[eq+1:]
			name = name[:eq]
		}
		g, ok = byShort[name]
		if !ok {
			return
		}
		if g.TakesValue {
			if hasEq {
				return g, val, 1, true
			}
			if i+1 < len(args) {
				return g, args[i+1], 2, true
			}
			return g, "", 1, true // no value: caller errors
		}
		return g, "true", 1, true
	}
	return
}

// indexGlobals builds short- and long-name lookup tables for a global set.
func indexGlobals(globals []Global) (byShort, byLong map[string]Global) {
	byShort = map[string]Global{}
	byLong = map[string]Global{}
	for _, g := range globals {
		if g.Short != "" {
			byShort[g.Short] = g
		}
		if g.Long != "" {
			byLong[g.Long] = g
		}
	}
	return
}

// isFlagTok reports whether a is flag-shaped: at least two chars and
// leading '-'. A bare "-" (stdin/stdout convention) is positional.
func isFlagTok(a string) bool {
	return len(a) >= 2 && a[0] == '-'
}

// shq single-quotes s for a POSIX shell, the one quoting rule the grammar
// and the few hand-built remote command fragments (profile/proposal paths)
// use. It is the sole serializer for Command.String; keeping it here makes
// the grammar self-contained and the quoting rule unambiguous.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Compile-time assertion that Command satisfies the read-only view the
// handlers depend on. If a refactor drops an accessor, this fails to
// build — the same belt-and-braces a `var _ Provider = ...` would give
// the cloud path.
var _ Parsed = Command{}
