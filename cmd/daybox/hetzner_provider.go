package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// hetzner_provider.go — the Go port of providers/hetzner.sh. Same five
// primitives, same REST surface (no third-party SDK), same per-provider
// state layout (state/providers/hetzner/). The HTTP transport is an
// injectable interface (httpDoer) so the whole provider is exercised in
// tests against recorded responses — no real cloud, no money.

const hetznerAPI = "https://api.hetzner.cloud/v1"

// httpDoer is the subset of *http.Client the provider uses. A test fake
// implements it to answer canned responses keyed by method+path.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// realHTTP is the default transport: a client with the same 60s max and
// 10s connect semantics as the bash curl flags.
var realHTTP httpDoer = &http.Client{Timeout: 60 * time.Second}

// hetznerProvider implements Provider against the Hetzner Cloud REST API.
// The token is read from tokenFile on every call (the bash `api()` did the
// same via $(cat "$TOKEN_FILE")) so a re-issued token takes effect without
// a process restart — important for the long-lived reaper.
type hetznerProvider struct {
	tokenFile string
	stateDir  string // providers/hetzner state (ssh_key_names.json cache)
	do        httpDoer
}

// newHetznerProvider constructs a provider that reads its token from
// tokenFile and caches resolved ssh-key names under stateDir. stateDir is
// created on demand.
func newHetznerProvider(tokenFile, stateDir string) *hetznerProvider {
	return &hetznerProvider{tokenFile: tokenFile, stateDir: stateDir, do: realHTTP}
}

func (h *hetznerProvider) Name() string { return "hetzner" }

func (h *hetznerProvider) HasCredentials() bool {
	_, err := os.Stat(h.tokenFile)
	return err == nil
}

func (h *hetznerProvider) CheckCredentials() error {
	if _, err := os.Stat(h.tokenFile); err != nil {
		return fmt.Errorf("no API token at %s\n  Create one: Hetzner Cloud Console > project > Security > API tokens (read/write),\n  then:  install -m 600 /dev/stdin %s   (paste token, ctrl-d)", h.tokenFile, h.tokenFile)
	}
	return nil
}

// token reads the credential file fresh each call (supports rotation).
func (h *hetznerProvider) token() (string, error) {
	b, err := os.ReadFile(h.tokenFile)
	if err != nil {
		return "", fmt.Errorf("cannot read Hetzner token at %s: %w", h.tokenFile, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// hetznerAPIError mirrors the `.error` object Hetzner returns on API
// failures. A non-nil Code means the request reached the API but was
// rejected (quota, bad name, locked volume, …).
type hetznerAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// api is the faithful port of bash `api()`: do the request, then surface an
// API-level error via the `.error` object (bash checked `.error.code`).
// Transport failures (no network, DNS, timeout) return their own error.
// `out` (when non-nil) receives the parsed response body.
func (h *hetznerProvider) api(method, path string, body any, out any) error {
	tok, err := h.token()
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s body: %w", method, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, hetznerAPI+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.do.Do(req)
	if err != nil {
		return fmt.Errorf("curl failed: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	// An API-level error is carried in `.error`, independent of HTTP status
	// (bash relied on `.error.code` presence). Surface it with the API's own
	// message so the caller sees e.g. "quota_exceeded" by name.
	var he struct {
		Error *hetznerAPIError `json:"error"`
	}
	if json.Unmarshal(respBody, &he) == nil && he.Error != nil && he.Error.Code != "" {
		return fmt.Errorf("API %s %s: %s (%s)", method, path, he.Error.Message, he.Error.Code)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parse %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// hzServerRaw is the raw Hetzner server object; toServer normalizes it to
// the contract's Server (id stringified, type from server_type.name).
type hzServerRaw struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Created   string `json:"created"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (s hzServerRaw) toServer() Server {
	return Server{
		ID:      strconv.FormatInt(s.ID, 10),
		Name:    s.Name,
		IP:      s.PublicNet.IPv4.IP,
		Status:  s.Status,
		Created: s.Created,
		Type:    s.ServerType.Name,
	}
}

func (h *hetznerProvider) Probe(name string) (*Server, error) {
	var got struct {
		Servers []hzServerRaw `json:"servers"`
	}
	if err := h.api("GET", "/servers?name="+name, nil, &got); err != nil {
		return nil, err
	}
	if len(got.Servers) == 0 {
		return nil, nil // the literal `null` of the bash contract
	}
	s := got.Servers[0].toServer()
	return &s, nil
}

func (h *hetznerProvider) Summon(name, serverType, image, location, volumeID, userData string) (Server, error) {
	keys, err := h.sshKeyNames()
	if err != nil {
		return Server{}, err
	}
	vid, err := strconv.ParseInt(volumeID, 10, 64)
	if err != nil {
		return Server{}, fmt.Errorf("volume id %q is not numeric: %w", volumeID, err)
	}
	body := map[string]any{
		"name": name, "server_type": serverType, "image": image, "location": location,
		"ssh_keys": keys, "volumes": []int64{vid}, "automount": false,
		"user_data": userData, "labels": map[string]string{"role": "daybox"},
	}
	var created struct {
		Server hzServerRaw `json:"server"`
	}
	if err := h.api("POST", "/servers", body, &created); err != nil {
		return Server{}, err
	}
	// bash guard: a die inside the $(...) subshell exited only that subshell
	// (bash doesn't carry errexit into command substitutions) and the
	// summon once proceeded with an empty id/ip. Here api returns a real
	// error, but keep the defensive check: never announce a box that was
	// never created.
	if created.Server.ID == 0 || created.Server.PublicNet.IPv4.IP == "" {
		return Server{}, fmt.Errorf("server create failed (no id/ip in response) — nothing was created")
	}
	say("server %d created, ip %s — waiting for it to run", created.Server.ID, created.Server.PublicNet.IPv4.IP)

	// wait for running (60 attempts @ 2s, matching the bash poll).
	id := created.Server.ID
	for i := 0; i < 60; i++ {
		var one struct {
			Server hzServerRaw `json:"server"`
		}
		if err := h.api("GET", fmt.Sprintf("/servers/%d", id), nil, &one); err != nil {
			return Server{}, err
		}
		if one.Server.Status == "running" {
			return one.Server.toServer(), nil
		}
		time.Sleep(2 * time.Second)
	}
	return Server{}, fmt.Errorf("server %d never reached 'running'", id)
}

func (h *hetznerProvider) Reap(id string) error {
	return h.api("DELETE", "/servers/"+id, nil, nil)
}

func (h *hetznerProvider) VolumeEnsure(name string, sizeGB int, location string) (string, error) {
	var got struct {
		Volumes []struct {
			ID int64 `json:"id"`
		} `json:"volumes"`
	}
	if err := h.api("GET", "/volumes?name="+name, nil, &got); err != nil {
		return "", err
	}
	if len(got.Volumes) > 0 {
		say("volume '%s' already exists (id %d)", name, got.Volumes[0].ID)
		return strconv.FormatInt(got.Volumes[0].ID, 10), nil
	}
	say("creating %dGB volume '%s' in %s (pre-formatted ext4)", sizeGB, name, location)
	body := map[string]any{
		"name": name, "size": sizeGB, "location": location,
		"format": "ext4", "labels": map[string]string{"role": "daybox"},
	}
	var created struct {
		Volume struct {
			ID int64 `json:"id"`
		} `json:"volume"`
	}
	if err := h.api("POST", "/volumes", body, &created); err != nil {
		return "", err
	}
	if created.Volume.ID == 0 {
		return "", fmt.Errorf("volume create returned no id")
	}
	return strconv.FormatInt(created.Volume.ID, 10), nil
}

func (h *hetznerProvider) VolumeAttachedTo(id string) (string, error) {
	var got struct {
		Volume struct {
			Server *int64 `json:"server"`
		} `json:"volume"`
	}
	if err := h.api("GET", "/volumes/"+id, nil, &got); err != nil {
		return "", err
	}
	if got.Volume.Server == nil {
		return "", nil
	}
	return strconv.FormatInt(*got.Volume.Server, 10), nil
}

func (h *hetznerProvider) VolumeDetach(id string) error {
	return h.api("POST", "/volumes/"+id+"/actions/detach", map[string]any{}, nil)
}

func (h *hetznerProvider) VolumeSize(id string) (int, error) {
	var got struct {
		Volume struct {
			Size int `json:"size"`
		} `json:"volume"`
	}
	if err := h.api("GET", "/volumes/"+id, nil, &got); err != nil {
		return 0, err
	}
	if got.Volume.Size == 0 {
		return 0, fmt.Errorf("volume %s has no size?", id)
	}
	return got.Volume.Size, nil
}

func (h *hetznerProvider) VolumeRename(id, newName string) error {
	return h.api("PUT", "/volumes/"+id, map[string]string{"name": newName}, nil)
}

func (h *hetznerProvider) VolumeDelete(id string) error {
	return h.api("DELETE", "/volumes/"+id, nil, nil)
}

func (h *hetznerProvider) UserDataMaxBytes() int { return hetznerUserDataCap }

func (h *hetznerProvider) PriceHourly(serverType, location string) string {
	var got struct {
		Types []struct {
			Prices []struct {
				Location   string `json:"location"`
				PriceHourly struct {
					Gross string `json:"gross"`
				} `json:"price_hourly"`
			} `json:"prices"`
		} `json:"server_types"`
	}
	if err := h.api("GET", "/server_types?name="+serverType, nil, &got); err != nil || len(got.Types) == 0 {
		return ""
	}
	for _, p := range got.Types[0].Prices {
		if p.Location == location {
			// bash: cut -c1-6 — first 6 chars of the gross string. Hourly
			// prices are <€10 so this never truncates a meaningful digit.
			return firstN(p.PriceHourly.Gross, 6)
		}
	}
	return ""
}

// firstN returns the first n bytes of s (bash `cut -c1-6`).
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// PrepareSSHKeys registers every *.pub in dir (if its fingerprint is new)
// and caches the resolved names. bash: provider_prepare_ssh_keys. A key may
// already exist under a different name — reference by resolved name, not
// filename. Two files can resolve to one key: dedupe for the API.
func (h *hetznerProvider) PrepareSSHKeys(dir string) error {
	entries, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("put authorized pubkeys in %s/<name>.pub first", dir)
	}
	if err := os.MkdirAll(h.stateDir, 0o755); err != nil {
		return fmt.Errorf("create provider state dir: %w", err)
	}
	// adopt the pre-namespacing cache so an existing deployment doesn't
	// re-register every key on the next setup (bash did the same mv).
	legacy := filepath.Join(filepath.Dir(h.stateDir), "ssh_key_names.json")
	cache := filepath.Join(h.stateDir, "ssh_key_names.json")
	if _, err := os.Stat(legacy); err == nil {
		if _, err := os.Stat(cache); err != nil {
			if err := os.Rename(legacy, cache); err != nil {
				return fmt.Errorf("adopt legacy key cache: %w", err)
			}
		}
	}
	var names []string
	for _, f := range entries {
		name := strings.TrimSuffix(filepath.Base(f), ".pub")
		fp, err := md5Fingerprint(f)
		if err != nil {
			return err
		}
		existing, err := h.findSSHKeyName(fp)
		if err != nil {
			return err
		}
		if existing != "" {
			say("ssh key '%s' already registered as '%s'", name, existing)
			names = append(names, existing)
			continue
		}
		say("registering ssh key '%s'", name)
		if err := h.registerSSHKey(name, f); err != nil {
			return err
		}
		names = append(names, name)
	}
	// dedupe + persist as a JSON array of strings
	names = dedupStrings(names)
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return os.WriteFile(cache, b, 0o644)
}

func (h *hetznerProvider) findSSHKeyName(fp string) (string, error) {
	var got struct {
		Keys []struct {
			Name string `json:"name"`
		} `json:"ssh_keys"`
	}
	if err := h.api("GET", "/ssh_keys?fingerprint="+fp, nil, &got); err != nil {
		return "", err
	}
	if len(got.Keys) == 0 {
		return "", nil
	}
	return got.Keys[0].Name, nil
}

func (h *hetznerProvider) registerSSHKey(name, pubkeyPath string) error {
	b, err := os.ReadFile(pubkeyPath)
	if err != nil {
		return err
	}
	return h.api("POST", "/ssh_keys", map[string]string{
		"name":       name,
		"public_key": strings.TrimSpace(string(b)),
	}, nil)
}

// sshKeyNames returns the resolved key names PrepareSSHKeys cached. A
// summon without them is a setup gap, not a silent skip.
func (h *hetznerProvider) sshKeyNames() ([]string, error) {
	cache := filepath.Join(h.stateDir, "ssh_key_names.json")
	b, err := os.ReadFile(cache)
	if err != nil {
		return nil, fmt.Errorf("no resolved ssh keys — run: daybox setup")
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cache, err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no resolved ssh keys — run: daybox setup")
	}
	return names, nil
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
