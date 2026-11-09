package runhost

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
)

// The device's side: its record of each runhost in the runner home, which
// holds no secret, and its two keys per runhost in the system keychain.

// RecordsFile is the device's record of its runhosts, under the runner home.
const RecordsFile = "runhosts.yaml"

// Record is what a device knows of one runhost.
type Record struct {
	Name string `yaml:"name" json:"name"`
	// Host and Port are the server's SSH address, as the person gave it.
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// HostKey is the server's host key, pinned at setup, in authorized_keys
	// form. Every later connection must be offered exactly this key.
	HostKey string `yaml:"hostKey" json:"hostKey"`
	// HostKeyFingerprint is HostKey's SHA256 fingerprint, for a person.
	HostKeyFingerprint string `yaml:"hostKeyFingerprint" json:"hostKeyFingerprint"`
	Owner              Owner  `yaml:"owner" json:"owner"`
	// DeviceID is the id this device paired under.
	DeviceID string `yaml:"deviceID" json:"deviceID"`
	// CLIVersion, Arch and OS are the server's, as of the last check.
	CLIVersion string `yaml:"cliVersion" json:"cliVersion"`
	Arch       string `yaml:"arch" json:"arch"`
	OS         string `yaml:"os" json:"os"`
	GVisor     string `yaml:"gvisor" json:"gvisor"`
	AddedAt    string `yaml:"addedAt" json:"addedAt"`
	// Capabilities are the runs the runhost takes, as its status said at
	// the last check.
	Capabilities map[string]bool `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	// Credits is the Agentry credits token this device made for the
	// runhost and the runhost keeps, when there is one: never the token.
	Credits *CreditsRecord `yaml:"credits,omitempty" json:"credits,omitempty"`
	// SignIns are the model sign-ins this device made on the runhost, which
	// the runhost alone keeps: never a token.
	SignIns []SignInRecord `yaml:"signIns,omitempty" json:"signIns,omitempty"`
	// Models are the saved models the runhost holds, as it last said them:
	// never a credential. Nil until this device first asked.
	Models []ModelRecord `yaml:"models,omitempty" json:"models,omitempty"`
}

// ModelRecord names one of a runhost's saved models.
type ModelRecord struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name" json:"name"`
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Default  bool   `yaml:"default,omitempty" json:"default,omitempty"`
}

// SignInRecord names a model sign-in a runhost keeps: the vendor and the
// account the vendor named.
type SignInRecord struct {
	Vendor  string `yaml:"vendor" json:"vendor"`
	Label   string `yaml:"label,omitempty" json:"label,omitempty"`
	AddedAt string `yaml:"addedAt" json:"addedAt"`
}

// CreditsRecord names a runhost's credits token, which the runhost alone
// holds: its registry, its id and its name among the account's tokens.
type CreditsRecord struct {
	Registry  string `yaml:"registry" json:"registry"`
	TokenID   string `yaml:"tokenID" json:"tokenID"`
	TokenName string `yaml:"tokenName" json:"tokenName"`
	AddedAt   string `yaml:"addedAt" json:"addedAt"`
}

// Records is the records file.
type Records struct {
	Runhosts []Record `yaml:"runhosts"`
}

// LoadRecords reads the records file under home; a missing file is no
// runhosts.
func LoadRecords(home string) (*Records, error) {
	b, err := os.ReadFile(filepath.Join(home, RecordsFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, os.ErrNotExist) {
		return &Records{}, nil
	}
	if err != nil {
		return nil, err
	}
	var r Records
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(home, RecordsFile), err)
	}
	return &r, nil
}

// Save writes the records file, owner-only, replacing it whole.
func (r *Records) Save(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, ".runhosts-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(home, RecordsFile))
}

// Find is the runhost named name.
func (r *Records) Find(name string) (*Record, bool) {
	for i := range r.Runhosts {
		if r.Runhosts[i].Name == name {
			return &r.Runhosts[i], true
		}
	}
	return nil, false
}

// Put adds rec, or replaces the record of the same name.
func (r *Records) Put(rec Record) {
	if old, ok := r.Find(rec.Name); ok {
		*old = rec
		return
	}
	r.Runhosts = append(r.Runhosts, rec)
}

// Drop removes the record named name.
func (r *Records) Drop(name string) {
	out := r.Runhosts[:0]
	for _, rec := range r.Runhosts {
		if rec.Name != name {
			out = append(out, rec)
		}
	}
	r.Runhosts = out
}

// DeviceIDFile holds this device's id, under the runner home.
const DeviceIDFile = "runhost-device-id"

// DeviceID is this device's id, made the first time it is asked for.
func DeviceID(home string) (string, error) {
	p := filepath.Join(home, DeviceIDFile)
	if b, err := os.ReadFile(p); err == nil { //nolint:gosec // the runner home's own file
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(p, []byte(id+"\n"), 0o600)
}

// DeviceKeys are a device's two keys for one runhost: the ed25519 run key it
// signs in with, and the X25519 sealing key the runhost seals its output to.
type DeviceKeys struct {
	Run  ed25519.PrivateKey
	Seal *ecdh.PrivateKey
}

// NewDeviceKeys makes a fresh pair.
func NewDeviceKeys() (*DeviceKeys, error) {
	_, run, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	seal, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &DeviceKeys{Run: run, Seal: seal}, nil
}

// Signer is the run key as an SSH signer.
func (k *DeviceKeys) Signer() (ssh.Signer, error) { return ssh.NewSignerFromKey(k.Run) }

// RunPublic is the run key's public half, in authorized_keys form without a
// comment.
func (k *DeviceKeys) RunPublic() (string, error) {
	pub, err := ssh.NewPublicKey(k.Run.Public())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}

// SealPublic is the sealing key's public half, base64.
func (k *DeviceKeys) SealPublic() string {
	return base64.StdEncoding.EncodeToString(k.Seal.PublicKey().Bytes())
}

// Keychain accounts of a runhost's keys, under the CLI's keychain service.
func runKeyAccount(name string) string  { return "runhost:" + name + ":ssh" }
func sealKeyAccount(name string) string { return "runhost:" + name + ":seal" }

// SaveKeys keeps k in the system keychain for the runhost name.
func SaveKeys(name string, k *DeviceKeys) error {
	block, err := ssh.MarshalPrivateKey(k.Run, DeviceComment(name))
	if err != nil {
		return err
	}
	if err := keyring.Set(modelkey.Service, runKeyAccount(name), string(pem.EncodeToMemory(block))); err != nil {
		return fmt.Errorf("keep the run key in the system keychain: %w", err)
	}
	if err := keyring.Set(modelkey.Service, sealKeyAccount(name), base64.StdEncoding.EncodeToString(k.Seal.Bytes())); err != nil {
		_ = keyring.Delete(modelkey.Service, runKeyAccount(name))
		return fmt.Errorf("keep the sealing key in the system keychain: %w", err)
	}
	return nil
}

// LoadKeys reads the runhost name's keys from the system keychain.
func LoadKeys(name string) (*DeviceKeys, error) {
	runPEM, err := keyring.Get(modelkey.Service, runKeyAccount(name))
	if err != nil {
		return nil, fmt.Errorf("this device's run key for %s is not in the system keychain (%v): pair it again with `agentry runhost add`", name, err)
	}
	raw, err := ssh.ParseRawPrivateKey([]byte(runPEM))
	if err != nil {
		return nil, fmt.Errorf("the run key for %s in the system keychain is not readable: %w", name, err)
	}
	run, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the run key for %s in the system keychain is not an ed25519 key", name)
	}
	sealB64, err := keyring.Get(modelkey.Service, sealKeyAccount(name))
	if err != nil {
		return nil, fmt.Errorf("this device's sealing key for %s is not in the system keychain: %w", name, err)
	}
	sealRaw, err := base64.StdEncoding.DecodeString(sealB64)
	if err != nil {
		return nil, fmt.Errorf("the sealing key for %s in the system keychain is not readable: %w", name, err)
	}
	seal, err := ecdh.X25519().NewPrivateKey(sealRaw)
	if err != nil {
		return nil, fmt.Errorf("the sealing key for %s in the system keychain is not readable: %w", name, err)
	}
	return &DeviceKeys{Run: *run, Seal: seal}, nil
}

// DeleteKeys removes the runhost name's keys from the system keychain; keys
// already gone are not an error.
func DeleteKeys(name string) error {
	var errs []error
	for _, acct := range []string{runKeyAccount(name), sealKeyAccount(name)} {
		if err := keyring.Delete(modelkey.Service, acct); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
