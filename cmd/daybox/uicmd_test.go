package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUIStatus: GET /api/status shells out to `daybox status` (via the
// injected exec) and returns its stdout. The exec is injected so the
// handler is testable without a net or a real daybox binary — the same
// seam relayMux(whoisNode, store) gives the relay.
func TestUIStatus(t *testing.T) {
	var got Command
	mux := uiMux(func(c Command, w io.Writer, _ io.Reader) error {
		got = c
		io.WriteString(w, "profile 'default':\n  no box running\n")
		return nil
	}, "")

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if got.Verb() != "status" {
		t.Errorf("exec verb = %q, want %q", got.Verb(), "status")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "no box running") {
		t.Errorf("body = %q, want the status output", w.Body.String())
	}
}

// TestUIStatusExecError: a failing exec surfaces as 500, not a 200 with
// partial output.
func TestUIStatusExecError(t *testing.T) {
	mux := uiMux(func(c Command, w io.Writer, _ io.Reader) error {
		return fmt.Errorf("boom")
	}, "")
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// TestUIDashboard: GET / serves the embedded dashboard HTML.
func TestUIDashboard(t *testing.T) {
	mux := uiMux(func(c Command, w io.Writer, _ io.Reader) error { return nil }, "")
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status code = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "daybox") {
		t.Errorf("body = %q, want the dashboard", w.Body.String())
	}
}

// TestCommandArgv: the exec path rebuilds argv from a Command (verb +
// hoisted globals as --long value + rest tokens), so the daybox binary is
// invoked directly — no shell, no PATH dependency, no String() re-quoting.
func TestCommandArgv(t *testing.T) {
	c, _ := Parse([]string{"status"}, globalFlags)
	assertArgv(t, c, "status")

	c, _ = Parse([]string{"up", "-p", "default", "--detach"}, globalFlags)
	assertArgv(t, c, "up", "--profile", "default", "--detach")
}

func assertArgv(t *testing.T, c Command, want ...string) {
	t.Helper()
	got := commandArgv(c)
	if len(got) != len(want) {
		t.Fatalf("commandArgv = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("commandArgv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
