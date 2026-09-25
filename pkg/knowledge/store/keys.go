package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry/pkg/packseal"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
)

// The keychain accounts the store uses, under modelkey.Service beside the
// registry tokens (a bare host) and the model keys ("provider:…"). Each
// holds a colon, so none can be a host a registry token is kept under.
const (
	// cachePrefix names a pack's cached data key: knowledge:<pack id>.
	cachePrefix = "knowledge:"
	// markerPrefix names the presence marker beside it, so a client can
	// say a pack opens offline without reading its key.
	markerPrefix = "stored:knowledge:"
	markerValue  = "1"
	// machineAccount is this machine's key, which wraps a pack's data key
	// while no registry is signed in (plan D20).
	machineAccount = "knowledge:machine"
)

// machineInfo binds a machine-wrapped key to its pack: the same bytes
// presented for another pack do not open.
const machineInfo = "agentry/knowledge/machine/v1\x00"

// OSKeychain is the OS keychain (Keychain on macOS, Credential Manager on
// Windows, the Secret Service on Linux), unless AGENTRY_NO_KEYRING is set.
type OSKeychain struct{}

// Get implements Keychain.
func (OSKeychain) Get(account string) (string, error) {
	if modelkey.Disabled() {
		return "", fmt.Errorf("%w (%s is set)", ErrNoKeychain, modelkey.EnvNoKeyring)
	}
	v, err := keyring.Get(modelkey.Service, account)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNoEntry
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrNoKeychain, err)
	}
	return v, nil
}

// Set implements Keychain.
func (OSKeychain) Set(account, value string) error {
	if modelkey.Disabled() {
		return fmt.Errorf("%w (%s is set)", ErrNoKeychain, modelkey.EnvNoKeyring)
	}
	if err := keyring.Set(modelkey.Service, account, value); err != nil {
		return fmt.Errorf("%w: %w", ErrNoKeychain, err)
	}
	return nil
}

// Delete implements Keychain.
func (OSKeychain) Delete(account string) error {
	if modelkey.Disabled() {
		return fmt.Errorf("%w (%s is set)", ErrNoKeychain, modelkey.EnvNoKeyring)
	}
	if err := keyring.Delete(modelkey.Service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNoKeychain, err)
	}
	return nil
}

// cache keeps a registry-unwrapped data key in the keychain, marker last.
// It is a convenience: a keychain that will not take it leaves the pack
// opening through the registry each time, never failing the caller.
func (s *Store) cache(id string, dataKey []byte) {
	if s.o.Keys == nil {
		return
	}
	if err := s.o.Keys.Set(cachePrefix+id, base64.StdEncoding.EncodeToString(dataKey)); err != nil {
		return
	}
	_ = s.o.Keys.Set(markerPrefix+id, markerValue)
}

// cached is a pack's cached data key, if the keychain holds a well-formed
// one.
func (s *Store) cached(id string) ([]byte, bool) {
	if s.o.Keys == nil {
		return nil, false
	}
	v, err := s.o.Keys.Get(cachePrefix + id)
	if err != nil {
		return nil, false
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(k) != packseal.KeySize {
		return nil, false
	}
	return k, true
}

// uncache removes a pack's cached key, marker first: a removal cut short
// leaves a key with no marker, never a marker with no key.
func (s *Store) uncache(id string) error {
	if s.o.Keys == nil {
		return nil
	}
	if err := s.o.Keys.Delete(markerPrefix + id); err != nil && !errors.Is(err, ErrNoKeychain) {
		return err
	}
	if err := s.o.Keys.Delete(cachePrefix + id); err != nil && !errors.Is(err, ErrNoKeychain) {
		return err
	}
	return nil
}

// Cached says whether a pack's data key is cached here, from its presence
// marker alone, so the key never enters the process to answer it: the
// desktop's "opens offline".
func (s *Store) Cached(id string) bool {
	if s.o.Keys == nil {
		return false
	}
	v, err := s.o.Keys.Get(markerPrefix + id)
	return err == nil && v == markerValue
}

// machineKey is this machine's key; with create, one is made the first
// time it is needed.
func (s *Store) machineKey(create bool) ([]byte, error) {
	if s.o.Keys == nil {
		return nil, ErrNoKeychain
	}
	v, err := s.o.Keys.Get(machineAccount)
	switch {
	case err == nil:
		k, derr := base64.StdEncoding.DecodeString(v)
		if derr != nil || len(k) != packseal.KeySize {
			return nil, fmt.Errorf("the OS keychain's %s entry is not a knowledge machine key", machineAccount)
		}
		return k, nil
	case !errors.Is(err, ErrNoEntry) || !create:
		return nil, err
	}
	k, err := packseal.NewKey()
	if err != nil {
		return nil, err
	}
	if err := s.o.Keys.Set(machineAccount, base64.StdEncoding.EncodeToString(k)); err != nil {
		return nil, err
	}
	return k, nil
}

// machineRecipient names a machine key by a digest of it, never the key,
// so a pack wrapped to another machine's key says so.
func machineRecipient(k []byte) string {
	sum := sha256.Sum256(k)
	return "machine:" + hex.EncodeToString(sum[:8])
}

// machineWrap seals a data key under this machine's key: AES-256-GCM with
// the pack's id in the additional data.
func (s *Store) machineWrap(id string, dataKey []byte) (WrappedKey, error) {
	k, err := s.machineKey(true)
	if err != nil {
		return WrappedKey{}, err
	}
	aead, err := gcm(k)
	if err != nil {
		return WrappedKey{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return WrappedKey{}, err
	}
	sealed := aead.Seal(nonce, nonce, dataKey, []byte(machineInfo+id))
	return WrappedKey{Kind: WrapperMachine, Recipient: machineRecipient(k), Key: sealed}, nil
}

// machineUnwrap opens a data key this machine wrapped.
func (s *Store) machineUnwrap(id string, wk WrappedKey) ([]byte, error) {
	k, err := s.machineKey(false)
	if errors.Is(err, ErrNoEntry) {
		return nil, errors.New("it is wrapped to a machine key this computer's keychain does not hold")
	}
	if err != nil {
		return nil, err
	}
	if wk.Recipient != machineRecipient(k) {
		return nil, errors.New("it is wrapped to another machine's key")
	}
	aead, err := gcm(k)
	if err != nil {
		return nil, err
	}
	if len(wk.Key) < aead.NonceSize() {
		return nil, errors.New("its machine-wrapped key is malformed")
	}
	dataKey, err := aead.Open(nil, wk.Key[:aead.NonceSize()], wk.Key[aead.NonceSize():], []byte(machineInfo+id))
	if err != nil || len(dataKey) != packseal.KeySize {
		return nil, errors.New("its machine-wrapped key does not open with this machine's key")
	}
	return dataKey, nil
}

func gcm(k []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// validID holds a pack id to kp_ and 32 lowercase hex digits: it names a
// directory of the store and comes, on a download, from a header the
// registry served.
func validID(id string) bool {
	if !strings.HasPrefix(id, "kp_") || len(id) != 35 {
		return false
	}
	for _, r := range id[3:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// newID is a fresh pack id, the form the apply step gives a new pack.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "kp_" + hex.EncodeToString(b), nil
}
