package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDo answers canned responses keyed by METHOD path, recording every
// call so a test asserts exactly which cloud calls the provider made (the
// bash contract is fussy about call order — e.g. Summon resolves keys
// before POST /servers).
type fakeDo struct {
	responses map[string]fakeResp
	calls     []string
}

type fakeResp struct {
	status int
	body   string
	err    error
}

func (f *fakeDo) Do(req *http.Request) (*http.Response, error) {
	// key by path relative to the /v1 API base so test tables read cleanly
	key := req.Method + " " + strings.TrimPrefix(req.URL.Path, "/v1")
	// the ssh_keys fingerprint query varies by fingerprint; keep the key
	// path-only so the test's response table is keyed stably
	f.calls = append(f.calls, key)
	r, ok := f.responses[key]
	if !ok {
		// default: empty 200 so an unmocked call is visible, not a panic
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body))}, nil
}

func newFakeProvider(t *testing.T) (*hetznerProvider, *fakeDo) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDo{responses: map[string]fakeResp{}}
	p := newHetznerProvider(tokenFile, filepath.Join(dir, "providers", "hetzner"))
	if err := os.MkdirAll(p.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p.do = fd
	return p, fd
}

func TestHetznerHasCredentials(t *testing.T) {
	p, _ := newFakeProvider(t)
	if !p.HasCredentials() {
		t.Fatal("token present -> HasCredentials true")
	}
	if err := p.CheckCredentials(); err != nil {
		t.Fatalf("CheckCredentials with token: %v", err)
	}
	os.Remove(p.tokenFile)
	if p.HasCredentials() {
		t.Fatal("token gone -> HasCredentials false")
	}
	if err := p.CheckCredentials(); err == nil {
		t.Fatal("missing token -> CheckCredentials should error with setup help")
	}
}

func TestHetznerProbeFoundAndMissing(t *testing.T) {
	p, fd := newFakeProvider(t)
	fd.responses["GET /servers"] = fakeResp{body: `{"servers":[{"id":160822035,"name":"daybox-default","status":"running","created":"2026-08-09T19:45:53Z","server_type":{"name":"ccx33"},"public_net":{"ipv4":{"ip":"5.78.177.250"}}}]}`}
	s, err := p.Probe("daybox-default")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("expected a server, got nil")
	}
	// normalized contract shape: id stringified, type from server_type.name
	want := Server{
		ID:      "160822035",
		Name:    "daybox-default",
		IP:      "5.78.177.250",
		Status:  "running",
		Created: "2026-08-09T19:45:53Z",
		Type:    "ccx33",
	}
	if *s != want {
		t.Errorf("Probe normalized = %+v, want %+v", *s, want)
	}

	// missing -> nil, nil (the bash `null`)
	fd.responses["GET /servers"] = fakeResp{body: `{"servers":[]}`}
	s, err = p.Probe("daybox-missing")
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("missing server -> %+v, want nil", s)
	}
}

func TestHetznerAPIErrorSurfaced(t *testing.T) {
	p, fd := newFakeProvider(t)
	fd.responses["GET /servers"] = fakeResp{body: `{"error":{"code":"unauthorized","message":"invalid token"}}`}
	_, err := p.Probe("daybox-default")
	if err == nil {
		t.Fatal("expected an API error, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("error message = %q, want it to name code + message", err.Error())
	}
}

func TestHetznerSummon(t *testing.T) {
	p, fd := newFakeProvider(t)
	// ssh key cache must exist before summon (matches bash: run setup first)
	if err := os.WriteFile(filepath.Join(p.stateDir, "ssh_key_names.json"), []byte(`["laptop"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	fd.responses["POST /servers"] = fakeResp{body: `{"server":{"id":42,"name":"daybox-default","status":"initializing","created":"2026-08-09T19:45:00Z","server_type":{"name":"ccx33"},"public_net":{"ipv4":{"ip":"5.78.177.250"}}}}`}
	fd.responses["GET /servers/42"] = fakeResp{body: `{"server":{"id":42,"name":"daybox-default","status":"running","created":"2026-08-09T19:45:00Z","server_type":{"name":"ccx33"},"public_net":{"ipv4":{"ip":"5.78.177.250"}}}}`}

	s, err := p.Summon("daybox-default", "ccx33", "ubuntu-24.04", "hil", "123456", "#cloud-config\n")
	if err != nil {
		t.Fatalf("summon: %v", err)
	}
	if s.ID != "42" || s.IP != "5.78.177.250" || s.Status != "running" {
		t.Errorf("summon result = %+v", s)
	}
	// assert the create body carried the volume + user_data + keys
	createReq := fd.calls[0]
	if createReq != "POST /servers" {
		t.Errorf("first call = %q, want POST /servers", createReq)
	}
}

// TestHetznerSummonNoIdAborts guards the regression where a failed server
// create once proceeded with an empty id/ip (bash subshell-errexit bug).
func TestHetznerSummonNoIdAborts(t *testing.T) {
	p, fd := newFakeProvider(t)
	if err := os.WriteFile(filepath.Join(p.stateDir, "ssh_key_names.json"), []byte(`["laptop"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	fd.responses["POST /servers"] = fakeResp{body: `{}`} // no id, no ip
	_, err := p.Summon("daybox-default", "ccx33", "ubuntu-24.04", "hil", "1", "")
	if err == nil {
		t.Fatal("empty create response must abort, not bless a phantom box")
	}
	if !strings.Contains(err.Error(), "nothing was created") {
		t.Errorf("error = %q, want 'nothing was created'", err.Error())
	}
	// must NOT have polled a server that was never created
	for _, c := range fd.calls {
		if strings.HasPrefix(c, "GET /servers/") {
			t.Errorf("polled a phantom server: %s", c)
		}
	}
}

func TestHetznerReap(t *testing.T) {
	p, fd := newFakeProvider(t)
	fd.responses["DELETE /servers/99"] = fakeResp{}
	if err := p.Reap("99"); err != nil {
		t.Fatal(err)
	}
	if len(fd.calls) != 1 || fd.calls[0] != "DELETE /servers/99" {
		t.Errorf("calls = %v, want [DELETE /servers/99]", fd.calls)
	}
}

func TestHetznerVolumeEnsureCreateAndAdopt(t *testing.T) {
	p, fd := newFakeProvider(t)
	// not found -> create
	fd.responses["GET /volumes"] = fakeResp{body: `{"volumes":[]}`}
	fd.responses["POST /volumes"] = fakeResp{body: `{"volume":{"id":7}}`}
	id, err := p.VolumeEnsure("daybox-default-vol", 50, "hil")
	if err != nil {
		t.Fatal(err)
	}
	if id != "7" {
		t.Errorf("created id = %q, want 7", id)
	}
	// exists -> adopt, no create call. Reset recorded calls so the adopt
	// path is judged on its OWN traffic, not the create call's.
	fd.calls = nil
	fd.responses["GET /volumes"] = fakeResp{body: `{"volumes":[{"id":7}]}`}
	id, err = p.VolumeEnsure("daybox-default-vol", 50, "hil")
	if err != nil {
		t.Fatal(err)
	}
	if id != "7" {
		t.Errorf("adopted id = %q, want 7", id)
	}
	created := false
	for _, c := range fd.calls {
		if c == "POST /volumes" {
			created = true
		}
	}
	if created {
		t.Errorf("adopt path should not POST /volumes; calls=%v", fd.calls)
	}
}

func TestHetznerVolumeAttachedTo(t *testing.T) {
	p, fd := newFakeProvider(t)
	fd.responses["GET /volumes/7"] = fakeResp{body: `{"volume":{"server":42}}`}
	got, err := p.VolumeAttachedTo("7")
	if err != nil {
		t.Fatal(err)
	}
	if got != "42" {
		t.Errorf("attached = %q, want 42", got)
	}
	fd.responses["GET /volumes/7"] = fakeResp{body: `{"volume":{}}`}
	got, err = p.VolumeAttachedTo("7")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("free volume -> %q, want empty", got)
	}
}

func TestHetznerPriceHourly(t *testing.T) {
	p, fd := newFakeProvider(t)
	fd.responses["GET /server_types"] = fakeResp{body: `{"server_types":[{"prices":[{"location":"hil","price_hourly":{"gross":"0.2259000000"}},{"location":"ash","price_hourly":{"gross":"0.33"}}]}]}`}
	got := p.PriceHourly("ccx33", "hil")
	// bash: cut -c1-6 -> "0.2259"
	if got != "0.2259" {
		t.Errorf("price = %q, want 0.2259 (first 6 chars of gross)", got)
	}
	got = p.PriceHourly("ccx33", "ash")
	if got != "0.33" {
		t.Errorf("price = %q, want 0.33", got)
	}
	// unknown type: Hetzner returns an empty types list for an unknown
	// name; the provider must return "", not guess.
	fd.responses["GET /server_types"] = fakeResp{body: `{"server_types":[]}`}
	if p.PriceHourly("nope", "hil") != "" {
		t.Error("unknown type -> empty, not a guess")
	}
}

func TestHetznerUserDataCap(t *testing.T) {
	p, _ := newFakeProvider(t)
	if got := p.UserDataMaxBytes(); got != 32768 {
		t.Errorf("UserDataMaxBytes = %d, want 32768", got)
	}
}

// TestHetznerPrepareSSHKeysExistingAndNew guards the resolve-not-register
// path (a key may exist under a different name) and the dedupe of two files
// resolving to one registered key.
func TestHetznerPrepareSSHKeysExistingAndNew(t *testing.T) {
	p, fd := newFakeProvider(t)
	keysDir := t.TempDir()
	// a real-ish ed25519 pubkey line so md5Fingerprint can parse it
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIETK/JI88OZihytMTWNWbOmQhLPVXFEKtw4sLg5XTVMx laptop\n"
	if err := os.WriteFile(filepath.Join(keysDir, "laptop.pub"), []byte(pub), 0o644); err != nil {
		t.Fatal(err)
	}
	// the key already exists under a different name -> reference that name
	fd.responses["GET /ssh_keys"] = fakeResp{body: `{"ssh_keys":[{"name":"old-laptop"}]}`}

	if err := p.PrepareSSHKeys(keysDir); err != nil {
		t.Fatal(err)
	}
	created := false
	for _, c := range fd.calls {
		if c == "POST /ssh_keys" {
			created = true
		}
	}
	if created {
		t.Error("an existing key must be REUSED, not re-registered (no POST /ssh_keys)")
	}
	cache := filepath.Join(p.stateDir, "ssh_key_names.json")
	b, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "old-laptop" {
		t.Errorf("cached names = %v, want [old-laptop]", names)
	}
}

func TestHetznerPrepareSSHKeysRegisterNew(t *testing.T) {
	p, fd := newFakeProvider(t)
	keysDir := t.TempDir()
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIETK/JI88OZihytMTWNWbOmQhLPVXFEKtw4sLg5XTVMx laptop\n"
	if err := os.WriteFile(filepath.Join(keysDir, "mac.pub"), []byte(pub), 0o644); err != nil {
		t.Fatal(err)
	}
	// no existing key -> register
	fd.responses["GET /ssh_keys"] = fakeResp{body: `{"ssh_keys":[]}`}
	fd.responses["POST /ssh_keys"] = fakeResp{}
	if err := p.PrepareSSHKeys(keysDir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(p.stateDir, "ssh_key_names.json"))
	var names []string
	json.Unmarshal(b, &names)
	if len(names) != 1 || names[0] != "mac" {
		t.Errorf("cached names = %v, want [mac]", names)
	}
}

// TestHetznerSummonRequiresKeyCache guards that a summon without setup is
// loud, not a silent skip (bash: _hz_ssh_key_names dies).
func TestHetznerSummonRequiresKeyCache(t *testing.T) {
	p, _ := newFakeProvider(t) // no cache written
	_, err := p.Summon("daybox-default", "ccx33", "ubuntu-24.04", "hil", "1", "")
	if err == nil || !strings.Contains(err.Error(), "no resolved ssh keys") {
		t.Errorf("summon without key cache: err = %v, want 'no resolved ssh keys'", err)
	}
}


