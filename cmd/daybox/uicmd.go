package main

// uicmd.go — the control-plane web UI (`daybox ui`): a localhost HTTP
// surface that wraps the existing daybox verbs, so an operator can see and
// manage boxes from a browser without the laptop CLI. See
// daybox-internal/docs/control-plane-ui.md.
//
// daybox provides a UI, not an edge: the daemon binds 127.0.0.1 and the
// hoster fronts it (reverse proxy / tunnel / their own net) for whatever
// reachability they want. TLS, auth, CSRF all live in the hoster's layer;
// daybox ships none of it (SECURITY.md). A confirmation header on
// state-changing verbs is a fat-finger guard, not an auth boundary.
//
// The shape mirrors relaycmd.go — an injectable mux so the HTTP surface is
// testable without a net or a real binary — even though this listener is a
// plain net.Listen, not tsnet. Nothing on the net calls the UI (browsers
// do), so tsnet buys nothing here.
//
// One exec seam, used two ways: status runs it to a buffer (sync); write
// verbs (up/down/reap) run it in a goroutine as a polled job (async).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const uiDefaultPort = 4748 // relay is 4747; the UI takes the next one

// uiExec runs a daybox verb, streaming combined stdout+stderr to w, and
// returns the subprocess error. Injected into uiMux so handlers are
// unit-testable without a binary. Combined output matters for jobs: the
// narration (`say` writes to stderr) IS the progress stream.
type uiExec func(c Command, w io.Writer) error

// uiMux is the UI's whole HTTP surface. exec is injected (testability, like
// relayMux's whoisNode); store is the profiles dir, unused by the current
// endpoints but wired for the profiles/keep/proposals views to come.
func uiMux(exec uiExec, store string) *http.ServeMux {
	jobs := newJobStore(exec)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiIndexHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		// wraps `daybox status` (all profiles). ?profile= scoping comes later.
		c, _ := Parse([]string{"status"}, globalFlags)
		var buf bytes.Buffer
		if err := exec(c, &buf); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(buf.Bytes())
	})
	mux.HandleFunc("POST /api/up", jobHandler(jobs, "up"))
	mux.HandleFunc("POST /api/down", jobHandler(jobs, "down"))
	mux.HandleFunc("POST /api/reap", jobHandler(jobs, "reap"))
	mux.HandleFunc("GET /api/jobs/{id}", jobPollHandler(jobs))
	return mux
}

// cmdUI is the `daybox ui` verb (resident daemon, runs under systemd on the
// plane like `relay`). Binds 127.0.0.1 — the hoster fronts it.
func cmdUI(p Parsed) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	addr := fs.String("addr", fmt.Sprintf("127.0.0.1:%d", uiDefaultPort),
		"address to listen on (the hoster fronts this; daybox provides no edge)")
	store := fs.String("store", filepath.Join(confDir(), "profiles"),
		"profiles dir (for the profiles/keep/proposals endpoints)")
	fs.Parse(p.Rest())

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("ui: listen %s: %v", *addr, err)
	}
	log.Printf("daybox ui on %s — hoster fronts this; daybox provides no edge (SECURITY.md)", *addr)
	log.Fatal(http.Serve(ln, uiMux(realExec, *store)))
}

// realExec runs a daybox verb by re-invoking this binary with argv rebuilt
// from the Command — no shell, no PATH dependency (os.Executable resolves
// the absolute path). Combined stdout+stderr so the job stream sees the
// narration. It is wiring (untested, like the relay's listener);
// commandArgv is the tested pure part.
func realExec(c Command, w io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, commandArgv(c)...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// commandArgv rebuilds argv from a Command for direct subprocess exec:
// verb, then each hoisted global as --long value, then the verb's own rest
// tokens. Globals are emitted in globalFlags order (flag order is
// irrelevant to the verb); rest tokens (e.g. --detach) keep their order.
// Distinct from Command.String() (shell-quoted, arrival order) which is for
// ssh delegation; this is for exec.Command.
func commandArgv(c Command) []string {
	argv := []string{c.Verb()}
	for _, g := range globalFlags {
		if v := c.Global(g.Long); v != "" {
			argv = append(argv, "--"+g.Long, v)
		}
	}
	argv = append(argv, c.Rest()...)
	return argv
}

// ---------------------------------------------------------------- jobs -----

// A job is a long-running verb (up/down/reap) running as a subprocess; the
// browser polls /api/jobs/<id> for its state + accumulated output. One job
// per verb+profile is allowed at a time — a second is 409'd — so a stray
// double-click can't double-summon (real money) or double-reap.

type jobState string

const (
	jobRunning jobState = "running"
	jobDone    jobState = "done"
	jobFailed  jobState = "failed"
)

var errJobConflict = fmt.Errorf("a job for this verb/profile is already running")

// job is a single verb execution. It implements io.Writer so the exec can
// stream output into it; a mutex guards the state + buffer against concurrent
// reads from the poll handler.
type job struct {
	id      string
	verb    string
	profile string
	state   jobState
	buf     bytes.Buffer
	started time.Time
	mu      sync.Mutex
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.Write(p)
}

func (j *job) snapshot() (jobState, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state, j.buf.String()
}

// jobSnapshot is the JSON shape returned by GET /api/jobs/<id>.
type jobSnapshot struct {
	ID      string `json:"id"`
	Verb    string `json:"verb"`
	Profile string `json:"profile"`
	State   string `json:"state"`
	Output  string `json:"output"`
}

type jobStore struct {
	mu   sync.Mutex
	next int
	jobs map[string]*job
	exec uiExec
}

func newJobStore(exec uiExec) *jobStore {
	return &jobStore{jobs: map[string]*job{}, exec: exec}
}

// start begins a job for the verb+profile. Returns errJobConflict if one is
// already running for that key — the double-summon guard.
func (s *jobStore) start(c Command, profile string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.verb == c.Verb() && j.profile == profile && j.state == jobRunning {
			return "", errJobConflict
		}
	}
	s.next++
	id := strconv.Itoa(s.next)
	j := &job{id: id, verb: c.Verb(), profile: profile, state: jobRunning, started: time.Now()}
	s.jobs[id] = j
	go s.run(j, c)
	return id, nil
}

func (s *jobStore) run(j *job, c Command) {
	if err := s.exec(c, j); err != nil {
		j.mu.Lock()
		j.state = jobFailed
		j.mu.Unlock()
		return
	}
	j.mu.Lock()
	j.state = jobDone
	j.mu.Unlock()
}

func (s *jobStore) get(id string) (*job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

// buildJobCommand shapes the daybox Command for a write verb from the
// request body. up always passes --detach (a browser has no tty to ssh
// into); down takes a profile; reap takes neither. An empty profile omits
// -p so daybox defaults to the 'default' profile.
func buildJobCommand(verb string, body map[string]string) (Command, string) {
	profile := body["profile"]
	var args []string
	switch verb {
	case "up":
		args = []string{"up"}
		if profile != "" {
			args = append(args, "-p", profile)
		}
		args = append(args, "--detach")
		if t := body["type"]; t != "" {
			args = append(args, t)
		}
	case "down":
		args = []string{"down"}
		if profile != "" {
			args = append(args, "-p", profile)
		}
	case "reap":
		args = []string{"reap"}
	}
	c, _ := Parse(args, globalFlags)
	return c, profile
}

// jobHandler handles POST /api/up|down|reap: confirm guard, parse body,
// start the job, 202 + id. The confirm header is a fat-finger guard, not
// auth (daybox provides no edge — SECURITY.md).
func jobHandler(jobs *jobStore, verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Confirm") != "yes" {
			http.Error(w, "confirm required (Confirm: yes header)", http.StatusBadRequest)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) // tolerate empty/invalid → nil map
		c, profile := buildJobCommand(verb, body)
		id, err := jobs.start(c, profile)
		if err == errJobConflict {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

func jobPollHandler(jobs *jobStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, ok := jobs.get(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		state, output := j.snapshot()
		json.NewEncoder(w).Encode(jobSnapshot{
			ID: j.id, Verb: j.verb, Profile: j.profile,
			State: string(state), Output: output,
		})
	}
}
