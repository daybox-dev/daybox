#!/bin/bash
# Render cloud-init with the REAL bash renderer into stdout, sandboxed.
# Setup BEFORE source so config.local + keys exist at the source-time
# config.local sourcing + KEYS_DIR resolution.
set -uo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
HOME=$(mktemp -d); export HOME
CONF_DIR="$HOME/.config/daybox"
STATE_DIR="$CONF_DIR/state"
mkdir -p "$CONF_DIR/keys" "$STATE_DIR/profiles/default" "$CONF_DIR/profiles/default"
printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIETK/JI88OZihytMTWNWbOmQhLPVXFEKtw4sLg5XTVMx laptop\n' > "$CONF_DIR/keys/laptop.pub"
printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkey2macbookairabcdef1234567890 mac\n' > "$CONF_DIR/keys/mac.pub"
cat > "$CONF_DIR/config.local" <<CFG
LITTLEBOX_IP=203.0.113.10
GIT_NAME="Alice O'Brien"
GIT_EMAIL="alice@example.com"
REMOTE_USER=dev
PROVIDER=hetzner
SERVER_TYPE=ccx33
LOCATION=hil
NET_USER=dev
NET_PORT=8080
CFG
printf '123456' > "$STATE_DIR/profiles/default/volume_id"
cat > "$CONF_DIR/profiles/default/profile.toml" <<SEED
# this is a comment that must be stripped from the seed
[meta]
owner = "alice"

[packages]
apt = ["ripgrep", "htop"]

[setup]
# another stripped comment
once = """
echo hello
# this #-line is INSIDE a multiline string — it must survive (content)
echo done
"""
SEED
export DAYBOX_SOURCE_ONLY=1
source "$REPO/bin/daybox"
unset DAYBOX_SOURCE_ONLY
render_user_data 123456
