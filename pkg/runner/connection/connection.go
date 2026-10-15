// Package connection keeps a person's OAuth connections to remote MCP
// servers on this machine: one per QuickAgent release (ref plus
// archive digest) and server, never opened for another QuickAgent or
// another version of the same one, always kept until the person removes it
// or the version is uninstalled.
//
// Under the runner home:
//
//	connections/<id>.json      the record, 0600: no token, no client secret
//	connections/<id>.lock      held while a refresh writes the record back
//	connections/<id>.secret    the refresh token and a registered client's
//	                           secret, 0600, only where no keychain answers
//	connections/clients/<h>.json, <h>.secret   the person's own clients
//	connections/audit.ndjson   removals and their revocation outcome
//
// The secret half is kept in the OS keychain where one answers (account
// connection:<id>, mcp-client:<h>), in the 0600 file beside the record
// otherwise. No access token is kept: the runner refreshes at each run's
// start (Use) and hands the run the access token alone.
package connection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/internal/storefile"
	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// Version is the record schema.
const Version = 1

// Dir is the store under a runner home.
func Dir(home string) string { return filepath.Join(home, "connections") }

// Key names a connection: the release and the server.
type Key struct {
	Ref    string // ns/name@version
	Digest string // the release's archive digest
	Server string // the remote server's address
}

// ID is the connection's file and keychain name.
func (k Key) ID() string {
	sum := sha256.Sum256([]byte("agentry/connection/v1\x00" + k.Ref + "\x00" + k.Digest + "\x00" + k.Server))
	return hex.EncodeToString(sum[:12])
}

// Connection is the kept record, without its secrets.
type Connection struct {
	Version int    `json:"version"`
	Ref     string `json:"ref"`
	Digest  string `json:"digest"`
	Server  string `json:"server"`

	Issuer             string `json:"issuer"`
	Resource           string `json:"resource,omitempty"`
	TokenEndpoint      string `json:"tokenEndpoint"`
	RevocationEndpoint string `json:"revocationEndpoint,omitempty"`
	// Client is who signed in: its id and kind; a secret is never here.
	Client mcpauth.Client `json:"client"`
	// Scopes are what was granted (or asked, where the vendor did not say).
	Scopes []string `json:"scopes,omitempty"`
	Label  string   `json:"label,omitempty"`

	CreatedAt   string `json:"createdAt"`
	RefreshedAt string `json:"refreshedAt,omitempty"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
	// Broken is why the vendor refused the last refresh; the next Start
	// asks for consent again.
	Broken string `json:"broken,omitempty"`
	// SecretIn is where the secret half is: "keychain" or "file".
	SecretIn string `json:"secretIn"`
}

// Key is the connection's key.
func (c *Connection) Key() Key { return Key{Ref: c.Ref, Digest: c.Digest, Server: c.Server} }

// MadeWith is whether g was made with the client c was made with, at the
// same issuer. Such a grant replaces c by overwriting it, never by revoking
// c's refresh token: a vendor that revokes an account's whole grant to a
// client (Google) would end g, and every other connection made with that
// client, with it.
func (c *Connection) MadeWith(g *mcpauth.Grant) bool {
	return c.Client.ID != "" && c.Client.ID == g.Client.ID && c.Issuer == g.Discovery.Server.Issuer
}

// SharesGrant is every other connection in all made with c's client at c's
// issuer: the connections a vendor that revokes the whole grant ends when
// c is revoked. A grant shared across one vendor project's different
// clients (Google's revocation is per project) is not seen here.
func SharesGrant(all []*Connection, c *Connection) []*Connection {
	var out []*Connection
	for _, o := range all {
		if o.Key() != c.Key() && c.Client.ID != "" && o.Client.ID == c.Client.ID && o.Issuer == c.Issuer {
			out = append(out, o)
		}
	}
	return out
}

// secrets is the half never written beside the record.
type secrets struct {
	RefreshToken string `json:"refreshToken,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// ErrNone is no connection for that release and server.
var ErrNone = errors.New("no connection")

// BrokenError is a connection the vendor no longer honours.
type BrokenError struct {
	Server string
	Reason string
}

func (e *BrokenError) Error() string {
	return fmt.Sprintf("the connection to %s no longer works (%s); connect again", e.Server, e.Reason)
}

// Store is the connection store of one runner home.
type Store struct {
	dir  string
	keys Keychain
	now  func() time.Time
}

// Open is the store under home, keeping secrets in keys (OSKeychain{} in
// production; nil keeps every secret in its 0600 file).
func Open(home string, keys Keychain) *Store {
	return &Store{dir: Dir(home), keys: keys, now: time.Now}
}

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339) }

func (s *Store) recordPath(id string) string { return filepath.Join(s.dir, id+".json") }
func (s *Store) secretPath(id string) string { return filepath.Join(s.dir, id+".secret") }
func (s *Store) lockPath(id string) string   { return filepath.Join(s.dir, id+".lock") }

// Get is the connection for k, or ErrNone. A record whose release or
// server is not k's is not k's connection, whatever its file is called.
func (s *Store) Get(k Key) (*Connection, error) {
	c, err := s.load(k.ID())
	if err != nil {
		return nil, err
	}
	if c.Ref != k.Ref || c.Digest != k.Digest || c.Server != k.Server {
		return nil, ErrNone
	}
	return c, nil
}

func (s *Store) load(id string) (*Connection, error) {
	raw, err := os.ReadFile(s.recordPath(id)) //nolint:gosec // our store layout
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNone
	}
	if err != nil {
		return nil, fmt.Errorf("read the connection %s: %w", id, err)
	}
	var c Connection
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the connection record %s is not readable: %w", s.recordPath(id), err)
	}
	if c.Version != Version {
		return nil, fmt.Errorf("the connection record %s has version %d, not %d: remove it and connect again", s.recordPath(id), c.Version, Version)
	}
	return &c, nil
}

// Put keeps a finished consent as k's connection, replacing any before it
// (whose grant the caller revokes, if it wants to, and only when the new
// grant was not made with the same client: MadeWith).
func (s *Store) Put(k Key, g *mcpauth.Grant, label string) (*Connection, error) {
	if g.Token.RefreshToken == "" {
		return nil, fmt.Errorf("%s gave no refresh token, so the connection cannot be kept for the next run", k.Server)
	}
	scopes := g.Token.Scopes
	if len(scopes) == 0 {
		scopes = g.Scopes
	}
	c := &Connection{
		Version: Version, Ref: k.Ref, Digest: k.Digest, Server: k.Server,
		Issuer: g.Discovery.Server.Issuer, Resource: g.Discovery.Resource,
		TokenEndpoint: g.Discovery.Server.TokenEndpoint, RevocationEndpoint: g.Discovery.Server.RevocationEndpoint,
		Client: mcpauth.Client{ID: g.Client.ID, Kind: g.Client.Kind, AuthMethod: g.Client.AuthMethod},
		Scopes: scopes, Label: label, CreatedAt: s.stamp(),
	}
	sec := secrets{RefreshToken: g.Token.RefreshToken}
	if g.Client.Kind != mcpauth.ClientOwn {
		// A registered client's secret is this connection's; the person's
		// own client's is kept once, with the client.
		sec.ClientSecret = g.Client.Secret
	}
	id := k.ID()
	unlock, err := s.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.writeSecrets(c, id, sec); err != nil {
		return nil, err
	}
	if err := s.save(id, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) save(id string, c *Connection) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return storefile.WriteAtomic(s.recordPath(id), append(b, '\n'))
}

// writeSecrets keeps the secret half in the keychain, or in the 0600 file
// where none answers, and records which on c.
func (s *Store) writeSecrets(c *Connection, id string, sec secrets) error {
	raw, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if s.keys != nil {
		if err := s.keys.Set(keychainAccount(id), string(raw)); err == nil {
			c.SecretIn = secretInKeychain
			_ = os.Remove(s.secretPath(id))
			return nil
		}
	}
	if err := storefile.WriteAtomic(s.secretPath(id), raw); err != nil {
		return fmt.Errorf("keep the connection's refresh token: %w", err)
	}
	// A keychain that refused this write may still hold an older secret:
	// it is not left behind.
	if s.keys != nil {
		_ = s.keys.Delete(keychainAccount(id))
	}
	c.SecretIn = secretInFile
	return nil
}

func (s *Store) readSecrets(c *Connection, id string) (secrets, error) {
	var raw string
	switch c.SecretIn {
	case secretInKeychain:
		if s.keys == nil {
			return secrets{}, errors.New("the connection's refresh token is in the OS keychain, which is not in use here")
		}
		v, err := s.keys.Get(keychainAccount(id))
		if err != nil {
			return secrets{}, fmt.Errorf("read the connection's refresh token from the OS keychain: %w", err)
		}
		raw = v
	default:
		b, err := os.ReadFile(s.secretPath(id)) //nolint:gosec // our store layout
		if err != nil {
			return secrets{}, fmt.Errorf("read the connection's refresh token: %w", err)
		}
		raw = string(b)
	}
	var sec secrets
	if err := json.Unmarshal([]byte(raw), &sec); err != nil {
		return secrets{}, errors.New("the connection's kept secret is not readable")
	}
	return sec, nil
}

func keychainAccount(id string) string { return "connection:" + id }

// Where a secret half is kept (Connection.SecretIn, OwnClient.SecretIn).
const (
	secretInKeychain = "keychain"
	secretInFile     = "file"
)

// Use is the refresh at a run's start and before its token expires mid-run:
// under the connection's lock, it trades the kept refresh token for an access
// token, writes a rotated refresh token back before returning, and stamps the
// record. A refusal that means the grant is gone marks the connection broken
// (*BrokenError); the next Start asks again. The access token is the only thing
// a run is given.
func (s *Store) Use(ctx context.Context, k Key, httpc *http.Client, clients *Clients) (Access, *Connection, error) {
	id := k.ID()
	unlock, err := s.lock(id)
	if err != nil {
		return Access{}, nil, err
	}
	defer unlock()
	c, err := s.Get(k)
	if err != nil {
		return Access{}, nil, err
	}
	if c.Broken != "" {
		return Access{}, c, &BrokenError{Server: c.Server, Reason: c.Broken}
	}
	sec, err := s.readSecrets(c, id)
	if err != nil {
		return Access{}, c, err
	}
	client, err := s.clientOf(c, sec, clients)
	if err != nil {
		return Access{}, c, err
	}
	tok, err := mcpauth.Refresh(ctx, httpc, c.TokenEndpoint, client, sec.RefreshToken, c.Resource)
	if mcpauth.GrantRevoked(err) {
		c.Broken = err.Error()
		if serr := s.save(id, c); serr != nil {
			return Access{}, c, serr
		}
		return Access{}, c, &BrokenError{Server: c.Server, Reason: c.Broken}
	}
	if err != nil {
		return Access{}, c, fmt.Errorf("refresh the connection to %s: %w", c.Server, err)
	}
	if tok.RefreshToken != "" && tok.RefreshToken != sec.RefreshToken {
		sec.RefreshToken = tok.RefreshToken
		if err := s.writeSecrets(c, id, sec); err != nil {
			return Access{}, c, err
		}
	}
	now := s.stamp()
	c.RefreshedAt, c.LastUsedAt = now, now
	if err := s.save(id, c); err != nil {
		return Access{}, c, err
	}
	return Access{Token: tok.AccessToken, ExpiresAt: tok.ExpiresAt}, c, nil
}

// Access is what Use gives a run of a connection: an access token and its
// expiry (zero when the vendor did not say). Never the refresh token.
type Access struct {
	Token     string
	ExpiresAt time.Time
}

// clientOf is the client a connection refreshes as: a registered client
// with its kept secret, or the person's own client, which must still be
// the one the grant was made with.
func (s *Store) clientOf(c *Connection, sec secrets, clients *Clients) (mcpauth.Client, error) {
	cl := c.Client
	if cl.Kind != mcpauth.ClientOwn {
		cl.Secret = sec.ClientSecret
		return cl, nil
	}
	if clients == nil {
		return mcpauth.Client{}, errors.New("the connection signs in with your own OAuth client, and no client store was given")
	}
	own, err := clients.Get(c.Issuer)
	if err != nil {
		return mcpauth.Client{}, err
	}
	if own == nil || own.ID != cl.ID {
		return mcpauth.Client{}, &BrokenError{Server: c.Server, Reason: "the OAuth client it was made with is no longer kept for " + c.Issuer}
	}
	cl.Secret = own.Secret
	return cl, nil
}

// List is every kept connection, by ref then server. A record that cannot
// be read is skipped.
func (s *Store) List() ([]*Connection, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Connection
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		c, err := s.load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Server < out[j].Server
	})
	return out, nil
}

// Revocation outcomes, as a removal records them.
const (
	RevokeDone       = "revoked"
	RevokeNotOffered = "not offered by the vendor"
)

// Remove deletes k's connection and first attempts the vendor's revocation
// of its refresh token where the vendor offers one. The outcome is
// returned and written to the store's audit log; a failed revocation does
// not keep the connection.
func (s *Store) Remove(ctx context.Context, k Key, httpc *http.Client, clients *Clients, why string) (string, error) {
	id := k.ID()
	unlock, err := s.lock(id)
	if err != nil {
		return "", err
	}
	c, err := s.Get(k)
	if err != nil {
		unlock()
		return "", err
	}
	outcome := RevokeNotOffered
	if c.RevocationEndpoint != "" {
		outcome = s.revoke(ctx, c, id, httpc, clients)
	}
	if c.SecretIn == secretInKeychain && s.keys != nil {
		_ = s.keys.Delete(keychainAccount(id))
	}
	// The lock file stays: removing it while another process waits on it
	// would let a third lock a new file beside the old one.
	rmErr := errors.Join(storefile.RemoveIfThere(s.secretPath(id)), storefile.RemoveIfThere(s.recordPath(id)))
	unlock()
	s.audit(c, "removed", why, outcome)
	return outcome, rmErr
}

// RevokeSameClient is the outcome a replacement made with the same client
// records: the old refresh token is dropped from the store, not revoked
// (MadeWith).
const RevokeSameClient = "not attempted: replaced by a grant made with the same client, which a vendor that revokes the whole grant would end too"

// RecordReplaced writes old's replacement by a grant made with the same
// client to the audit log, as a removal is, with RevokeSameClient as its
// outcome: the caller then overwrites it with Put.
func (s *Store) RecordReplaced(old *Connection, why string) {
	s.audit(old, "replaced", why, RevokeSameClient)
}

func (s *Store) revoke(ctx context.Context, c *Connection, id string, httpc *http.Client, clients *Clients) string {
	sec, err := s.readSecrets(c, id)
	if err != nil {
		return "not attempted: " + err.Error()
	}
	if sec.RefreshToken == "" {
		return "not attempted: no refresh token kept"
	}
	client, err := s.clientOf(c, sec, clients)
	if err != nil {
		var be *BrokenError
		if !errors.As(err, &be) {
			return "not attempted: " + err.Error()
		}
		client = c.Client // the person's client is gone: try as a public one
	}
	if err := mcpauth.Revoke(ctx, httpc, c.RevocationEndpoint, client, sec.RefreshToken, "refresh_token"); err != nil {
		return "failed: " + err.Error()
	}
	return RevokeDone
}

// audit appends a removal or a replacement to the store's log: what, why
// and the revocation's outcome, never a token.
func (s *Store) audit(c *Connection, action, why, outcome string) {
	line, _ := json.Marshal(map[string]string{
		"at": s.stamp(), "action": action, "why": why,
		"ref": c.Ref, "digest": c.Digest, "server": c.Server, "issuer": c.Issuer, "revocation": outcome,
	})
	f, err := os.OpenFile(filepath.Join(s.dir, "audit.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // our store layout
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// lock holds the connection's lock file until the returned func is called;
// it waits for another process holding it (a run and `agentry connect` at
// once), so one writer at a time writes a rotated refresh token back.
func (s *Store) lock(id string) (func(), error) {
	return storefile.Lock(s.lockPath(id), "the connection "+id)
}
