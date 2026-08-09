package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionSpellings guards the regression where `daybox -v` fell through
// to usage (exit 2) instead of printing the version. Every spelling must
// print exactly the version and exit 0 — and must NOT print the usage text.
func TestVersionSpellings(t *testing.T) {
	for _, c := range []string{"version", "-v", "--version"} {
		var out, errout bytes.Buffer
		code := run([]string{c}, &out, &errout)
		if code != 0 {
			t.Errorf("daybox %s: exit %d, want 0", c, code)
		}
		got := strings.TrimSpace(out.String())
		if got != version {
			t.Errorf("daybox %s stdout = %q, want %q", c, got, version)
		}
		if errout.Len() != 0 {
			t.Errorf("daybox %s wrote %d bytes to stderr, want 0: %q", c, errout.Len(), errout.String())
		}
		if strings.Contains(out.String(), "usage:") {
			t.Errorf("daybox %s printed usage text — should print only the version", c)
		}
	}
}

// TestNoVerbExits2 keeps the bare-invocation contract: no verb => usage to
// stderr, exit 2 (so `daybox` alone in a shell is loud, not silent success).
func TestNoVerbExits2(t *testing.T) {
	var out, errout bytes.Buffer
	code := run(nil, &out, &errout)
	if code != 2 {
		t.Errorf("daybox (no args): exit %d, want 2", code)
	}
	if !strings.Contains(errout.String(), "usage:") {
		t.Errorf("daybox (no args) did not print usage to stderr")
	}
}

// TestUnknownVerbExits2 guards that an unknown verb still falls through to
// usage + exit 2 (the path `-v` used to wrongly take).
func TestUnknownVerbExits2(t *testing.T) {
	var out, errout bytes.Buffer
	code := run([]string{"definitely-not-a-verb"}, &out, &errout)
	if code != 2 {
		t.Errorf("daybox <unknown>: exit %d, want 2", code)
	}
	if !strings.Contains(errout.String(), "usage:") {
		t.Errorf("daybox <unknown> did not print usage to stderr")
	}
}
