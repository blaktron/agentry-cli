// Package integritykey is the runner home's integrity key: 32 random
// bytes in <runner home>/plan-integrity.key (0600), outside every install
// directory. Two records are made under it, and a reader who can write an
// install directory but not read the key can make neither pass after an
// edit: the plan-integrity record beside an installed plan.json
// (compile, agentry-cli#266) and the installed-release record beside a
// release's install.json (installdir, agentry-cli#326).
//
// It is its own package so installdir, which lays releases down, and
// compile, which compiles plans, share one key without either importing
// the other.
package integritykey

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File is the key's name directly under the runner home. The name is the
// plan-integrity key's, which it was first.
const File = "plan-integrity.key"

// KeyLen is the key's length in bytes.
const KeyLen = 32

// Load returns the runner home's key, creating it on first use. Creation
// is race-free across processes: the key is written aside and hard-linked
// into place, so a concurrent first run either wins the link or reads the
// winner's complete key.
func Load(home string) ([]byte, error) {
	path := filepath.Join(home, File)
	if key, err := read(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create runner home: %w", err)
	}
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("integrity key: %w", err)
	}
	tmp, err := os.CreateTemp(home, File+".*")
	if err != nil {
		return nil, fmt.Errorf("integrity key: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	// CreateTemp makes the file 0600.
	if _, err := tmp.Write([]byte(hex.EncodeToString(key) + "\n")); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("integrity key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("integrity key: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		// No hard links here (some filesystems): a rename can lose a
		// first-run race, and the loser's records then name another key,
		// which a reader treats as no record, never as a match.
		if rerr := os.Rename(tmp.Name(), path); rerr != nil {
			return nil, fmt.Errorf("integrity key: %w", rerr)
		}
	}
	return read(path)
}

// Read returns the runner home's key without creating it: an error
// matching fs.ErrNotExist when the home has none yet, so nothing can have
// been recorded under it. A reader that must not write (a preview) uses it.
func Read(home string) ([]byte, error) { return read(filepath.Join(home, File)) }

func read(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // fixed name under the runner home
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != KeyLen {
		return nil, fmt.Errorf("integrity key %s is malformed — remove it and re-run (installed plans are then recompiled, installed releases re-fetched)", path)
	}
	return key, nil
}
