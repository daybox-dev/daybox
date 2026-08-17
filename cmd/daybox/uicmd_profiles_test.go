package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var nopExec = func(c Command, w io.Writer, _ io.Reader) error { return nil }

// setupProfile writes a profile.toml into store/<name>/.
func setupProfile(t *testing.T, store, name, seed string) {
	t.Helper()
	dir := filepath.Join(store, name)
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "profile.toml"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setupProposal writes a proposal into store/<profile>/proposals/<id>.toml.
func setupProposal(t *testing.T, store, profile, id, content string) {
	t.Helper()
	dir := filepath.Join(store, profile, "proposals")
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, id+".toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUIProfilesList(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProfile(t, store, "work", "packages = []\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/profiles", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d %s", w.Code, w.Body)
	}
	var got []string
	json.NewDecoder(w.Body).Decode(&got)
	if len(got) != 2 || got[0] != "default" || got[1] != "work" {
		t.Errorf("profiles = %v, want [default work]", got)
	}
}

func TestUIProfileSeedGet(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/profiles/default/seed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("seed get: got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "jq") {
		t.Errorf("body = %q, want the seed with jq", w.Body.String())
	}
}

func TestUIProfileSeedNotFound(t *testing.T) {
	store := t.TempDir()
	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/profiles/nope/seed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing seed: got %d, want 404", w.Code)
	}
}

func TestUIProfileBadName(t *testing.T) {
	store := t.TempDir()
	mux := uiMux(nopExec, store)
	// a name with characters validProfileName rejects (uppercase)
	req := httptest.NewRequest("GET", "/api/profiles/Bad/seed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad name: got %d, want 400", w.Code)
	}
}

func TestUIProfileSeedPut(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("PUT", "/api/profiles/default/seed",
		strings.NewReader("packages = [\"jq\"]\n"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("seed put: got %d %s", w.Code, w.Body)
	}
	b, _ := os.ReadFile(filepath.Join(store, "default", "profile.toml"))
	if !strings.Contains(string(b), "jq") {
		t.Errorf("seed not written: %q", b)
	}
}

func TestUIProfileSeedPutValidates(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("PUT", "/api/profiles/default/seed",
		strings.NewReader("bad_key = 1\n")) // unknown top-level key
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad seed: got %d, want 422", w.Code)
	}
	b, _ := os.ReadFile(filepath.Join(store, "default", "profile.toml"))
	if strings.Contains(string(b), "bad_key") {
		t.Errorf("invalid seed was written: %q", b)
	}
}

func TestUIProposalsList(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProposal(t, store, "default", "20260817-120000-default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/proposals", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proposals list: got %d", w.Code)
	}
	var got []proposalInfo
	json.NewDecoder(w.Body).Decode(&got)
	if len(got) != 1 || got[0].Profile != "default" || got[0].ID != "20260817-120000-default" {
		t.Errorf("proposals = %+v, want one for default", got)
	}
}

func TestUIProposalDiff(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProposal(t, store, "default", "20260817-120000-default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/proposals/20260817-120000-default", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proposal diff: got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "jq") || !strings.Contains(body, "default") {
		t.Errorf("body = %q, want the diff with jq + profile name", body)
	}
}

func TestUIProposalAccept(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProposal(t, store, "default", "20260817-120000-default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("POST", "/api/proposals/20260817-120000-default/accept", nil)
	req.Header.Set("Confirm", "yes")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("accept: got %d %s", w.Code, w.Body)
	}
	// live seed now has jq
	b, _ := os.ReadFile(filepath.Join(store, "default", "profile.toml"))
	if !strings.Contains(string(b), "jq") {
		t.Errorf("live seed not updated: %q", b)
	}
	// proposal is gone
	_, err := os.Stat(filepath.Join(store, "default", "proposals", "20260817-120000-default.toml"))
	if !os.IsNotExist(err) {
		t.Errorf("proposal not removed: %v", err)
	}
}

func TestUIProposalAcceptNoConfirm(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProposal(t, store, "default", "20260817-120000-default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("POST", "/api/proposals/20260817-120000-default/accept", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("accept without confirm: got %d, want 400", w.Code)
	}
}

func TestUIProposalReject(t *testing.T) {
	store := t.TempDir()
	setupProfile(t, store, "default", "packages = []\n")
	setupProposal(t, store, "default", "20260817-120000-default", "packages = [\"jq\"]\n")

	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("POST", "/api/proposals/20260817-120000-default/reject", nil)
	req.Header.Set("Confirm", "yes")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("reject: got %d", w.Code)
	}
	// live seed unchanged
	b, _ := os.ReadFile(filepath.Join(store, "default", "profile.toml"))
	if strings.Contains(string(b), "jq") {
		t.Errorf("live seed changed on reject: %q", b)
	}
	// proposal is gone
	_, err := os.Stat(filepath.Join(store, "default", "proposals", "20260817-120000-default.toml"))
	if !os.IsNotExist(err) {
		t.Errorf("proposal not removed: %v", err)
	}
}

func TestUIProposalNotFound(t *testing.T) {
	store := t.TempDir()
	mux := uiMux(nopExec, store)
	req := httptest.NewRequest("GET", "/api/proposals/nope", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing proposal: got %d, want 404", w.Code)
	}
}
