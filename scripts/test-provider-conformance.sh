#!/usr/bin/env bash
# scripts/test-provider-conformance.sh — the provider contract's acceptance
# suite. REAL cloud calls, REAL money: it summons a throwaway box on a temp
# profile ('conform'), walks the whole lifecycle, and tears everything down —
# box, net node, volume. Cost is a few cents (one cheap box for ~minutes plus
# a pro-rated volume). Run it on a control plane before trusting any new
# providers/<name>.sh; select the provider under test the normal way
# (PROVIDER in config.local, or in the conform profile's config).
#
#   scripts/test-provider-conformance.sh --i-know-this-costs-money [server-type]
#
# server-type defaults to cpx11 (cheap, shared vCPU). It must exist at the
# deployment's LOCATION.
#
# One check needs a vantage this script does not have: that the box's public
# :22 is dark FROM THE INTERNET. The control plane is allowlisted by design,
# so from here the port answers. The script asserts the lockdown is ACTIVE
# (ufw: default deny incoming, :22 allowlisted only) and prints the one-liner
# to run from any outside host for the real thing.
set -uo pipefail

REPO_DIR=$(cd "$(dirname "$0")/.." && pwd)
DAYBOX="$REPO_DIR/bin/daybox"
PROFILE=conform
TYPE=${2:-cpx11}

[ "${1:-}" = "--i-know-this-costs-money" ] || {
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}
[ ! -d "$HOME/.config/daybox/state/profiles/$PROFILE" ] \
    || { echo "a '$PROFILE' profile already exists — a previous run left debris?"; \
         echo "inspect it, then: $DAYBOX -p $PROFILE down && $DAYBOX profile rm $PROFILE --purge"; exit 2; }

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ✓ %s\n' "$*"; }
bad() { FAIL=$((FAIL+1)); printf '  ✗ %s\n' "$*"; }
dssh() { "$DAYBOX" -p "$PROFILE" ssh "$@" 2>/dev/null; }

# safety net: whatever happens, no box keeps billing and no debris survives.
# The happy path already tore everything down; these are then quiet no-ops.
cleanup() {
    "$DAYBOX" -p "$PROFILE" down >/dev/null 2>&1
    "$DAYBOX" profile rm "$PROFILE" --purge >/dev/null 2>&1
}
trap cleanup EXIT

echo "=== summon (money starts here) ==="
"$DAYBOX" profile add "$PROFILE" "$TYPE" || { bad "profile add"; exit 1; }
up_out=$("$DAYBOX" -p "$PROFILE" up) || { bad "summon"; exit 1; }
ip=$(printf '%s\n' "$up_out" | awk '/^IP /{print $2}')
[ -n "$ip" ] && ok "summon emitted IP ($ip)" || { bad "summon emitted no IP line"; exit 1; }
printf '%s\n' "$up_out" | grep -q '^HOSTKEY ' \
    && ok "summon emitted the pinned HOSTKEY" || bad "no HOSTKEY line on stdout"

echo
echo "=== the box is what the contract promised ==="
dssh 'mountpoint -q /work' \
    && ok "workspace volume attached + mounted at /work" || bad "/work not mounted"
[ "$(dssh 'cat /var/lib/daybox/seed.status')" = "ok" ] \
    && ok "seed verdict ok" || bad "seed verdict missing or not ok"
lockdown=$(dssh 'sudo ufw status verbose')
printf '%s' "$lockdown" | grep -qi 'deny (incoming)' \
    && ok "ingress lockdown active (default deny incoming)" || bad "ufw not default-deny"
[ "$(printf '%s\n' "$lockdown" | grep -c '^22/tcp.*ALLOW')" = "1" ] \
    && ok "public :22 allowlisted to exactly one address" || bad ":22 ufw rules not exactly one allow"
echo "  → MANUAL (needs an outside vantage): from any host that is NOT the"
echo "    control plane:  nc -z -w 5 $ip 22  — must time out"

echo
echo "=== probe: normalized JSON drives status + idempotent re-summon ==="
st=$("$DAYBOX" -p "$PROFILE" status 2>/dev/null)
printf '%s' "$st" | grep -q "big box: daybox-$PROFILE" \
    && ok "status renders name/id/type/status" || bad "status did not render"
printf '%s' "$st" | grep -q "ip: $ip" \
    && ok "status ip matches summon's" || bad "status ip mismatch"
printf '%s' "$st" | grep -q 'null' \
    && bad "status shows a null field — provider's normalized JSON is incomplete" \
    || ok "no null fields in normalized JSON"
[ "$(printf '%s\n' "$("$DAYBOX" -p "$PROFILE" up)" | awk '/^IP /{print $2}')" = "$ip" ] \
    && ok "re-summon is a no-op returning the same box" || bad "re-summon did not settle on the same box"
"$DAYBOX" net | awk -v n="daybox-$PROFILE" '$2==n' | grep -q 'online.*ephemeral' \
    && ok "box on the net: online + ephemeral" || bad "box not online+ephemeral in 'daybox net'"

echo
echo "=== reap: billing stops, nothing survives ==="
"$DAYBOX" -p "$PROFILE" down \
    && ok "down ran clean" || bad "down failed"
"$DAYBOX" -p "$PROFILE" status 2>/dev/null | grep -q 'no big box running' \
    && ok "probe after reap reads null (box is gone)" || bad "box still probed after down"
"$DAYBOX" net | awk -v n="daybox-$PROFILE" '$2==n' | grep -q . \
    && bad "ghost node left in 'daybox net'" || ok "zero ghosts in the net"
"$DAYBOX" profile rm "$PROFILE" --purge \
    && ok "volume purged, profile gone (money stops here)" || bad "profile rm --purge failed"
[ ! -d "$HOME/.config/daybox/state/profiles/$PROFILE" ] \
    && ok "no state debris" || bad "state dir survived the purge"

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
