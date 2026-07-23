#!/usr/bin/env bash
# scripts/test-summon-status.sh — run from anywhere; no cloud calls, no cost.
# Two regressions guarded here:
#   1. a failed server create must ABORT the summon — die inside $(...) exits
#      only that subshell, so without explicit checks cmd_up once proceeded
#      with an empty id/ip ("server  created, ip  —") and hung
#   2. bare `daybox status` shows EVERY profile plus the net table (the
#      consolidated view the README promises); -p scopes to one profile
set -uo pipefail
# sandbox HOME: sourcing bin/daybox reads $HOME/.config/daybox and creates
# state dirs — the test must never touch (or depend on) the real deployment
HOME=$(mktemp -d); export HOME
trap 'rm -rf "$HOME"' EXIT
export DAYBOX_SOURCE_ONLY=1
# shellcheck disable=SC1091
source "$(cd "$(dirname "$0")/.." && pwd)/bin/daybox"
unset DAYBOX_SOURCE_ONLY
# sourcing bin/daybox enables errexit; this harness asserts on failures
# explicitly, so a failing call under test must not kill the run
set +e

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ✓ $*"; }
bad() { FAIL=$((FAIL+1)); echo "  ✗ $*"; }

echo "=== provider_summon refuses an empty create response ==="
# mimic the real failure (2026-07-23, dedicated-core quota): api's die exits
# only the $(...) subshell, so provider_summon sees an empty $resp
api() { echo "[daybox] ERROR: API POST /servers: quota" >&2; exit 1; }
_hz_ssh_key_names() { echo '["k"]'; }
out=$(printf 'ud' | provider_summon name ccx33 img loc 123 2>&1); rc=$?
[ "$rc" -ne 0 ] && ok "non-zero exit (rc=$rc)" || bad "exited 0"
case "$out" in
  *"created, ip"*)         bad "still announced a created server: $out" ;;
  *"nothing was created"*) ok  "clean abort message" ;;
  *)                       bad "unexpected output: $out" ;;
esac

echo "=== cmd_up dies when provider_summon fails ==="
need_config() { :; }; provider_check_credentials() { :; }
need_seed() { :; }; need_net() { :; }
provider_probe() { echo null; }
volume_id() { echo 1; }
provider_volume_attached_to() { echo ""; }
render_user_data() { echo ud; }
provider_user_data_max_bytes() { echo 99999; }
provider_price_hourly() { echo 0.03; }
provider_summon() { exit 1; } # create failed
wait_ready() { echo "WAIT_READY_REACHED"; }
out=$( (cmd_up cpx11) 2>&1 ); rc=$?
[ "$rc" -ne 0 ] && ok "cmd_up aborts (rc=$rc)" || bad "cmd_up exited 0"
case "$out" in *WAIT_READY_REACHED*) bad "kept going after failed summon" ;;
               *"summon failed"*)    ok  "names the failure" ;;
               *)                    bad "message: $out" ;; esac

echo "=== bare status: every profile + the net table ==="
mkdir -p "$STATE_DIR/profiles/alpha" "$STATE_DIR/profiles/beta"
PROFILE_EXPLICIT=""
derive_profile() { PROFILE=$1; SERVER_NAME="daybox-$1"; PROFILE_STATE="$STATE_DIR/profiles/$1"; }
provider_has_credentials() { :; }
provider_probe() {
  if [ "$1" = "daybox-alpha" ]; then
    printf '{"id":"1","name":"daybox-alpha","ip":"192.0.2.9",'
    printf '"status":"running","created":"2026-07-23T15:25:03Z","type":"ccx33"}'
  else echo null; fi
}
provider_price_hourly() { echo 0.2259; }
box_age_min() { echo 80; }
net_enabled() { :; }
net_table() { echo "ID NAME (stub table)"; }
REAP_AFTER_IDLE_MIN=30; MAX_LIFETIME_HOURS=12
out=$(cmd_status 2>&1); rc=$?
[ "$rc" -eq 0 ] && ok "exits 0" || bad "rc=$rc: $out"
case "$out" in *"profile 'alpha':"*"daybox-alpha"*) ok "running profile block" ;;
               *) bad "no alpha block: $out" ;; esac
case "$out" in *"profile 'beta':"*"no box running"*) ok "empty profile block" ;;
               *) bad "no beta block: $out" ;; esac
case "$out" in *"net members:"*"stub table"*) ok "net table appended" ;;
               *) bad "no net table: $out" ;; esac

echo "=== status -p scopes to one profile ==="
PROFILE_EXPLICIT=1
PROFILE=beta; SERVER_NAME=daybox-beta; PROFILE_STATE="$STATE_DIR/profiles/beta"
out=$(cmd_status 2>&1); rc=$?
case "$out" in *alpha*) bad "-p leaked other profiles: $out" ;;
               *"profile 'beta':"*) ok "single block only" ;;
               *) bad "output: $out" ;; esac
case "$out" in *"net members:"*) bad "net table in scoped view" ;;
               *) ok "no net table when scoped" ;; esac

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
