package connection

import (
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/internal/storefile"
	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// Handed is a connection as it is handed to another machine of the
// person's, which then refreshes it itself: the record, its refresh token
// and, for a client of the vendor's registration, that client's secret; for
// the person's own OAuth client, that client's id and secret. Both machines
// then hold the same grant, so a vendor that rotates its refresh tokens
// leaves one of them to be signed in again.
type Handed struct {
	Record       Connection `json:"record"`
	RefreshToken string     `json:"refreshToken"`
	ClientSecret string     `json:"clientSecret,omitempty"`
	// OwnClientID and OwnClientSecret are the person's own OAuth client's,
	// when the connection was made with it.
	OwnClientID     string `json:"ownClientId,omitempty"`
	OwnClientSecret string `json:"ownClientSecret,omitempty"`
}

// Hand is k's connection with what another machine needs to refresh it.
func (s *Store) Hand(k Key, clients *Clients) (*Handed, error) {
	c, err := s.Get(k)
	if err != nil {
		return nil, err
	}
	if c.Broken != "" {
		return nil, &BrokenError{Server: c.Server, Reason: c.Broken}
	}
	sec, err := s.readSecrets(c, k.ID())
	if err != nil {
		return nil, err
	}
	if sec.RefreshToken == "" {
		return nil, fmt.Errorf("the connection to %s holds no refresh token, so nothing could refresh it there", c.Server)
	}
	h := &Handed{Record: *c, RefreshToken: sec.RefreshToken, ClientSecret: sec.ClientSecret}
	if c.Client.Kind == mcpauth.ClientOwn {
		if clients == nil {
			return nil, errors.New("the connection signs in with your own OAuth client, and no client store was given")
		}
		own, err := clients.Get(c.Issuer)
		if err != nil {
			return nil, err
		}
		if own == nil || own.ID != c.Client.ID {
			return nil, &BrokenError{Server: c.Server, Reason: "the OAuth client it was made with is no longer kept for " + c.Issuer}
		}
		h.OwnClientID, h.OwnClientSecret = own.ID, own.Secret
	}
	return h, nil
}

// Take keeps a handed connection in this store, its secrets in its
// keychain (or 0600 files where none answers), replacing one of the same
// key, and the person's own OAuth client it needs in clients.
func (s *Store) Take(h *Handed, clients *Clients) error {
	if h.RefreshToken == "" {
		return errors.New("a handed connection without a refresh token")
	}
	c := h.Record
	k := c.Key()
	if k.Ref == "" || k.Digest == "" || k.Server == "" {
		return errors.New("a handed connection names no release or server")
	}
	if c.Client.Kind == mcpauth.ClientOwn {
		if clients == nil || h.OwnClientID != c.Client.ID {
			return errors.New("a handed connection's own OAuth client does not match it")
		}
		if err := clients.Set(c.Issuer, h.OwnClientID, h.OwnClientSecret); err != nil {
			return err
		}
	}
	c.Broken, c.LastUsedAt = "", ""
	id := k.ID()
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.writeSecrets(&c, id, secrets{RefreshToken: h.RefreshToken, ClientSecret: h.ClientSecret}); err != nil {
		return err
	}
	return s.save(id, &c)
}

// ForgetHanded removes k's connection from this store without revoking it
// at the vendor: the grant is the person's other machine's too.
func (s *Store) ForgetHanded(k Key) error {
	id := k.ID()
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	if s.keys != nil {
		_ = s.keys.Delete(keychainAccount(id))
	}
	return errors.Join(storefile.RemoveIfThere(s.secretPath(id)), storefile.RemoveIfThere(s.recordPath(id)))
}
