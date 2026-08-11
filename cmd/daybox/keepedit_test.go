package main

import (
	"strings"
	"testing"
)

// keepedit_test.go — tests the pure validation + the no-box refusal. The
// fetch→edit→push round-trip is ssh-bound (laptop → plane → box) and is
// covered by the live verification (W11), not the unit suite.

func TestValidateKeepTomlValid(t *testing.T) {
	if err := validateKeepToml(`# my keep
[[files]]
path = "/work/state/claude/projects"
within = "10m"

[[files]]
path = "/tmp/build.log"
within = "2m"
`); err != nil {
		t.Errorf("valid keep rejected: %v", err)
	}
}

func TestValidateKeepTomlEmpty(t *testing.T) {
	// empty keep.toml = ssh+load only (the safe baseline) — valid.
	if err := validateKeepToml("# no signals\n"); err != nil {
		t.Errorf("empty keep rejected: %v", err)
	}
}

func TestValidateKeepTomlBadTOML(t *testing.T) {
	if err := validateKeepToml("this is not = = valid {{{"); err == nil {
		t.Error("bad TOML should be rejected")
	}
}

func TestValidateKeepTomlBadPath(t *testing.T) {
	cases := map[string]string{
		"relative":       `[[files]]` + "\n" + `path = "relative"` + "\n" + `within = "10m"`,
		"shell metachar": `[[files]]` + "\n" + `path = "/has;rm"` + "\n" + `within = "10m"`,
		"space":          `[[files]]` + "\n" + `path = "/has space"` + "\n" + `within = "10m"`,
	}
	for name, body := range cases {
		if err := validateKeepToml(body); err == nil {
			t.Errorf("%s path should be rejected", name)
		}
	}
}

func TestValidateKeepTomlBadWithin(t *testing.T) {
	cases := map[string]string{
		"missing":      `[[files]]` + "\n" + `path = "/ok/path"`,
		"non-duration": `[[files]]` + "\n" + `path = "/ok/path"` + "\n" + `within = "not-a-duration"`,
		"zero":         `[[files]]` + "\n" + `path = "/ok/path"` + "\n" + `within = "0s"`,
		"negative":     `[[files]]` + "\n" + `path = "/ok/path"` + "\n" + `within = "-5m"`,
	}
	for name, body := range cases {
		if err := validateKeepToml(body); err == nil {
			t.Errorf("%s within should be rejected", name)
		}
	}
}

// TestBoxServerNoBox: keep is volume-only — editing it requires a live box
// to mount the volume. boxServer refuses when no box is running.
func TestBoxServerNoBox(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\nREMOTE_USER=dev\nPROVIDER=hetzner\n")
	// fake provider whose Probe returns nil = no box running
	prov := &reapProvider{probe: nil}
	providerFactory = func(*deployment, string) (Provider, error) { return prov, nil }
	defer func() { providerFactory = realProviderFactory }()
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = boxServer(d, p)
	if err == nil {
		t.Fatal("boxServer with no box should error")
	}
	if !strings.Contains(err.Error(), "no big box running") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestBoxServerWithBox: a running box resolves to a server record.
func TestBoxServerWithBox(t *testing.T) {
	d := newTestDeployment(t)
	writeConfig(t, d, "LITTLEBOX_IP=10.0.0.1\nREMOTE_USER=dev\nPROVIDER=hetzner\n")
	prov := &reapProvider{probe: probeServer(5, true)}
	providerFactory = func(*deployment, string) (Provider, error) { return prov, nil }
	defer func() { providerFactory = realProviderFactory }()
	p, err := d.deriveProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	s, err := boxServer(d, p)
	if err != nil {
		t.Fatalf("boxServer with a box: %v", err)
	}
	if s == nil {
		t.Error("want a server record")
	}
}
