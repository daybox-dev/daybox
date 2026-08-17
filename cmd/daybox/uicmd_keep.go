package main

// uicmd_keep.go — keep.toml endpoints for the control-plane UI.
//
// Unlike profiles/proposals (plane-local files), keep.toml lives on the
// BOX's /work volume — so the UI wraps the plane's internal `keep cat` /
// `keep put` subverbs, which ssh from the plane to the running box. This
// means these handlers use the exec seam (subprocess), not direct file I/O,
// and `keep put` feeds the request body to the subprocess's stdin (which
// `keep put` on the plane reads and ssh-feeds to the box).
//
// Requires a live box (keep is volume-only — the file is on the mounted
// volume). If no box is up, `keep cat`/`keep put` fail with an actionable
// error; the handler surfaces that as 500.
//
// Reuses validateKeepToml (keepedit.go) — the same gate the laptop's keep
// edit loop uses — so a bad edit dies at the UI, not on the box.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// keepGetHandler: GET /api/keep/{name} wraps `daybox -p <name> keep cat`,
// which ssh's to the running box and prints its keep.toml.
func keepGetHandler(exec uiExec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validProfileName(name) {
			http.Error(w, "invalid profile name", http.StatusBadRequest)
			return
		}
		c, _ := Parse([]string{"keep", "-p", name, "cat"}, globalFlags)
		var buf bytes.Buffer
		if err := exec(c, &buf, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, buf.String())
	}
}

// keepPutHandler: PUT /api/keep/{name} validates the body as keep.toml, then
// wraps `daybox -p <name> keep put`, feeding the body via stdin (the plane's
// keep put reads stdin and ssh-feeds it to the box). Validates BEFORE
// invoking exec — a bad edit never reaches the box.
func keepPutHandler(exec uiExec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validProfileName(name) {
			http.Error(w, "invalid profile name", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateKeepToml(string(body)); err != nil {
			http.Error(w, "not a valid keep.toml: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		c, _ := Parse([]string{"keep", "-p", name, "put"}, globalFlags)
		if err := exec(c, io.Discard, strings.NewReader(string(body))); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "keep.toml updated — takes effect on the next reaper tick (5min)")
	}
}
