#!/bin/bash
# build.sh — cross-compile daybox for the platforms we deploy to (quick dev
# build; the release path is checksummed and version-stamped).
# Artifacts land in dist/ at the repo root (gitignored; installed copies are
# machine-local — see the README for where each one goes).
set -euo pipefail
cd "$(dirname "$0")/../.."          # repo root == module root (go.mod)
mkdir -p dist
for target in linux/amd64 darwin/arm64; do
    GOOS=${target%/*} GOARCH=${target#*/}
    echo "building dist/daybox-$GOOS-$GOARCH"
    CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
        go build -ldflags="-s -w" -o "dist/daybox-$GOOS-$GOARCH" ./cmd/daybox
done
# devbox pushes still reference the daybox-agent name; same binary
cp dist/daybox-linux-amd64 dist/daybox-agent-linux-amd64
