package main

// payload.go — where `daybox init` gets the control-plane tree from.
//
// A curl-installed binary has no repo checkout, so init cannot tar a local
// clone. Instead it downloads a pinned, signed control-plane payload and
// unpacks it to a temp dir laid out exactly like a checkout — so every
// downstream step (pushTree, the agent binary, controlplane-setup.sh) works
// unchanged whether the tree came from a release or from a developer's clone.
//
// Integrity mirrors the installer, deliberately: the anchor is the minisign
// public key pinned below, not the host the bytes came from. SHA256SUMS is
// signed; the payload is checksummed against it. Integrity files are fetched
// from a verify base that is independent of the artifact base, so repointing
// the artifact store never moves the anchor. There is NO advisory mode — an
// unpinned key, a bad signature, or a checksum mismatch all abort.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

const (
	// Artifact base and integrity base. Separate on purpose (see above).
	defaultArtifactBase = "https://daybox.dev/dl"
	defaultVerifyBase   = "https://daybox.dev/dl"
	controlPlaneAsset   = "daybox-controlplane.tar.gz"
	maxPayloadBytes     = 64 << 20 // a control-plane tree is ~250KB; cap the blast radius
)

// minisignPubKey is the release signing key's PUBLIC half — the second line
// of the .pub file. Pinned here so it is auditable in the repo. Empty means
// this build cannot verify a release, and standalone init fails closed.
// Overridable at build time for tests: -ldflags "-X main.minisignPubKey=..."
var minisignPubKey = "RWSIiu1rtvgQzS1cqko1+oQxjHyw07jZqyzaid/zVPFIzxKyQ+rkz0/2"

// artifactBase/verifyBase are vars so tests can point them at a local server.
var (
	artifactBase = defaultArtifactBase
	verifyBase   = defaultVerifyBase
)

// ---------------------------------------------------------------- minisign --

// verifyMinisign checks a minisign signature over msg using a pinned public
// key line. Handles both the prehashed ("ED", BLAKE2b-512 — what modern
// minisign emits) and legacy ("Ed", raw) algorithms, and also verifies the
// global signature so the trusted comment cannot be swapped. On success it
// returns the trusted comment — the ONE signed field that can carry claims
// beyond the file bytes (release.sh puts `version:<v>` there), which is what
// lets callers bind a signed SHA256SUMS to the release it was cut for.
func verifyMinisign(pubLine string, msg, sigFile []byte) (string, error) {
	if strings.TrimSpace(pubLine) == "" {
		return "", fmt.Errorf("no release signing key pinned in this build — refusing to trust a downloaded payload")
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubLine))
	if err != nil || len(pub) != 42 {
		return "", fmt.Errorf("malformed pinned public key")
	}
	pubKeyID, pubKey := pub[2:10], ed25519.PublicKey(pub[10:])

	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 {
		return "", fmt.Errorf("malformed signature file")
	}
	sigBlob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sigBlob) != 74 {
		return "", fmt.Errorf("malformed signature line")
	}
	algo, sigKeyID, sig := sigBlob[:2], sigBlob[2:10], sigBlob[10:]

	if !bytes.Equal(pubKeyID, sigKeyID) {
		return "", fmt.Errorf("signature was made by a different key than the one pinned in this build")
	}

	signed := msg
	switch string(algo) {
	case "ED": // prehashed: signature covers BLAKE2b-512 of the content
		h := blake2b.Sum512(msg)
		signed = h[:]
	case "Ed": // legacy: signature covers the content directly
	default:
		return "", fmt.Errorf("unsupported signature algorithm %q", algo)
	}
	if !ed25519.Verify(pubKey, signed, sig) {
		return "", fmt.Errorf("SIGNATURE VERIFICATION FAILED")
	}

	// Global signature covers sig || trusted_comment; without this the
	// trusted comment could be rewritten while staying "verified".
	const tcPrefix = "trusted comment: "
	if !strings.HasPrefix(lines[2], tcPrefix) {
		return "", fmt.Errorf("malformed trusted comment")
	}
	globalSig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(globalSig) != 64 {
		return "", fmt.Errorf("malformed global signature")
	}
	trustedComment := lines[2][len(tcPrefix):]
	if !ed25519.Verify(pubKey, append(append([]byte{}, sig...), []byte(trustedComment)...), globalSig) {
		return "", fmt.Errorf("GLOBAL SIGNATURE VERIFICATION FAILED (trusted comment tampered)")
	}
	return trustedComment, nil
}

// attestsVersion reports whether a (already signature-verified) trusted
// comment carries the token `version:<ver>`. Whitespace-split tokens, exact
// match — no substring tricks with a crafted version string.
func attestsVersion(trustedComment, ver string) bool {
	for _, f := range strings.Fields(trustedComment) {
		if f == "version:"+ver {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ fetch ---

func fetchURL(url string, limit int64) ([]byte, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	return b, nil
}

// checksumFor pulls one file's expected sha256 out of a SHA256SUMS body.
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s in SHA256SUMS", name)
}

// ---------------------------------------------------------------- extract ---

// extractTarGz unpacks into dest, refusing any entry that would escape it.
// This archive arrives over the network; a "../.." entry must never be able
// to write outside the temp dir.
func extractTarGz(data []byte, dest string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		clean := filepath.Clean(strings.TrimPrefix(h.Name, "./"))
		if clean == "." {
			continue
		}
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing unsafe path in archive: %q", h.Name)
		}
		target := filepath.Join(dest, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxPayloadBytes)); err != nil {
				f.Close()
				return err
			}
			f.Close()
		default:
			// symlinks et al. have no business in a control-plane payload
			return fmt.Errorf("refusing archive entry %q of unsupported type", h.Name)
		}
	}
}

// ---------------------------------------------------------------- payload ---

// fetchControlPlanePayload downloads, verifies and unpacks the pinned
// control-plane tree, returning a directory laid out like a checkout.
func fetchControlPlanePayload(version string) (string, error) {
	verBase := strings.TrimRight(verifyBase, "/") + "/" + version
	artBase := strings.TrimRight(artifactBase, "/") + "/" + version

	sums, err := fetchURL(verBase+"/SHA256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := fetchURL(verBase+"/SHA256SUMS.minisig", 1<<20)
	if err != nil {
		return "", err
	}
	tc, err := verifyMinisign(minisignPubKey, sums, sig)
	if err != nil {
		return "", err
	}
	// Bind the signed sums to the version directory they were fetched from.
	// Without this, anyone who can write to the artifact store — the exact
	// adversary the signing scheme exists for — can copy an OLDER release's
	// validly-signed SHA256SUMS + tarball under /dl/<new>/ and downgrade
	// every init (rollback). The trusted comment is covered by the global
	// signature, so the version claim inside it is as strong as the sums
	// themselves. "latest" is exempt: it is the dev-build path, makes no
	// version claim, and released binaries never fetch it.
	if version != "latest" && !attestsVersion(tc, version) {
		return "", fmt.Errorf("SIGNED VERSION MISMATCH: /dl/%s/SHA256SUMS is validly signed but does not attest version:%s (trusted comment: %q)\n"+
			"  the artifact store may be serving another release's files under this version — refusing (rollback protection).\n"+
			"  Note: releases signed before version binding existed cannot be fetched by version pin.", version, version, tc)
	}

	want, err := checksumFor(sums, controlPlaneAsset)
	if err != nil {
		return "", err
	}
	blob, err := fetchURL(artBase+"/"+controlPlaneAsset, maxPayloadBytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return "", fmt.Errorf("CHECKSUM MISMATCH for %s\n  expected: %s\n  actual:   %s", controlPlaneAsset, want, got)
	}

	dir, err := os.MkdirTemp("", "daybox-payload-")
	if err != nil {
		return "", err
	}
	if err := extractTarGz(blob, dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "remote", "controlplane-setup.sh")); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("payload is missing remote/controlplane-setup.sh — not a control-plane tree")
	}
	return dir, nil
}

// resolvePayload decides where the control-plane tree comes from:
//   - explicit --repo: that checkout
//   - explicit --version: that release, even inside a checkout
//   - otherwise: an auto-detected checkout if there is one (the dev path),
//     else the pinned release (the curl-installed path)
//
// Returns the tree dir and a cleanup func for anything it downloaded.
func resolvePayload(flagRepo, flagVersion string) (string, func()) {
	noop := func() {}
	if flagRepo != "" {
		return findRepo(flagRepo), noop
	}
	if flagVersion == "" {
		if dir, ok := lookupRepo(); ok {
			say("• using the daybox checkout at %s", dir)
			return dir, noop
		}
	}
	// A released binary pins its own version, and fetchControlPlanePayload
	// additionally requires the signed SHA256SUMS to attest that version in
	// its trusted comment — so an artifact-store writer can neither point a
	// release at /dl/latest/ NOR park an older, validly-signed release's
	// files under this version's path (rollback). "latest" remains only for
	// dev builds and makes no version claim.
	ver := flagVersion
	if ver == "" {
		if strings.HasPrefix(version, "v") {
			ver = version
		} else {
			ver = "latest"
		}
	}
	say("• no checkout here — fetching the signed %s control-plane payload", ver)
	dir, err := fetchControlPlanePayload(ver)
	if err != nil {
		log.Fatalf("%v", err)
	}
	say("  payload verified (signature + checksum)")
	return dir, func() { os.RemoveAll(dir) }
}
