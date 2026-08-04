#!/bin/bash
# release.sh — cut, sign, publish, verify. The one release command.
#
# This is the publish half that cut.sh deliberately doesn't have. cut.sh
# builds the artifacts offline (keyless, reproducible); this script takes a
# cut release the rest of the way: signs SHA256SUMS with the release key,
# uploads every artifact to R2, and verifies the live release end-to-end
# before printing ✓. Nothing publishes or verifies by hand anymore — the
# checklist that used to be printed at the end of cut.sh IS this script.
#
# THIS IS NOT CI, AND MUST NOT BECOME CI. A release pipeline with publish
# rights is precisely the supply-chain surface daybox defends against
# (SECURITY.md, iron rule 3). This runs only on the trusted laptop, only
# with a human y/N at the one irreversible step (the first R2 PUT), and
# only for a version you name explicitly — there is no default-to-latest
# footgun.
#
# Why the control plane uploads, not the laptop: the R2 credentials live
# ONLY on the control plane (~/.config/daybox/r2, mode 600 — never copied,
# never on the laptop, never in argv). So this script ships the signed
# artifacts to the control plane over ssh and runs the PUTs there, the same
# laptop-initiated / control-plane-credentialed shape `daybox upgrade` uses.
# The signing key is the mirror case: its secret half lives ONLY in this
# Mac's login Keychain, so signing happens on the laptop, never on the box.
#
# Usage:
#   scripts/release.sh v0.1.0        # cut (if needed) -> sign -> publish -> verify
#
# Prerequisites: a clean tree tagged <version> (cut.sh enforces this), the
# minisign secret key in the Keychain (item 'daybox-release-signing'), and a
# config.local with CONTROL_HOST set (the box with the R2 creds).
set -euo pipefail

BIN=daybox
KEYCHAIN_ITEM=daybox-release-signing

cd "$(dirname "$0")/.."
ROOT=$(pwd)
DIST="$ROOT/dist"
CONFIG="$HOME/.config/daybox/config.local"

die() { echo "release.sh: $*" >&2; exit 1; }
say() { echo "• $*"; }

# ---- version (required, explicit — no default) ----
[ $# -ge 1 ] || die "usage: scripts/release.sh <version>   (e.g. v0.2.8)
  cut a release first: scripts/cut.sh $VERSION  (or this script runs it)"
VERSION=$1
case "$VERSION" in
    v[0-9]*) ;;
    *) die "version must look like v0.2.8 (got '$VERSION')" ;;
esac

# The release signing key's PUBLIC half — one source of truth, extracted
# from the same file cut.sh cross-checks against web/install.sh. Verifying
# the signature against THIS key is what binds a published release to the
# key pinned in every shipped binary and installer.
PUBKEY=$(sed -n 's/^var minisignPubKey = "\(RW[A-Za-z0-9+/=]*\)".*/\1/p' "$ROOT/cmd/daybox/payload.go")
[ -n "$PUBKEY" ] || die "no minisignPubKey in cmd/daybox/payload.go — refusing to publish"
BINKEY=$PUBKEY
SHKEY=$(sed -n 's/^DAYBOX_MINISIGN_PUBKEY="\([^"]*\)".*/\1/p' "$ROOT/web/install.sh")
[ "$BINKEY" = "$SHKEY" ] || die "pinned keys differ between payload.go and install.sh — rotate BOTH, then cut"

# The control plane: where the R2 creds live. Same host `daybox upgrade`
# targets, read the same way.
[ -f "$CONFIG" ] || die "no $CONFIG — this laptop has no deployment to publish from"
CONTROL=$(sed -n 's/^CONTROL_HOST=//p' "$CONFIG" | tr -d '"' | head -1)
[ -n "$CONTROL" ] || die "no CONTROL_HOST in $CONFIG — publish needs the box with the R2 creds"

# ---- 1. cut (idempotent: skip if this version's artifacts already exist) ----
# Re-running release.sh for the same version (e.g. to re-verify after a
# cache poke) must not rebuild and re-sign over a good dist/. A complete,
# matching dist/ — src tarball named for this version + sums + stamped
# installer — is accepted as-is; anything missing and we cut fresh.
if ! [ -f "$DIST/$BIN-$VERSION-src.tar.gz" ] || ! [ -f "$DIST/SHA256SUMS" ] \
   || ! [ -f "$DIST/install.sh" ]; then
    say "cutting $VERSION (no complete dist/ for it)"
    "$ROOT/scripts/cut.sh" "$VERSION"
else
    say "dist/ already has a cut for $VERSION — skipping cut"
fi

cd "$DIST"
ARTIFACTS="$BIN-agent-linux-amd64 $BIN-controlplane.tar.gz $BIN-darwin-amd64 $BIN-darwin-arm64 $BIN-linux-amd64 $BIN-linux-arm64 $BIN-$VERSION-src.tar.gz"
for f in $ARTIFACTS SHA256SUMS; do
    [ -f "$f" ] || die "dist/ is missing $f — re-run: scripts/cut.sh $VERSION"
done

# ---- 2. sign SHA256SUMS with the release key (Keychain-gated) ----
# The secret key never touches disk in plaintext outside the signing instant.
# The version: token is REQUIRED — 'daybox init' refuses a signed SHA256SUMS
# that does not attest the version it was fetched for (rollback protection).
if [ -f SHA256SUMS.minisig ] && minisign -Vm SHA256SUMS -x SHA256SUMS.minisig -P "$PUBKEY" >/dev/null 2>&1; then
    say "SHA256SUMS already signed for $VERSION (verifies against pinned key) — skipping sign"
else
    say "signing SHA256SUMS (Keychain item '$KEYCHAIN_ITEM')"
    KEYFILE=$(mktemp /tmp/daybox-ms.XXXXXX) || die "mktemp failed"
    chmod 600 "$KEYFILE"
    trap 'rm -f "$KEYFILE"' EXIT
    if ! security find-generic-password -s "$KEYCHAIN_ITEM" -w 2>/dev/null | base64 -d > "$KEYFILE" 2>/dev/null || [ ! -s "$KEYFILE" ]; then
        die "could not retrieve minisign secret key from Keychain item '$KEYCHAIN_ITEM'"
    fi
    minisign -Sm SHA256SUMS -s "$KEYFILE" -t "file:SHA256SUMS version:$VERSION"
    rm -f "$KEYFILE"; trap - EXIT
    # Verify our own signature against the pinned public key BEFORE shipping:
    # a signature that doesn't verify here would fail every installer too.
    minisign -Vm SHA256SUMS -x SHA256SUMS.minisig -P "$PUBKEY" >/dev/null \
        || die "signature does not verify against the pinned key — aborting before publish"
fi
[ -f SHA256SUMS.minisig ] || die "no SHA256SUMS.minisig"

# ---- 3. approval: the one irreversible step ----
echo
echo "About to publish $VERSION to R2 (daybox.dev):"
echo "  /dl/$VERSION/  and  /dl/latest/   (artifacts + signed sums)"
echo "  /install.sh                       (pins $VERSION)"
echo "  /src/$VERSION/  and  /src/latest/ (browsable source tree)"
echo "  control plane: $CONTROL (R2 creds live here)"
echo
read -rp "publish $VERSION to R2? [y/N] " ans
[ "$ans" = "y" ] || die "aborted — dist/ is signed and ready; re-run to publish"

# ---- 4. ship to the control plane + upload from there ----
# COPYFILE_DISABLE kills the ._appleDouble + LIBARCHIVE.xattr noise that
# otherwise rides tar out of macOS and lands as junk in the staging dir.
STAGE="release-staging/$VERSION"
say "shipping artifacts to $CONTROL:$STAGE"
COPYFILE_DISABLE=1 tar czf - $ARTIFACTS SHA256SUMS SHA256SUMS.minisig install.sh \
    | ssh -o BatchMode=yes "$CONTROL" "rm -rf ~/$STAGE && mkdir -p ~/$STAGE && tar xzf - -C ~/$STAGE" \
    || die "shipping artifacts to $CONTROL failed"

say "uploading to R2 from $CONTROL (creds stay there)"
# Quoted heredoc: nothing expands on the laptop; $1 carries VERSION to the
# remote shell, the R2 creds are sourced on the box. The worker strips /dl/
# from the URL, so bucket keys are <version>/<file> and latest/<file> — NO
# dl/ prefix (an upload to dl/... keys is silently never served; it bit once).
ssh -o BatchMode=yes "$CONTROL" 'bash -s' "$VERSION" <<'UPLOAD'
set -euo pipefail
VERSION=$1
. ~/.config/daybox/r2
[ -n "${CF_ACCOUNT_ID:-}" ] && [ -n "${R2_ACCESS_KEY_ID:-}" ] && [ -n "${R2_SECRET_ACCESS_KEY:-}" ] \
    || { echo "release.sh: missing R2 creds in ~/.config/daybox/r2" >&2; exit 1; }
BUCKET=daybox-releases
STAGE=~/release-staging/$VERSION
cd "$STAGE"

put() { # put <r2-key> <file> <content-type>
    curl -fsS -X PUT \
        "https://${CF_ACCOUNT_ID}.r2.cloudflarestorage.com/${BUCKET}/$1" \
        --aws-sigv4 "aws:amz:auto:s3" \
        --user "${R2_ACCESS_KEY_ID}:${R2_SECRET_ACCESS_KEY}" \
        -H "content-type: $3" \
        --data-binary "@$2" >/dev/null \
        || { echo "release.sh: upload of $2 -> $1 failed" >&2; exit 1; }
}

for f in daybox-* SHA256SUMS SHA256SUMS.minisig; do
    [ -e "$f" ] || continue
    case "$f" in
        *.tar.gz) ct="application/gzip" ;;
        SHA256SUMS|*.minisig) ct="text/plain; charset=utf-8" ;;
        *) ct="application/octet-stream" ;;
    esac
    put "$VERSION/$f" "$f" "$ct"
    put "latest/$f" "$f" "$ct"
done

# The installer pins this release's sums hash; the previously-served one
# attests an older release, so leaving it makes the one-liner fail. NOT optional.
put "site/install.sh" "install.sh" "text/plain; charset=utf-8"

# Browsable source tree: the same git-archive tarball, exploded, plus the
# src/LATEST pointer /src/latest/* resolves through.
SRCTMP=$(mktemp -d)
tar -xzf "daybox-$VERSION-src.tar.gz" -C "$SRCTMP"
TREE="$SRCTMP/daybox-$VERSION"
[ -d "$TREE" ] || { echo "release.sh: tarball has no daybox-$VERSION/" >&2; exit 1; }
find "$TREE" -type f | LC_ALL=C sort | while IFS= read -r f; do
    rel=${f#"$TREE"/}
    put "src/$VERSION/$rel" "$f" "text/plain; charset=utf-8"
done
printf '%s\n' "$VERSION" > "$SRCTMP/LATEST"
put "src/LATEST" "$SRCTMP/LATEST" "text/plain; charset=utf-8"
rm -rf "$SRCTMP"
UPLOAD
say "uploaded"

# ---- 5. verify what the edge actually serves (the part that earns its keep) ----
# Every release that ever shipped half-broken and was noticed late — the
# false-reproducible v0.2.3, the /dl/-prefix silent-404, the stale installer
# pin — would have been caught here. Any mismatch exits nonzero: no ✓ until
# the live release is provably the one we just signed.
say "verifying the live release"
SITE=https://daybox.dev
sha() { shasum -a 256 | cut -d' ' -f1; }   # reads stdin
FAIL=0

# local sums hash (what install.sh must pin)
SUMS_SHA=$(sha < SHA256SUMS)

vfail() { echo "  ✗ $*" >&2; FAIL=1; }
vok()   { echo "  ✓ $*"; }

# /dl/<version>/ signed chain
curl -fsS "$SITE/dl/$VERSION/SHA256SUMS" -o /tmp/release-vfy.sums || vfail "GET /dl/$VERSION/SHA256SUMS"
curl -fsS "$SITE/dl/$VERSION/SHA256SUMS.minisig" -o /tmp/release-vfy.sig || vfail "GET /dl/$VERSION/SHA256SUMS.minisig"
if [ -s /tmp/release-vfy.sums ] && [ -s /tmp/release-vfy.sig ]; then
    if minisign -Vm /tmp/release-vfy.sums -x /tmp/release-vfy.sig -P "$PUBKEY" >/dev/null 2>&1; then
        vok "/dl/$VERSION/ signed chain verifies against pinned key"
    else
        vfail "/dl/$VERSION/ signature does NOT verify against pinned key"
    fi
    if diff -q SHA256SUMS /tmp/release-vfy.sums >/dev/null; then
        vok "/dl/$VERSION/SHA256SUMS matches local"
    else
        vfail "/dl/$VERSION/SHA256SUMS differs from local"
    fi
fi

# /dl/latest/ — edge-cached 60s; the versioned path above already proves the
# bytes are right, so this only confirms the pointer flipped. Retry briefly.
for i in 1 2 3 4 5 6 7; do
    if curl -fsS "$SITE/dl/latest/SHA256SUMS" -o /tmp/release-vfy.lat 2>/dev/null \
       && diff -q SHA256SUMS /tmp/release-vfy.lat >/dev/null 2>&1; then
        vok "/dl/latest/ points at $VERSION (try $i)"; break
    fi
    [ "$i" = 7 ] && vfail "/dl/latest/ not serving $VERSION after ${i} tries (edge cache?)"
    sleep 10
done

# installer pin
INST=$(curl -fsS "$SITE/install.sh" || true)
if echo "$INST" | grep -q "^DAYBOX_RELEASE=\"$VERSION\"$" \
   && echo "$INST" | grep -q "^DAYBOX_SUMS_SHA256=\"$SUMS_SHA\"$"; then
    vok "/install.sh pins $VERSION + sums $SUMS_SHA"
else
    vfail "/install.sh does not pin $VERSION / $SUMS_SHA"
fi

# one binary: sha matches sums AND it runs as this version
curl -fsS "$SITE/dl/latest/$BIN-darwin-arm64" -o /tmp/release-vfy.bin || vfail "GET binary"
if [ -s /tmp/release-vfy.bin ]; then
    BIN_SHA=$(sha < /tmp/release-vfy.bin)
    WANT_SHA=$(grep " $BIN-darwin-arm64$" SHA256SUMS | awk '{print $1}')
    if [ "$BIN_SHA" = "$WANT_SHA" ]; then vok "binary sha matches SHA256SUMS"
    else vfail "binary sha $BIN_SHA != sums $WANT_SHA"; fi
    GOT_VER=$(chmod +x /tmp/release-vfy.bin 2>/dev/null; /tmp/release-vfy.bin version 2>/dev/null || true)
    if [ "$GOT_VER" = "$VERSION" ]; then vok "binary reports $VERSION"
    else vfail "binary reports '${GOT_VER:-<none>}' != $VERSION"; fi
fi

# source tree + LATEST pointer
LOCAL_GOMOD=$(sha < "$ROOT/go.mod")
for p in "src/$VERSION/go.mod" "src/latest/go.mod"; do
    GOT=$(curl -fsS "$SITE/$p" 2>/dev/null | sha || true)
    if [ "$GOT" = "$LOCAL_GOMOD" ]; then vok "/$p matches local"
    else vfail "/$p differs from local (latest/ heals in 60s; a versioned mismatch is real)"; fi
done
LAT=$(curl -fsS "$SITE/src/LATEST" 2>/dev/null || true)
if [ "$LAT" = "$VERSION" ]; then vok "/src/LATEST = $VERSION"
else vfail "/src/LATEST = '${LAT:-<none>}' != $VERSION"; fi

rm -f /tmp/release-vfy.sums /tmp/release-vfy.sig /tmp/release-vfy.lat /tmp/release-vfy.bin

echo
if [ "$FAIL" = 1 ]; then
    echo "✗ $VERSION uploaded but live verification FAILED — investigate before announcing" >&2
    exit 1
fi
say "✓ $VERSION live at $SITE (signed, published, verified)"
say "  control plane: run 'daybox upgrade' to move a deployment to it"
