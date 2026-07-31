#!/usr/bin/env bash
# scripts/test-ssh-retry.sh — run from anywhere; no cloud, no cost.
# Guards the transport-retry wrappers in bin/daybox:
#   - a transient ssh/scp exit 255 is retried and eventually succeeds
#   - a non-255 failure (the remote command itself) is NOT retried
#   - the retry wrapper forwards args verbatim and keeps LABEL out of them
# Mirrors the harness in test-summon-status.sh: sandbox HOME, source the
# script under DAYBOX_SOURCE_ONLY, stub the commands under test.
set -uo pipefail
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

# stub the backoff sleeper so the suite is instant
sleep() { :; }

echo "=== ssh_retry rides out a transient 255 then succeeds ==="
CALLS=0
ssh() { CALLS=$((CALLS+1)); [ "$CALLS" -lt 3 ] && return 255 || return 0; }
NOTICE_ERR="$HOME/notice.err"
ssh_retry testlabel -o Foo=1 user@host 'cmd' 2>"$NOTICE_ERR" >/dev/null; rc=$?
NOTICE=$(cat "$NOTICE_ERR")
[ "$rc" -eq 0 ] && ok "succeeds (rc=0)" || bad "rc=$rc"
[ "$CALLS" -eq 3 ] && ok "tried 3 times (2 retries)" || bad "calls=$CALLS want 3"
# log() passes the message as one arg to a single-`%s` printf, so format
# specifiers in the message are NOT re-interpreted — a regression that
# builds the notice with `log "%s ..." args` prints literal %s/%d. Catch it.
case "$NOTICE" in
  *'%s'*|*'%d'*) bad "notice has literal format specifiers: $NOTICE" ;;
  *"testlabel unreachable (ssh 255) — retry 1/2 in 2s"*) ok "notice rendered: $(printf '%s' "$NOTICE" | head -1)" ;;
  *) bad "unexpected notice: $NOTICE" ;;
esac

echo "=== ssh_retry gives up after 3 attempts of 255 ==="
CALLS=0
ssh() { CALLS=$((CALLS+1)); return 255; }
ssh_retry testlabel user@host 'cmd'; rc=$?
[ "$rc" -eq 255 ] && ok "returns 255" || bad "rc=$rc"
[ "$CALLS" -eq 3 ] && ok "stopped at 3 attempts" || bad "calls=$CALLS want 3"

echo "=== ssh_retry does NOT retry a non-255 failure ==="
CALLS=0
ssh() { CALLS=$((CALLS+1)); return 1; }
ssh_retry testlabel user@host 'cmd'; rc=$?
[ "$rc" -eq 1 ] && ok "returns 1 (the command's own code)" || bad "rc=$rc"
[ "$CALLS" -eq 1 ] && ok "tried once — no retry" || bad "calls=$CALLS want 1"

echo "=== scp_retry rides out a transient 255 then succeeds ==="
CALLS=0
scp() { CALLS=$((CALLS+1)); [ "$CALLS" -lt 2 ] && return 255 || return 0; }
scp_retry testlabel -q src user@host:dst; rc=$?
[ "$rc" -eq 0 ] && ok "succeeds (rc=0)" || bad "rc=$rc"
[ "$CALLS" -eq 2 ] && ok "tried 2 times (1 retry)" || bad "calls=$CALLS want 2"

echo "=== ssh_retry forwards args verbatim and keeps LABEL out of them ==="
SEEN=""
ssh() { SEEN="$*"; return 0; }
ssh_retry mylabel -o Opt=1 -o Opt2=2 user@host 'the command'; rc=$?
[ "$rc" -eq 0 ] || bad "rc=$rc"
case "$SEEN" in
  *"-o Opt=1"*"-o Opt2=2"*"user@host"*"the command"*) ok "args forwarded: $SEEN" ;;
  *) bad "args mangled: $SEEN" ;;
esac
case "$SEEN" in
  *mylabel*) bad "label leaked into ssh args: $SEEN" ;;
  *) ok "label not forwarded to ssh" ;;
esac

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
