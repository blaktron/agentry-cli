package clamdb

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The recorded signatures (testdata): clamav-db.sigstore.json is the
// clamav-db workflow's signature over the first bundle,
// agentry-clamav-db:2026.10.04.1 (run 37244540966), whose manifest is
// bundle-manifest.json; files.sigstore.json is the files workflow's over
// agentry-files:2026.10.03.2. trusted_root.json is Sigstore's public-good
// root as its TUF repository served it on 2026-10-04.
const (
	recordedBundleDigest = "sha256:84cbc016d58b4b70b7337b78cfd01929d6ad2646ac5d773be66efab8b3e2d10c"
	recordedFilesDigest  = "sha256:ee6b4f66fa2f30444cf9ffbac4acb83e4829baa6915963bdc322d547463d5ddd"
)

func recordedRoot(t *testing.T) root.TrustedMaterial {
	t.Helper()
	tr, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheRecordedBundleIsTheOneItsSignatureNames(t *testing.T) {
	// The manifest the signature is over hashes to its digest, and is the
	// bundle's shape.
	body := readFile(t, "testdata/bundle-manifest.json")
	if got := sha256Hex(body); got != recordedBundleDigest {
		t.Fatalf("bundle-manifest.json hashes to %s, not %s", got, recordedBundleDigest)
	}
	layers, err := bundleLayers(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 || layers["daily.cvd"].Size == 0 {
		t.Fatalf("layers = %+v", layers)
	}
}

func TestVerifySignature(t *testing.T) {
	tm := recordedRoot(t)
	sig := readFile(t, "testdata/clamav-db.sigstore.json")
	if err := verifySignature(tm, sig, recordedBundleDigest); err != nil {
		t.Fatalf("the clamav-db workflow's own signature was refused: %v", err)
	}
	t.Run("another digest", func(t *testing.T) {
		other := "sha256:" + strings.Repeat("0", 64)
		if err := verifySignature(tm, sig, other); err == nil {
			t.Fatal("a signature over one bundle verified another")
		}
	})
	t.Run("another workflow", func(t *testing.T) {
		// The files workflow's signature is genuine, over its own image,
		// but not the identity a bundle must carry.
		files := readFile(t, "testdata/files.sigstore.json")
		err := verifySignature(tm, files, recordedFilesDigest)
		if err == nil || !strings.Contains(err.Error(), SignerIdentity) {
			t.Fatalf("the files workflow's signature was taken as the bundle's: %v", err)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		bad := []byte(strings.Replace(string(sig), `"sig":"`, `"sig":"AAAA`, 1))
		if string(bad) == string(sig) {
			t.Fatal("the test did not change the signature")
		}
		if err := verifySignature(tm, bad, recordedBundleDigest); err == nil {
			t.Fatal("a tampered signature verified")
		}
	})
	t.Run("not a bundle", func(t *testing.T) {
		if err := verifySignature(tm, []byte(`{"mediaType":"x"}`), recordedBundleDigest); err == nil {
			t.Fatal("a non-bundle verified")
		}
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
