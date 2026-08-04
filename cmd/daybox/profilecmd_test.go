package main

import (
	"os"
	"strings"
	"testing"
)

func TestValidateProfileAcceptsFullShape(t *testing.T) {
	src := `
packages = ["ripgrep", "jq"]
repos = ["git@github.com:you/yourthing.git"]

[persist]
".claude/" = "claude"
".config/gh/" = "gh"

[setup]
once = ["echo once"]
every_boot = ["echo boot"]

[tools]
node = "24.18.0"

[tools.settings]
locked = true
minimum_release_age = "3d"
`
	if err := validateProfile(src); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

// The shipped seed template must always pass the laptop-side validator —
// they share a schema with remote/apply-seed.py by contract.
func TestValidateProfileAcceptsDefaultSeed(t *testing.T) {
	b, err := os.ReadFile("../../profile.default.toml")
	if err != nil {
		t.Fatalf("read profile.default.toml: %v", err)
	}
	if err := validateProfile(string(b)); err != nil {
		t.Fatalf("profile.default.toml rejected: %v", err)
	}
}

func TestValidateProfileRejectsUnknownTopLevelKey(t *testing.T) {
	err := validateProfile(`package = ["typo"]`)
	if err == nil {
		t.Fatal("unknown top-level key accepted")
	}
	if !strings.Contains(err.Error(), "package") || !strings.Contains(err.Error(), "packages") {
		t.Fatalf("error should name the typo and the known keys: %v", err)
	}
}

func TestValidateProfileRejectsUnknownSetupKey(t *testing.T) {
	err := validateProfile("[setup]\nalways = [\"echo\"]")
	if err == nil {
		t.Fatal("unknown [setup] key accepted")
	}
	if !strings.Contains(err.Error(), "always") || !strings.Contains(err.Error(), "every_boot") {
		t.Fatalf("error should name the typo and the known keys: %v", err)
	}
}

func TestValidateProfileRejectsNonTableSetup(t *testing.T) {
	if err := validateProfile(`setup = "oops"`); err == nil {
		t.Fatal("non-table [setup] accepted")
	}
	if err := validateProfile("[[setup]]\nonce = [\"echo\"]"); err == nil {
		t.Fatal("array-of-tables [[setup]] accepted")
	}
}

func TestValidateProfileRejectsBrokenTOML(t *testing.T) {
	if err := validateProfile(`packages = [`); err == nil {
		t.Fatal("broken TOML accepted")
	}
}

func TestValidProfileName(t *testing.T) {
	for _, good := range []string{"default", "daybox", "a-1"} {
		if !validProfileName(good) {
			t.Errorf("%q should be valid", good)
		}
	}
	for _, bad := range []string{"", "Big", "a_b", "a b", "a'b", "a/b", "café"} {
		if validProfileName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
