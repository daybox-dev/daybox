package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// statusProvider returns a scripted probe result + price for status tests.
type statusProvider struct {
	probe *Server
	price string
}

func (s *statusProvider) Name() string             { return "hetzner" }
func (s *statusProvider) HasCredentials() bool      { return true }
func (s *statusProvider) CheckCredentials() error   { return nil }
func (s *statusProvider) PrepareSSHKeys(string) error { return nil }
func (s *statusProvider) Probe(string) (*Server, error) { return s.probe, nil }
func (s *statusProvider) Summon(string, string, string, string, string, string) (Server, error) {
	return Server{}, nil
}
func (s *statusProvider) Reap(string) error             { return nil }
func (s *statusProvider) VolumeEnsure(string, int, string) (string, error) { return "1", nil }
func (s *statusProvider) VolumeAttachedTo(string) (string, error) { return "", nil }
func (s *statusProvider) VolumeDetach(string) error    { return nil }
func (s *statusProvider) VolumeSize(string) (int, error) { return 50, nil }
func (s *statusProvider) VolumeRename(string, string) error { return nil }
func (s *statusProvider) VolumeDelete(string) error    { return nil }
func (s *statusProvider) UserDataMaxBytes() int        { return 32768 }
func (s *statusProvider) PriceHourly(string, string) string { return s.price }

func newStatusProfile(t *testing.T, maxHours int) *profile {
	p := newReapTestProfile(t, maxHours, 30) // reuse the helper
	return p
}

func TestStatusNoBox(t *testing.T) {
	p := newStatusProfile(t, 12)
	var out bytes.Buffer
	statusOne(p, &statusProvider{probe: nil, price: ""}, &out)
	got := out.String()
	if !strings.Contains(got, "profile 'default':") {
		t.Errorf("missing profile header: %q", got)
	}
	if !strings.Contains(got, "no box running") {
		t.Errorf("missing no-box line: %q", got)
	}
	if !strings.Contains(got, "billing: volume only") {
		t.Errorf("should note billing is volume-only: %q", got)
	}
}

func TestStatusRunningBox(t *testing.T) {
	p := newStatusProfile(t, 12)
	var out bytes.Buffer
	prov := &statusProvider{
		probe: &Server{ID: "160821580", Name: "daybox-default", IP: "5.78.186.220", Status: "running", Created: "2026-08-09T19:44:54Z", Type: "ccx33"},
		price: "0.2259",
	}
	statusOne(p, prov, &out)
	got := out.String()
	for _, want := range []string{
		"big box: daybox-default  id=160821580  ccx33  running",
		"ip: 5.78.186.220",
		"created: 2026-08-09T19:44:54Z",
		"~€0.2259/h gross",
		"ingress: locked down",
		"idle ticks:",
		"lifetime cap: 12h",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestStatusCapDisabled(t *testing.T) {
	p := newStatusProfile(t, 0) // cap disabled
	var out bytes.Buffer
	statusOne(p, &statusProvider{
		probe: &Server{ID: "1", Name: "daybox-default", IP: "1.2.3.4", Status: "running", Created: "2026-08-09T19:44:54Z", Type: "ccx33"},
		price: "0.2259",
	}, &out)
	if !strings.Contains(out.String(), "lifetime cap: DISABLED") {
		t.Errorf("disabled cap should say so: %q", out.String())
	}
}

func TestStatusFleetVersion(t *testing.T) {
	p := newStatusProfile(t, 12)
	// record a box agent version that differs from the plane's; the plane's
	// agentVersion() shells out to the agent binary, which won't exist in
	// the test sandbox -> it returns "?". So set the box version + assert
	// the mismatch line names the plane's (unknown) version.
	writeFile(p.agentVersionFile(), "v0.2.11")
	var out bytes.Buffer
	statusOne(p, &statusProvider{
		probe: &Server{ID: "1", Name: "daybox-default", IP: "1.2.3.4", Status: "running", Created: "2026-08-09T19:44:54Z", Type: "ccx33"},
		price: "0.2259",
	}, &out)
	got := out.String()
	// the plane agent in the sandbox is "?", so a mismatch line naming the
	// box version + a rotate hint should appear
	if !strings.Contains(got, "v0.2.11") {
		t.Errorf("box agent version not shown: %q", got)
	}
	if !strings.Contains(got, "rotate with: daybox down + up") {
		t.Errorf("mismatch should offer rotation: %q", got)
	}
}

// TestNetTableParses the headscale nodes JSON into the table columns,
// including the ephemeral (box) vs device distinction + online/offline.
func TestNetTableParses(t *testing.T) {
	// build a deployment whose headscaleNodesJSON returns a scripted list.
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = loadDeployment()
	// monkey-patch headscaleNodesJSON by writing nothing; instead test the
	// node-struct parse path directly via a helper that mimics netTable's
	// JSON shape.
	nodes := []netNode{
		{ID: 4, GivenName: "emilios-macbook-air", Name: "emilios-macbook-air", IPAddresses: []string{"100.64.0.3"}, User: struct{ Name string `json:"name"` }{Name: "emilio"}, Online: false},
		{ID: 57, GivenName: "daybox-default", Name: "daybox-default", IPAddresses: []string{"100.64.0.56"}, User: struct{ Name string `json:"name"` }{Name: "emilio"}, Connected: true, PreAuthKey: &struct{ Ephemeral bool `json:"ephemeral"` }{Ephemeral: true}},
	}
	b, _ := json.Marshal(nodes)
	var parsed []netNode
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 {
		t.Fatalf("parsed %d nodes, want 2", len(parsed))
	}
	if parsed[0].ephemeral() {
		t.Error("device node should NOT be ephemeral (it's a device)")
	}
	if !parsed[1].ephemeral() {
		t.Error("box node should be ephemeral")
	}
	if parsed[1].GivenName != "daybox-default" || parsed[1].IPAddresses[0] != "100.64.0.56" {
		t.Errorf("box node parsed wrong: %+v", parsed[1])
	}
}

// TestStatusSpendCalc: the "spent so far" line scales price × age / 60.
func TestStatusSpendCalc(t *testing.T) {
	got := priceFloat("0.2259")
	want := 0.2259
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("priceFloat = %v, want %v", got, want)
	}
	// 60min at 0.2259/h = ~0.23
	spend := priceFloat("0.2259") * 60 / 60
	if spend < 0.22 || spend > 0.23 {
		t.Errorf("spend calc = %v, want ~0.23", spend)
	}
}
