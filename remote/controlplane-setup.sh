#!/bin/bash
# controlplane-setup.sh — turn this box into a daybox control plane.
# Run by `daybox init` over ssh; IDEMPOTENT: every step skips what already
# exists, so re-running heals drift and never clobbers a working deployment.
#
# env: PUBLIC_IP (required)  GIT_NAME  GIT_EMAIL  NET_USER  NET_PORT
#      HEADSCALE_VERSION
set -euo pipefail

: "${PUBLIC_IP:?PUBLIC_IP required}"
: "${NET_USER:=dev}"
: "${NET_PORT:=8080}"
: "${HEADSCALE_VERSION:=0.29.2}"
# sha256 of headscale_${HEADSCALE_VERSION}_linux_amd64.deb, pinned HERE so
# integrity does not depend on the host serving the artifact. Overriding
# HEADSCALE_VERSION requires overriding this too.
: "${HEADSCALE_SHA256:=eeee36b3e9ca9db780d3836426bdc8041938992bea3e9201e88b2f22aeda6a24}"

log() { printf '[setup] %s\n' "$*" >&2; }
REPO="$HOME/daybox"
CONF="$HOME/.config/daybox"
[ -d "$REPO" ] || { log "ERROR: ~/daybox missing (init pushes it first)"; exit 1; }
sudo -n true 2>/dev/null || { log "ERROR: need passwordless sudo"; exit 1; }

log "packages"
sudo DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null
sudo DEBIAN_FRONTEND=noninteractive apt-get -qq install -y \
    git jq curl tmux ca-certificates >/dev/null

mkdir -p "$CONF/state" "$CONF/keys" "$CONF/agent"

# The control plane must be able to ssh into the boxes it summons (net join,
# wait_ready, reaper probe, volume unmount). Give it its OWN keypair and
# authorize it on every box: its pubkey joins KEYS_DIR, which seeds each
# box's authorized_keys at summon (and is registered with the provider by
# `daybox setup`). The private key never leaves this box. Idempotent.
if [ ! -f "$HOME/.ssh/id_ed25519" ]; then
    log "generating the control-plane ssh key (to reach summoned boxes)"
    install -d -m 700 "$HOME/.ssh"
    ssh-keygen -t ed25519 -N '' -q -C "daybox-control@$(hostname)" -f "$HOME/.ssh/id_ed25519"
fi
cp "$HOME/.ssh/id_ed25519.pub" "$CONF/keys/control-plane.pub"

if [ ! -f "$CONF/config.local" ]; then
    log "writing config.local (deployment identity)"
    umask 077
    cat > "$CONF/config.local" <<EOF
# daybox deployment config — written by controlplane-setup.sh
LITTLEBOX_IP=$PUBLIC_IP
GIT_NAME="${GIT_NAME:-}"
GIT_EMAIL=${GIT_EMAIL:-}
NET_USER=$NET_USER
NET_PORT=$NET_PORT
EOF
else
    log "config.local exists — leaving it"
    # …except to heal the one omission that breaks every summon: the
    # headscale user is created below with $NET_USER, but bin/daybox
    # defaults NET_USER=dev. If config.local never pinned it, a deployment
    # with any other name fails closed at net join on every 'daybox up'
    # (paid summon, teardown, repeat). Append, never rewrite.
    if ! grep -q '^NET_USER=' "$CONF/config.local"; then
        log "config.local lacks NET_USER — appending NET_USER=$NET_USER"
        printf 'NET_USER=%s\n' "$NET_USER" >> "$CONF/config.local"
    fi
    if ! grep -q '^NET_PORT=' "$CONF/config.local"; then
        printf 'NET_PORT=%s\n' "$NET_PORT" >> "$CONF/config.local"
    fi
fi

# user units (the idle reaper) must outlive ssh sessions
sudo loginctl enable-linger "$USER" 2>/dev/null || true
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"

log "installing the CLI + reaper (install.sh)"
"$REPO/install.sh"

# ---------------------------------------------------------- headscale ----
if ! command -v headscale >/dev/null; then
    log "installing headscale v$HEADSCALE_VERSION (pinned checksum)"
    # verified against the sha256 pinned above — NOT against a checksums
    # file fetched from the same release, which only proves transport
    # integrity and would pass a compromised release by construction
    cd "$(mktemp -d)"
    curl -fsSLO "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_amd64.deb"
    echo "$HEADSCALE_SHA256  headscale_${HEADSCALE_VERSION}_linux_amd64.deb" | sha256sum -c - >/dev/null \
        || { log "ERROR: headscale .deb does not match the pinned sha256"; exit 1; }
    sudo dpkg -i "headscale_${HEADSCALE_VERSION}_linux_amd64.deb" >/dev/null
else
    log "headscale already installed ($(headscale version 2>/dev/null || true))"
fi

if ! sudo grep -q "server_url: http://$PUBLIC_IP:$NET_PORT" /etc/headscale/config.yaml 2>/dev/null; then
    log "writing /etc/headscale/config.yaml (server_url http://$PUBLIC_IP:$NET_PORT)"
    sed -e "s|CONTROL_PLANE_IP:8080|$PUBLIC_IP:$NET_PORT|" \
        -e "s|0.0.0.0:8080|0.0.0.0:$NET_PORT|" \
        "$REPO/headscale/config.template.yaml" | sudo tee /etc/headscale/config.yaml >/dev/null
    sudo systemctl restart headscale
else
    log "headscale config already points at this box — leaving it"
fi

sudo usermod -aG headscale "$USER"
if sudo ufw status 2>/dev/null | grep -q "Status: active"; then
    sudo ufw allow "$NET_PORT/tcp" comment "headscale control" >/dev/null
fi
sudo systemctl enable --now headscale >/dev/null 2>&1 || true

for i in $(seq 1 15); do
    curl -fsS "http://127.0.0.1:$NET_PORT/health" >/dev/null 2>&1 && break
    sleep 2
done
curl -fsS "http://127.0.0.1:$NET_PORT/health" >/dev/null || { log "ERROR: headscale unhealthy"; exit 1; }
log "headscale healthy"

if ! sudo headscale users list -o json | jq -e --arg u "$NET_USER" \
        '(. // [])[] | select(.name==$u)' >/dev/null 2>&1; then
    log "creating headscale user '$NET_USER'"
    sudo headscale users create "$NET_USER"
fi

log "control plane ready"
