package main

// keepedit.go — `daybox keep edit` (laptop) + the plane-side `keep cat`/`keep
// put` it drives. keep.toml lives on the BOX's /work volume, so editing it
// from the laptop goes laptop → plane → box: the laptop runs the editor,
// the plane is the ssh hop to the box (the laptop can't reach the box
// directly — the box only allows the plane in, and the laptop doesn't have
// the box's IP or host keys). See docs/keep-and-proposals.md (W5).
//
// Mirrors `profile edit` (profilecmd.go): non-delegating, the editor runs
// on the laptop, the box is just the file store. Unlike `profile edit`
// (which fetches/pushes a plane-side seed), keep fetches/pushes a box-side
// file — so the plane has two internal subverbs (`keep cat`/`keep put`)
// the laptop invokes over ssh. Those are internal (not user-facing); the
// only user verb is `daybox keep edit`.

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// cmdKeep routes the `keep` group. `keep edit` is laptop-only (runs
// $EDITOR); `keep cat`/`keep put` are plane-only (invoked by `keep edit`
// over ssh). Unknown/no subverb on a laptop prints usage.
func cmdKeep(p Parsed) {
	rest := p.Rest()
	if len(rest) > 0 && rest[0] == "edit" {
		cmdKeepEdit(p)
		return
	}
	if amPlane() {
		cmdKeepPlane(p)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: daybox keep edit [-p prof]  (edit the box's keep.toml; run while the box is up)")
}

// cmdKeepEdit: `daybox keep edit [-p prof]` — fetch the box's keep.toml via
// the plane, open $EDITOR, validate, push back. Refuses if no box is up
// (keep is volume-only; there's no plane-side master to edit when down).
func cmdKeepEdit(p Parsed) {
	host := mustControl()
	name := p.Global("profile")
	if name == "" {
		n, err := remoteDefaultProfile(host)
		if err != nil {
			log.Fatalf("could not resolve the default profile: %v", err)
		}
		name = n
	}
	if !validProfileName(name) {
		log.Fatalf("invalid profile '%s' (lowercase letters, digits, dashes)", name)
	}
	current, err := fetchKeep(host, name)
	if err != nil {
		log.Fatalf("could not read keep.toml on the box: %v\n  (keep.toml lives on the box volume — edit while a box is up; a fresh box has an empty one)", err)
	}
	tmp, err := os.CreateTemp("", "daybox-keep-*.toml")
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
			say("no changes — keep.toml untouched")
			return
		}
		if verr := validateKeepToml(edited); verr == nil {
			break
		} else {
			say("keep.toml rejected: %v", verr)
			fmt.Fprintf(os.Stderr, "re-edit? [Y/n] ")
			var answer string
			fmt.Fscanln(os.Stdin, &answer)
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "n") {
				log.Fatalf("keep.toml not pushed — your edit is preserved at %s", tmpPath)
			}
		}
	}
	if err := pushKeep(host, name, edited); err != nil {
		log.Fatalf("push failed — your edit is preserved at %s: %v", tmpPath, err)
	}
	os.Remove(tmpPath)
	say("keep.toml updated on the box — takes effect on the next reaper tick (5min)")
}

// cmdKeepPlane: the plane-side `keep cat`/`keep put` the laptop's keep edit
// drives over ssh. `cat` prints the box's keep.toml; `put` replaces it
// (backup + temp+rename so a dropped connection can't leave a half-written
// file for the next reaper tick). Internal — not user-facing.
func cmdKeepPlane(p Parsed) {
	rest := p.Rest()
	if len(rest) == 0 {
		log.Fatal("usage: daybox keep cat|put  (invoked by 'daybox keep edit' on a laptop)")
	}
	dep := loadDeployment()
	prof, err := dep.deriveProfile(profileNameOrCurrent(dep, p.Global("profile")))
	if err != nil {
		log.Fatal(err)
	}
	switch rest[0] {
	case "cat":
		out, err := sshBoxCapture(dep, prof, "cat /work/state/daybox/keep.toml 2>/dev/null")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(out)
	case "put":
		content, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		// backup + temp+rename: never a half-written file for the reaper.
		cmd := `f=/work/state/daybox/keep.toml; [ -f "$f" ] && cp -p "$f" "$f.bak"; cat > "$f.tmp" && mv "$f.tmp" "$f"`
		if err := sshBoxFeed(dep, prof, cmd, string(content)); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown keep subverb %q (internal: cat|put)", rest[0])
	}
}

// fetchKeep reads the box's keep.toml via the plane (the laptop → plane →
// box hop). name is validated (validProfileName) before interpolation.
// Uses remoteDaybox (the absolute path) — the plane's non-interactive ssh
// PATH doesn't include ~/.local/bin, so a bare "daybox" fails with
// "command not found" (caught live in W11).
func fetchKeep(host, name string) (string, error) {
	return sshCapture(host, remoteDaybox+" -p "+name+" keep cat")
}

// pushKeep writes content to the box's keep.toml via the plane (laptop →
// plane → box over ssh stdin, so the file never hits argv). remoteDaybox
// for the same PATH reason as fetchKeep.
func pushKeep(host, name, content string) error {
	return sshFeed(host, remoteDaybox+" -p "+name+" keep put", content)
}

// validateKeepToml checks content is a valid keep.toml: parses as TOML and
// every [[files]] entry has a valid path (keepPathRe) + a positive within.
// Stricter than loadKeepToml (which logs+skips bad entries to degrade
// gracefully): the edit loop rejects a bad edit and re-prompts, so a typo
// never reaches the box. Mirrors validateProfile for seeds.
func validateKeepToml(content string) error {
	var doc struct {
		Files []keepFileEntry `toml:"files"`
	}
	if _, err := toml.Decode(content, &doc); err != nil {
		return fmt.Errorf("not valid TOML: %w", err)
	}
	for i, f := range doc.Files {
		if !keepPathRe.MatchString(f.Path) {
			return fmt.Errorf("[[files]] entry %d: path %q must be absolute, no shell metachars", i, f.Path)
		}
		if f.Within == "" {
			return fmt.Errorf("[[files]] entry %d (%s): missing within (e.g. within = \"10m\")", i, f.Path)
		}
		dur, err := time.ParseDuration(f.Within)
		if err != nil {
			return fmt.Errorf("[[files]] entry %d (%s): bad within %q (must be a duration like 10m)", i, f.Path, f.Within)
		}
		if dur <= 0 {
			return fmt.Errorf("[[files]] entry %d (%s): within must be positive", i, f.Path)
		}
	}
	return nil
}

// --- plane → box non-tty ssh helpers (sshRunningBox is the tty variant) ---

// boxServer resolves the running box's server record (provider Probe).
// Returns an error if no box is running — keep is volume-only, so editing
// it requires a live box to mount the volume.
func boxServer(dep *deployment, p *profile) (*Server, error) {
	prov, err := dep.loadProvider(p.provider)
	if err != nil {
		return nil, err
	}
	s, err := prov.Probe(p.serverName)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("no big box running — summon with: daybox up")
	}
	return s, nil
}

// sshBoxCapture runs a non-tty command on the running box from the plane,
// returning stdout. (sshRunningBox is the tty variant for `daybox ssh`.)
func sshBoxCapture(dep *deployment, p *profile, cmd string) (string, error) {
	s, err := boxServer(dep, p)
	if err != nil {
		return "", err
	}
	args := append([]string{"ssh"}, sshBoxOpts(p)...)
	args = append(args, p.remoteUser+"@"+s.IP, cmd)
	out, err := exec.Command(args[0], args[1:]...).Output()
	return string(out), err
}

// sshBoxFeed writes content to a command on the running box over ssh stdin
// (so the keep.toml content never hits argv).
func sshBoxFeed(dep *deployment, p *profile, cmd, content string) error {
	s, err := boxServer(dep, p)
	if err != nil {
		return err
	}
	args := append([]string{"ssh"}, sshBoxOpts(p)...)
	args = append(args, p.remoteUser+"@"+s.IP, cmd)
	c := exec.Command(args[0], args[1:]...)
	c.Stdin = strings.NewReader(content)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
