package main

// The agent relay — the control plane's proposal intake (§1e P4).
//
// This is the first custom daemon on the control plane (a posture shift,
// named in SECURITY.md and the unit file): until now the plane ran
// headscale + sshd + a reaper *timer*, nothing resident that we wrote. It
// exists so a box can PROPOSE a profile change while holding no credential
// that could ever write one: callers are authenticated by net identity
// (WhoIs — the first instance of the broker scaffold docs/secrets.md names
// as the product end-state), a box is bound to the one profile it was
// summoned under (node "daybox-<profile>" → profile), the payload must
// already validate as a profile, and all the relay ever does with it is
// stage an inert file for `daybox profile proposals` on the laptop.
// Approval never happens here. Blast radius of a compromised box:
// proposal spam, bounded by the per-profile pending cap below.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	relayDefaultPort = 4747
	relayMaxProposal = 256 << 10 // a seed is ~KBs; anything bigger is not one
	relayMaxPending  = 32        // per profile — bounds proposal-spam disk use
)

func cmdRelay(p Parsed) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	f := addNetFlags(fs, filepath.Join(confDir(), "relay-tsnet"))
	port := fs.Int("port", relayDefaultPort, "port to listen on (net-side only)")
	store := fs.String("store", filepath.Join(confDir(), "profiles"),
		"profiles dir holding <name>/profile.toml (+ proposals/)")
	// a stable, well-known name: boxes find the relay by peer name, and the
	// machine hostname (the flag's usual default) would be wrong on every
	// deployment
	f.hostname = "daybox-relay"
	fs.Lookup("hostname").DefValue = "daybox-relay"
	fs.Parse(p.Rest())

	if f.authKey() == "" && !fileExists(filepath.Join(f.state, "tailscaled.state")) {
		f.authkey = relaySelfEnroll()
	}
	s := f.server(false) // persistent identity: the relay outlives restarts
	defer s.Close()
	ip := up(s)
	log.Printf("relay up as %q (%s), listening on :%d", f.hostname, ip, *port)

	lc, err := s.LocalClient()
	if err != nil {
		log.Fatalf("local client: %v", err)
	}
	ln, err := s.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	whoisNode := func(ctx context.Context, remoteAddr string) (string, error) {
		who, err := lc.WhoIs(ctx, remoteAddr)
		if err != nil {
			return "", err
		}
		if who.Node == nil {
			return "", fmt.Errorf("no node for %s", remoteAddr)
		}
		if who.Node.ComputedName != "" {
			return who.Node.ComputedName, nil
		}
		return strings.TrimSuffix(who.Node.Name, "."), nil
	}
	log.Fatal(http.Serve(ln, relayMux(whoisNode, *store)))
}

// relayMux is the relay's whole HTTP surface, with identity resolution
// injected so the auth path is testable without a net.
func relayMux(whoisNode func(ctx context.Context, remoteAddr string) (string, error), store string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "daybox relay %s\n", version)
	})
	mux.HandleFunc("POST /propose", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		node, err := whoisNode(ctx, r.RemoteAddr)
		if err != nil {
			// on this net, an unidentifiable peer has no business here
			http.Error(w, "unidentified caller", http.StatusForbidden)
			return
		}
		profile := profileForNode(node)
		if profile == "" {
			log.Printf("propose: refused non-box node %q", node)
			http.Error(w, "only summoned boxes may propose", http.StatusForbidden)
			return
		}
		if !fileExists(filepath.Join(store, profile, "profile.toml")) {
			log.Printf("propose: node %q maps to unknown profile %q", node, profile)
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, relayMaxProposal))
		if err != nil {
			http.Error(w, "proposal too large", http.StatusRequestEntityTooLarge)
			return
		}
		// same validator that gates a laptop edit: garbage dies at the door,
		// not in the review queue
		if err := validateProfile(string(body)); err != nil {
			http.Error(w, "not a valid profile: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		id, err := stageProposal(store, profile, body)
		if err != nil {
			log.Printf("propose: %s: %v", profile, err)
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		log.Printf("propose: staged %s from node %q", id, node)
		fmt.Fprintf(w, "%s\nstaged for review — the laptop decides: daybox profile proposals\n", id)
	})
	return mux
}

// profileForNode maps a net node to the one profile it may propose into:
// summoned boxes are named daybox-<profile> (bin/daybox: SERVER_NAME).
// Anything else — laptops, the plane, the relay itself — maps to nothing.
func profileForNode(node string) string {
	base := strings.ToLower(strings.Split(node, ".")[0])
	p := strings.TrimPrefix(base, "daybox-")
	if p == base || !validProfileName(p) {
		return ""
	}
	return p
}

// stageProposal writes the (already validated) proposal as an inert file
// the laptop reviews. O_EXCL naming: two proposals in the same second get
// distinct ids, never a silent overwrite.
func stageProposal(store, profile string, body []byte) (string, error) {
	dir := filepath.Join(store, profile, "proposals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	pending := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".toml") {
			pending++
		}
	}
	if pending >= relayMaxPending {
		return "", fmt.Errorf("%d proposals already pending for '%s' — review them first", pending, profile)
	}
	ts := time.Now().UTC().Format("20060102-150405")
	for n := 0; ; n++ {
		id := fmt.Sprintf("%s-%s", ts, profile)
		if n > 0 {
			id = fmt.Sprintf("%s.%d", id, n)
		}
		fd, err := os.OpenFile(filepath.Join(dir, id+".toml"),
			os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := fd.Write(body)
		cerr := fd.Close()
		if werr != nil || cerr != nil {
			os.Remove(filepath.Join(dir, id+".toml"))
			return "", fmt.Errorf("write proposal: %v", werr)
		}
		return id, nil
	}
}

// relaySelfEnroll mints the relay's one-time join key. Only possible where
// the relay is meant to run — on the control plane, next to headscale; a
// first start anywhere else fails with instructions rather than a nod.
func relaySelfEnroll() string {
	if _, err := exec.LookPath("headscale"); err != nil {
		log.Fatal("relay has no net identity yet and no headscale here to mint one —\n" +
			"  run it on the control plane, or pass -authkey-file")
	}
	netUser := loadConfig().get("NET_USER", "dev")
	out, err := exec.Command("headscale", "users", "list", "-o", "json").Output()
	if err != nil {
		log.Fatalf("headscale users list: %v", err)
	}
	var users []struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
	}
	json.Unmarshal(out, &users)
	uid := ""
	for _, u := range users {
		if u.Name == netUser {
			uid = u.ID.String()
		}
	}
	if uid == "" {
		log.Fatalf("no headscale user %q (NET_USER in config.local)", netUser)
	}
	out, err = exec.Command("headscale", "preauthkeys", "create",
		"--user", uid, "--expiration", "15m").Output()
	if err != nil {
		log.Fatalf("headscale preauthkeys create: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		log.Fatalf("could not parse a key out of headscale's output: %q", out)
	}
	log.Printf("first start: joining the net as %q", "daybox-relay")
	return fields[len(fields)-1]
}
