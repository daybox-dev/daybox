package main

import (
	"fmt"
	"os"
	"path/filepath"
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
// An existing file is preserved as .bak (it may be a half-filled example).
func writeLocalConfig(vals [][2]string) error {
	if err := os.MkdirAll(confDir(), 0o755); err != nil {
		return err
	}
	p := configPath()
	if _, err := os.Stat(p); err == nil {
		os.Rename(p, p+".bak")
	}
	var b strings.Builder
	b.WriteString("# daybox deployment config — written by 'daybox init'.\n")
	b.WriteString("# Machine-local, never committed. All knobs: see the README (Configuration).\n")
	for _, kv := range vals {
		v := kv[1]
		if strings.ContainsAny(v, " \t") {
			v = `"` + v + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", kv[0], v)
	}
	return os.WriteFile(p, []byte(b.String()), 0o600)
}
