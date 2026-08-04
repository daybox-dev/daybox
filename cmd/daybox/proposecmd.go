package main

// `daybox profile propose` — the box side of §1e (P5).
//
// A box legitimately discovers what its profile should declare (a tool
// installed mid-session, a pin that lags what's actually running) but must
// never be able to change the profile itself. This verb reads the seed the
// box was summoned with (/opt/daybox/profile.toml), applies ONLY detected
// [tools]/[packages] drift as textual edits — comments and everything else
// carry through byte-identical, which the semantic check below enforces —
// and submits the whole rewritten file to the control plane's relay, where
// it stages inert until the laptop reviews the diff. [setup]/[persist] are
// never touched by auto-detection; under full-rewrite review they'd show
// flagged in the diff if they ever were.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// substratePackages are installed by daybox itself on every box —
// present in apt's history without any profile declaring them.
var substratePackages = map[string]bool{"tmux": true, "git": true, "ufw": true}

type drift struct {
	toolAdds  map[string]string // tool → pin to add under [tools]
	toolBumps map[string]string // tool → new pin (profile pin is stale)
	pkgAdds   []string          // packages to append to packages = [...]
}

func (d drift) empty() bool {
	return len(d.toolAdds) == 0 && len(d.toolBumps) == 0 && len(d.pkgAdds) == 0
}

func cmdProfilePropose(args []string) {
	fs := flag.NewFlagSet("propose", flag.ExitOnError)
	seedPath := fs.String("seed", "/opt/daybox/profile.toml", "the seed this box was summoned with")
	relayURL := fs.String("relay", fmt.Sprintf("http://127.0.0.1:%d", relayDefaultPort),
		"relay endpoint (the agent proxies localhost to the control plane's relay)")
	dryRun := fs.Bool("dry-run", false, "show what would be proposed, submit nothing")
	fs.Parse(args)

	raw, err := os.ReadFile(*seedPath)
	if err != nil {
		log.Fatalf("no seed at %s — is this a summoned box? (%v)", *seedPath, err)
	}
	current := string(raw)
	var seed map[string]any
	if _, err := toml.Decode(current, &seed); err != nil {
		log.Fatalf("seed does not parse: %v", err)
	}

	d := detectDrift(seed, miseTools(), aptSessionPackages())
	if d.empty() {
		say("nothing to propose — the profile already declares everything detected")
		return
	}

	proposed, err := applyDrift(current, d)
	if err != nil {
		log.Fatalf("could not apply detected drift: %v", err)
	}
	if err := verifyDriftOnly(current, proposed, d); err != nil {
		// the textual edit did something the detector didn't intend —
		// refuse to submit rather than propose a surprise
		log.Fatalf("rewrite verification failed: %v", err)
	}

	say("proposing to profile (diff as the laptop will review it):")
	fmt.Print(renderProposalDiff(current, proposed))
	if *dryRun {
		say("dry run — nothing submitted")
		return
	}

	resp, err := http.Post(*relayURL+"/propose", "application/toml", strings.NewReader(proposed))
	if err != nil {
		log.Fatalf("relay unreachable (%v) — is daybox-relay enabled on the control plane?", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("relay refused the proposal (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
}

// detectDrift compares what the box actually carries against what the seed
// declares. Only [tools] and [packages] — never [setup]/[persist].
func detectDrift(seed map[string]any, installedTools map[string]string, sessionPkgs []string) drift {
	d := drift{toolAdds: map[string]string{}, toolBumps: map[string]string{}}

	declaredTools := map[string]string{}
	if tt, ok := seed["tools"].(map[string]any); ok {
		for k, v := range tt {
			if s, ok := v.(string); ok { // skips the [tools.settings] subtable
				declaredTools[k] = s
			}
		}
	}
	for tool, version := range installedTools {
		switch pin, ok := declaredTools[tool]; {
		case !ok:
			d.toolAdds[tool] = version
		case pin != version:
			d.toolBumps[tool] = version
		}
	}

	declaredPkgs := map[string]bool{}
	if pp, ok := seed["packages"].([]any); ok {
		for _, p := range pp {
			if s, ok := p.(string); ok {
				declaredPkgs[s] = true
			}
		}
	}
	for _, p := range sessionPkgs {
		if !declaredPkgs[p] && !substratePackages[p] {
			d.pkgAdds = append(d.pkgAdds, p)
		}
	}
	sort.Strings(d.pkgAdds)
	return d
}

// miseTools asks mise what the box's global config carries (a `mise use -g`
// mid-session lands there). Requested version = the pin a profile would
// declare. No mise, or no tools: nothing to detect.
func miseTools() map[string]string {
	out, err := exec.Command("mise", "ls", "--global", "--json").Output()
	if err != nil {
		return nil
	}
	var ls map[string][]struct {
		Version          string `json:"version"`
		RequestedVersion string `json:"requested_version"`
	}
	if err := json.Unmarshal(out, &ls); err != nil {
		return nil
	}
	tools := map[string]string{}
	for name, installs := range ls {
		for _, in := range installs {
			v := in.RequestedVersion
			if v == "" || v == "latest" {
				v = in.Version // pin what's actually there, not a moving target
			}
			if v != "" {
				tools[name] = v
			}
		}
	}
	return tools
}

// aptSessionPackages: packages apt-installed on THIS box since boot (the
// root disk is rebuilt each summon, so apt's history spans exactly this
// box's life) that are also marked manual. `apt-mark showmanual` alone
// would drag in the whole base image's manual set — dozens of packages no
// one chose — and a noisy first proposal trains exactly the rubber-stamping
// the review is designed to prevent.
func aptSessionPackages() []string {
	hist, err := os.ReadFile("/var/log/apt/history.log")
	if err != nil {
		return nil
	}
	installed := parseAptHistory(string(hist))
	if len(installed) == 0 {
		return nil
	}
	out, err := exec.Command("apt-mark", "showmanual").Output()
	if err != nil {
		return nil
	}
	manual := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		manual[strings.TrimSpace(l)] = true
	}
	var pkgs []string
	for _, p := range installed {
		if manual[p] {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

// parseAptHistory pulls explicitly-named packages out of apt install
// command lines ("Commandline: apt-get install -y jq ripgrep").
func parseAptHistory(hist string) []string {
	seen := map[string]bool{}
	var pkgs []string
	for _, line := range strings.Split(hist, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Commandline:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Commandline:"))
		inst := -1
		for i, f := range fields {
			if f == "install" {
				inst = i
				break
			}
		}
		if inst < 0 {
			continue
		}
		for _, f := range fields[inst+1:] {
			if strings.HasPrefix(f, "-") || f == "" {
				continue
			}
			if !seen[f] {
				seen[f] = true
				pkgs = append(pkgs, f)
			}
		}
	}
	return pkgs
}

// tomlKey renders a [tools] key the way a seed writes it: bare when it can
// be, quoted when it can't ("npm:@scope/pkg").
func tomlKey(k string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(k) {
		return k
	}
	return `"` + k + `"`
}

// applyDrift edits the seed TEXT — a proposal is a full rewrite reviewed by
// diff, so comments and layout must survive; regenerating from the parse
// would shred the file the user owns.
func applyDrift(src string, d drift) (string, error) {
	lines := strings.Split(src, "\n")

	// pin bumps: rewrite the version string on the pin's own line
	for tool, version := range d.toolBumps {
		re := regexp.MustCompile(`^(\s*(?:"` + regexp.QuoteMeta(tool) + `"|` + regexp.QuoteMeta(tool) + `)\s*=\s*")[^"]*(")`)
		found := false
		for i, l := range lines {
			if re.MatchString(l) {
				lines[i] = re.ReplaceAllString(l, "${1}"+version+"${2}")
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("pin for %q not found as its own line", tool)
		}
	}

	// tool additions: directly under the [tools] header, keeping the
	// section's own pins together (subtables like [tools.settings] follow)
	if len(d.toolAdds) > 0 {
		var adds []string
		for tool := range d.toolAdds {
			adds = append(adds, tool)
		}
		sort.Strings(adds)
		hdr := -1
		for i, l := range lines {
			if strings.TrimSpace(l) == "[tools]" {
				hdr = i
				break
			}
		}
		if hdr < 0 {
			return "", fmt.Errorf("no [tools] section to add pins to")
		}
		var block []string
		for _, tool := range adds {
			block = append(block, tomlKey(tool)+` = "`+d.toolAdds[tool]+`"`)
		}
		lines = append(lines[:hdr+1], append(block, lines[hdr+1:]...)...)
	}

	// package additions: before the closing bracket of packages = [...]
	if len(d.pkgAdds) > 0 {
		src = strings.Join(lines, "\n")
		open := regexp.MustCompile(`(?m)^packages\s*=\s*\[`)
		loc := open.FindStringIndex(src)
		if loc == nil {
			return "", fmt.Errorf("no packages = [...] to add to")
		}
		close := strings.Index(src[loc[1]:], "]")
		if close < 0 {
			return "", fmt.Errorf("packages array never closes")
		}
		at := loc[1] + close
		if nl := strings.LastIndex(src[loc[1]:at], "\n"); nl >= 0 {
			// multiline array: new entries as their own lines before the
			// bracket — first making sure the current last entry carries
			// the comma the added lines will need
			body := src[loc[1] : loc[1]+nl]
			if last := lastArrayEntryEnd(body); last >= 0 && body[last] != ',' {
				p := loc[1] + last + 1
				src = src[:p] + "," + src[p:]
				at++
			}
			insert := ""
			for _, p := range d.pkgAdds {
				insert += `  "` + p + "\",\n"
			}
			at = loc[1] + strings.LastIndex(src[loc[1]:at], "\n") + 1
			return src[:at] + insert + src[at:], nil
		}
		// single-line array: extend it in place
		sep := ", "
		if strings.TrimSpace(src[loc[1]:at]) == "" {
			sep = ""
		}
		var quoted []string
		for _, p := range d.pkgAdds {
			quoted = append(quoted, `"`+p+`"`)
		}
		return src[:at] + sep + strings.Join(quoted, ", ") + src[at:], nil
	}

	return strings.Join(lines, "\n"), nil
}

// lastArrayEntryEnd finds the index of the last content character in an
// array body, ignoring per-line # comments — where a missing trailing
// comma would sit.
func lastArrayEntryEnd(body string) int {
	end := -1
	off := 0
	for _, line := range strings.Split(body, "\n") {
		content := line
		if i := strings.Index(content, "#"); i >= 0 {
			content = content[:i]
		}
		if t := strings.TrimRight(content, " \t"); t != "" {
			end = off + len(t) - 1
		}
		off += len(line) + 1
	}
	return end
}

// verifyDriftOnly proves the textual rewrite means exactly what the
// detector intended: the additions and bumps are present, and every other
// part of the seed parses identical. This is the guard that makes "the box
// only ever touches [tools]/[packages]" a checked property, not a habit.
func verifyDriftOnly(current, proposed string, d drift) error {
	var before, after map[string]any
	if _, err := toml.Decode(current, &before); err != nil {
		return err
	}
	if _, err := toml.Decode(proposed, &after); err != nil {
		return fmt.Errorf("rewritten seed does not parse: %v", err)
	}
	if err := validateProfile(proposed); err != nil {
		return err
	}

	afterTools, _ := after["tools"].(map[string]any)
	for tool, v := range d.toolAdds {
		if afterTools[tool] != v {
			return fmt.Errorf("addition %s = %q did not land", tool, v)
		}
	}
	for tool, v := range d.toolBumps {
		if afterTools[tool] != v {
			return fmt.Errorf("bump %s → %q did not land", tool, v)
		}
	}
	afterPkgs, _ := after["packages"].([]any)
	have := map[any]bool{}
	for _, p := range afterPkgs {
		have[p] = true
	}
	for _, p := range d.pkgAdds {
		if !have[p] {
			return fmt.Errorf("package %q did not land", p)
		}
	}

	// everything outside [tools]/[packages] must be untouched
	for _, key := range []string{"setup", "persist", "repos"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			return fmt.Errorf("[%s] changed — auto-detection must never touch it", key)
		}
	}
	return nil
}
