package main

import (
	"strings"
	"testing"
)

// The suggested device-name default comes from the machine hostname; on a
// stock mac that is "Emilios-MacBook-Air", and v0.2.4 offered it verbatim,
// then fatally rejected it — init died on <enter>. Whatever sanitize
// returns must be accepted by validDeviceName (or be empty, meaning no
// default is offered).
func TestSanitizeDeviceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Emilios-MacBook-Air", "emilios-macbook-air"},
		{"Emilio's Mac", "emilio-s-mac"},
		{"héllo", "h-llo"},
		{"--weird--", "weird"},
		{"...", ""},
		{"", ""},
		{"already-fine-42", "already-fine-42"},
	}
	for _, c := range cases {
		got := sanitizeDeviceName(c.in)
		if got != c.want {
			t.Errorf("sanitizeDeviceName(%q) = %q, want %q", c.in, got, c.want)
		}
		if got != "" && !validDeviceName(got) {
			t.Errorf("sanitizeDeviceName(%q) = %q — fails validDeviceName", c.in, got)
		}
	}
	if got := sanitizeDeviceName(strings.Repeat("a", 80) + "-b"); !validDeviceName(got) {
		t.Errorf("long input: %q fails validDeviceName", got)
	}
}
