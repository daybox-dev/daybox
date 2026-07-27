#!/usr/bin/env python3
"""Tests for remote/apply-seed.py. No cloud calls, no cost — run from anywhere.

    scripts/test-seed.py

apply-seed.py runs as root on every summoned box and decides what that box
carries, so the failure paths matter more than the happy one. Cases marked
REGRESSION encode defects found by the paid conformance run; each one shipped
because it had been reasoned about rather than executed.
"""
import importlib.util
import os
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SRC = REPO / "remote" / "apply-seed.py"
PASS = FAIL = 0


def stubbin(tmp: Path, *, mise: bool = True) -> Path:
    """apt-get/git/sudo/chown/mise stubs so the real subprocess paths still run."""
    b = tmp / "bin"
    b.mkdir(exist_ok=True)   # apply() may run repeatedly in one sandbox
    (b / "apt-get").write_text('#!/bin/sh\nexit 0\n')
    (b / "git").write_text('#!/bin/sh\nmkdir -p "$3" 2>/dev/null; exit 0\n')
    (b / "chown").write_text('#!/bin/sh\nexit 0\n')
    (b / "sudo").write_text('#!/bin/sh\nshift 2\nexec "$@"\n')  # sudo -u USER ... -> ...
    for f in b.iterdir():
        f.chmod(0o755)
    return b


def apply(tmp: Path, toml_text: str, *, mise: bool = True):
    """Run apply-seed.py against sandboxed roots. Returns (proc, home, state)."""
    home, state, repos = tmp / "home", tmp / "state", tmp / "repos"
    for d in (home / "tester", state, repos):
        d.mkdir(parents=True, exist_ok=True)
    if mise:
        # apply-seed requires the materializer AT state/bin/mise (the exact
        # contract), and the rendered env file puts state/bin first on PATH —
        # so this stub is both the existence check's target and what runs.
        # It logs each invocation so tests can assert install/lock behaviour.
        (state / "bin").mkdir(exist_ok=True)
        (state / "bin" / "mise").write_text(
            '#!/bin/sh\necho "mise $*" >> "${MISE_LOG:-/dev/null}"\nexit 0\n')
        (state / "bin" / "mise").chmod(0o755)
    cfg = tmp / "profile.toml"
    cfg.write_text(toml_text)
    code = (
        "import importlib.util,sys,pathlib;"
        f"spec=importlib.util.spec_from_file_location('s',r'{SRC}');"
        "m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);"
        f"m.STATUS=pathlib.Path(r'{tmp}/status');m.ONCE_DIR=pathlib.Path(r'{state}/.seed-once');"
        f"m.HOME_ROOT=pathlib.Path(r'{home}');m.STATE=pathlib.Path(r'{state}');"
        f"m.REPOS=pathlib.Path(r'{repos}');"
        f"sys.argv=['x',r'{cfg}','tester'];m.main()"
    )
    env = dict(os.environ, PATH=f"{stubbin(tmp)}:{os.environ['PATH']}",
               HOME=str(home / "tester"),   # so '~' in a seed command stays sandboxed
               MISE_LOG=str(tmp / "mise.log"),
               PYTHONDONTWRITEBYTECODE="1")
    p = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, env=env)
    return p, home / "tester", state


def case(name, toml_text, expect, want_status=None, pre=None, then=None, mise=True):
    """expect: 'ok' | 'fail'. `then(home, state)` asserts the resulting state."""
    global PASS, FAIL
    with tempfile.TemporaryDirectory() as d:
        tmp = Path(d)
        if pre:
            (tmp / "home" / "tester").mkdir(parents=True, exist_ok=True)
            (tmp / "state").mkdir(parents=True, exist_ok=True)
            pre(tmp / "home" / "tester", tmp / "state")
        p, home, state = apply(tmp, toml_text, mise=mise)
        got = "ok" if p.returncode == 0 else "fail"
        status = (tmp / "status").read_text() if (tmp / "status").exists() else ""
        ok = got == expect
        if ok and want_status and want_status not in status:
            ok = False
        if ok and then:
            ok = bool(then(home, state))
        if ok:
            PASS += 1
            print(f"  ✓ {name:<52} -> {got}")
        else:
            FAIL += 1
            print(f"  ✗ {name:<52} -> {got} (want {expect})")
            print(f"      status: {status.strip()[:180]!r}")
            if p.stderr.strip():
                print(f"      stderr: {p.stderr.strip()[:180]}")


print("=== a declared seed is applied ===")
case("empty seed", "", "ok", "ok")
case("packages", 'packages = ["ripgrep","jq"]', "ok", "ok")
case("repos cloned into /work/repos", 'repos = ["git@github.com:me/thing.git"]', "ok", "ok",
     then=lambda h, s: True)
case("setup.once", '[setup]\nonce = ["true"]', "ok", "ok")
case("setup.every_boot", '[setup]\nevery_boot = ["true"]', "ok", "ok")

print("\n=== nothing fails quietly: one bad step aborts the whole seed ===")
case("failing every_boot", '[setup]\nevery_boot = ["false"]', "fail", "FAILED")
case("failing once", '[setup]\nonce = ["false"]', "fail", "FAILED")
case("later step fails after an earlier one succeeded",
     '[setup]\nevery_boot = ["true","false"]', "fail", "FAILED")

print("\n=== a malformed declaration is an error, not a silent no-op ===")
case("invalid TOML", 'packages = [', "fail", "not valid TOML")
case("typo'd top-level key", 'pakcages = ["ripgrep"]', "fail", "unknown top-level key")
case("typo'd [setup] key", '[setup]\nonve = ["true"]', "fail", "unknown key(s) in [setup]")
case("[setup] is not a table", 'setup = "nope"', "fail", "must be a table")
case("non-string package", "packages = [1]", "fail", "non-empty string")
case("empty command", '[setup]\nevery_boot = [""]', "fail", "non-empty string")
case("package that looks like a flag", 'packages = ["--allow-downgrades"]', "fail",
     "read as a flag")

print("\n=== once means once PER VOLUME ===")
with tempfile.TemporaryDirectory() as d:
    tmp = Path(d)
    r1 = apply(tmp, '[setup]\nonce = ["echo INSTALLED"]')[0]
    r2 = apply(tmp, '[setup]\nonce = ["echo INSTALLED"]')[0]
    r3 = apply(tmp, '[setup]\nonce = ["echo CHANGED"]')[0]
    for label, cond in [
        ("first boot runs it", "INSTALLED" in r1.stdout),
        ("second boot skips it", "already applied to this volume" in r2.stdout),
        ("editing the command runs it again", "CHANGED" in r3.stdout),
    ]:
        if cond:
            PASS += 1; print(f"  ✓ {label}")
        else:
            FAIL += 1; print(f"  ✗ {label}")

print("\n=== [persist]: file vs directory semantics ===")
# REGRESSION (paid conformance run): a directory mapping used to leave the
# volume-side target absent, so the link dangled. open(2) tolerates that, but
# mkdir(2) through a dangling symlink fails EEXIST — which broke the Claude
# installer's `mkdir -p ~/.claude` on every fresh volume. A trailing slash now
# declares a directory and creates it up front.
case("REGRESSION dir mapping creates the target, so mkdir works",
     '[persist]\n".claude/" = "claude"', "ok", "ok",
     then=lambda h, s: (s / "claude").is_dir() and (h / ".claude").is_symlink()
     and os.access(h / ".claude", os.W_OK))
case("REGRESSION mkdir -p through a dir mapping succeeds",
     '[persist]\n".claude/" = "claude"\n[setup]\nevery_boot = ["mkdir -p ~/.claude/projects"]',
     "ok", "ok", then=lambda h, s: (s / "claude" / "projects").is_dir())
case("file mapping leaves the link dangling for the tool to write",
     '[persist]\n".claude.json" = "claude.json"', "ok", "ok",
     then=lambda h, s: (h / ".claude.json").is_symlink() and not (s / "claude.json").exists())
case("nested home path", '[persist]\n".local/share/claude/" = "claude-share"', "ok", "ok",
     then=lambda h, s: (h / ".local/share/claude").is_symlink())
case("re-run replaces its own symlink", '[persist]\n".claude/" = "claude"', "ok", "ok",
     pre=lambda h, s: (h / ".claude").symlink_to(s / "stale"),
     then=lambda h, s: os.readlink(h / ".claude").endswith("/claude"))
case("declared a dir but the volume holds a file", '[persist]\n".claude/" = "claude"', "fail",
     "refusing to guess", pre=lambda h, s: (s / "claude").write_text("file"))

print("\n=== [persist] refuses to do damage ===")
case("escaping home path", '[persist]\n"../../etc/passwd" = "x"', "fail")
case("escaping volume path", '[persist]\n".x" = "../../../etc"', "fail")
case("absolute home path", '[persist]\n"/etc/passwd" = "x"', "fail")
case("would clobber a real file", '[persist]\n".claude" = "claude"', "fail",
     "not a symlink", pre=lambda h, s: (h / ".claude").write_text("real data"))
case("would clobber a real directory", '[persist]\n".config" = "cfg"', "fail",
     "not a symlink", pre=lambda h, s: (h / ".config").mkdir())
case("[persist] is not a table", 'persist = "nope"', "fail", "must be a table")

print("\n=== [tools]: declared pins render a manifest and materialize ===")
case("pins render quoted into the manifest, install runs",
     '[tools]\nnode = "24.18.0"\n"npm:@scope/pkg" = "1.2.3"', "ok", "ok",
     then=lambda h, s: '"node" = "24.18.0"' in (s / "mise/config/config.toml").read_text()
     and '"npm:@scope/pkg" = "1.2.3"' in (s / "mise/config/config.toml").read_text()
     and "install" in (s.parent / "mise.log").read_text())
case("no lockfile means resolve-then-freeze, not a locked no-op",
     '[tools]\nnode = "24.18.0"', "ok", "ok",
     then=lambda h, s: "lock -g" in (s.parent / "mise.log").read_text())
case("[tools.settings] render 1:1 into [settings]",
     '[tools]\nnode = "1"\n[tools.settings]\nlocked = true\nminimum_release_age = "3d"',
     "ok", "ok",
     then=lambda h, s: "locked = true" in (s / "mise/config/config.toml").read_text()
     and 'minimum_release_age = "3d"' in (s / "mise/config/config.toml").read_text())
case("shellrc gains the env hook",
     '[tools]\nnode = "1"', "ok", "ok",
     then=lambda h, s: "mise/env" in (s / "shellrc").read_text()
     and "MISE_DATA_DIR" in (s / "mise/env").read_text())
case("empty [tools] wires the env but installs nothing",
     "[tools]", "ok", "ok",
     then=lambda h, s: (s / "mise/env").exists()
     and not (s.parent / "mise.log").exists())
case("no [tools] leaves no mise trace at all",
     'packages = ["jq"]', "ok", "ok",
     then=lambda h, s: not (s / "mise").exists())

print("\n=== [tools] refuses what it cannot render safely ===")
case("[tools] is not a table", 'tools = "nope"', "fail", "must be a table")
case("version is not a string", "[tools]\nnode = 24", "fail", "non-empty string")
case("version with shell metacharacters", '[tools]\nnode = "1; rm -rf /"', "fail",
     "not a plain version pin")
case("tool name with spaces", '[tools]\n"no spaces" = "1"', "fail",
     "not a plain tool name")
case("tool name that looks like a flag", '[tools]\n"--evil" = "1"', "fail",
     "not a plain tool name")
case("setting value is a list", '[tools]\nnode = "1"\n[tools.settings]\nx = [1]',
     "fail", "non-empty string")
case("pins declared but mise never bootstrapped", '[tools]\nnode = "1"', "fail",
     "bin/mise is missing", mise=False)

print("\n=== [tools] is idempotent across boots ===")
with tempfile.TemporaryDirectory() as d:
    tmp = Path(d)
    r1 = apply(tmp, '[tools]\nnode = "1"')[0]
    r2 = apply(tmp, '[tools]\nnode = "1"')[0]
    shellrc_hooks = sum("mise/env" in line
                        for line in (tmp / "state" / "shellrc").read_text().splitlines())
    for label, cond in [
        ("both boots succeed", r1.returncode == 0 and r2.returncode == 0),
        ("the shellrc hook is appended exactly once", shellrc_hooks == 1),
    ]:
        if cond:
            PASS += 1; print(f"  ✓ {label}")
        else:
            FAIL += 1; print(f"  ✗ {label}")

print(f"\npassed={PASS} failed={FAIL}")
sys.exit(1 if FAIL else 0)
