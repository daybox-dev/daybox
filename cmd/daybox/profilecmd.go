package main

// Profile editing — laptop authority (TODO §1e, P1+P2).
//
// A profile's seed (profile.toml) LIVES on the control plane
// (~/.config/daybox/profiles/<name>/profile.toml) but is AUTHORED from the
// laptop: the laptop is already the trusted authority for every
// deployment-shaping verb (init, upgrade), and the plane is the store, not
// the author. `daybox profile edit` is fetch → $EDITOR → validate → push,
// with a remote backup before the replace. Edits take effect at the next
// `daybox up` — the seed is frozen into cloud-init user_data at render time,
// so no new apply mechanism is needed.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// validProfileName mirrors bin/daybox's valid_profile_name: the name lands
// inside remote shell commands and derived paths, so the character set is
// the safety boundary, not just cosmetics.
func validProfileName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// remoteSeedPath is the control-plane location of a profile's seed —
// $HOME-relative because non-interactive ssh shells expand no ~ inside a
// quoted command. Callers must have validated name.
func remoteSeedPath(name string) string {
	return `"$HOME"/.config/daybox/profiles/` + shq(name) + `/profile.toml`
}

// remoteDefaultProfile resolves the control plane's `profile use` selection,
// the same fallback bin/daybox applies when -p is absent.
func remoteDefaultProfile(host string) (string, error) {
	out, err := sshCapture(host,
		`cat "$HOME"/.config/daybox/state/current_profile 2>/dev/null || echo default`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// fetchProfile reads a profile's seed off the control plane (the read half
// of `profile seed show`, without the path banner).
func fetchProfile(host, name string) (string, error) {
	out, err := sshCapture(host, "cat "+remoteSeedPath(name))
	if err != nil {
		return "", fmt.Errorf("profile '%s' has no seed on the control plane — create it with: daybox profile seed init %s", name, name)
	}
	return out, nil
}

// pushProfile replaces a profile's seed on the control plane, backing up the
// live file first (profile.toml.bak.<ts>) and writing via temp + rename so a
// dropped connection can't leave a half-written seed for the next summon to
// freeze into user_data. Content travels over ssh stdin (never argv).
func pushProfile(host, name, content, ts string) error {
	f := remoteSeedPath(name)
	cmd := "f=" + f + " && " +
		`[ -f "$f" ] && cp -p "$f" "$f.bak.` + ts + `"; ` +
		`cat > "$f.tmp" && mv "$f.tmp" "$f"`
	return sshFeed(host, cmd, content)
}

// sshFeed runs a command on host with stdin fed from content — the
// file-moving sibling of sshCapture. Same transport-retry rules; safe
// because the write is idempotent.
func sshFeed(host, cmd, content string) error {
	return sshRetry("control plane", func() error {
		c := exec.Command("ssh", append(sshOpts(true), host, cmd)...)
		c.Stdin = strings.NewReader(content)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return c.Run()
	})
}

// profileKnownKeys is the seed's schema surface, kept in lockstep with
// remote/apply-seed.py (the firstboot applier) — this is its known-keys typo
// check lifted onto the laptop, so a bad edit dies here, not at next summon.
var profileKnownKeys = []string{"packages", "persist", "repos", "setup", "tools"}
var profileSetupKeys = []string{"every_boot", "once"}

// validateProfile checks an edited seed the way apply-seed.py will: valid
// TOML, no unknown top-level keys, [setup] a table holding only
// once/every_boot. Typos must not be silently ignored, or a profile will
// quietly not carry what its author believed it declared.
func validateProfile(src string) error {
	var seed map[string]any
	if _, err := toml.Decode(src, &seed); err != nil {
		return fmt.Errorf("not valid TOML: %v", err)
	}
	known := map[string]bool{}
	for _, k := range profileKnownKeys {
		known[k] = true
	}
	var unknown []string
	for k := range seed {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown top-level key(s): %s. Known: %s",
			strings.Join(unknown, ", "), strings.Join(profileKnownKeys, ", "))
	}
	if raw, ok := seed["setup"]; ok {
		setup, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("[setup] must be a table")
		}
		knownSetup := map[string]bool{}
		for _, k := range profileSetupKeys {
			knownSetup[k] = true
		}
		unknown = nil
		for k := range setup {
			if !knownSetup[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("unknown key(s) in [setup]: %s. Known: %s",
				strings.Join(unknown, ", "), strings.Join(profileSetupKeys, ", "))
		}
	}
	return nil
}

// cmdProfileEdit: `daybox profile edit [name]` — the one profile subverb that
// does not delegate: the editor runs here, on the laptop.
func cmdProfileEdit(args []string) {
	host := mustControl()
	var name string
	if len(args) > 0 {
		name = args[0]
	} else {
		n, err := remoteDefaultProfile(host)
		if err != nil {
			log.Fatalf("could not resolve the default profile: %v", err)
		}
		name = n
	}
	if !validProfileName(name) {
		log.Fatalf("invalid profile '%s' (lowercase letters, digits, dashes)", name)
	}

	current, err := fetchProfile(host, name)
	if err != nil {
		log.Fatal(err)
	}

	tmp, err := os.CreateTemp("", "daybox-profile-"+name+"-*.toml")
	if err != nil {
		log.Fatal(err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(current); err != nil {
		log.Fatal(err)
	}
	tmp.Close()

	var edited string
	for {
		if err := runEditor(tmpPath); err != nil {
			log.Fatalf("editor failed: %v", err)
		}
		b, err := os.ReadFile(tmpPath)
		if err != nil {
			log.Fatal(err)
		}
		edited = string(b)
		if edited == current {
			os.Remove(tmpPath)
			say("no changes — profile '%s' untouched", name)
			return
		}
		verr := validateProfile(edited)
		if verr == nil {
			break
		}
		say("profile '%s' rejected: %v", name, verr)
		fmt.Fprintf(os.Stderr, "re-edit? [Y/n] ")
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "n") {
			log.Fatalf("profile not pushed — your edit is preserved at %s", tmpPath)
		}
	}

	ts := time.Now().Format("20060102-150405")
	if err := pushProfile(host, name, edited, ts); err != nil {
		log.Fatalf("push failed — your edit is preserved at %s: %v", tmpPath, err)
	}
	os.Remove(tmpPath)
	say("profile '%s' updated (backup on the control plane: profile.toml.bak.%s)", name, ts)
	say("takes effect at the next daybox up — rotate a running box with down + up")
}

// runEditor opens path in the user's editor ($VISUAL, $EDITOR, else vi),
// through the shell so multi-word values ("code --wait") work.
func runEditor(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+" "+shq(path))
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
