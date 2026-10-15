package modelkey

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

// The sealed source: a schedule that runs while its person is signed out finds
// the keychain locked, so the keychain values its fire reads are sealed, when
// it is made, into an encrypted systemd credential (`systemd-creds encrypt
// --user`). Its service unit loads the credential with
// LoadCredentialEncrypted=, and systemd decrypts it, for as long as the service
// runs, into $CREDENTIALS_DIRECTORY: a folder in memory under the user's
// runtime directory, which processes of the same user can read, as they can
// read an unlocked keychain. The unit also sets AGENTRY_NO_KEYRING, so nothing
// waits on a keychain that cannot unlock.
//
// While a sealed credential is present, every read of a keychain account
// here (a model key, a person's own provider key, a registry token) is
// answered from it, and from nothing else: no plaintext copy is written to
// disk and there is no fallback. Whoever sets this process's environment
// could point it at other keys, as they could set the key variables
// themselves.
const (
	// EnvCredentialsDirectory is the variable systemd sets for a service
	// that loads credentials.
	EnvCredentialsDirectory = "CREDENTIALS_DIRECTORY"
	// CredentialName is the credential's name, bound into it when it is
	// encrypted and its file name in $CREDENTIALS_DIRECTORY.
	CredentialName = "agentry-keys"
)

// sealedVersion is the plaintext's shape.
const sealedVersion = 1

type sealedKeys struct {
	Version  int               `json:"v"`
	Accounts map[string]string `json:"accounts"`
}

// SealPlaintext is what a sealed credential holds for values (keychain
// account → value): the bytes handed to systemd-creds on its stdin, never
// written anywhere in the clear.
func SealPlaintext(values map[string]string) ([]byte, error) {
	for account, v := range values {
		if strings.TrimSpace(account) == "" || v == "" {
			return nil, fmt.Errorf("seal the keys: an empty account or value (%q)", account)
		}
	}
	return json.Marshal(sealedKeys{Version: sealedVersion, Accounts: values})
}

// readSealed reads the credential systemd placed for this service. ok is
// false when this process was given none.
func readSealed() (accounts map[string]string, ok bool, err error) {
	dir := os.Getenv(EnvCredentialsDirectory)
	if dir == "" {
		return nil, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, CredentialName)) //nolint:gosec // systemd's credential folder for this service
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read the schedule's sealed keys: %w", err)
	}
	var k sealedKeys
	if err := json.Unmarshal(b, &k); err != nil || k.Version != sealedVersion {
		return nil, true, errors.New("the schedule's sealed keys are not in a shape this agentry reads: make the schedule again, or run `agentry setup` to seal them again")
	}
	return k.Accounts, true, nil
}

// SealedAccount reads account from this process's sealed credential.
// sealed is false when the process has none, and the keychain is then the
// source; with one, a missing account is "" and the keychain is not asked.
func SealedAccount(account string) (val string, sealed bool, err error) {
	accounts, ok, err := readSealed()
	if !ok || err != nil {
		return "", ok, err
	}
	return accounts[account], true, nil
}

func notSealedErr(account string) error {
	return fmt.Errorf("this signed-out schedule's sealed keys hold no %s: store the key with `agentry setup`, which seals every signed-out schedule's keys again", account)
}

// Collect reads accounts from the OS keychain, for sealing: each required
// one must be there; an optional one the keychain does not hold is left
// out, as a run signed in would go on without it. It reads the keychain
// itself, never a sealed credential.
func Collect(required, optional []string) (map[string]string, error) {
	if Disabled() {
		return nil, disabledErr("read the keys to seal")
	}
	out := map[string]string{}
	for i, account := range append(append([]string{}, required...), optional...) {
		if _, done := out[account]; done {
			continue
		}
		val, err := keyringGet(Service, account)
		switch {
		case errors.Is(err, keyring.ErrNotFound) || (err == nil && val == ""):
			if i < len(required) {
				return nil, fmt.Errorf("the OS keychain has no entry %s: run `agentry setup` to store the key", account)
			}
		case err != nil:
			return nil, fmt.Errorf("read %s from the OS keychain: %w", account, err)
		default:
			out[account] = val
		}
	}
	return out, nil
}
