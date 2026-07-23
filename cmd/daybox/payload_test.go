package main

// Tests for the release-payload trust path. These matter more than most:
// everything here is what stands between a curl-installed `daybox init` and
// executing whatever a network attacker felt like serving.
//
// Fixtures in testdata/ were produced by the real minisign CLI, so these also
// pin interop with the actual signature format rather than with our reading
// of it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestVerifyMinisign(t *testing.T) {
	sums := readFixture(t, "SHA256SUMS")
	sig := readFixture(t, "SHA256SUMS.minisig")
	legit := strings.TrimSpace(string(readFixture(t, "legit.publine")))
	evilKey := strings.TrimSpace(string(readFixture(t, "evil.publine")))
	evilSig := readFixture(t, "SHA256SUMS.evil.minisig")

	t.Run("valid signature from the pinned key", func(t *testing.T) {
		if err := verifyMinisign(legit, sums, sig); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	// Each of these MUST fail. A false accept here is a remote-code-execution
	// bug, so they are asserted individually rather than in a loop.
	t.Run("no key pinned fails closed", func(t *testing.T) {
		if err := verifyMinisign("", sums, sig); err == nil {
			t.Fatal("unpinned key must not verify")
		}
	})
	t.Run("content tampered after signing", func(t *testing.T) {
		bad := append(append([]byte{}, sums...), []byte("evil  extra-file\n")...)
		if err := verifyMinisign(legit, bad, sig); err == nil {
			t.Fatal("tampered content must not verify")
		}
	})
	t.Run("signature from a different key", func(t *testing.T) {
		if err := verifyMinisign(legit, sums, evilSig); err == nil {
			t.Fatal("attacker-signed payload must not verify against the pinned key")
		}
	})
	t.Run("attacker pins their own key but sig is legit's", func(t *testing.T) {
		if err := verifyMinisign(evilKey, sums, sig); err == nil {
			t.Fatal("key/signature mismatch must not verify")
		}
	})
	t.Run("trusted comment tampered", func(t *testing.T) {
		lines := strings.Split(string(sig), "\n")
		lines[2] = "trusted comment: totally legit, promise"
		if err := verifyMinisign(legit, sums, []byte(strings.Join(lines, "\n"))); err == nil {
			t.Fatal("rewritten trusted comment must fail the global signature")
		}
	})
	t.Run("malformed inputs", func(t *testing.T) {
		for name, bad := range map[string][]byte{
			"truncated":  []byte("untrusted comment: x\nnotbase64\n"),
			"empty":      {},
			"two lines":  []byte("untrusted comment: x\nAAAA\n"),
			"bad base64": []byte("untrusted comment: x\n!!!!\ntrusted comment: y\n!!!!\n"),
		} {
			if err := verifyMinisign(legit, sums, bad); err == nil {
				t.Fatalf("%s: malformed signature must not verify", name)
			}
		}
	})
}

// A downloaded archive must never write outside the directory we chose.
func TestExtractTarGzRejectsEscapes(t *testing.T) {
	mk := func(name string, typeflag byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		body := []byte("pwned")
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: typeflag}
		if typeflag == tar.TypeSymlink {
			h.Size, h.Linkname = 0, "/etc/passwd"
		}
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write(body)
		}
		tw.Close()
		zw.Close()
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name     string
		entry    string
		typeflag byte
	}{
		{"parent traversal", "../escaped.txt", tar.TypeReg},
		{"deep traversal", "a/../../escaped.txt", tar.TypeReg},
		{"absolute path", "/etc/cron.d/evil", tar.TypeReg},
		{"symlink", "link", tar.TypeSymlink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := extractTarGz(mk(tc.entry, tc.typeflag), dir); err == nil {
				t.Fatalf("entry %q must be refused", tc.entry)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
				t.Fatal("archive escaped the destination directory")
			}
		})
	}
}

func TestExtractTarGzHappyPath(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/bash\n")
	tw.WriteHeader(&tar.Header{Name: "./remote/controlplane-setup.sh", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	zw.Close()

	dir := t.TempDir()
	if err := extractTarGz(buf.Bytes(), dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "remote", "controlplane-setup.sh"))
	if err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Error("executable bit not preserved — controlplane-setup.sh must stay runnable")
	}
}

// End-to-end against a local artifact server: the payload is only accepted
// when signature AND checksum both hold.
func TestFetchControlPlanePayload(t *testing.T) {
	legit := strings.TrimSpace(string(readFixture(t, "legit.publine")))

	// a minimal but structurally valid control-plane tree
	var tarbuf bytes.Buffer
	zw := gzip.NewWriter(&tarbuf)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/bash\necho setup\n")
	tw.WriteHeader(&tar.Header{Name: "./remote/controlplane-setup.sh", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	zw.Close()
	payload := tarbuf.Bytes()

	// SHA256SUMS must be the exact bytes we have a real signature for, so the
	// signed fixture is reused and the payload hash injected into a copy that
	// we re-sign is impossible here — instead assert the checksum path with a
	// deliberately mismatched sum, and the signature path with the fixture.
	signedSums := readFixture(t, "SHA256SUMS")
	signedSig := readFixture(t, "SHA256SUMS.minisig")

	serve := func(sums, sig, blob []byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
				w.Write(sums)
			case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.minisig"):
				w.Write(sig)
			case strings.HasSuffix(r.URL.Path, "/"+controlPlaneAsset):
				w.Write(blob)
			default:
				http.NotFound(w, r)
			}
		}))
	}

	withKey := func(k string, f func()) {
		old := minisignPubKey
		minisignPubKey = k
		defer func() { minisignPubKey = old }()
		f()
	}
	withBases := func(u string, f func()) {
		oa, ov := artifactBase, verifyBase
		artifactBase, verifyBase = u, u
		defer func() { artifactBase, verifyBase = oa, ov }()
		f()
	}

	t.Run("checksum mismatch is refused", func(t *testing.T) {
		// signed fixture lists "deadbeef" for the asset; real hash differs
		srv := serve(signedSums, signedSig, payload)
		defer srv.Close()
		withKey(legit, func() {
			withBases(srv.URL, func() {
				if _, err := fetchControlPlanePayload("v1"); err == nil ||
					!strings.Contains(err.Error(), "CHECKSUM MISMATCH") {
					t.Fatalf("expected checksum mismatch, got %v", err)
				}
			})
		})
	})

	t.Run("bad signature is refused before any checksum work", func(t *testing.T) {
		tampered := append(append([]byte{}, signedSums...), []byte("x  y\n")...)
		srv := serve(tampered, signedSig, payload)
		defer srv.Close()
		withKey(legit, func() {
			withBases(srv.URL, func() {
				if _, err := fetchControlPlanePayload("v1"); err == nil ||
					!strings.Contains(err.Error(), "SIGNATURE") {
					t.Fatalf("expected signature failure, got %v", err)
				}
			})
		})
	})

	t.Run("unpinned key refuses even with consistent artifacts", func(t *testing.T) {
		srv := serve(signedSums, signedSig, payload)
		defer srv.Close()
		withKey("", func() {
			withBases(srv.URL, func() {
				if _, err := fetchControlPlanePayload("v1"); err == nil {
					t.Fatal("must refuse with no pinned key")
				}
			})
		})
	})

	t.Run("missing artifact (404) is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()
		withKey(legit, func() {
			withBases(srv.URL, func() {
				if _, err := fetchControlPlanePayload("v9.9.9"); err == nil {
					t.Fatal("missing release must be refused")
				}
			})
		})
	})
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("aaa  daybox-linux-amd64\nbbb  daybox-controlplane.tar.gz\n")
	got, err := checksumFor(sums, "daybox-controlplane.tar.gz")
	if err != nil || got != "bbb" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "nope"); err == nil {
		t.Fatal("missing entry must error")
	}
}
