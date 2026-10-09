package connection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/internal/storefile"
	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
)

// Keychain is where secrets are kept (OSKeychain in production).
type Keychain interface {
	Get(account string) (string, error)
	Set(account, value string) error
	Delete(account string) error
}

// OSKeychain is the OS keychain under the clients' service (modelkey's),
// unless AGENTRY_NO_KEYRING is set, when every call fails and the store
// keeps its 0600 files.
type OSKeychain struct{}

var errKeychainOff = errors.New("the OS keychain is turned off (" + modelkey.EnvNoKeyring + ")")

// Get implements Keychain.
func (OSKeychain) Get(account string) (string, error) {
	if modelkey.Disabled() {
		return "", errKeychainOff
	}
	return keyring.Get(modelkey.Service, account)
}

// Set implements Keychain.
func (OSKeychain) Set(account, value string) error {
	if modelkey.Disabled() {
		return errKeychainOff
	}
	return keyring.Set(modelkey.Service, account, value)
}

// Delete implements Keychain.
func (OSKeychain) Delete(account string) error {
	if modelkey.Disabled() {
		return errKeychainOff
	}
	if err := keyring.Delete(modelkey.Service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// Clients keeps the person's own OAuth clients, one per issuer (D23): the
// id in a 0600 record, the secret in the keychain or a 0600 file beside it.
// Every consent to that issuer uses it; each release still makes its own
// connection (D10).
type Clients struct {
	dir  string
	keys Keychain
}

// OpenClients is the client store under home.
func OpenClients(home string, keys Keychain) *Clients {
	return &Clients{dir: filepath.Join(Dir(home), "clients"), keys: keys}
}

// OwnClient is a kept client of the person's.
type OwnClient struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	SecretIn string `json:"secretIn,omitempty"` // "keychain", "file", or "" for none
}

func issuerID(issuer string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(issuer, "/")))
	return hex.EncodeToString(sum[:8])
}

func clientAccount(id string) string           { return "mcp-client:" + id }
func (c *Clients) recordPath(id string) string { return filepath.Join(c.dir, id+".json") }
func (c *Clients) secretPath(id string) string { return filepath.Join(c.dir, id+".secret") }

// NormalizeIssuer holds an issuer to an https address with no query or
// fragment, without a trailing slash.
func NormalizeIssuer(issuer string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%q is not an issuer: an https address such as https://accounts.google.com", issuer)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Set keeps issuer's client, replacing any kept before.
func (c *Clients) Set(issuer, clientID, secret string) error {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || strings.ContainsAny(clientID, " \t\r\n") {
		return errors.New("the client id is empty or has spaces in it")
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	id := issuerID(issuer)
	rec := OwnClient{Issuer: issuer, ClientID: clientID}
	// The new secret is kept first; then the place it is not kept is
	// cleared, so neither a failure nor a keychain that refuses leaves
	// an old secret behind or the client without one.
	switch {
	case secret == "":
	case c.keys != nil && c.keys.Set(clientAccount(id), secret) == nil:
		rec.SecretIn = secretInKeychain
	default:
		if err := storefile.WriteAtomic(c.secretPath(id), []byte(secret)); err != nil {
			return fmt.Errorf("keep the client secret: %w", err)
		}
		rec.SecretIn = secretInFile
	}
	if rec.SecretIn != secretInFile {
		_ = storefile.RemoveIfThere(c.secretPath(id))
	}
	if rec.SecretIn != secretInKeychain && c.keys != nil {
		_ = c.keys.Delete(clientAccount(id))
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	return storefile.WriteAtomic(c.recordPath(id), append(b, '\n'))
}

// Get is issuer's client with its secret, or nil when none is kept.
func (c *Clients) Get(issuer string) (*mcpauth.Client, error) {
	rec, err := c.record(issuer)
	if err != nil || rec == nil {
		return nil, err
	}
	cl := &mcpauth.Client{ID: rec.ClientID, Kind: mcpauth.ClientOwn}
	id := issuerID(rec.Issuer)
	switch rec.SecretIn {
	case secretInKeychain:
		if c.keys == nil {
			return nil, errors.New("your OAuth client's secret is in the OS keychain, which is not in use here")
		}
		v, err := c.keys.Get(clientAccount(id))
		if err != nil {
			return nil, fmt.Errorf("read your OAuth client's secret for %s from the OS keychain: %w", rec.Issuer, err)
		}
		cl.Secret = v
	case secretInFile:
		b, err := os.ReadFile(c.secretPath(id)) //nolint:gosec // our store layout
		if err != nil {
			return nil, fmt.Errorf("read your OAuth client's secret for %s: %w", rec.Issuer, err)
		}
		cl.Secret = string(b)
	}
	return cl, nil
}

func (c *Clients) record(issuer string) (*OwnClient, error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(c.recordPath(issuerID(issuer))) //nolint:gosec // our store layout
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec OwnClient
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Issuer != issuer {
		return nil, fmt.Errorf("the kept OAuth client for %s is not readable", issuer)
	}
	return &rec, nil
}

// List is every kept client, without secrets.
func (c *Clients) List() ([]OwnClient, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []OwnClient
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(c.dir, e.Name())) //nolint:gosec // our store layout
		if err != nil {
			continue
		}
		var rec OwnClient
		if json.Unmarshal(raw, &rec) == nil && rec.Issuer != "" {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issuer < out[j].Issuer })
	return out, nil
}

// Remove forgets issuer's client; false when none was kept. Connections
// made with it stop refreshing and ask again.
func (c *Clients) Remove(issuer string) (bool, error) {
	rec, err := c.record(issuer)
	if err != nil || rec == nil {
		return false, err
	}
	id := issuerID(rec.Issuer)
	if c.keys != nil && rec.SecretIn == secretInKeychain {
		_ = c.keys.Delete(clientAccount(id))
	}
	return true, errors.Join(storefile.RemoveIfThere(c.secretPath(id)), storefile.RemoveIfThere(c.recordPath(id)))
}
