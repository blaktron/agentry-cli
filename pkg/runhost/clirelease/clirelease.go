// Package clirelease decides whether a CLI binary is one of Agentry's signed
// releases: SHA256SUMS signed with an update-signing key this build carries,
// a signed trusted comment naming the product and the version, and the
// binary's own line in those sums. A remote runhost runs only such a binary,
// so a release this package refuses never reaches a server.
//
// The key set is embedded, never read from disk: a file beside the binary is
// one an attacker who can write there could replace. A build from source
// carries an empty set and verifies nothing.
package clirelease

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"aead.dev/minisign"
)

// Product is the name the signed trusted comment gives the CLI.
const Product = "agentry-cli"

//go:embed keys.json
var keysJSON []byte

var (
	// ErrNoKeys means this build carries no update-signing key.
	ErrNoKeys = errors.New("this build carries no update-signing key, so it can verify no CLI release (a build from source cannot set up a remote runhost; use a release of the CLI)")
	// ErrUnknownKey means the signature names a key this build does not trust.
	ErrUnknownKey = errors.New("the release is signed with a key this build does not trust")
	// ErrBadSignature means the signature does not cover the bytes given.
	ErrBadSignature = errors.New("the release's signature does not match its SHA256SUMS")
	// ErrRetiredKey means the release is genuine but its key is retired, so
	// nothing is installed from it.
	ErrRetiredKey = errors.New("the release is signed with a retired key")
)

// KeyEntry is one key in keys.json.
type KeyEntry struct {
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	KeyID     string `json:"keyID"`
	Retired   bool   `json:"retired"`
}

type keyFile struct {
	Keys []KeyEntry `json:"keys"`
}

type key struct {
	name    string
	retired bool
	pub     minisign.PublicKey
}

func (k key) id() string { return fmt.Sprintf("%016X", k.pub.ID()) }

// KeySet is the set of keys a release may be signed with.
type KeySet struct{ keys []key }

// ParseKeys reads a keys.json. An entry whose stated id is not its public
// key's id is refused, as is a repeated name or id.
func ParseKeys(data []byte) (*KeySet, error) {
	var f keyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("the update-signing key set is not readable: %w", err)
	}
	set := &KeySet{}
	names, ids := map[string]bool{}, map[string]bool{}
	for i, e := range f.Keys {
		if e.Name == "" {
			return nil, fmt.Errorf("key %d has no name", i)
		}
		var pub minisign.PublicKey
		if err := pub.UnmarshalText([]byte(strings.TrimSpace(e.PublicKey))); err != nil {
			return nil, fmt.Errorf("key %q does not carry a minisign public key: %w", e.Name, err)
		}
		k := key{name: e.Name, retired: e.Retired, pub: pub}
		if want := strings.ToUpper(strings.TrimSpace(e.KeyID)); want != "" && want != k.id() {
			return nil, fmt.Errorf("key %q says its id is %s but its public key's id is %s", e.Name, want, k.id())
		}
		if names[e.Name] || ids[k.id()] {
			return nil, fmt.Errorf("key %q appears twice", e.Name)
		}
		names[e.Name], ids[k.id()] = true, true
		set.keys = append(set.keys, k)
	}
	return set, nil
}

var carried = sync.OnceValues(func() (*KeySet, error) { return ParseKeys(keysJSON) })

// Carried is the key set this build embeds.
func Carried() (*KeySet, error) { return carried() }

// trustedCommentRE is the signed comment's shape: `agentry-cli 0.6.1 (commit …)`.
var trustedCommentRE = regexp.MustCompile(`^([a-z][a-z0-9-]*) ([^\s(]+) \(commit ([0-9a-fA-F]{7,64})\)$`)

// VerifySums checks that sig is a current key's signature over sums and
// that its signed comment names product at version. The signature is
// checked against each key in turn, never chosen by the key id it claims,
// which is unsigned.
func (s *KeySet) VerifySums(sums, sig []byte, product, version string) error {
	if len(s.keys) == 0 {
		return ErrNoKeys
	}
	var parsed minisign.Signature
	if err := parsed.UnmarshalText(sig); err != nil {
		return fmt.Errorf("%w: it is not a minisign signature: %v", ErrBadSignature, err)
	}
	if parsed.Algorithm != minisign.EdDSA {
		return fmt.Errorf("%w: it is not an EdDSA signature", ErrBadSignature)
	}
	var signer *key
	for i := range s.keys {
		if minisign.Verify(s.keys[i].pub, sums, sig) {
			signer = &s.keys[i]
			break
		}
	}
	if signer == nil {
		claimed := fmt.Sprintf("%016X", parsed.KeyID)
		for _, k := range s.keys {
			if k.id() == claimed {
				return ErrBadSignature
			}
		}
		return fmt.Errorf("%w (it claims key %s)", ErrUnknownKey, claimed)
	}
	if signer.retired {
		return fmt.Errorf("%w (%s, %s)", ErrRetiredKey, signer.name, signer.id())
	}
	m := trustedCommentRE.FindStringSubmatch(strings.TrimSpace(parsed.TrustedComment))
	if m == nil {
		return fmt.Errorf("%w: its signed comment %q names no release", ErrBadSignature, parsed.TrustedComment)
	}
	if m[1] != product || m[2] != version {
		return fmt.Errorf("the signature is over %s %s, not %s %s", m[1], m[2], product, version)
	}
	return nil
}

// BinaryName is a release's Linux binary for an architecture (amd64, arm64).
func BinaryName(version, arch string) string {
	return "agentry-" + version + "-linux-" + arch
}

// SumOf is the SHA-256 that sums gives file, in hex.
func SumOf(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no line for %s", file)
}

// Binary is a release's Linux binary for one architecture, with the signed
// sums that cover it.
type Binary struct {
	Version string
	Arch    string
	Name    string
	Bytes   []byte
	SHA256  string
	Sums    []byte
	Sig     []byte
}

// BaseURL is where releases are downloaded from: the CLI's public releases.
var BaseURL = "https://github.com/blaktron/agentry-cli/releases/download"

// maxBinary bounds a download: the CLI is far smaller.
const maxBinary = 512 << 20

// Fetch downloads the release version's Linux binary for arch and checks it:
// the signature over SHA256SUMS against keys, then the binary's hash
// against its signed line. Nothing it returns is unchecked.
func Fetch(ctx context.Context, client *http.Client, keys *KeySet, version, arch string) (*Binary, error) {
	if keys == nil || len(keys.keys) == 0 {
		return nil, ErrNoKeys
	}
	base := BaseURL + "/v" + version + "/"
	sums, err := get(ctx, client, base+"SHA256SUMS", 1<<20)
	if err != nil {
		return nil, err
	}
	sig, err := get(ctx, client, base+"SHA256SUMS.minisig", 64<<10)
	if err != nil {
		return nil, err
	}
	if err := keys.VerifySums(sums, sig, Product, version); err != nil {
		return nil, fmt.Errorf("CLI release %s: %w", version, err)
	}
	name := BinaryName(version, arch)
	want, err := SumOf(sums, name)
	if err != nil {
		return nil, fmt.Errorf("CLI release %s: %w", version, err)
	}
	bin, err := get(ctx, client, base+name, maxBinary)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("CLI release %s: %s does not match its signed SHA-256", version, name)
	}
	return &Binary{Version: version, Arch: arch, Name: name, Bytes: bin, SHA256: want, Sums: sums, Sig: sig}, nil
}

func get(ctx context.Context, client *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", u, limit)
	}
	return b, nil
}
