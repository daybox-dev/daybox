package main

// uicmd_profiles.go — profile + proposal endpoints for the control-plane UI.
//
// These are plane-local file ops, not subprocess calls: the profile seed
// (profile.toml) and pending proposals both live on the control plane at
// ~/.config/daybox/profiles/<name>/, and the ui daemon runs on the plane, so
// the handlers read/write directly — no ssh hop, no binary exec. This is the
// structural advantage the UI has over the laptop (which delegates over ssh):
// it is already where the files are.
//
// Reuses the existing validators + diff renderer:
//   - validateProfile (profilecmd.go) — same gate the relay + laptop edit use
//   - renderProposalDiff (proposalcmd.go) — the same review diff the laptop sees
//   - validProfileName / validProposalID — the same path-safety bounds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// proposalInfo is the JSON shape for GET /api/proposals (list) and the
// metadata part of GET /api/proposals/{id} (diff).
type proposalInfo struct {
	ID      string `json:"id"`
	Profile string `json:"profile"`
}

// findProposal walks store/*/proposals/<id>.toml — ids are globally unique
// (the relay mints timestamped names), so a single id resolves to one file.
// Returns the profile, the proposed content, and the path. Mirrors the
// laptop's findProposal but local (no ssh).
func findProposalLocal(store, id string) (profile, path string, content []byte, ok bool) {
	entries, err := os.ReadDir(store)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !validProfileName(e.Name()) {
			continue
		}
		p := filepath.Join(store, e.Name(), "proposals", id+".toml")
		if b, err := os.ReadFile(p); err == nil {
			return e.Name(), p, b, true
		}
	}
	return
}

// listProposalsLocal enumerates every pending proposal in store, sorted by
// profile then id (same order as the laptop's listProposals).
func listProposalsLocal(store string) []proposalInfo {
	var out []proposalInfo
	entries, err := os.ReadDir(store)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || !validProfileName(e.Name()) {
			continue
		}
		pdir := filepath.Join(store, e.Name(), "proposals")
		props, err := os.ReadDir(pdir)
		if err != nil {
			continue
		}
		for _, p := range props {
			if p.IsDir() || !validProposalID(trimSuffix(p.Name(), ".toml")) {
				continue
			}
			id := trimSuffix(p.Name(), ".toml")
			out = append(out, proposalInfo{ID: id, Profile: e.Name()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

// writeSeedAtomic replaces a profile's seed with backup + temp+rename, so a
// crash mid-write can't leave a half-written seed for the next summon to
// freeze into cloud-init user_data. Same discipline as pushProfile (laptop)
// but plane-local.
func writeSeedAtomic(store, name string, content []byte) error {
	live := filepath.Join(store, name, "profile.toml")
	ts := time.Now().Format("20060102-150405")
	if _, err := os.Stat(live); err == nil {
		b, err := os.ReadFile(live)
		if err != nil {
			return err
		}
		if err := os.WriteFile(live+".bak."+ts, b, 0o600); err != nil {
			return err
		}
	}
	tmp := live + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, live)
}

// --- handlers ---------------------------------------------------------------

func profileListHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(store)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() || !validProfileName(e.Name()) {
				continue
			}
			if _, err := os.Stat(filepath.Join(store, e.Name(), "profile.toml")); err == nil {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		json.NewEncoder(w).Encode(names)
	}
}

func profileSeedGetHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validProfileName(name) {
			http.Error(w, "invalid profile name", http.StatusBadRequest)
			return
		}
		b, err := os.ReadFile(filepath.Join(store, name, "profile.toml"))
		if os.IsNotExist(err) {
			http.Error(w, "no such profile", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b)
	}
}

func profileSeedPutHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validProfileName(name) {
			http.Error(w, "invalid profile name", http.StatusBadRequest)
			return
		}
		body := make([]byte, r.ContentLength)
		n, _ := r.Body.Read(body)
		body = body[:n]
		if err := validateProfile(string(body)); err != nil {
			http.Error(w, "not a valid profile: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err := writeSeedAtomic(store, name, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "seed updated — takes effect at the next daybox up")
	}
}

func proposalListHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(listProposalsLocal(store))
	}
}

func proposalGetHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validProposalID(id) {
			http.Error(w, "invalid proposal id", http.StatusBadRequest)
			return
		}
		profile, _, prop, ok := findProposalLocal(store, id)
		if !ok {
			http.Error(w, "no such proposal", http.StatusNotFound)
			return
		}
		live, err := os.ReadFile(filepath.Join(store, profile, "profile.toml"))
		if err != nil {
			http.Error(w, "could not read live seed", http.StatusInternalServerError)
			return
		}
		diff := renderProposalDiff(string(live), string(prop))
		json.NewEncoder(w).Encode(struct {
			proposalInfo
			Diff string `json:"diff"`
		}{proposalInfo{ID: id, Profile: profile}, diff})
	}
}

func proposalAcceptHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Confirm") != "yes" {
			http.Error(w, "confirm required (Confirm: yes header)", http.StatusBadRequest)
			return
		}
		id := r.PathValue("id")
		if !validProposalID(id) {
			http.Error(w, "invalid proposal id", http.StatusBadRequest)
			return
		}
		profile, ppath, prop, ok := findProposalLocal(store, id)
		if !ok {
			http.Error(w, "no such proposal", http.StatusNotFound)
			return
		}
		if err := validateProfile(string(prop)); err != nil {
			http.Error(w, "proposal is not a valid profile: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if err := writeSeedAtomic(store, profile, prop); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		os.Remove(ppath)
		fmt.Fprintf(w, "profile '%s' updated — takes effect at the next daybox up\n", profile)
	}
}

func proposalRejectHandler(store string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Confirm") != "yes" {
			http.Error(w, "confirm required (Confirm: yes header)", http.StatusBadRequest)
			return
		}
		id := r.PathValue("id")
		if !validProposalID(id) {
			http.Error(w, "invalid proposal id", http.StatusBadRequest)
			return
		}
		_, ppath, _, ok := findProposalLocal(store, id)
		if !ok {
			http.Error(w, "no such proposal", http.StatusNotFound)
			return
		}
		os.Remove(ppath)
		fmt.Fprintln(w, "proposal rejected")
	}
}
