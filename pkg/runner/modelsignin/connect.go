package modelsignin

// Connect your account: OpenRouter, Poe and Hugging Face hand the person's
// computer an ordinary API key (or, Hugging Face, an inference token) over
// OAuth. It is kept where a pasted key is, the OS keychain entry
// provider:<provider> that runs read (pkg/runner/modelkey), so a run spends
// it exactly as it spends a key; the record here only marks it connected,
// for its Disconnect, says when a Poe key expires, and holds Hugging Face's
// refresh token, which the CLI alone spends, at each run's start.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// connectPages are where a person deletes, at the vendor, what a connect
// made: an app can revoke none of it there (OpenRouter, Poe), or only where
// the vendor offers a way (Hugging Face).
var connectPages = map[string]struct{ name, page string }{
	OpenRouter:  {"OpenRouter", mcpauth.OpenRouterKeysPage},
	Poe:         {"Poe", mcpauth.PoeKeysPage},
	HuggingFace: {"Hugging Face", mcpauth.HuggingFaceAppsPage},
}

// ConnectName is how a connect vendor is named to a person, "" for a
// vendor that is not one.
func ConnectName(vendor string) string { return connectPages[vendor].name }

// ConnectPage is where a person revokes a connect at the vendor.
func ConnectPage(vendor string) string { return connectPages[vendor].page }

// The outcomes Remove reports for a connected key.
const (
	// RevokeConnectedKey: the vendor gives apps no way to revoke the key it
	// minted, so it is forgotten here and stays valid there.
	RevokeConnectedKey = "forgotten here; the key stays valid at %s until you delete it there: %s"
	// RevokeConnectedNotOffered: the vendor names no revocation endpoint.
	RevokeConnectedNotOffered = "not offered by %s: forgotten here; remove Agentry's access there: %s"
)

// ConnectedVendor is the connect vendor whose key provider's keychain entry
// holds, "" for a provider no connect makes a key for.
func ConnectedVendor(provider string) string {
	for v, k := range vendorKinds {
		if k.connected && k.provider == provider {
			return v
		}
	}
	return ""
}

// IsConnectVendor reports whether vendor is one a connect makes a key for.
func IsConnectVendor(vendor string) bool { return vendorKinds[vendor].connected }

// ProviderOfVendor is the policy provider a vendor's credential pays for.
func ProviderOfVendor(vendor string) string { return vendorKinds[vendor].provider }

func (s *Store) storeKey(provider, key string) error {
	if s.StoreKey != nil {
		return s.StoreKey(provider, key)
	}
	return modelkey.Store(provider, key)
}

func (s *Store) deleteKey(provider string) error {
	if s.DeleteKey != nil {
		return s.DeleteKey(provider)
	}
	return modelkey.Delete(provider)
}

// KeepConnected keeps a finished connect: the key or token in the model
// key's keychain entry for vendor's provider, replacing a key kept there,
// and the record marked connected, with Hugging Face's refresh token kept
// as a sign-in's is. Nothing stays marked connected when the key could not
// be kept.
func (s *Store) KeepConnected(vendor string, k *mcpauth.ConnectedKey) (*Record, error) {
	if !IsConnectVendor(vendor) {
		return nil, fmt.Errorf("%s is not an account a connect makes a key for", vendor)
	}
	if k == nil || k.Key == "" {
		return nil, fmt.Errorf("the %s connect gave no key to keep", ConnectName(vendor))
	}
	rec := Record{Label: k.Label, Scopes: k.Scopes, ClientID: k.ClientID}
	// A key with no refresh expires once; a refreshed token's expiry moves
	// with every refresh, which the keychain entry does not record.
	if k.RefreshToken == "" && !k.ExpiresAt.IsZero() {
		rec.KeyExpiresAt = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	r, err := s.Put(vendor, rec, Grant{RefreshToken: k.RefreshToken})
	if err != nil {
		return nil, err
	}
	if err := s.storeKey(ProviderOfVendor(vendor), k.Key); err != nil {
		s.forget(vendor)
		return nil, fmt.Errorf("the %s key could not be kept, so nothing is: %w", ConnectName(vendor), err)
	}
	return r, nil
}

// forget drops vendor's record and secret, revoking nothing.
func (s *Store) forget(vendor string) {
	unlock, err := s.lock(vendor)
	if err != nil {
		return
	}
	defer unlock()
	s.dropSecret(vendor)
	_ = s.removeRecord(vendor)
}

// ForgetConnected forgets vendor's connect because the person pasted a key
// in its place: the record and any refresh token go, revoked as Remove
// revokes them, and the keychain entry, which now holds the pasted key, is
// left alone, so a run never renews or refuses a key no connect made. ""
// when no connect is kept.
func (s *Store) ForgetConnected(ctx context.Context, vendor string) (string, error) {
	if !IsConnectVendor(vendor) {
		return "", fmt.Errorf("%s is not an account a connect makes a key for", vendor)
	}
	out, err := s.remove(ctx, vendor, "replaced by a pasted key", false)
	if errors.Is(err, ErrNone) {
		return "", nil
	}
	return out, err
}

// ExpiredError is a connected key past the expiry its vendor gave.
type ExpiredError struct {
	Vendor string
	At     string
}

func (e *ExpiredError) Error() string {
	return fmt.Sprintf("the %s key Agentry connected expired at %s; connect again (agentry model-signins add %s), or paste a key (agentry setup)", ConnectName(e.Vendor), e.At, e.Vendor)
}

// RefreshConnected readies a connected key for a run that reads provider's
// keychain entry, at the run's start: a Hugging Face token is refreshed
// and the new one written into the entry; a Poe key past its expiry
// is refused, naming the fix. A provider with no connected key, or a key
// the person pasted, is left as it is.
func (s *Store) RefreshConnected(ctx context.Context, provider string) error {
	vendor := ConnectedVendor(provider)
	if vendor == "" {
		return nil
	}
	r, err := s.Get(vendor)
	if errors.Is(err, ErrNone) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.SecretIn == "" {
		return s.checkExpiry(r)
	}
	a, err := s.Use(ctx, vendor)
	var be *BrokenError
	if errors.As(err, &be) {
		return fmt.Errorf("the %s account Agentry connected no longer works (%s); connect again (agentry model-signins add %s): %w", ConnectName(vendor), be.Reason, vendor, err)
	}
	if err != nil {
		return err
	}
	if err := s.storeKey(provider, a.Token); err != nil {
		return fmt.Errorf("the %s token was renewed and could not be kept for the run: %w", ConnectName(vendor), err)
	}
	return nil
}

// SignedOutError is a connected key a signed-out schedule's fire cannot ready:
// a Hugging Face token is renewed at each run's start with the refresh token
// kept in the keychain, which is locked while the person is signed out, and the
// copy sealed for the schedule is never renewed. The refresh token is not
// sealed beside it: a rotated one could not be written back, and the keychain's
// would stop working.
type SignedOutError struct{ Vendor string }

func (e *SignedOutError) Error() string {
	name := ConnectName(e.Vendor)
	return fmt.Sprintf("the %s token Agentry connected is renewed at each run's start from the OS keychain, which is locked while you are signed out, so a run cannot use it then: run it while you are signed in, or paste a %s token that does not expire (agentry setup), which seals it for your signed-out schedules", name, name)
}

// CheckSealed is RefreshConnected for a run that reads provider's key from
// the copy sealed for a signed-out schedule, which nothing renews. A
// connect that is renewed is refused (SignedOutError), a key past the
// expiry its vendor gave is refused (ExpiredError), and a key no connect
// made is left alone. It reads the record alone, never the keychain.
func (s *Store) CheckSealed(provider string) error {
	vendor := ConnectedVendor(provider)
	if vendor == "" {
		return nil
	}
	r, err := s.Get(vendor)
	if errors.Is(err, ErrNone) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.SecretIn != "" {
		return &SignedOutError{Vendor: vendor}
	}
	return s.checkExpiry(r)
}

// checkExpiry refuses r's key past the expiry its vendor gave.
func (s *Store) checkExpiry(r *Record) error {
	if r.KeyExpiresAt != "" {
		if at, err := time.Parse(time.RFC3339, r.KeyExpiresAt); err == nil && !s.now().Before(at) {
			return &ExpiredError{Vendor: r.Vendor, At: r.KeyExpiresAt}
		}
	}
	return nil
}

// ReadyForRun readies, at a run's start, the connected key its plan reads
// from the keychain (RefreshConnected), for every run of this computer:
// `agentry run`, a built-in's, the desktop's. Nothing is done for a plan
// that spends a model sign-in or a key whose variable is set (it wins). A
// signed-out schedule's fire reads the copy sealed for it, which is
// checked and never renewed (CheckSealed).
func (s *Store) ReadyForRun(ctx context.Context, plan *spec.Plan) error {
	if plan == nil || plan.Harness.ModelSignIn != nil {
		return nil
	}
	env := plan.Harness.Agent.Env
	if plan.Harness.Host != nil {
		env = plan.Harness.Host.Env
	}
	for _, e := range env {
		if e.FromKeyring == "" || (e.FromHostEnv != "" && os.Getenv(e.FromHostEnv) != "") {
			continue
		}
		provider := modelkey.ProviderOf(e.FromKeyring)
		if _, sealed, _ := modelkey.SealedAccount(e.FromKeyring); sealed {
			if err := s.CheckSealed(provider); err != nil {
				return err
			}
			continue
		}
		if err := s.RefreshConnected(ctx, provider); err != nil {
			return err
		}
	}
	return nil
}

// HuggingFaceVendor refreshes and revokes Hugging Face connects through
// flow, as the client each record names.
type HuggingFaceVendor struct{ Flow mcpauth.HuggingFace }

// The production Hugging Face vendor, through the egress guard.
func init() {
	RegisterVendor(HuggingFace, HuggingFaceVendor{Flow: mcpauth.HuggingFace{KeyConnect: mcpauth.KeyConnect{HTTP: mcpauth.Guard{}.Client()}}})
}

// Refresh implements Vendor. Hugging Face refusing the refresh token is
// ErrRevoked, so the connect is marked broken.
func (v HuggingFaceVendor) Refresh(ctx context.Context, rec Record, refreshToken string) (Refreshed, error) {
	t, err := v.Flow.Refresh(ctx, rec.ClientID, refreshToken)
	if mcpauth.GrantRevoked(err) {
		return Refreshed{}, fmt.Errorf("%w (%v)", ErrRevoked, err)
	}
	if err != nil {
		return Refreshed{}, err
	}
	return Refreshed{AccessToken: t.AccessToken, ExpiresAt: t.ExpiresAt, RefreshToken: t.RefreshToken}, nil
}

// Revoke implements Vendor: mcpauth.ErrRevokeNotOffered while Hugging
// Face's discovery names no revocation endpoint.
func (v HuggingFaceVendor) Revoke(ctx context.Context, rec Record, refreshToken string) error {
	return v.Flow.Revoke(ctx, rec.ClientID, refreshToken)
}
