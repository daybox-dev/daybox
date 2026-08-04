package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileForNode(t *testing.T) {
	cases := map[string]string{
		"daybox-default":               "default",
		"daybox-daybox":                "daybox",
		"Daybox-Default.ts.net":        "default", // FQDN, any case
		"daybox-relay":                 "relay",   // gated later by profile existence
		"mac":                          "",        // a laptop is not a box
		"daybox-":                      "",
		"daybox-Bad_Name":              "",
		"":                             "",
		"evil.daybox-default.internal": "", // only the first label counts
	}
	for node, want := range cases {
		if got := profileForNode(node); got != want {
			t.Errorf("profileForNode(%q) = %q, want %q", node, got, want)
		}
	}
}

func TestStageProposal(t *testing.T) {
	store := t.TempDir()
	body := []byte("packages = [\"jq\"]\n")

	id1, err := stageProposal(store, "default", body)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := stageProposal(store, "default", body)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("same-second proposals share an id: %s", id1)
	}
	if !strings.HasSuffix(id2, ".1") && id1[:15] == id2[:15] {
		t.Errorf("collision suffix missing: %s vs %s", id1, id2)
	}
	b, err := os.ReadFile(filepath.Join(store, "default", "proposals", id1+".toml"))
	if err != nil || string(b) != string(body) {
		t.Fatalf("staged content mismatch: %q, %v", b, err)
	}
}

func TestStageProposalPendingCap(t *testing.T) {
	store := t.TempDir()
	dir := filepath.Join(store, "default", "proposals")
	os.MkdirAll(dir, 0o700)
	for i := 0; i < relayMaxPending; i++ {
		os.WriteFile(filepath.Join(dir, string(rune('a'+i%26))+strings.Repeat("x", i)+".toml"), nil, 0o600)
	}
	if _, err := stageProposal(store, "default", []byte("x = 1")); err == nil {
		t.Fatal("pending cap not enforced")
	}
}

// The full auth path through the HTTP surface, with identity faked per
// caller address: only a box node, bound to an existing profile, submitting
// a valid profile, gets a proposal staged.
func TestRelayPropose(t *testing.T) {
	store := t.TempDir()
	os.MkdirAll(filepath.Join(store, "default", "proposals"), 0o700)
	os.WriteFile(filepath.Join(store, "default", "profile.toml"), []byte("packages = []\n"), 0o600)

	nodeByAddr := map[string]string{
		"100.64.0.10:1": "daybox-default", // the profile's own box
		"100.64.0.11:1": "daybox-ghost",   // box of a profile that doesn't exist
		"100.64.0.12:1": "mac",            // an enrolled laptop — not a box
	}
	mux := relayMux(func(_ context.Context, addr string) (string, error) {
		if n, ok := nodeByAddr[addr]; ok {
			return n, nil
		}
		return "", fmt.Errorf("unknown peer")
	}, store)

	post := func(addr, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/propose", strings.NewReader(body))
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	if w := post("100.64.0.10:1", "packages = [\"jq\"]\n"); w.Code != http.StatusOK {
		t.Errorf("valid propose: got %d %s", w.Code, w.Body)
	}
	if w := post("100.64.0.10:1", "bogus_key = 1\n"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid profile accepted: got %d", w.Code)
	}
	if w := post("100.64.0.11:1", "packages = []\n"); w.Code != http.StatusNotFound {
		t.Errorf("unknown profile: got %d", w.Code)
	}
	if w := post("100.64.0.12:1", "packages = []\n"); w.Code != http.StatusForbidden {
		t.Errorf("non-box node allowed: got %d", w.Code)
	}
	if w := post("100.64.0.99:1", "packages = []\n"); w.Code != http.StatusForbidden {
		t.Errorf("unidentified caller allowed: got %d", w.Code)
	}

	ents, _ := os.ReadDir(filepath.Join(store, "default", "proposals"))
	if len(ents) != 1 {
		t.Errorf("expected exactly the one valid proposal staged, found %d", len(ents))
	}
}
