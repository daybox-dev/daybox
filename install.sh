#!/bin/bash
# install.sh — wire this clone into the current machine. Idempotent; run it
# again after any `git pull` that touches systemd units, and after
# `cmd/daybox/build.sh` on the mac (symlinked files update on pull automatically).
#
#   macOS  -> laptop role: the daybox Go CLI (built by cmd/daybox/build.sh)
#   Linux  -> control-plane role: daybox CLI, tmux glue, idle-reaper timer
#
# Secrets and per-machine state are NEVER touched: they live in
# ~/.config/daybox/ (token, ts_authkey, config.local, state/).

set -euo pipefail

REPO="$(cd "$(dirname "$0")" && pwd)"

backup() { # preserve a pre-repo regular file once; symlinks are ours already
    local f=$1
    if [ -e "$f" ] && [ ! -L "$f" ] && [ ! -e "$f.pre-repo.bak" ]; then
        cp -a "$f" "$f.pre-repo.bak"
        echo "  (backed up $f -> $f.pre-repo.bak)"
    fi
}

link() {
    local src=$1 dst=$2
    mkdir -p "$(dirname "$dst")"
    backup "$dst"
    ln -sfn "$src" "$dst"
    echo "  $dst -> $src"
}

case "$(uname -s)" in
Darwin)
    echo "[install] mac role: daybox CLI"
    BIN="$REPO/dist/daybox-darwin-arm64"
    [ -f "$BIN" ] || { echo "  no binary at $BIN — run cmd/daybox/build.sh first" >&2; exit 1; }
    mkdir -p "$HOME/bin"
    backup "$HOME/bin/daybox"
    install -m 755 "$BIN" "$HOME/bin/daybox"
    echo "  installed ~/bin/daybox (a build artifact — re-run after cmd/daybox/build.sh)"
    echo "[install] done."
    echo "  - ~/bin must be on PATH"
    ;;
Linux)
    echo "[install] control-plane role: daybox CLI (single Go binary) + reaper timer"
    # One binary now — the Go CLI runs on the plane as well as the laptop
    # (the bash bin/daybox is retired). It reads the cloud-init template +
    # remote/ box-provisioning files from this checkout at runtime, so the
    # repo tree stays; only the entry-point binary changes.
    BIN="$REPO/dist/daybox-linux-amd64"
    if [ ! -f "$BIN" ]; then
        # the release payload (init pushTree / upgrade replaceTree) ships
        # dist/daybox-linux-amd64 in-tree, so a signed install finds it here.
        # A from-source checkout has no dist/ — build it first:
        # cmd/daybox/build.sh.
        echo "  no binary at $BIN — build it first: cmd/daybox/build.sh" >&2
        exit 1
    fi
    mkdir -p "$HOME/.local/bin"
    backup "$HOME/.local/bin/daybox"
    install -m 755 "$BIN" "$HOME/.local/bin/daybox"
    link "$REPO/remote/devbox-tmux" "$HOME/.local/bin/devbox-tmux"
    link "$REPO/remote/tmux.conf"   "$HOME/.tmux.conf"
    mkdir -p "$HOME/.config/daybox/state"

    mkdir -p "$HOME/.config/systemd/user"
    cp "$REPO/systemd/daybox-reaper.service" "$HOME/.config/systemd/user/"
    cp "$REPO/systemd/daybox-reaper.timer"   "$HOME/.config/systemd/user/"
    cp "$REPO/systemd/daybox-relay.service"  "$HOME/.config/systemd/user/"
    cp "$REPO/systemd/daybox-ui.service"     "$HOME/.config/systemd/user/"
    systemctl --user daemon-reload
    systemctl --user enable --now daybox-reaper.timer
    echo "  reaper timer enabled (runs 'daybox reap' every 5min)"
    systemctl --user enable --now daybox-relay
    echo "  relay enabled (box-proposed seed changes: a box proposes, the laptop accepts)"
    systemctl --user enable --now daybox-ui
    echo "  ui enabled (control-plane web UI at 127.0.0.1:4748 — the hoster fronts it; daybox provides no edge)"

    echo "[install] done."
    if [ ! -f "$HOME/.config/daybox/config.local" ]; then
        cat <<'EOF'
NEXT: create your deployment config (the repo is the tool; you are the deployment):
  run 'daybox init' to write ~/.config/daybox/config.local, or create it by hand
  from the REQUIRED values in the README (Configuration).
EOF
    fi
    if [ ! -f "$HOME/.config/daybox/token" ]; then
        cat <<'EOF'
NEXT: create a project-scoped read/write Hetzner API token and store it:
  install -m 600 /dev/stdin ~/.config/daybox/token    # paste token, ctrl-d
then drop device pubkeys in ~/.config/daybox/keys/ and run: daybox setup
EOF
    fi
    ;;
*)
    echo "install.sh: unsupported platform $(uname -s)" >&2
    exit 1
    ;;
esac
