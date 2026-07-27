# Verifying a daybox release

Every release at `https://daybox.dev/dl/<version>/` contains the platform
binaries, this source tree as `daybox-<version>-src.tar.gz`, and a
`SHA256SUMS` manifest signed with minisign. The same tree is browsable at
`https://daybox.dev/src/<version>/`. One key signs everything; it is
pinned in `install.sh`, in `cmd/daybox/payload.go`, and here:

    RWSIiu1rtvgQzS1cqko1+oQxjHyw07jZqyzaid/zVPFIzxKyQ+rkz0/2

Verification is four steps: check the manifest signature, check the
tarball against the manifest, rebuild, compare hashes. The build is
reproducible (`-trimpath`, `CGO_ENABLED=0`, stripped, exact toolchain from
`go.mod`), so a matching hash proves the binary you run was built from
exactly this source — not just that both exist.

    V=v0.2.3   # the release you are verifying
    B="https://daybox.dev/dl/$V"
    curl -fsSL -O "$B/SHA256SUMS" -O "$B/SHA256SUMS.minisig" \
         -O "$B/daybox-$V-src.tar.gz"

    # 1. the manifest is signed by the pinned key and names this version
    minisign -Vm SHA256SUMS \
      -P RWSIiu1rtvgQzS1cqko1+oQxjHyw07jZqyzaid/zVPFIzxKyQ+rkz0/2

    # 2. the source tarball is in the signed manifest
    sha256sum -c --ignore-missing SHA256SUMS

    # 3. rebuild for your platform (any modern `go` auto-downloads the
    #    exact toolchain go.mod names)
    tar xzf "daybox-$V-src.tar.gz" && cd "daybox-$V"
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=$V" -o daybox-rebuilt ./cmd/daybox

    # 4. the rebuild, the released binary, and your installed daybox match
    sha256sum daybox-rebuilt "$(command -v daybox)"
    grep "daybox-$(go env GOOS)-$(go env GOARCH)" ../SHA256SUMS

All three hashes must be identical. On macOS, `shasum -a 256` stands in
for `sha256sum` (including `shasum -a 256 -c` in step 2).

Cross-compiling works too: set `GOOS`/`GOARCH` in step 3 and compare
against that platform's line in `SHA256SUMS` — the output is bit-identical
regardless of the machine that builds it.
