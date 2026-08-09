package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// profile.go — the plane-side config + profile model, ported from
// bin/daybox's config.local sourcing, derive_profile, and the per-profile
// knob layering. A profile is a whole daybox (own server + volume + state);
// -p <name> selects one, else the 'current_profile' file, else 'default'.
//
// The load-bearing property this preserves: in any loop over profiles (reap,
// status) one profile's knobs must NOT leak into the next. bash snapshotted a
// deployment baseline (DEPLOY_<KNOB>) and derive_profile reset every knob to
// it before layering the profile's own config — a leaked REMOTE_USER once made
// the next profile's probe fail as the wrong user until it was force-reaped.
// deriveProfile rebuilds each profile's values from scratch from the
// deployment config + the profile's overlay, so nothing carries over.

// repoDir is where the control-plane payload (cloud-init template, keys/,
// profile.default.toml, remote/) lives on the plane. init unpacks it to
// ~/daybox (pushTree). Overridable by DAYBOX_REPO_DIR for tests + dev.
func repoDir() string {
	if d := os.Getenv("DAYBOX_REPO_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "daybox")
}

// deployment is the machine-local config parsed once: ~/.config/daybox's
// config.local (KEY=VALUE) plus the deployment-wide defaults. Per-profile
// overlays are layered on top in deriveProfile.
type deployment struct {
	confDir  string // ~/.config/daybox
	stateDir string // ~/.config/daybox/state
	repoDir  string // ~/daybox (the payload)
}

func loadDeployment() *deployment {
	return &deployment{
		confDir:  confDir(),
		stateDir: filepath.Join(confDir(), "state"),
		repoDir:  repoDir(),
	}
}

// profile is one daybox: its resolved knobs + on-disk paths. Every field is
// derived fresh in deriveProfile (no shared mutable state between profiles).
type profile struct {
	dep  *deployment
	name string

	profileConf  string // ~/.config/daybox/profiles/<name>
	profileState string // ~/.config/daybox/state/profiles/<name>
	serverName   string // daybox-<name>
	volumeName   string // daybox-<name>-vol
	knownHosts   string // profileState/known_hosts

	// per-profile knobs (layered: deployment baseline + profile overlay)
	provider         string
	serverType       string
	image            string
	location         string
	volumeSizeGB     int
	remoteUser       string
	reapAfterIdleMin int
	loadBusy         float64
	maxLifetimeHours int
	netUser          string
	gitName          string
	gitEmail         string

	// deployment-wide values a profile does not override
	littleboxIP   string
	netPort       int
	netControlURL string
}

// profileKnobs is the set of knobs a profile's config may override; the
// same set bash reset per-profile. Anything NOT here is deployment-wide.
var profileKnobs = []string{
	"PROVIDER", "SERVER_TYPE", "IMAGE", "LOCATION", "VOLUME_SIZE_GB",
	"REMOTE_USER", "REAP_AFTER_IDLE_MIN", "LOAD_BUSY", "MAX_LIFETIME_HOURS",
	"NET_USER", "GIT_NAME", "GIT_EMAIL",
}

// validProfileName lives in profilecmd.go (shared by the laptop profile verbs
// + the plane-side deriveProfile). Lowercase letters, digits, dashes — the
// name lands in a server name, a volume name, and on-disk paths.

// deriveProfile resolves a profile's knobs + paths from the deployment
// baseline plus the profile's own overlay. Idempotent + leak-free: every
// field is rebuilt from config sources, never inherited from a prior call.
func (d *deployment) deriveProfile(name string) (*profile, error) {
	if !validProfileName(name) {
		return nil, fmt.Errorf("invalid profile '%s' (lowercase letters, digits, dashes)", name)
	}
	// Load BOTH the deployment-wide config (config.local) AND the profile's
	// own overlay fresh, each call — bash re-sourced them per derive_profile,
	// so a config edit takes effect on the next invocation without a
	// process restart (important for the long-lived reaper).
	base := loadConfigFile(configPath())
	overlay := loadConfigFile(filepath.Join(d.confDir, "profiles", name, "config"))
	p := &profile{
		dep:          d,
		name:         name,
		profileConf:  filepath.Join(d.confDir, "profiles", name),
		profileState: filepath.Join(d.stateDir, "profiles", name),
		serverName:   "daybox-" + name,
		volumeName:   "daybox-" + name + "-vol",
		knownHosts:   filepath.Join(d.stateDir, "profiles", name, "known_hosts"),
	}
	if err := os.MkdirAll(p.profileState, 0o755); err != nil {
		return nil, fmt.Errorf("create profile state dir: %w", err)
	}
	// layer: baseline (deployment config) first, then the profile overlay on
	// top, for every overridable knob. lookup overlays first so a profile's
	// own value wins; falling back to the deployment config's value.
	get := func(key string) string {
		if v, ok := overlay.vals[key]; ok && v != "" {
			return v
		}
		return base.get(key, "")
	}
	p.provider = firstNonEmpty(get("PROVIDER"), defaultProvider)
	p.serverType = firstNonEmpty(get("SERVER_TYPE"), defaultServerType)
	p.image = firstNonEmpty(get("IMAGE"), defaultImage)
	p.location = firstNonEmpty(get("LOCATION"), defaultLocation)
	p.remoteUser = firstNonEmpty(get("REMOTE_USER"), defaultRemoteUser)
	p.netUser = firstNonEmpty(get("NET_USER"), defaultNetUser)
	p.gitName = get("GIT_NAME")
	p.gitEmail = get("GIT_EMAIL")
	var err error
	p.volumeSizeGB, err = atoiDefault(get("VOLUME_SIZE_GB"), defaultVolumeSizeGB)
	if err != nil {
		return nil, err
	}
	p.reapAfterIdleMin, err = atoiDefault(get("REAP_AFTER_IDLE_MIN"), defaultReapAfterIdleMin)
	if err != nil {
		return nil, err
	}
	p.maxLifetimeHours, err = atoiDefault(get("MAX_LIFETIME_HOURS"), defaultMaxLifetimeHours)
	if err != nil {
		return nil, err
	}
	p.loadBusy, err = atofDefault(get("LOAD_BUSY"), defaultLoadBusy)
	if err != nil {
		return nil, err
	}
	p.littleboxIP = base.get("LITTLEBOX_IP", "")
	p.netPort, err = atoiDefault(base.get("NET_PORT", strconv.Itoa(defaultNetPort)), defaultNetPort)
	if err != nil {
		return nil, err
	}
	// NET_CONTROL_URL: derived from LITTLEBOX_IP:NET_PORT unless overridden
	// (bash: NET_CONTROL_URL default = http://${LITTLEBOX_IP}:${NET_PORT})
	p.netControlURL = firstNonEmpty(base.get("NET_CONTROL_URL", ""),
		fmt.Sprintf("http://%s:%d", p.littleboxIP, p.netPort))
	if err := validateIdentity(p.littleboxIP, p.remoteUser); err != nil {
		return nil, err
	}
	return p, nil
}

// currentProfile resolves which profile a bare verb targets: the -p flag
// (caller passes name), else the current_profile file, else "default".
// explicit reports whether -p was given (status distinguishes "scoped to
// one profile" from "show every profile" — the default fallback erases it).
func (d *deployment) currentProfile(name string) (string, bool) {
	if name != "" {
		return name, true
	}
	b, err := os.ReadFile(filepath.Join(d.stateDir, "current_profile"))
	if err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, false
		}
	}
	return "default", false
}

// seedFile is the profile's declared profile.toml — what a box CARRIES.
// bash: SEED_FILE_FOR. Inventing a default at summon time would mean a box
// silently carrying something its profile never declared — the drift this
// design removes.
func (p *profile) seedFile() string {
	return filepath.Join(p.dep.confDir, "profiles", p.name, "profile.toml")
}

func (p *profile) volumeIDFile() string    { return filepath.Join(p.profileState, "volume_id") }
func (p *profile) idleTicksFile() string   { return filepath.Join(p.profileState, "idle_ticks") }
func (p *profile) unreachTicksFile() string { return filepath.Join(p.profileState, "unreachable_ticks") }
func (p *profile) agentVersionFile() string { return filepath.Join(p.profileState, "agent_version") }

// volumeID reads the cached volume id, or errors with setup help (bash:
// "profile has no volume yet — run: daybox setup").
func (p *profile) volumeID() (string, error) {
	b, err := os.ReadFile(p.volumeIDFile())
	if err != nil {
		return "", fmt.Errorf("profile '%s' has no volume yet — run: daybox -p %s profile add %s (or: daybox setup)", p.name, p.name, p.name)
	}
	return strings.TrimSpace(string(b)), nil
}

// keysDir: machine-local wins; the repo's keys/ is the fallback. bash:
// KEYS_DIR. Summoned boxes inherit every pubkey here as authorized_keys.
func (d *deployment) keysDir() string {
	local := filepath.Join(d.confDir, "keys")
	if matches, _ := filepath.Glob(filepath.Join(local, "*.pub")); len(matches) > 0 {
		return local
	}
	return filepath.Join(d.repoDir, "keys")
}

// listProfiles: every profile that has been set up (has a state dir).
// bash: list_profiles. Used by status/reap to iterate the fleet.
func (d *deployment) listProfiles() []string {
	base := filepath.Join(d.stateDir, "profiles")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && validProfileName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// tokenFile is where the Hetzner API token lives (chmod 600, never copied
// off the plane). bash: TOKEN_FILE.
func (d *deployment) tokenFile() string { return filepath.Join(d.confDir, "token") }

// agentBin is the daybox-agent linux binary pushed to each summoned box;
// its presence + headscale = net join. bash: NET_AGENT_BIN.
func (d *deployment) agentBin() string { return filepath.Join(d.confDir, "agent", "daybox-agent") }

// amPlane reports whether this process IS the control plane (does the cloud
// work locally) vs a laptop (delegates over ssh). The laptop always has
// CONTROL_HOST configured (written by `daybox init`); the plane does not —
// it IS the host. This is the role gate for every verb that has both a
// laptop (delegate) and plane (local) implementation.
func amPlane() bool { return loadConfig().controlHost() == "" }

// validateIdentity is the injection guard bash ran at load + after every
// profile layer: LITTLEBOX_IP must be a plain IPv4 (a malformed value
// breaks the bootcmd firewall YAML and could leave :22 OPEN), and
// REMOTE_USER must be a plain unix username (it reaches a root shell, a
// remote ssh command line, and the firewall YAML).
var (
	ipv4Re   = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
	unameRe  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func validateIdentity(littleboxIP, remoteUser string) error {
	if littleboxIP != "" && !ipv4Re.MatchString(littleboxIP) {
		return fmt.Errorf("LITTLEBOX_IP must be a plain IPv4 address (got '%s')", littleboxIP)
	}
	if !unameRe.MatchString(remoteUser) {
		return fmt.Errorf("REMOTE_USER must be a plain unix username (got '%s')", remoteUser)
	}
	return nil
}

// loadProvider constructs the named provider for this deployment, wiring
// its token + per-provider state dir. bash: load_provider. An unknown
// provider name is a config error, not a silent skip.
func (d *deployment) loadProvider(name string) (Provider, error) {
	if !validProfileName(name) {
		return nil, fmt.Errorf("invalid provider name '%s'", name)
	}
	switch name {
	case "hetzner", "":
		return newHetznerProvider(d.tokenFile(), filepath.Join(d.stateDir, "providers", "hetzner")), nil
	default:
		return nil, fmt.Errorf("unknown provider '%s' — expected hetzner", name)
	}
}

// deployment-wide defaults (bash: the `: "${VAR:=default}` block).
const (
	defaultProvider        = "hetzner"
	defaultServerType      = "ccx33"
	defaultImage           = "ubuntu-24.04"
	defaultLocation        = "hil"
	defaultVolumeSizeGB    = 50
	defaultRemoteUser      = "dev"
	defaultReapAfterIdleMin = 30
	defaultLoadBusy        = 0.40
	defaultMaxLifetimeHours = 12
	defaultNetUser         = "dev"
	defaultNetPort         = 8080
)

// --- small config helpers ---

// loadConfigFile parses a KEY=VALUE file (config.local or a profile config)
// the same way bash sources it: # comments, trailing comments, surrounding
// quotes stripped. Reuses the config type so behavior is identical to
// loadConfig (the laptop-side parser, already in config.go).
func loadConfigFile(path string) *config {
	c := &config{vals: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		val = strings.Trim(val, `"'`)
		c.vals[key] = val
	}
	return c
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got '%s'", s)
	}
	return n, nil
}

func atofDefault(s string, def float64) (float64, error) {
	if s == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number, got '%s'", s)
	}
	return f, nil
}
