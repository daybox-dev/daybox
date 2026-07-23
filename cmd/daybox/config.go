package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// config.local is shared with the bash tooling, so it stays a flat
// KEY=VALUE file that bash can source. We parse the subset we write.

func confDir() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "daybox")
}

func configPath() string { return filepath.Join(confDir(), "config.local") }

type config struct{ vals map[string]string }

func loadConfig() *config {
	c := &config{vals: map[string]string{}}
	b, err := os.ReadFile(configPath())
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
		// strip a trailing comment, then surrounding quotes
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		val = strings.Trim(val, `"'`)
		c.vals[key] = val
	}
	return c
}

func (c *config) get(key, def string) string {
	if v, ok := c.vals[key]; ok && v != "" {
		return v
	}
	return def
}

// controlHost is the ssh destination of the control plane.
func (c *config) controlHost() string {
	if v := os.Getenv("DAYBOX_CONTROL_HOST"); v != "" {
		return v
	}
	return c.get("CONTROL_HOST", "")
}

// controlURL is the coordination server (headscale) endpoint.
// The documented placeholder IP counts as unset.
func (c *config) controlURL() string {
	if v := os.Getenv("DAYBOX_CONTROL"); v != "" {
		return v
	}
	// NET_URL in config.local overrides the derived http URL entirely —
	// the path to headscale-behind-TLS without a code change. (Control
	// traffic is Noise-encrypted either way; see the README.)
	if v := c.get("NET_URL", ""); v != "" {
		return v
	}
	ip := c.get("LITTLEBOX_IP", "")
	if ip == "" || ip == "203.0.113.10" {
		return ""
	}
	return fmt.Sprintf("http://%s:%s", ip, c.get("NET_PORT", "8080"))
}

// writeLocalConfig rewrites config.local with the values init gathered.
// Every OTHER key already present is carried forward verbatim: init owns the
// deployment's identity (CONTROL_HOST, LITTLEBOX_IP, git identity), not its
// knobs. Dropping the rest once clobbered NET_USER, so every later enroll
// fell back to headscale user "dev" and died — and the same clobber applies
// to PROVIDER, REMOTE_USER, reaper tuning, NET_URL/NET_PORT. Re-running init
// must heal drift, not reset the deployment to defaults.
// An existing file is preserved as .bak (it may be a half-filled example).
func writeLocalConfig(vals [][2]string) error {
	if err := os.MkdirAll(confDir(), 0o755); err != nil {
		return err
	}
	old := loadConfig()
	p := configPath()
	if _, err := os.Stat(p); err == nil {
		os.Rename(p, p+".bak")
	}
	quote := func(v string) string {
		if strings.ContainsAny(v, " \t") {
			return `"` + v + `"`
		}
		return v
	}
	var b strings.Builder
	b.WriteString("# daybox deployment config — written by 'daybox init'.\n")
	b.WriteString("# Machine-local, never committed. All knobs: see the README (Configuration).\n")
	written := map[string]bool{}
	for _, kv := range vals {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], quote(kv[1]))
		written[kv[0]] = true
	}
	var keep []string
	for k := range old.vals {
		if !written[k] {
			keep = append(keep, k)
		}
	}
	sort.Strings(keep)
	if len(keep) > 0 {
		b.WriteString("# carried forward from the previous config.local:\n")
		for _, k := range keep {
			fmt.Fprintf(&b, "%s=%s\n", k, quote(old.vals[k]))
		}
	}
	return os.WriteFile(p, []byte(b.String()), 0o600)
}
