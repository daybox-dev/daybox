#!/usr/bin/env bash
# scripts/test-provider-select.sh — run from anywhere; no cloud calls, no cost.
# Exercise per-profile provider selection with stub provider files. The wrong
# provider makes probe read `null`, which makes the reaper reset its counters
# and never stop the billing — so which file derive_profile loads, and that a
# profile's choice never LEAKS into the next profile in a loop, deserve a test.
set -uo pipefail
# sandbox HOME: sourcing bin/daybox reads $HOME/.config/daybox and creates
# state dirs — the test must never touch (or depend on) the real deployment
HOME=$(mktemp -d); export HOME
export DAYBOX_SOURCE_ONLY=1
# shellcheck disable=SC1091
source "$(cd "$(dirname "$0")/.." && pwd)/bin/daybox"
unset DAYBOX_SOURCE_ONLY

PASS=0; FAIL=0

# a fake deployment: three profiles, two stub providers + a stub default.
# Each stub only stamps a marker — which marker is live IS the assertion.
TMP=$(mktemp -d); trap 'rm -rf "$TMP" "$HOME"' EXIT
CONF_DIR="$TMP/conf"; STATE_DIR="$TMP/state"; REPO_DIR="$TMP/repo"
mkdir -p "$REPO_DIR/providers" \
         "$CONF_DIR/profiles/pa" "$CONF_DIR/profiles/pb" "$CONF_DIR/profiles/pc"
for p in base fakea fakeb; do
    printf 'PROVIDER_MARKER=%s\n' "$p" > "$REPO_DIR/providers/$p.sh"
done
echo "PROVIDER=fakea" > "$CONF_DIR/profiles/pa/config"
echo "PROVIDER=fakeb" > "$CONF_DIR/profiles/pb/config"
: > "$CONF_DIR/profiles/pc/config"    # picks nothing — deployment default

DEPLOY_PROVIDER=base
_PROVIDER_LOADED=""
PROVIDER_MARKER=""

check() { # check NAME PROFILE EXPECTED_MARKER
    local name=$1 profile=$2 want=$3
    derive_profile "$profile"
    if [ "$PROVIDER_MARKER" = "$want" ]; then
        PASS=$((PASS+1)); printf '  ✓ %-46s -> %s\n' "$name" "$PROVIDER_MARKER"
    else
        FAIL=$((FAIL+1)); printf '  ✗ %-46s -> %s (want %s)\n' "$name" "$PROVIDER_MARKER" "$want"
    fi
}

echo "=== each profile loads ITS provider ==="
check "profile pa picks fakea"                 pa fakea
check "profile pb picks fakeb"                 pb fakeb
check "back to pa reloads fakea"               pa fakea

echo
echo "=== the leak: a loop must not carry one profile's choice forward ==="
derive_profile pb
check "pc after pb falls back to the default"  pc base
check "pa after pc still picks fakea"          pa fakea

echo
echo "=== EVERY knob resets between profiles, not just PROVIDER ==="
# One profile disabling the runaway cap (or changing REMOTE_USER) must never
# leak into the next profile in a loop: a leaked MAX_LIFETIME_HOURS=0 removes
# the next box's billing backstop; a leaked REMOTE_USER makes its reaper
# probe fail as the wrong user until the 1h rule force-reaps a healthy box.
printf 'MAX_LIFETIME_HOURS=0\nREMOTE_USER=admin\n' >> "$CONF_DIR/profiles/pa/config"
DEPLOY_MAX_LIFETIME_HOURS=12
DEPLOY_REMOTE_USER=dev
derive_profile pa
if [ "$MAX_LIFETIME_HOURS" = 0 ] && [ "$REMOTE_USER" = admin ]; then
    PASS=$((PASS+1)); echo "  ✓ pa's own overrides apply (cap=0, user=admin)"
else
    FAIL=$((FAIL+1)); echo "  ✗ pa's overrides did not apply (cap=$MAX_LIFETIME_HOURS user=$REMOTE_USER)"
fi
derive_profile pc
if [ "$MAX_LIFETIME_HOURS" = 12 ] && [ "$REMOTE_USER" = dev ]; then
    PASS=$((PASS+1)); echo "  ✓ pc after pa is back to the deployment baseline"
else
    FAIL=$((FAIL+1)); echo "  ✗ pa leaked into pc (cap=$MAX_LIFETIME_HOURS user=$REMOTE_USER)"
fi

echo
echo "=== bad providers die instead of loading ==="
mkdir -p "$CONF_DIR/profiles/pd" "$CONF_DIR/profiles/pe"
echo "PROVIDER=../evil" > "$CONF_DIR/profiles/pd/config"
echo "PROVIDER=nofile" > "$CONF_DIR/profiles/pe/config"
for case in "pd:traversal in PROVIDER dies" "pe:missing provider file dies"; do
    p=${case%%:*}; name=${case#*:}
    if ( derive_profile "$p" ) >/dev/null 2>&1; then
        FAIL=$((FAIL+1)); printf '  ✗ %s\n' "$name"
    else
        PASS=$((PASS+1)); printf '  ✓ %s\n' "$name"
    fi
done

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
