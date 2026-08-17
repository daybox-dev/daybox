package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// controllableExec returns an exec that records the command, signals it has
// started, blocks until release is closed, then writes "done\n" and returns
// nil — so a test can observe a job mid-flight and then complete it.
func controllableExec(t *testing.T, got *Command) (uiExec, chan<- struct{}, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	exec := func(c Command, w io.Writer) error {
		*got = c
		once.Do(func() { close(started) }) // idempotent: multi-job tests start >1 exec
		<-release
		io.WriteString(w, "done\n")
		return nil
	}
	return exec, release, started
}

func postConfirm(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Confirm", "yes")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func getJob(t *testing.T, mux *http.ServeMux, id string) jobSnapshot {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/jobs/"+id, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var snap jobSnapshot
	json.NewDecoder(w.Body).Decode(&snap)
	return snap
}

// pollJob reads a job until its state leaves "running" (the exec completes
// asynchronously once released) or the deadline fires.
func pollJob(t *testing.T, mux *http.ServeMux, id string) jobSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := getJob(t, mux, id); snap.State != string(jobRunning) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job never left running state")
	return jobSnapshot{}
}

// TestUIJobUpLifecycle: the full e2e shape — confirm guard, 202 + id, the
// command is `up -p default --detach`, mid-flight polls as running, and on
// completion the snapshot is done with output.
func TestUIJobUpLifecycle(t *testing.T) {
	var got Command
	exec, release, started := controllableExec(t, &got)
	mux := uiMux(exec, "")

	// Fat-finger guard: POST without Confirm header -> 400.
	req := httptest.NewRequest("POST", "/api/up", strings.NewReader(`{"profile":"default"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no-confirm POST: got %d, want 400", w.Code)
	}

	// POST with Confirm -> 202 + job id.
	w = postConfirm(t, mux, "/api/up", `{"profile":"default"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("up POST: got %d, want 202: %s", w.Code, w.Body)
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	<-started // exec got the command

	// The command was `up -p default --detach` (a browser has no tty to ssh in).
	if got.Verb() != "up" || got.Global("profile") != "default" {
		t.Errorf("exec command = %q (profile=%q), want up/default", got.Verb(), got.Global("profile"))
	}
	if !strings.Contains(strings.Join(got.Rest(), " "), "--detach") {
		t.Errorf("exec rest = %v, want --detach (no tty to connect)", got.Rest())
	}

	// Mid-flight: running.
	if snap := getJob(t, mux, res.ID); snap.State != string(jobRunning) {
		t.Errorf("mid-flight state = %q, want running", snap.State)
	}

	// Release -> completes -> done with output.
	close(release)
	snap := pollJob(t, mux, res.ID)
	if snap.State != string(jobDone) {
		t.Errorf("final state = %q, want done", snap.State)
	}
	if !strings.Contains(snap.Output, "done") {
		t.Errorf("output = %q, want 'done'", snap.Output)
	}
	if snap.Verb != "up" || snap.Profile != "default" {
		t.Errorf("snapshot verb/profile = %q/%q, want up/default", snap.Verb, snap.Profile)
	}
}

// TestUIJobConflict: a second up for a profile with one already running is
// 409 — no double-summon (real money). A different profile is allowed.
func TestUIJobConflict(t *testing.T) {
	var got Command
	exec, release, _ := controllableExec(t, &got)
	mux := uiMux(exec, "")
	defer close(release)

	if w := postConfirm(t, mux, "/api/up", `{"profile":"default"}`); w.Code != http.StatusAccepted {
		t.Fatalf("first up: got %d", w.Code)
	}
	if w := postConfirm(t, mux, "/api/up", `{"profile":"default"}`); w.Code != http.StatusConflict {
		t.Errorf("duplicate up: got %d, want 409", w.Code)
	}
	if w := postConfirm(t, mux, "/api/up", `{"profile":"other"}`); w.Code != http.StatusAccepted {
		t.Errorf("other profile up: got %d, want 202", w.Code)
	}
}

func TestUIJobUnknown(t *testing.T) {
	mux := uiMux(func(c Command, w io.Writer) error { return nil }, "")
	req := httptest.NewRequest("GET", "/api/jobs/nope", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown job: got %d, want 404", w.Code)
	}
}

// TestUIJobReap: reap takes no profile; the command is bare `reap`.
func TestUIJobReap(t *testing.T) {
	var got Command
	exec, release, started := controllableExec(t, &got)
	mux := uiMux(exec, "")
	defer close(release)

	if w := postConfirm(t, mux, "/api/reap", `{}`); w.Code != http.StatusAccepted {
		t.Fatalf("reap POST: got %d, want 202", w.Code)
	}
	<-started
	if got.Verb() != "reap" {
		t.Errorf("reap verb = %q, want reap", got.Verb())
	}
	if got.Global("profile") != "" {
		t.Errorf("reap took a profile (%q), want none", got.Global("profile"))
	}
}
