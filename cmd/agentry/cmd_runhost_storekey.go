package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/store"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// The runhost store's key. Where the server's systemd seals a credential to
// one user (256 or newer), the key is kept only as a systemd credential bound
// to the server (its TPM2 where it has one, its credential key otherwise) and
// to the agentry account, and each process of the account that opens the
// store decrypts it into memory; a key file kept before is sealed, checked
// and removed. So a copied disk or a backup holds the store's entries as
// ciphertext and no key that opens them; root on the running server can still
// decrypt the credential, as it can read the account's memory. On an older
// systemd the key stays a file only the account reads, and the runhost takes
// no schedule.

// runhostStoreCredName is the credential's name, bound into it.
const runhostStoreCredName = "agentry-runhost-store"

var (
	runhostStoreKeyCred = filepath.Join(runhost.StateDir, "store", "store.key.cred")
	// runhostDecryptCredential runs `systemd-creds decrypt --user`; tests
	// replace it, as schedule.EncryptCredential for the other way.
	runhostDecryptCredential = func(name, file string) ([]byte, error) {
		cmd := exec.Command("systemd-creds", "decrypt", "--user", "--name="+name, file, "-") //nolint:gosec // the store's own credential
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("systemd-creds decrypt --user: %v: %s", err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
	}
	// runhostSystemdVersion is the server's systemd's major version, 0 when
	// it cannot be read.
	runhostSystemdVersion = func() int {
		out, err := exec.Command("systemctl", "--version").Output()
		if err != nil {
			return 0
		}
		f := strings.Fields(string(out))
		if len(f) < 2 {
			return 0
		}
		n, _ := strconv.Atoi(f[1])
		return n
	}
)

var runhostStoreKeyCache struct {
	sync.Mutex
	key    []byte
	sealed bool
}

// runhostStoreSeals says whether this server's systemd seals the store's key
// to the account, which a schedule needs.
func runhostStoreSeals() bool { return runhostSystemdVersion() >= schedule.MinSystemd }

// runhostStoreKeyBytes is the store's key and whether it is sealed. A first call
// makes the key, or seals a key file kept before, when systemd can.
func runhostStoreKeyBytes() ([]byte, bool, error) {
	c := &runhostStoreKeyCache
	c.Lock()
	defer c.Unlock()
	if c.key != nil {
		return c.key, c.sealed, nil
	}
	if _, err := os.Stat(runhostStoreKeyCred); err == nil {
		key, err := runhostDecryptCredential(runhostStoreCredName, runhostStoreKeyCred)
		if err != nil {
			return nil, false, fmt.Errorf("the runhost store's sealed key: %w", err)
		}
		if len(key) != 32 {
			return nil, false, errors.New("the runhost store's sealed key is not a 32-byte key")
		}
		// A key file beside it is one an interrupted sealing left.
		_ = os.Remove(runhostStoreKeyFile)
		c.key, c.sealed = key, true
		return key, true, nil
	}
	if !runhostStoreSeals() {
		key, err := loadStoreKeyFile()
		if err != nil {
			return nil, false, err
		}
		c.key = key
		return key, false, nil
	}
	key, err := os.ReadFile(runhostStoreKeyFile) //nolint:gosec // the store's own key file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if key, err = store.NewKey(); err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, err
	case len(key) != 32:
		return nil, false, fmt.Errorf("the store's key %s is not a 32-byte key", runhostStoreKeyFile)
	}
	if err := os.MkdirAll(filepath.Dir(runhostStoreKeyCred), 0o700); err != nil {
		return nil, false, err
	}
	tmp := runhostStoreKeyCred + ".tmp"
	_ = os.Remove(tmp)
	if err := schedule.EncryptCredential(runhostStoreCredName, key, tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, false, fmt.Errorf("seal the runhost store's key: %w", err)
	}
	// The sealed key must open before the plain one goes.
	back, err := runhostDecryptCredential(runhostStoreCredName, tmp)
	if err != nil || !bytes.Equal(back, key) {
		_ = os.Remove(tmp)
		return nil, false, fmt.Errorf("the runhost store's key, sealed, does not open again: %v", err)
	}
	if err := os.Rename(tmp, runhostStoreKeyCred); err != nil {
		return nil, false, err
	}
	if err := os.Remove(runhostStoreKeyFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	c.key, c.sealed = key, true
	return key, true, nil
}

// loadStoreKeyFile reads the store's key file, making it at first use.
func loadStoreKeyFile() ([]byte, error) {
	st, err := store.Open(runhostStoreDir, runhostStoreKeyFile)
	if err != nil {
		return nil, err
	}
	_ = st
	return os.ReadFile(runhostStoreKeyFile) //nolint:gosec // the store's own key file
}

// forgetRunhostStoreKey drops the key held in memory; tests use it.
func forgetRunhostStoreKey() {
	c := &runhostStoreKeyCache
	c.Lock()
	c.key, c.sealed = nil, false
	c.Unlock()
}
