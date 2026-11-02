// Package store is the runhost's store of the releases it has run: each
// one's archive and the facts it arrived with, encrypted at rest, so a later
// run of the same release need not send it again. Whatever comes out of the
// store is verified again before it runs; the store keeps bytes, not trust.
// It also keeps named entries, a credential the runhost holds at rest for
// its runs (the credits token), under the same key.
//
// An entry is the magic, a 12-byte nonce, and AES-256-GCM over a length-
// prefixed JSON document (job.Release) followed by the archive, with the
// release's digest as additional data, so an entry moved to another digest's
// name does not open. The key is the store's own, made at its first use and
// kept owner-only beside it.
package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runhost/job"
)

const magic = "AGYRHS1\n"

// KeyFile is the store key's file name, beside the store's directory.
const KeyFile = "store.key"

// ErrNotStored means the store holds no release of that digest.
var ErrNotStored = errors.New("the runhost's store holds no such release")

// ErrNoEntry means the store holds no entry of that name.
var ErrNoEntry = errors.New("the runhost's store holds no such entry")

// ErrDamaged means an entry did not open with the store's key: it was
// changed, moved or cut.
var ErrDamaged = errors.New("the runhost's stored release does not open: it was changed since it was stored")

// Store is the release store under Dir.
type Store struct {
	dir string
	key []byte
}

// Open opens the store under dir (made 0700), whose key is keyFile, made at
// first use.
func Open(dir, keyFile string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadKey(keyFile)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, key: key}, nil
}

func loadKey(file string) ([]byte, error) {
	if b, err := os.ReadFile(file); err == nil { //nolint:gosec // the store's own key file
		if len(b) != 32 {
			return nil, fmt.Errorf("the store's key %s is not a 32-byte key", file)
		}
		return b, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the store's own key file
	if errors.Is(err, fs.ErrExist) {
		return loadKey(file)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, err
	}
	return key, f.Close()
}

func (s *Store) path(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%q is not a sha256 digest", digest)
	}
	return filepath.Join(s.dir, hex+".release"), nil
}

func (s *Store) aead() (cipher.AEAD, error) {
	b, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Has says whether the store holds a release of digest.
func (s *Store) Has(digest string) bool {
	p, err := s.path(digest)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Put stores the release rel and its archive, replacing an entry of the same
// digest. The entry is written whole under a temporary name, then renamed.
func (s *Store) Put(rel job.Release, archive []byte) error {
	p, err := s.path(rel.Digest)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(rel)
	if err != nil {
		return err
	}
	var plain bytes.Buffer
	_ = binary.Write(&plain, binary.BigEndian, uint32(len(meta)))
	plain.Write(meta)
	plain.Write(archive)
	aead, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append([]byte(magic), nonce...)
	out = aead.Seal(out, nonce, plain.Bytes(), []byte(rel.Digest))
	tmp, err := os.CreateTemp(s.dir, ".put-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Get reads the release of digest and its archive back.
func (s *Store) Get(digest string) (job.Release, []byte, error) {
	p, err := s.path(digest)
	if err != nil {
		return job.Release{}, nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // the store's own entry, named by a checked digest
	if errors.Is(err, fs.ErrNotExist) {
		return job.Release{}, nil, ErrNotStored
	}
	if err != nil {
		return job.Release{}, nil, err
	}
	aead, err := s.aead()
	if err != nil {
		return job.Release{}, nil, err
	}
	if len(b) < len(magic)+aead.NonceSize() || string(b[:len(magic)]) != magic {
		return job.Release{}, nil, ErrDamaged
	}
	nonce := b[len(magic) : len(magic)+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, b[len(magic)+aead.NonceSize():], []byte(digest))
	if err != nil || len(plain) < 4 {
		return job.Release{}, nil, ErrDamaged
	}
	n := binary.BigEndian.Uint32(plain[:4])
	if uint64(n) > uint64(len(plain)-4) {
		return job.Release{}, nil, ErrDamaged
	}
	var rel job.Release
	if err := json.Unmarshal(plain[4:4+n], &rel); err != nil || rel.Digest != digest {
		return job.Release{}, nil, ErrDamaged
	}
	return rel, plain[4+n:], nil
}

// Remove deletes the release of digest; one not stored is not an error.
func (s *Store) Remove(digest string) error {
	p, err := s.path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

const entryMagic = "AGYRHE1\n"

var entryNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func (s *Store) entryPath(name string) (string, error) {
	if !entryNameRE.MatchString(name) {
		return "", fmt.Errorf("%q is not a store entry's name", name)
	}
	return filepath.Join(s.dir, name+".entry"), nil
}

// PutEntry stores value as the entry name, replacing one of the same name.
// The entry's name is its additional data, so an entry renamed does not
// open as another.
func (s *Store) PutEntry(name string, value []byte) error {
	p, err := s.entryPath(name)
	if err != nil {
		return err
	}
	aead, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append([]byte(entryMagic), nonce...)
	out = aead.Seal(out, nonce, value, []byte("entry:"+name))
	tmp, err := os.CreateTemp(s.dir, ".put-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Entry reads the entry name back.
func (s *Store) Entry(name string) ([]byte, error) {
	p, err := s.entryPath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // the store's own entry, named by a checked name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoEntry
	}
	if err != nil {
		return nil, err
	}
	aead, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(b) < len(entryMagic)+aead.NonceSize() || string(b[:len(entryMagic)]) != entryMagic {
		return nil, ErrDamaged
	}
	nonce := b[len(entryMagic) : len(entryMagic)+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, b[len(entryMagic)+aead.NonceSize():], []byte("entry:"+name))
	if err != nil {
		return nil, ErrDamaged
	}
	return plain, nil
}

// RemoveEntry deletes the entry name; one not stored is not an error.
func (s *Store) RemoveEntry(name string) error {
	p, err := s.entryPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
