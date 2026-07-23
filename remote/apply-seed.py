#!/usr/bin/env python3
"""Apply a profile's seed to a freshly booted daybox.

The box carries what its profile declares and nothing else. This reads that
declaration (profile.toml, pushed by the control plane) and applies it.

Deliberately unforgiving: the first failing step aborts the run and the
outcome is written to a status file that `daybox up` reads. A box that comes
up missing what its profile declared is worse than no box, because it looks
fine. There is no partial success and no step that is allowed to fail.

Python because Ubuntu 24.04 ships 3.12, whose stdlib has tomllib — so the one
place that parses TOML needs no dependency anywhere in the system.
"""

import hashlib
import os
import subprocess
import sys
import tomllib
from pathlib import Path

STATUS = Path("/var/lib/daybox/seed.status")
# Marks which `once` steps have already run FOR THIS VOLUME. On the volume,
# not the root disk — that is what makes "once" mean once per workspace rather
# than once per boot.
ONCE_DIR = Path("/work/state/.seed-once")
# Roots, named so they are stated once and can be pointed elsewhere by tests.
HOME_ROOT = Path("/home")
STATE = Path("/work/state")
REPOS = Path("/work/repos")


def status(text: str) -> None:
    STATUS.parent.mkdir(parents=True, exist_ok=True)
    STATUS.write_text(text)


def die(step: str, detail: str) -> None:
    msg = f"FAILED: {step}\n{detail}"
    print(msg, file=sys.stderr, flush=True)
    status(msg)
    sys.exit(1)


def run(step: str, argv, *, user: str | None = None, shell_cmd: str | None = None) -> None:
    """Run a step, streaming output. Any non-zero exit aborts the whole seed."""
    if shell_cmd is not None:
        argv = ["sudo", "-u", user, "bash", "-lc", shell_cmd] if user else ["bash", "-lc", shell_cmd]
    print(f"[seed] {step}", flush=True)
    try:
        p = subprocess.run(argv, capture_output=True, text=True)
    except OSError as e:
        die(step, str(e))
    if p.stdout:
        print(p.stdout, flush=True)
    if p.returncode != 0:
        die(step, f"exit {p.returncode}\n{p.stderr.strip()}")


def _safe_name(kind: str, value) -> str:
    """A non-empty string that cannot be mistaken for a command-line flag.

    Package names go to apt-get as argv, so shell metacharacters are harmless
    — but a leading dash would be parsed as an option, which is not.
    """
    if not isinstance(value, str) or not value.strip():
        die(kind, "every entry must be a non-empty string")
    if value.startswith("-"):
        die(kind, f"{value!r} starts with '-' and would be read as a flag")
    return value


def apply_packages(pkgs, _user):
    if not pkgs:
        return
    for p in pkgs:
        _safe_name("packages", p)
    run(f"apt install: {' '.join(pkgs)}",
        ["apt-get", "install", "-y", "-q",
         "-o", "Dpkg::Options::=--force-confold", "--no-install-recommends", *pkgs])


def apply_repos(repos, user):
    if not repos:
        return
    for url in repos:
        _safe_name("repos", url)
        name = url.rstrip("/").split("/")[-1]
        if name.endswith(".git"):
            name = name[:-4]
        if not name:
            die("repos", f"cannot derive a directory name from {url!r}")
        dest = REPOS / name
        if dest.exists():
            print(f"[seed] repo {name} already present — leaving it", flush=True)
            continue
        run(f"clone {url}",
            ["sudo", "-u", user, "git", "clone", "--", url, str(dest)])


def _safe_rel(kind: str, value: str) -> str:
    """Reject anything that would escape the directory it is anchored to."""
    if not isinstance(value, str) or not value.strip():
        die(kind, "every entry must be a non-empty string")
    norm = os.path.normpath(value)
    if os.path.isabs(norm) or norm == ".." or norm.startswith("../"):
        die(kind, f"{value!r} must be a relative path that stays inside its root")
    return norm


def apply_persist(mapping, user):
    """Symlink paths in HOME onto the volume, so they survive a reap.

    This is what makes the volume "memory" for *any* tool, instead of only for
    the ones cloud-init happened to hardcode. `~/.claude -> /work/state/claude`
    is now a profile's declaration, not a product assumption.

    A trailing slash on either side declares a DIRECTORY mapping and the
    volume-side target is created up front. Without it the mapping has file
    semantics: the link dangles until the tool writes through it, which
    open(2) permits. The distinction is load-bearing — mkdir(2) through a
    dangling symlink fails EEXIST, so a directory target that nothing has
    created yet breaks the very tool the mapping exists for (the Claude
    installer's `mkdir -p ~/.claude` was the discovering case).
    """
    if not mapping:
        return
    if not isinstance(mapping, dict):
        die("persist", "[persist] must be a table of \"home path\" = \"volume path\"")
    home = HOME_ROOT / user
    for rel, vol in mapping.items():
        is_dir = rel.endswith("/") or vol.endswith("/")
        link = home / _safe_rel("persist", rel.rstrip("/"))
        target = STATE / _safe_rel("persist", vol.rstrip("/"))
        if link.is_symlink():
            link.unlink()
        elif link.exists():
            die("persist", f"{link} already exists and is not a symlink — "
                           f"refusing to replace real data")
        if target.exists() and target.is_dir() != is_dir:
            kind = "a directory" if is_dir else "a file"
            die("persist", f"{target} exists but the mapping declares {kind} "
                           f"(trailing slash = directory) — refusing to guess")
        link.parent.mkdir(parents=True, exist_ok=True)
        if is_dir:
            target.mkdir(parents=True, exist_ok=True)
        else:
            target.parent.mkdir(parents=True, exist_ok=True)
        link.symlink_to(target)
        run(f"persist {link} -> {target}",
            ["chown", "-h", f"{user}:{user}", str(link)])
        run(f"own {target.parent}", ["chown", "-R", f"{user}:{user}", str(target.parent)])


def apply_once(cmds, user):
    """Steps that install into /work and therefore survive a reap."""
    if not cmds:
        return
    ONCE_DIR.mkdir(parents=True, exist_ok=True)
    for cmd in cmds:
        if not isinstance(cmd, str) or not cmd.strip():
            die("setup.once", "every entry must be a non-empty string")
        # Key on the command text: edit the command and it runs again, which
        # is what you want — the declaration changed.
        marker = ONCE_DIR / hashlib.sha256(cmd.encode()).hexdigest()[:32]
        if marker.exists():
            print(f"[seed] once (already applied to this volume): {cmd}", flush=True)
            continue
        run(f"once: {cmd}", None, user=user, shell_cmd=cmd)
        marker.write_text(cmd + "\n")


def apply_every_boot(cmds, user):
    """Steps landing on the ephemeral root disk, so they must repeat."""
    for cmd in cmds or []:
        if not isinstance(cmd, str) or not cmd.strip():
            die("setup.every_boot", "every entry must be a non-empty string")
        run(f"every_boot: {cmd}", None, user=user, shell_cmd=cmd)


def main() -> None:
    if len(sys.argv) != 3:
        print("usage: apply-seed.py <profile.toml> <user>", file=sys.stderr)
        sys.exit(2)
    path, user = Path(sys.argv[1]), sys.argv[2]

    if not path.is_file():
        die("read seed", f"{path} does not exist — the control plane did not push a seed")
    try:
        seed = tomllib.loads(path.read_text())
    except tomllib.TOMLDecodeError as e:
        die("parse seed", f"{path} is not valid TOML: {e}")

    known = {"packages", "repos", "persist", "setup"}
    unknown = set(seed) - known
    if unknown:
        # Typos must not be silently ignored, or a profile will quietly not
        # carry what its author believed it declared.
        die("parse seed", f"unknown top-level key(s): {', '.join(sorted(unknown))}. "
                          f"Known: {', '.join(sorted(known))}")
    setup = seed.get("setup", {})
    if not isinstance(setup, dict):
        die("parse seed", "[setup] must be a table")
    unknown_setup = set(setup) - {"once", "every_boot"}
    if unknown_setup:
        die("parse seed", f"unknown key(s) in [setup]: {', '.join(sorted(unknown_setup))}. "
                          f"Known: once, every_boot")

    apply_packages(seed.get("packages"), user)
    # persist BEFORE setup: an installer in [setup] may write straight
    # into a path that is supposed to land on the volume.
    apply_persist(seed.get("persist"), user)
    apply_repos(seed.get("repos"), user)
    apply_once(setup.get("once"), user)
    apply_every_boot(setup.get("every_boot"), user)

    status("ok")
    print("[seed] ok", flush=True)


if __name__ == "__main__":
    os.umask(0o022)
    main()
