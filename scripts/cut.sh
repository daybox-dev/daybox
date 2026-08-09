#!/bin/bash
# cut.sh — build the release artifacts: the offline, reproducible half of a
# release. Cross-compile, checksum, stamp the installer, drop the source.
# Keyless and network-free by design — you can cut a release on a train and
# publish it later.
#
# THIS IS NOT CI, AND MUST NOT BECOME CI. A release pipeline with publish
# rights is precisely the supply-chain surface daybox defends against
# (SECURITY.md, iron rule 3). Artifacts are cross-compiled and checksummed
# here on the trusted laptop; scripts/release.sh signs, publishes, and
# verifies them — also on the laptop, also never CI.
#
# Usage:
#   scripts/cut.sh v0.1.0        # stamp this version
#   scripts/cut.sh               # derive from `git describe` (dirty -> refused)
#
# Output: dist/ with one static binary per platform, the source drop, and
# SHA256SUMS covering all of it.
#   dist/daybox-darwin-arm64   dist/daybox-darwin-amd64
#   dist/daybox-linux-amd64    dist/daybox-linux-arm64
#   dist/daybox-<version>-src.tar.gz
#   dist/SHA256SUMS
#
# After it prints the checksums, scripts/release.sh <version> signs them with
# the release key, uploads to R2, and verifies the live release end-to-end.
# There is no other publication path: the code the public can read only
# changes when that two-step cut + release runs.
set -euo pipefail

# Product/binary name, in one place.
BIN=daybox

cd "$(dirname "$0")/.."
ROOT=$(pwd)
DIST="$ROOT/dist"       # artifacts land here; the Go module is the repo root

# ---- version ----
# A release must be reproducible from a clean, TAGGED tree. Both halves are
# enforced independently of how the version arrives, because each was
# bypassable on its own: an explicit version argument used to skip the
# dirty check, and `git describe --always` used to fall back to a bare
# commit hash when no tag existed — silently stamping a non-release as one.
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    echo "refusing to build a release from a dirty tree: commit first." >&2
    git status --short >&2
    exit 1
fi
# HEAD must be EXACTLY at a tag — `git describe --tags` without
# --exact-match happily stamps v0.1.0-3-gabc1234 on a tree three commits
# past the tag, and an explicit argument used to be taken on faith (passing
# v0.3.0 on a v0.2.0 checkout shipped mislabeled artifacts).
AT_TAG=$(git describe --tags --exact-match 2>/dev/null || true)
if [ $# -ge 1 ]; then
    VERSION=$1
    if [ "$AT_TAG" != "$VERSION" ]; then
        echo "refusing to build '$VERSION': HEAD is at '${AT_TAG:-no tag}'." >&2
        echo "  a release is built only from the commit its tag points at." >&2
        exit 1
    fi
else
    VERSION=$AT_TAG
    if [ -z "$VERSION" ]; then
        echo "refusing to build: HEAD is not at a tag, and no version argument." >&2
        echo "  tag it:   git tag -a v0.1.0 -m 'daybox v0.1.0'" >&2
        echo "  then:     scripts/release.sh v0.1.0" >&2
        exit 1
    fi
fi
case "$VERSION" in
    *-dirty | dev)
        echo "refusing to build release '$VERSION': commit (and tag) first." >&2
        echo "  a release must be reproducible from a clean, tagged tree." >&2
        exit 1
        ;;
esac
# ---- refuse to ship a binary that cannot verify a release payload ----
# A curl-installed `daybox init` fetches its control-plane tree over the
# network and verifies it against the pinned release key. Shipping a build
# with no key pinned would mean shipping an init that can only fail (or, if
# the check were ever softened, one that trusts anything).
BINKEY=$(sed -n 's/^var minisignPubKey = "\(RW[A-Za-z0-9+/=]*\)".*/\1/p' cmd/daybox/payload.go)
SHKEY=$(sed -n 's/^DAYBOX_MINISIGN_PUBKEY="\([^"]*\)".*/\1/p' web/install.sh)
if [ -z "$BINKEY" ]; then
    echo "refusing to build: no release signing key pinned." >&2
    echo "  paste the second line of your minisign .pub into" >&2
    echo "  cmd/daybox/payload.go (var minisignPubKey)." >&2
    exit 1
fi
# The installer and the binary MUST trust the same key: a rotation that
# touches one file but not the other ships an installer and an init that
# disagree about what a valid release is.
if [ "$BINKEY" != "$SHKEY" ]; then
    echo "refusing to build: pinned keys differ between cmd/daybox/payload.go" >&2
    echo "  and web/install.sh — rotate BOTH, in one commit." >&2
    exit 1
fi
# keys/*.pub are machine-local (gitignored — invisible to the dirty-tree
# check) but the control-plane payload cp -R's keys/ wholesale; a stray
# device pubkey would ship, identifying a real deployment, in every tarball.
if ls "$ROOT"/keys/*.pub >/dev/null 2>&1; then
    echo "refusing to build: keys/*.pub present in the checkout — machine-local" >&2
    echo "  device keys must not ship in the release payload. Move them to" >&2
    echo "  ~/.config/daybox/keys/ and clean the repo keys/ dir." >&2
    exit 1
fi

# ---- publication hygiene gate ----
# Every tracked byte ships, forever, in the source drop below. The denylist
# is machine-local ON PURPOSE: it names exactly the strings that must never
# be published, so it cannot live in this tree. One extended regex per
# line, matched case-insensitively; the canonical list is documented in the
# internal repo's opensource-hygiene.md.
DENYLIST="${DAYBOX_DENYLIST:-$HOME/.config/daybox/release-denylist}"
if [ ! -s "$DENYLIST" ]; then
    echo "refusing to build: no denylist at $DENYLIST" >&2
    echo "  (one ERE per line; see opensource-hygiene.md in the internal repo)" >&2
    exit 1
fi
if git grep -Iil -E -f "$DENYLIST" HEAD -- >&2; then
    echo "refusing to build: denylist hits in the files above — scrub first." >&2
    exit 1
fi

echo "building $BIN $VERSION"

# ---- clean output ----
rm -rf "$DIST"
mkdir -p "$DIST"

# ---- cross-compile ----
# -trimpath + -s -w + CGO off: reproducible, stripped, static single binaries.
# -buildvcs=false because this build runs in the git checkout but VERIFY.md's
# rebuild runs from the source tarball (no .git): the default vcs stamping
# embeds revision/time/module-version in one and not the other, so the
# hashes can never match without it (v0.2.3 shipped exactly that mismatch).
# The version the binary reports comes from -X main.version, not vcs.
LDFLAGS="-s -w -X main.version=$VERSION"
cd "$ROOT"      # module root; build the command package ./cmd/daybox
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
    GOOS=${target%/*} GOARCH=${target#*/}
    out="$DIST/$BIN-$GOOS-$GOARCH"
    echo "  $GOOS/$GOARCH -> ${out#$ROOT/}"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
        go build -trimpath -buildvcs=false -ldflags="$LDFLAGS" -o "$out" ./cmd/daybox
done

# devbox pushes still reference the linux/amd64 binary under the agent name;
# it is byte-identical, just a second filename init/setup already look for.
cp "$DIST/$BIN-linux-amd64" "$DIST/$BIN-agent-linux-amd64"

# ---- control-plane payload ----
# `daybox init` on a machine with NO checkout downloads this, verifies it
# against the signed SHA256SUMS, and pushes it to the control plane. It must be
# laid out exactly like a checkout — cmd/daybox/payload.go asserts as much by
# requiring remote/controlplane-setup.sh inside it. Bundling the linux binary
# in the same artifact means the tree and the agent can never version-skew.
echo "  control-plane payload -> dist/$BIN-controlplane.tar.gz"
PAYDIR="$DIST/.payload"
rm -rf "$PAYDIR"; mkdir -p "$PAYDIR/dist"
# One binary now — the bash bin/daybox + providers/*.sh are retired; the Go
# CLI runs on the plane as well as the laptop. The payload carries the
# runtime files the binary dereferences from $REPO_DIR (cloud-init template,
# remote/ box-provisioning files, keys/ fallback) + the binary itself.
for p in remote systemd cloud-init headscale keys \
         profile.default.toml install.sh README.md SECURITY.md LICENSE; do
    [ -e "$ROOT/$p" ] && cp -R "$ROOT/$p" "$PAYDIR/"
done
# the single Go binary: the plane's daybox AND the daybox-agent (same binary)
cp "$DIST/$BIN-linux-amd64"            "$PAYDIR/dist/$BIN-linux-amd64"
cp "$DIST/$BIN-linux-amd64"            "$PAYDIR/dist/$BIN-agent-linux-amd64"
# The payload must carry every $REPO_DIR path the binary dereferences at
# runtime. A gap here is invisible to dev-checkout inits (pushTree ships the
# whole clone) and surfaces only on a fresh machine running a release —
# v0.2.4 shipped without providers/ and every standalone init died at
# `daybox setup` with "unknown provider 'hetzner'". Fail the build instead.
for req in remote/controlplane-setup.sh install.sh \
           profile.default.toml \
           cloud-init/cloud-init.yaml.template \
           "dist/$BIN-linux-amd64" "dist/$BIN-agent-linux-amd64"; do
    [ -e "$PAYDIR/$req" ] || {
        echo "refusing to package an incomplete control-plane payload: missing $req" >&2
        exit 1
    }
done
# NB: not byte-reproducible (tar/gzip embed mtimes). Integrity comes from the
# signature over SHA256SUMS, not from rebuilding this identically.
tar -C "$PAYDIR" -czf "$DIST/$BIN-controlplane.tar.gz" .
rm -rf "$PAYDIR"

# ---- source drop ----
# The publication itself: the exact tagged tree via git archive — no
# history, no authorship metadata, by construction (HEAD is enforced
# clean-at-tag above, so tree == tag). It lands in the same signed
# SHA256SUMS as the binaries, so the published source, the hashes, and the
# binaries can never skew. VERIFY.md (in the tree) documents the
# reproducible rebuild that ties a binary hash back to this tarball.
echo "  source drop -> dist/$BIN-$VERSION-src.tar.gz"
git archive --format=tar.gz --prefix="$BIN-$VERSION/" \
    -o "$DIST/$BIN-$VERSION-src.tar.gz" HEAD

# ---- checksums ----
# NB: install.sh is deliberately NOT included here — it pins SHA256SUMS's own
# hash, so listing it would be circular. The "$BIN"-* glob already excludes it.
cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$BIN"-* > SHA256SUMS
else # macOS: shasum -a 256 emits the same "<hash>  <file>" format
    shasum -a 256 "$BIN"-* > SHA256SUMS
fi

# ---- stamp the web installer ----
# The one-liner's mandatory anchor is the sha256 of SHA256SUMS pinned inside
# install.sh, so a bare machine needs only sha256sum/shasum/openssl — not
# minisign. That means install.sh is a per-release artifact: generated here,
# never hand-edited, and uploaded alongside the rest.
if command -v sha256sum >/dev/null 2>&1; then
    SUMS_SHA=$(sha256sum SHA256SUMS | awk '{print $1}')
else
    SUMS_SHA=$(shasum -a 256 SHA256SUMS | awk '{print $1}')
fi
sed -e "s|__DAYBOX_RELEASE__|$VERSION|" \
    -e "s|__DAYBOX_SUMS_SHA256__|$SUMS_SHA|" \
    "$ROOT/web/install.sh" > "$DIST/install.sh"
chmod 755 "$DIST/install.sh"
# Fail loudly rather than shipping an installer that would refuse to run.
# Match only the stamped assignments: the file also contains `__DAYBOX_*` as
# case-glob patterns in its own runtime guards, which are meant to stay.
if grep -qE '^DAYBOX_(RELEASE|SUMS_SHA256)="__DAYBOX_' "$DIST/install.sh"; then
    echo "internal error: install.sh still has unstamped placeholders" >&2
    exit 1
fi
echo "  web installer -> dist/install.sh (pins $VERSION, sums $SUMS_SHA)"

echo
echo "artifacts in ${DIST#$ROOT/}/:"
ls -1 "$BIN"-*
echo
echo "SHA256SUMS:"
cat SHA256SUMS
echo
echo "next: scripts/release.sh $VERSION   # sign + publish to R2 + verify"
