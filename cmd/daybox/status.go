package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// status.go — the plane-side `status` (bash status_one + cmd_status +
// net_table). The whole deployment in one command: every profile's box
// (uptime, price, counters, agent version) plus the net table. -p scopes
// to a single profile's block.
//
// A cap you cannot see is a cap that ambushes you, so the lifetime cap is
// always printed — including when it is disabled.

// statusOne prints one profile's block to w. A missing box is not an error;
// a broken profile is reported and the loop continues (bash: the subshell).
func statusOne(p *profile, prov Provider, w io.Writer) {
	fmt.Fprintf(w, "profile '%s':\n", p.name)
	s, err := prov.Probe(p.serverName)
	if err != nil {
		fmt.Fprintf(w, "  status check failed: %v\n", err)
		return
	}
	if s == nil {
		fmt.Fprintln(w, "  no box running (volume + snapshot state persist; billing: volume only)")
		return
	}
	price := prov.PriceHourly(s.Type, p.location)
	priceStr := priceOrQ(price)
	fmt.Fprintf(w, "  big box: %s  id=%s  %s  %s\n", s.Name, s.ID, s.Type, s.Status)
	fmt.Fprintf(w, "  ip: %s\n", s.IP)
	fmt.Fprintf(w, "  created: %s   (~€%s/h gross)\n", s.Created, priceStr)

	// fleet version visibility: the box's summoned agent vs the plane's
	// current, so a stale box is obvious after `daybox upgrade` (boxes keep
	// their summoned version until reaped; down+up rotates one).
	boxAgent := readFileTrim(p.agentVersionFile())
	planeAgent := agentVersion()
	if boxAgent != "" {
		if boxAgent == planeAgent {
			fmt.Fprintf(w, "  agent: %s  (current)\n", boxAgent)
		} else {
			fmt.Fprintf(w, "  agent: %s  (plane: %s — rotate with: daybox down + up)\n", boxAgent, planeAgent)
		}
	} else {
		fmt.Fprintf(w, "  agent: ?  (plane: %s)\n", planeAgent)
	}
	fmt.Fprintln(w, "  ingress: locked down (public ssh closed; net + control plane only)")
	idle := readInt(p.idleTicksFile())
	fmt.Fprintf(w, "  idle ticks: %d/%d (reaper checks every 5min)\n", idle, reapTicks(p.reapAfterIdleMin))

	if ageMin, ok := boxAgeMinutes(s.Created); ok {
		spent := "?"
		if price != "" {
			spent = fmt.Sprintf("%.2f", priceFloat(price)*float64(ageMin)/60)
		}
		fmt.Fprintf(w, "  age: %dmin   spent so far: ~€%s\n", ageMin, spent)
		if p.maxLifetimeHours > 0 {
			left := p.maxLifetimeHours*60 - ageMin
			if left < 0 {
				left = 0
			}
			fmt.Fprintf(w, "  lifetime cap: %dh — %dmin left, then force-reaped even if busy\n", p.maxLifetimeHours, left)
		} else {
			fmt.Fprintln(w, "  lifetime cap: DISABLED (MAX_LIFETIME_HOURS=0) — only the idle reaper stops the meter")
		}
	}
}

// statusRun is the plane-side `status` (bash cmd_status): every profile's
// block (or one, with -p) plus the net table.
func statusRun(dep *deployment, w io.Writer, explicitName string) {
	if explicitName != "" {
		p, err := dep.deriveProfile(explicitName)
		if err != nil {
			fmt.Fprintf(w, "profile '%s': %v\n", explicitName, err)
			return
		}
		prov, err := dep.loadProvider(p.provider)
		if err != nil {
			fmt.Fprintf(w, "profile '%s': %v\n", p.name, err)
			return
		}
		statusOne(p, prov, w)
		return
	}
	profiles := dep.listProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(w, "no profiles set up yet — run: daybox init")
	} else {
		for _, name := range profiles {
			// a broken profile config or missing creds must not hide the
			// other profiles' status (bash: the subshell + || log).
			p, err := dep.deriveProfile(name)
			if err != nil {
				fmt.Fprintf(w, "profile '%s': %v\n", name, err)
				continue
			}
			prov, err := dep.loadProvider(p.provider)
			if err != nil {
				fmt.Fprintf(w, "profile '%s': %v\n", name, err)
				continue
			}
			if !prov.HasCredentials() {
				fmt.Fprintf(w, "profile '%s': no credentials for provider '%s'\n", name, p.provider)
				continue
			}
			statusOne(p, prov, w)
		}
	}
	fmt.Fprintln(w)
	if netEnabled(dep) {
		fmt.Fprintln(w, "net members:")
		netTable(dep, w)
	} else {
		fmt.Fprintln(w, "net: DOWN here (headscale/agent missing) — 'daybox up' will refuse")
	}
}

// netEnabled: headscale installed + the agent binary present (bash: net_enabled).
func netEnabled(d *deployment) bool {
	if _, err := exec.LookPath("headscale"); err != nil {
		return false
	}
	if info, err := os.Stat(d.agentBin()); err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	return true
}

// netTable prints the net members table (bash: net_table). Columns: ID,
// NAME, ADDRESS, USER, STATE, KIND, LASTSEEN.
func netTable(d *deployment, w io.Writer) {
	nodes, err := d.headscaleNodesJSON()
	if err != nil {
		fmt.Fprintf(w, "  (net unreachable: %v)\n", err)
		return
	}
	fmt.Fprintln(w, tabJoin("ID", "NAME", "ADDRESS", "USER", "STATE", "KIND", "LASTSEEN"))
	var list []netNode
	if err := json.Unmarshal([]byte(nodes), &list); err != nil {
		fmt.Fprintf(w, "  (net parse error: %v)\n", err)
		return
	}
	for _, n := range list {
		addr := "-"
		if len(n.IPAddresses) > 0 {
			addr = n.IPAddresses[0]
		}
		user := "-"
		if n.User.Name != "" {
			user = n.User.Name
		}
		state := "offline"
		if n.Online || n.Connected {
			state = "online"
		}
		kind := "device"
		if n.ephemeral() {
			kind = "ephemeral"
		}
		lastSeen := "-"
		if n.LastSeen.Seconds > 0 {
			lastSeen = time.Unix(n.LastSeen.Seconds, 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintln(w, tabJoin(fmt.Sprintf("%v", n.ID), n.GivenName, addr, user, state, kind, lastSeen))
	}
}

type netNode struct {
	ID          any      `json:"id"`
	GivenName   string   `json:"given_name"`
	Name        string   `json:"name"`
	IPAddresses []string `json:"ip_addresses"`
	Online      bool     `json:"online"`
	Connected   bool     `json:"connected"`
	User        struct {
		Name string `json:"name"`
	} `json:"user"`
	PreAuthKey *struct {
		Ephemeral bool `json:"ephemeral"`
	} `json:"pre_auth_key"`
	Ephemeral *bool `json:"ephemeral"`
	LastSeen  struct {
		Seconds int64 `json:"seconds"`
	} `json:"last_seen"`
}

func (n netNode) ephemeral() bool {
	if n.PreAuthKey != nil && n.PreAuthKey.Ephemeral {
		return true
	}
	if n.Ephemeral != nil {
		return *n.Ephemeral
	}
	return false
}

// --- small helpers ---

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func agentVersion() string {
	b, err := exec.Command(loadDeployment().agentBin(), "version").Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

func reapTicks(idleMin int) int {
	n := idleMin / 5
	if n < 1 {
		n = 1
	}
	return n
}

// priceFloat parses a "0.2259" price string; 0 on error.
func priceFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// tabJoin joins fields with a tab (bash: column -t -s$'\t' aligns them; we
// emit tab-separated and let the terminal / a column(1) align downstream).
func tabJoin(fields ...string) string { return strings.Join(fields, "\t") }
