package main

import (
	"io"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// profileplane.go — the plane-side profile verbs (bash cmd_profile:
// add/ls/use/rename/rm/seed) + setup. A profile is a whole daybox (own
// server + volume + state); `profile add` creates its volume, `up -p` sums
// it, profiles coexist + reap independently.
//
// The laptop already owns edit/proposals/accept/reject/propose (the §1e
// approval flow — approval is a laptop-side action by design). The plane
// verbs here are the box/volume lifecycle ones.

// defaultSeed is the seed template a new profile starts from. Embedded so a
// fresh install's first `daybox up` doesn't die for want of a seed the
// payload carries as a file (bash: cp "$REPO_DIR/profile.default.toml").
//
//go:embed profile.default.toml
var defaultSeed []byte

// profileAdd creates a profile's config + seed + volume (bash: profile_add).
// It never overwrites an existing profile.
func profileAdd(dep *deployment, name, serverType string) error {
	if !validProfileName(name) {
		return fmt.Errorf("invalid profile '%s' (lowercase letters, digits, dashes)", name)
	}
	prov, err := dep.loadProvider(loadConfigFile(configPath()).get("PROVIDER", "hetzner"))
	if err != nil {
		return err
	}
	if err := prov.CheckCredentials(); err != nil {
		return err
	}
	confDir := filepath.Join(dep.confDir, "profiles", name)
	if fileExists(filepath.Join(confDir, "config")) {
		return fmt.Errorf("profile '%s' already exists (edit %s)", name, filepath.Join(confDir, "config"))
	}
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	// seed a profile config the user can edit: box shape + this profile's
	// git identity (falls back to deployment-wide when left blank). Server +
	// volume names are DERIVED, not set.
	base := loadConfigFile(configPath())
	st := firstNonEmpty(serverType, base.get("SERVER_TYPE", defaultServerType))
	cfg := fmt.Sprintf(`# daybox profile '%s' — overrides config.local for this box only.
# Server + volume names are derived (daybox-%s / daybox-%s-vol), not set.
#SERVER_TYPE=%s
#LOCATION=%s
#VOLUME_SIZE_GB=%d
# This profile's git identity (its box's gh/git/ssh live on its own volume):
#GIT_NAME=%q
#GIT_EMAIL=%s
`, name, name, name, st, base.get("LOCATION", defaultLocation), defaultVolumeSizeGB, base.get("GIT_NAME", ""), base.get("GIT_EMAIL", ""))
	if serverType != "" {
		// uncomment the SERVER_TYPE line so the override is active
		cfg = strings.Replace(cfg, "#SERVER_TYPE=", "SERVER_TYPE=", 1)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config"), []byte(cfg), 0o600); err != nil {
		return err
	}
	// the seed: what this profile's boxes CARRY
	if err := os.WriteFile(filepath.Join(confDir, "profile.toml"), defaultSeed, 0o644); err != nil {
		return err
	}
	if err := prov.PrepareSSHKeys(dep.keysDir()); err != nil {
		return err
	}
	vid, err := ensureVolume(dep, name, prov)
	if err != nil {
		return err
	}
	say("profile '%s' ready (volume %s). summon it with: daybox up -p %s", name, vid, name)
	return nil
}

// ensureVolume creates (or adopts) the profile's volume + caches its id
// (bash: ensure_volume). Idempotent.
func ensureVolume(dep *deployment, name string, prov Provider) (string, error) {
	p, err := dep.deriveProfile(name)
	if err != nil {
		return "", err
	}
	vid, err := prov.VolumeEnsure(p.volumeName, p.volumeSizeGB, p.location)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p.volumeIDFile(), []byte(vid), 0o644); err != nil {
		return "", err
	}
	resetIdle(p)
	return vid, nil
}

// profileLs lists every profile + its box state + volume size (bash:
// profile_ls). The current profile is marked '*'.
func profileLs(dep *deployment, w io.Writer) {
	prov, err := dep.loadProvider(loadConfigFile(configPath()).get("PROVIDER", "hetzner"))
	if err != nil {
		fmt.Fprintf(w, "profile ls: %v\n", err)
		return
	}
	if err := prov.CheckCredentials(); err != nil {
		fmt.Fprintln(w, err)
		return
	}
	cur := readFileTrim(filepath.Join(dep.stateDir, "current_profile"))
	if cur == "" {
		cur = "default"
	}
	fmt.Fprintf(w, "%-16s %-8s %-10s %s\n", "PROFILE", "DEFAULT", "BOX", "VOLUME")
	for _, name := range dep.listProfiles() {
		p, err := dep.deriveProfile(name)
		if err != nil {
			say("[%s] listing failed — skipping: %v", name, err)
			continue
		}
		srv, _ := prov.Probe(p.serverName)
		state := "-"
		if srv != nil {
			state = srv.Status
		}
		vid, _ := p.volumeID()
		vol := "(no volume)"
		if vid != "" {
			if sz, err := prov.VolumeSize(vid); err == nil {
				vol = fmt.Sprintf("%dGB", sz)
			}
		}
		mark := "-"
		if name == cur {
			mark = "*"
		}
		fmt.Fprintf(w, "%-16s %-8s %-10s %s\n", name, mark, state, vol)
	}
}

// profileUse sets the default profile (bash: profile_use).
func profileUse(dep *deployment, name string) error {
	if !validProfileName(name) {
		return fmt.Errorf("invalid profile '%s'", name)
	}
	if !fileExists(filepath.Join(dep.stateDir, "profiles", name)) {
		return fmt.Errorf("no such profile '%s' — create it: daybox profile add %s", name, name)
	}
	if err := os.WriteFile(filepath.Join(dep.stateDir, "current_profile"), []byte(name), 0o644); err != nil {
		return err
	}
	say("default profile is now '%s' (bare 'daybox up' targets it)", name)
	return nil
}

// profileRename renames a profile's volume + state (bash: profile_rename).
// A live box would orphan the reaper counters + leave a net ghost, so the
// box must be down first.
func profileRename(dep *deployment, old, new string) error {
	if !validProfileName(new) {
		return fmt.Errorf("invalid profile '%s'", new)
	}
	if !fileExists(filepath.Join(dep.stateDir, "profiles", old)) {
		return fmt.Errorf("no such profile '%s'", old)
	}
	if fileExists(filepath.Join(dep.stateDir, "profiles", new)) {
		return fmt.Errorf("profile '%s' already exists", new)
	}
	prov, err := dep.loadProvider(loadConfigFile(configPath()).get("PROVIDER", "hetzner"))
	if err != nil {
		return err
	}
	if err := prov.CheckCredentials(); err != nil {
		return err
	}
	p, err := dep.deriveProfile(old)
	if err != nil {
		return err
	}
	if s, _ := prov.Probe(p.serverName); s != nil {
		return fmt.Errorf("profile '%s' has a live box — 'daybox down -p %s' before renaming", old, old)
	}
	if vid, _ := p.volumeID(); vid != "" {
		say("renaming volume daybox-%s-vol → daybox-%s-vol", old, new)
		if err := prov.VolumeRename(vid, "daybox-"+new+"-vol"); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(dep.stateDir, "profiles", old), filepath.Join(dep.stateDir, "profiles", new)); err != nil {
		return err
	}
	if fileExists(filepath.Join(dep.confDir, "profiles", old)) {
		os.Rename(filepath.Join(dep.confDir, "profiles", old), filepath.Join(dep.confDir, "profiles", new))
	}
	if readFileTrim(filepath.Join(dep.stateDir, "current_profile")) == old {
		os.WriteFile(filepath.Join(dep.stateDir, "current_profile"), []byte(new), 0o644)
	}
	say("renamed profile '%s' → '%s'", old, new)
	return nil
}

// profileRm deletes a profile's box (if up), optionally its volume, then its
// state + config (bash: profile_rm). The 'default' profile is protected.
func profileRm(dep *deployment, name, purge string) error {
	if name == "default" {
		return fmt.Errorf("refusing to remove the 'default' profile")
	}
	if !fileExists(filepath.Join(dep.stateDir, "profiles", name)) {
		return fmt.Errorf("no such profile '%s'", name)
	}
	prov, err := dep.loadProvider(loadConfigFile(configPath()).get("PROVIDER", "hetzner"))
	if err != nil {
		return err
	}
	if err := prov.CheckCredentials(); err != nil {
		return err
	}
	p, err := dep.deriveProfile(name)
	if err != nil {
		return err
	}
	// reap the box if it's up (frees the volume, stops billing)
	if s, _ := prov.Probe(p.serverName); s != nil {
		if err := downBox(p, prov, newPlaneDownOps(p)); err != nil {
			return err
		}
	}
	vid, _ := p.volumeID()
	if purge == "--purge" && vid != "" {
		say("deleting volume daybox-%s-vol (id %s) — workspace state is gone for good", name, vid)
		if err := prov.VolumeDelete(vid); err != nil {
			return err
		}
	} else if vid != "" {
		say("profile config/state removed; volume daybox-%s-vol (id %s) KEPT.", name, vid)
		say("  delete it yourself when sure:  daybox profile rm %s --purge", name)
	}
	os.RemoveAll(filepath.Join(dep.stateDir, "profiles", name))
	os.RemoveAll(filepath.Join(dep.confDir, "profiles", name))
	if readFileTrim(filepath.Join(dep.stateDir, "current_profile")) == name {
		os.Remove(filepath.Join(dep.stateDir, "current_profile"))
	}
	say("removed profile '%s'", name)
	return nil
}

// profileSeed show|init|path (bash: profile_seed). The seed is what a box
// CARRIES; there is no default at summon time.
func profileSeed(dep *deployment, sub, name string, w io.Writer) error {
	if name == "" {
		name = "default"
	}
	if !validProfileName(name) {
		return fmt.Errorf("invalid profile '%s'", name)
	}
	f := filepath.Join(dep.confDir, "profiles", name, "profile.toml")
	switch sub {
	case "", "show":
		if !fileExists(f) {
			return fmt.Errorf("profile '%s' has no seed at %s\n  create one with: daybox profile seed init %s", name, f, name)
		}
		fmt.Fprintf(w, "# %s\n", f)
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		w.Write(b)
	case "init":
		if fileExists(f) {
			return fmt.Errorf("'%s' already has a seed at %s (edit it directly)", name, f)
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f, defaultSeed, 0o644); err != nil {
			return err
		}
		say("wrote %s — edit it, then: daybox up -p %s", f, name)
	case "path":
		fmt.Fprintln(w, f)
	default:
		return fmt.Errorf("usage: daybox profile seed [show|init|path] [<name>]")
	}
	return nil
}

// setup is the one-time bootstrap of the current profile (bash: cmd_setup):
// register ssh keys, seed the profile.toml, create the volume.
func setup(dep *deployment) error {
	prov, err := dep.loadProvider(loadConfigFile(configPath()).get("PROVIDER", "hetzner"))
	if err != nil {
		return err
	}
	if err := prov.CheckCredentials(); err != nil {
		return err
	}
	if err := prov.PrepareSSHKeys(dep.keysDir()); err != nil {
		return err
	}
	name, _ := dep.currentProfile("")
	p, err := dep.deriveProfile(name)
	if err != nil {
		return err
	}
	// seed the profile.toml if absent — setup IS the profile's creation
	// moment, and a profile without a seed is unsummonable by design. Never
	// overwrite a seed the user may have edited.
	seed := p.seedFile()
	if !fileExists(seed) {
		if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(seed, defaultSeed, 0o644); err != nil {
			return err
		}
		say("seeded profile '%s' (view: daybox profile seed show %s)", name, name)
	}
	vid, err := ensureVolume(dep, name, prov)
	if err != nil {
		return err
	}
	say("setup complete for profile '%s'. volume id: %s", name, vid)
	return nil
}
