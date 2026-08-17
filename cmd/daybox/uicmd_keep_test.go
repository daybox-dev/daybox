package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// keepExec captures the command and (for put) the stdin fed to it.
func keepExec(t *testing.T, gotCmd *Command, gotStdin *string) uiExec {
	t.Helper()
	return func(c Command, w io.Writer, stdin io.Reader) error {
		*gotCmd = c
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			*gotStdin = string(b)
		}
		io.WriteString(w, "") // keep cat writes the file; keep put writes nothing
		return nil
	}
}

func TestUIKeepGet(t *testing.T) {
	store := t.TempDir()
	var got Command
	exec := func(c Command, w io.Writer, stdin io.Reader) error {
		got = c
		io.WriteString(w, "[[files]]\npath = \"/work/state\"\nwithin = \"10m\"\n")
		return nil
	}
	mux := uiMux(exec, store)

	req := httptest.NewRequest("GET", "/api/keep/default", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("keep get: got %d %s", w.Code, w.Body)
	}
	if got.Verb() != "keep" {
		t.Errorf("verb = %q, want keep", got.Verb())
	}
	if !strings.Contains(strings.Join(got.Rest(), " "), "cat") {
		t.Errorf("rest = %v, want cat subverb", got.Rest())
	}
	if got.Global("profile") != "default" {
		t.Errorf("profile = %q, want default", got.Global("profile"))
	}
	if !strings.Contains(w.Body.String(), "/work/state") {
		t.Errorf("body = %q, want the keep.toml content", w.Body.String())
	}
}

func TestUIKeepPut(t *testing.T) {
	store := t.TempDir()
	var got Command
	var gotStdin string
	mux := uiMux(keepExec(t, &got, &gotStdin), store)

	body := "[[files]]\npath = \"/work/state\"\nwithin = \"10m\"\n"
	req := httptest.NewRequest("PUT", "/api/keep/default", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("keep put: got %d %s", w.Code, w.Body)
	}
	if got.Verb() != "keep" {
		t.Errorf("verb = %q, want keep", got.Verb())
	}
	if !strings.Contains(strings.Join(got.Rest(), " "), "put") {
		t.Errorf("rest = %v, want put subverb", got.Rest())
	}
	if gotStdin != body {
		t.Errorf("stdin = %q, want the request body", gotStdin)
	}
}

func TestUIKeepPutValidates(t *testing.T) {
	store := t.TempDir()
	var execCalled bool
	mux := uiMux(func(c Command, w io.Writer, stdin io.Reader) error {
		execCalled = true
		return nil
	}, store)

	// relative path fails keepPathRe (^/[A-Za-z0-9._/-]+$)
	req := httptest.NewRequest("PUT", "/api/keep/default",
		strings.NewReader("[[files]]\npath = \"relative\"\nwithin = \"10m\"\n"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad keep: got %d, want 422", w.Code)
	}
	if execCalled {
		t.Error("exec was called despite invalid keep.toml — should validate first")
	}
}

func TestUIKeepBadName(t *testing.T) {
	store := t.TempDir()
	mux := uiMux(func(c Command, w io.Writer, stdin io.Reader) error { return nil }, store)
	req := httptest.NewRequest("GET", "/api/keep/Bad", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad name: got %d, want 400", w.Code)
	}
}

func TestUIKeepPutNoProfile(t *testing.T) {
	store := t.TempDir()
	var got Command
	mux := uiMux(keepExec(t, &got, new(string)), store)

	req := httptest.NewRequest("PUT", "/api/keep/default", strings.NewReader(""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty keep put: got %d %s", w.Code, w.Body)
	}
	// empty keep.toml is valid (no [[files]] entries)
	if got.Global("profile") != "default" {
		t.Errorf("profile = %q, want default", got.Global("profile"))
	}
}
