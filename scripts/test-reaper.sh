#!/usr/bin/env bash
# scripts/test-reaper.sh — run from anywhere; no cloud calls, no cost.
# Exercise the reaper's hard lifetime cap with stubs. The cap force-deletes a
# RUNNING machine, so each case asserts exactly whether a reap happened.
set -uo pipefail
# sandbox HOME: sourcing bin/daybox reads $HOME/.config/daybox and creates
# state dirs — the test must never touch (or depend on) the real deployment
HOME=$(mktemp -d); export HOME
trap 'rm -rf "$HOME"' EXIT
export DAYBOX_SOURCE_ONLY=1
# shellcheck disable=SC1091
source "$(cd "$(dirname "$0")/.." && pwd)/bin/daybox"
unset DAYBOX_SOURCE_ONLY

PASS=0; FAIL=0
PROFILE=test
LITTLEBOX_IP=203.0.113.1
REMOTE_USER=dev
KNOWN_HOSTS=/dev/null

# ISO timestamp N minutes in the past
ago() { date -d "-$1 minutes" +%Y-%m-%dT%H:%M:%S+00:00; }

check() { # check NAME CREATED CAP_HOURS EXPECT(reap|keep) [EXPECT_LOG_SUBSTR]
  local name=$1 created=$2 cap=$3 expect=$4 wantlog=${5:-}
  PROFILE_STATE=$(mktemp -d); REAPED=""; LOGS=""
  MAX_LIFETIME_HOURS=$cap
  echo 0 > "$PROFILE_STATE/idle_ticks"; echo 0 > "$PROFILE_STATE/unreachable_ticks"
  cmd_down() { REAPED=yes; }
  log() { LOGS+="$* "; }
  provider_probe() { printf '{"created":"%s","ip":"192.0.2.1"}' "$created"; }
  # ssh to 192.0.2.1 (TEST-NET-1) cannot connect, so the probe takes the
  # unreachable branch — which is fine: the cap is checked BEFORE the probe,
  # and that ordering is exactly what makes it independent of busy signals.
  local rc=0
  if reap_one >/dev/null 2>&1; then rc=0; else rc=$?; fi

  local got=keep; [ -n "$REAPED" ] && got=reap
  local ok=1
  [ "$got" = "$expect" ] || ok=0
  # reap_one MUST exit 0: it runs under set -e inside a loop over
  # profiles, so non-zero would silently stop reaping the rest.
  if [ "$rc" -ne 0 ]; then ok=0; printf "      reap_one exited %s (must be 0)\n" "$rc"; fi
  [ -n "$wantlog" ] && [[ "$LOGS" != *"$wantlog"* ]] && ok=0
  if [ "$ok" = 1 ]; then
    PASS=$((PASS+1)); printf '  ✓ %-44s -> %s\n' "$name" "$got"
  else
    FAIL=$((FAIL+1)); printf '  ✗ %-44s -> %s (want %s%s)\n' "$name" "$got" "$expect" \
      "${wantlog:+, log~'$wantlog'}"
    printf '      logs: %s\n' "$LOGS"
  fi
  rm -rf "$PROFILE_STATE"
}

echo "=== the runaway backstop: reap regardless of activity ==="
check "13h old, cap 12h"            "$(ago 780)"  12 reap "LIFETIME CAP"
check "exactly at the cap"          "$(ago 720)"  12 reap "LIFETIME CAP"
check "one minute under the cap"    "$(ago 719)"  12 keep
check "fresh box"                   "$(ago 5)"    12 keep

echo
echo "=== warning before the axe ==="
check "29min left -> warns, keeps"  "$(ago 691)"  12 keep "lifetime cap in"
check "31min left -> silent, keeps" "$(ago 689)"  12 keep

echo
echo "=== disabled / malformed input must never reap ==="
check "cap disabled, 100h old"      "$(ago 6000)" 0  keep
check "unparseable timestamp"       "not-a-date"  12 keep "could not parse"
check "empty timestamp"             ""            12 keep "could not parse"
check "jq null (missing .created)"  "null"        12 keep "could not parse"
check "whitespace timestamp"        "   "         12 keep "could not parse"
check "future timestamp (bad clock)" "$(date -d '+2 hours' +%Y-%m-%dT%H:%M:%S+00:00)" 12 keep

echo
echo "=== box_age_min directly ==="
for fmt in "2026-07-21T15:32:14+00:00" "2026-07-21T15:32:14Z"; do
  if box_age_min "$fmt" >/dev/null; then
    PASS=$((PASS+1)); printf '  ✓ parses %s\n' "$fmt"
  else FAIL=$((FAIL+1)); printf '  ✗ FAILED to parse %s\n' "$fmt"; fi
done
if box_age_min "garbage" >/dev/null 2>&1; then
  FAIL=$((FAIL+1)); echo "  ✗ accepted garbage timestamp"
else PASS=$((PASS+1)); echo "  ✓ rejects garbage timestamp"; fi

echo
echo "=== cmd_reap: one broken profile must not stop the others ==="
# The loop's belt-and-braces (`reap_one || log`) contains a non-zero RETURN,
# but die calls `exit` — only the per-profile subshell contains that. Three
# profiles; deriving the second dies (broken config); the third must still
# be checked, because its box is still billing.
MARK=$(mktemp)
LITTLEBOX_IP=203.0.113.1
list_profiles() { printf 'alpha\nbroken\ncharlie\n'; }
derive_profile() { PROFILE=$1; [ "$1" != broken ] || die "stub: bad provider in profile config"; }
reap_one() { echo "$PROFILE" >> "$MARK"; }
provider_has_credentials() { return 0; }
log() { :; }
( cmd_reap ) >/dev/null 2>&1
rc=$?
ok=1
[ "$rc" -eq 0 ] || { ok=0; echo "      cmd_reap exited $rc (must be 0)"; }
grep -qx alpha   "$MARK" || ok=0
grep -qx charlie "$MARK" || { ok=0; echo "      profile AFTER the broken one was never checked"; }
grep -qx broken  "$MARK" && { ok=0; echo "      broken profile was reaped anyway"; }
if [ "$ok" = 1 ]; then
  PASS=$((PASS+1)); echo "  ✓ a die in one profile is contained; later profiles still checked"
else
  FAIL=$((FAIL+1)); echo "  ✗ exit containment failed (checked: $(tr '\n' ' ' < "$MARK"))"
fi
rm -f "$MARK"

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
