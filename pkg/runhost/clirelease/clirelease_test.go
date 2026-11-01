package clirelease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

func testKey(t *testing.T, name string, retired bool) (minisign.PrivateKey, KeyEntry) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, KeyEntry{Name: name, PublicKey: pub.String(), KeyID: fmt.Sprintf("%016X", pub.ID()), Retired: retired}
}

func testSet(t *testing.T, entries ...KeyEntry) *KeySet {
	t.Helper()
	b, err := json.Marshal(keyFile{Keys: entries})
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sign(priv minisign.PrivateKey, msg []byte, comment string) []byte {
	return minisign.SignWithComments(priv, msg, comment, "untrusted comment")
}

func TestTheCarriedSetOfASourceBuildIsEmpty(t *testing.T) {
	s, err := Carried()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifySums([]byte("x"), []byte("y"), Product, "1.0.0"); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("a build from source verified a release: %v", err)
	}
}

func TestAReleaseSignedByACurrentKeyVerifies(t *testing.T) {
	priv, entry := testKey(t, "prod", false)
	set := testSet(t, entry)
	sums := []byte(strings.Repeat("a", 64) + "  agentry-1.2.3-linux-amd64\n")
	sig := sign(priv, sums, "agentry-cli 1.2.3 (commit 0123456789abcdef)")
	if err := set.VerifySums(sums, sig, Product, "1.2.3"); err != nil {
		t.Fatalf("a genuine release was refused: %v", err)
	}
}

func TestEveryWrongReleaseIsRefused(t *testing.T) {
	priv, entry := testKey(t, "prod", false)
	oldPriv, oldEntry := testKey(t, "dev", true)
	strangerPriv, _ := testKey(t, "stranger", false)
	set := testSet(t, entry, oldEntry)
	sums := []byte(strings.Repeat("a", 64) + "  agentry-1.2.3-linux-amd64\n")
	good := "agentry-cli 1.2.3 (commit 0123456789abcdef)"
	tampered := append([]byte(nil), sums...)
	tampered[0] = 'b'
	cases := []struct {
		name string
		sums []byte
		sig  []byte
		want error
	}{
		{"changed sums", tampered, sign(priv, sums, good), ErrBadSignature},
		{"a stranger's key", sums, sign(strangerPriv, sums, good), ErrUnknownKey},
		{"a retired key", sums, sign(oldPriv, sums, good), ErrRetiredKey},
		{"another product", sums, sign(priv, sums, "agentry-desktop 1.2.3 (commit 0123456789abcdef)"), nil},
		{"another version", sums, sign(priv, sums, "agentry-cli 1.2.4 (commit 0123456789abcdef)"), nil},
		{"a comment naming no release", sums, sign(priv, sums, "hello"), ErrBadSignature},
		{"not a signature", sums, []byte("nope"), ErrBadSignature},
	}
	for _, c := range cases {
		err := set.VerifySums(c.sums, c.sig, Product, "1.2.3")
		if err == nil {
			t.Errorf("%s: verified", c.name)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func TestAKeyWhoseStatedIDIsNotItsOwnIsRefused(t *testing.T) {
	_, e := testKey(t, "prod", false)
	e.KeyID = "0000000000000000"
	b, _ := json.Marshal(keyFile{Keys: []KeyEntry{e}})
	if _, err := ParseKeys(b); err == nil {
		t.Fatal("a key set with a wrong key id was accepted")
	}
}

func TestFetchChecksTheSignatureAndTheBinary(t *testing.T) {
	priv, entry := testKey(t, "prod", false)
	set := testSet(t, entry)
	bin := []byte("the binary")
	sum := sha256.Sum256(bin)
	name := BinaryName("1.2.3", "arm64")
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	sig := sign(priv, sums, "agentry-cli 1.2.3 (commit 0123456789abcdef)")
	served := map[string][]byte{"/v1.2.3/SHA256SUMS": sums, "/v1.2.3/SHA256SUMS.minisig": sig, "/v1.2.3/" + name: bin}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	got, err := Fetch(context.Background(), srv.Client(), set, "1.2.3", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Bytes) != string(bin) || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("fetched %q %s", got.Bytes, got.SHA256)
	}

	served["/v1.2.3/"+name] = []byte("another binary")
	if _, err := Fetch(context.Background(), srv.Client(), set, "1.2.3", "arm64"); err == nil {
		t.Fatal("a binary that does not match its signed line was accepted")
	}
	if _, err := Fetch(context.Background(), srv.Client(), set, "1.2.3", "amd64"); err == nil {
		t.Fatal("an architecture the sums do not list was accepted")
	}
}

// A CLI release exactly as published, and the publisher's key set: this
// package must accept what the release workflow signs, or a released CLI
// could set up no runhost.
func TestAPublishedReleaseVerifiesAgainstThePublishersKeys(t *testing.T) {
	keys, err := os.ReadFile(filepath.Join("testdata", "release-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("testdata", "agentry-cli-0.6.1013")
	sums, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	sig, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS.minisig"))
	if err := set.VerifySums(sums, sig, Product, "0.6.1013"); err != nil {
		t.Fatalf("a published release was refused: %v", err)
	}
	if err := set.VerifySums(sums, sig, Product, "0.6.1012"); err == nil {
		t.Fatal("a release's signature was taken for another version")
	}
	if _, err := SumOf(sums, BinaryName("0.6.1013", "arm64")); err != nil {
		t.Fatal(err)
	}
}
