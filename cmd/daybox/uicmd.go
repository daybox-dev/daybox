package main

// uicmd.go — the control-plane web UI (`daybox ui`): a localhost HTTP
// surface that wraps the existing daybox verbs, so an operator can see and
// manage boxes from a browser without the laptop CLI. See
// daybox-internal/docs/control-plane-ui.md.
//
// daybox provides a UI, not an edge: the daemon binds 127.0.0.1 and the
// hoster fronts it (reverse proxy / tunnel / their own net) for whatever
// reachability they want. TLS, auth, CSRF all live in the hoster's layer;
// daybox ships none of it (SECURITY.md). A confirmation token on
// state-changing verbs (later) is a fat-finger guard, not an auth boundary.
//
// The shape mirrors relaycmd.go — an injectable mux so the HTTP surface is
// testable without a net or a real binary — even though this listener is a
// plain net.Listen, not tsnet. Nothing on the net calls the UI (browsers
// do), so tsnet buys nothing here.

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

const uiDefaultPort = 4748 // relay is 4747; the UI takes the next one

// uiExec runs a daybox verb and returns its stdout. Injected into uiMux so
// handlers are unit-testable without shelling out.
type uiExec func(c Command) (string, error)

// uiMux is the UI's whole HTTP surface. exec is injected (testability, like
// relayMux's whoisNode); store is the profiles dir, unused by status today
// but wired for the profiles/keep/proposals endpoints to come.
func uiMux(exec uiExec, store string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiIndexHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		// wraps `daybox status` (all profiles). ?profile= scoping comes later.
		c, _ := Parse([]string{"status"}, globalFlags)
		out, err := exec(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, out)
	})
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
// the absolute path). It is wiring (untested, like the relay's listener);
// commandArgv is the tested pure part.
func realExec(c Command) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := exec.Command(exe, commandArgv(c)...)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	return out.String(), cmd.Run()
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
