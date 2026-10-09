// Package modelsignin keeps a person's model sign-ins on this machine
// (plan model-sign-ins.md §4.1, §4.2, D3, D7; agentry-cli#738, DR-86): the
// grants that let their local runs spend a model vendor's plan, one per
// vendor, apart from the MCP connections of pkg/runner/connection. Any
// number are kept; the policy's model setup names which kind is in use;
// switching signs nothing out. A hosted run never reads this store (D2).
//
// Under the runner home:
//
//	model-signins/<vendor>.json    the record, 0600: no token, no secret
//	model-signins/<vendor>.lock    held while a refresh writes back
//	model-signins/<vendor>.secret  the refresh token, 0600, only where no
//	                               keychain answers
//	model-signins/<vendor>.registration.json
//	                               the client the vendor issued this host and
//	                               the account it is for, 0600, kept across a
//	                               removal for the next sign-in (ChatGPT)
//	model-signins/host-id          this host's stable, opaque id (ChatGPT's
//	                               ext_agent_host_id), 0600
//	model-signins/audit.ndjson     removals and their revocation outcome
//
// The secret half is kept in the OS keychain where one answers (account
// model-signin:<vendor>), in the 0600 file otherwise. No access token is
// kept: Use refreshes at each run's start and before it expires mid-run,
// and hands the run the access token alone (invariant I3). A Claude
// sign-in keeps no secret here at all: Claude Code holds it, in a
// directory Agentry owns and never reads (D19, D20), which the record
// names. A connected key (OpenRouter, Poe, Hugging Face; D30) is an API
// key in the model key's keychain entry; its record here only marks it
// connected, for its Disconnect.
//
// The vendors' flows (M4–M7) register what refreshes and revokes their
// grants (RegisterVendor; ChatGPT's is registered by this package, M4); a
// vendor with none registered is kept and listed, and Use and Remove say
// what this build cannot do with it.
package modelsignin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/internal/storefile"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Version is the record schema.
const Version = 1

// refreshTimeout bounds one refresh, which no caller's cancellation cuts.
const refreshTimeout = time.Minute

// The vendors, by the name `agentry model-signins` takes.
const (
	ChatGPT     = "chatgpt"
	Claude      = "claude"
	Copilot     = "copilot"
	OpenRouter  = "openrouter"
	Poe         = "poe"
	HuggingFace = "huggingface"
)

// vendorKinds maps each vendor to the credential kind its sign-in is, and
// whether it is a connected key.
var vendorKinds = map[string]struct {
	kind, provider string
	connected      bool
}{
	ChatGPT:     {policy.KeyKindChatGPTPlan, policy.ProviderOpenAI, false},
	Claude:      {policy.KeyKindClaudeSignIn, policy.ProviderAnthropic, false},
	Copilot:     {policy.KeyKindCopilotSignIn, policy.ProviderCopilot, false},
	OpenRouter:  {policy.KeyKindAPI, "openrouter", true},
	Poe:         {policy.KeyKindAPI, "poe", true},
	HuggingFace: {policy.KeyKindAPI, "huggingface", true},
}

// Vendors lists the vendor names, sorted.
func Vendors() []string {
	out := make([]string, 0, len(vendorKinds))
	for v := range vendorKinds {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// KindOf is the credential kind a vendor's sign-in is ("" for no such
// vendor), and VendorOf the vendor of a model sign-in kind.
func KindOf(vendor string) string { return vendorKinds[vendor].kind }

// VendorOf is the vendor whose sign-in is kind, "" for a kind no vendor
// signs in as (api, subscription).
func VendorOf(kind string) string {
	if !policy.IsModelSignIn(kind) {
		return ""
	}
	for v, k := range vendorKinds {
		if k.kind == kind {
			return v
		}
	}
	return ""
}

// Dir is the store under a runner home.
func Dir(home string) string { return filepath.Join(home, "model-signins") }

// Record is a kept sign-in, without its secrets. Label is how the person
// knows the account ("Signed in as …"); it is shown to them and never
// enters a run, a plan or a receipt.
type Record struct {
	Version  int    `json:"version"`
	Vendor   string `json:"vendor"`
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	// Connected marks an API key a connect flow minted (D30).
	Connected bool     `json:"connected,omitempty"`
	Label     string   `json:"label,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	// ConfigDir is a Claude sign-in's directory (a host path, or a docker
	// volume's name), which only Claude Code writes.
	ConfigDir string `json:"configDir,omitempty"`
	// ClientID is the client the vendor issued this host at its first
	// sign-in, which every refresh and revocation presents (ChatGPT's
	// oaiapp_…). Not a secret: it proves nothing without the grant.
	ClientID string `json:"clientId,omitempty"`

	CreatedAt   string `json:"createdAt"`
	RefreshedAt string `json:"refreshedAt,omitempty"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
	// Broken is why the vendor refused the last refresh: sign in again.
	Broken string `json:"broken,omitempty"`
	// SecretIn is where the refresh token is: "keychain", "file", or ""
	// when none is kept.
	SecretIn string `json:"secretIn,omitempty"`
}

// Grant is what a vendor's sign-in hands Put: the refresh token is kept,
// the access token is not.
type Grant struct {
	RefreshToken string
}

// Access is what Use gives a run: an access token and its expiry (zero
// when the vendor did not say). Never the refresh token (I3).
type Access struct {
	Token     string
	ExpiresAt time.Time
}

// Refreshed is a vendor's answer to a refresh: the access token, and a
// rotated refresh token when it sent one (the ChatGPT grant rotates).
type Refreshed struct {
	AccessToken  string
	ExpiresAt    time.Time
	RefreshToken string
}

// Vendor is what a vendor's flow registers: how its grant is refreshed
// and revoked. Each is given the sign-in's record (its issued client) with
// the kept refresh token. ErrRevoked from Refresh marks the sign-in broken.
type Vendor interface {
	Refresh(ctx context.Context, rec Record, refreshToken string) (Refreshed, error)
	Revoke(ctx context.Context, rec Record, refreshToken string) error
}

// ErrRevoked is a vendor's refusal of a refresh because the grant is gone.
var ErrRevoked = errors.New("the vendor no longer honours this sign-in")

var (
	vendorsMu sync.RWMutex
	vendors   = map[string]Vendor{}
)

// RegisterVendor installs what refreshes and revokes name's grants, and
// returns what puts back the one it replaced (for a test).
func RegisterVendor(name string, v Vendor) (restore func()) {
	vendorsMu.Lock()
	defer vendorsMu.Unlock()
	if _, ok := vendorKinds[name]; !ok {
		panic("modelsignin: no such vendor " + name)
	}
	prev, had := vendors[name]
	vendors[name] = v
	return func() {
		vendorsMu.Lock()
		defer vendorsMu.Unlock()
		if had {
			vendors[name] = prev
		} else {
			delete(vendors, name)
		}
	}
}

func vendorOf(name string) Vendor {
	vendorsMu.RLock()
	defer vendorsMu.RUnlock()
	return vendors[name]
}

// ErrNone is no sign-in kept for that vendor.
var ErrNone = errors.New("no model sign-in")

// BrokenError is a sign-in the vendor no longer honours.
type BrokenError struct{ Vendor, Reason string }

func (e *BrokenError) Error() string {
	return fmt.Sprintf("the %s sign-in no longer works (%s); sign in again (agentry setup)", e.Vendor, e.Reason)
}

// Store is the model sign-in store of one runner home.
type Store struct {
	dir  string
	keys connection.Keychain
	now  func() time.Time
}

// Open is the store under home, keeping secrets in keys
// (connection.OSKeychain{} in production; nil keeps them in 0600 files).
func Open(home string, keys connection.Keychain) *Store {
	return &Store{dir: Dir(home), keys: keys, now: time.Now}
}

func (s *Store) stamp() string                   { return s.now().UTC().Format(time.RFC3339) }
func (s *Store) recordPath(vendor string) string { return filepath.Join(s.dir, vendor+".json") }
func (s *Store) secretPath(vendor string) string { return filepath.Join(s.dir, vendor+".secret") }
func (s *Store) lockPath(vendor string) string   { return filepath.Join(s.dir, vendor+".lock") }
func (s *Store) registrationPath(vendor string) string {
	return filepath.Join(s.dir, vendor+".registration.json")
}

func keychainAccount(vendor string) string { return "model-signin:" + vendor }

// Where a refresh token is kept (Record.SecretIn).
const (
	secretInKeychain = "keychain"
	secretInFile     = "file"
)

func checkVendor(vendor string) error {
	if _, ok := vendorKinds[vendor]; !ok {
		return fmt.Errorf("no model sign-in vendor %q (one of %s)", vendor, strings.Join(Vendors(), ", "))
	}
	return nil
}

// Put keeps vendor's sign-in, replacing one kept before: rec's label,
// scopes and config directory, and g's refresh token in the keychain or
// the 0600 file.
func (s *Store) Put(vendor string, rec Record, g Grant) (*Record, error) {
	if err := checkVendor(vendor); err != nil {
		return nil, err
	}
	unlock, err := s.lock(vendor)
	if err != nil {
		return nil, err
	}
	defer unlock()
	k := vendorKinds[vendor]
	if (rec.ConfigDir != "") != (k.kind == policy.KeyKindClaudeSignIn) {
		return nil, fmt.Errorf("a %s sign-in %s a config directory", vendor, map[bool]string{true: "needs", false: "takes no"}[k.kind == policy.KeyKindClaudeSignIn])
	}
	rec.Version, rec.Vendor, rec.Kind, rec.Provider, rec.Connected = Version, vendor, k.kind, k.provider, k.connected
	rec.CreatedAt, rec.RefreshedAt, rec.LastUsedAt, rec.Broken, rec.SecretIn = s.stamp(), "", "", "", ""
	if g.RefreshToken != "" {
		if err := s.writeSecret(&rec, vendor, g.RefreshToken); err != nil {
			return nil, err
		}
	} else {
		s.dropSecret(vendor)
	}
	if err := s.save(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Registration is what a host keeps of its registration with a vendor that
// issues one per host and account (ChatGPT), across removals: OpenAI asks
// that a later sign-in on the host reuse the issued client and that a
// sign-out keep the mapping.
type Registration struct {
	ClientID string `json:"clientId"`
	// Subject is the account the client was issued for (the ID token's
	// subject): opaque, never shown, never in a run.
	Subject string `json:"subject"`
}

// Registration is vendor's kept registration, nil when there is none.
func (s *Store) Registration(vendor string) (*Registration, error) {
	if err := checkVendor(vendor); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.registrationPath(vendor)) //nolint:gosec // our store layout
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Registration
	if err := json.Unmarshal(b, &r); err != nil || r.ClientID == "" {
		return nil, fmt.Errorf("the %s registration on this computer is not readable", vendor)
	}
	return &r, nil
}

// PutRegistration keeps vendor's registration, replacing one kept before.
func (s *Store) PutRegistration(vendor string, r Registration) error {
	if err := checkVendor(vendor); err != nil {
		return err
	}
	if r.ClientID == "" {
		return errors.New("a registration needs its client id")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return storefile.WriteAtomic(s.registrationPath(vendor), append(b, '\n'))
}

// DropRegistration forgets vendor's registration, so the next sign-in
// registers anew (another account on this host).
func (s *Store) DropRegistration(vendor string) error {
	if err := checkVendor(vendor); err != nil {
		return err
	}
	return storefile.RemoveIfThere(s.registrationPath(vendor))
}

// HostID is this host's stable id, made once: an opaque value a vendor
// uses to tell one host's registrations from another's, never a secret.
// newID makes one when none is kept.
func (s *Store) HostID(newID func() (string, error)) (string, error) {
	path := filepath.Join(s.dir, "host-id")
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // our store layout
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	if err := storefile.WriteAtomic(path, []byte(id+"\n")); err != nil {
		return "", fmt.Errorf("keep this computer's host id: %w", err)
	}
	return id, nil
}

// Get is vendor's kept record, ErrNone when there is none.
func (s *Store) Get(vendor string) (*Record, error) {
	if err := checkVendor(vendor); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.recordPath(vendor)) //nolint:gosec // our store layout
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNone
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil || r.Vendor != vendor {
		return nil, fmt.Errorf("the %s sign-in's record is not readable", vendor)
	}
	return &r, nil
}

// List is every kept sign-in, by vendor.
func (s *Store) List() ([]*Record, error) {
	var out []*Record
	for _, v := range Vendors() {
		r, err := s.Get(v)
		if errors.Is(err, ErrNone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Use is the refresh at a run's start and before its token expires
// mid-run: under the sign-in's lock it trades the kept refresh token for
// an access token through the vendor's registered flow, writes a rotated
// refresh token back before returning (a rotating grant's old token is
// spent), and stamps the record. Refreshes are serialized by the lock, so
// two runs never spend the same rotating token. A refusal that means the
// grant is gone marks the sign-in broken (*BrokenError).
func (s *Store) Use(ctx context.Context, vendor string) (Access, error) {
	if err := checkVendor(vendor); err != nil {
		return Access{}, err
	}
	unlock, err := s.lock(vendor)
	if err != nil {
		return Access{}, err
	}
	defer unlock()
	r, err := s.Get(vendor)
	if err != nil {
		return Access{}, err
	}
	if r.Broken != "" {
		return Access{}, &BrokenError{Vendor: vendor, Reason: r.Broken}
	}
	if r.SecretIn == "" {
		return Access{}, fmt.Errorf("the %s sign-in keeps no grant for a run to refresh", vendor)
	}
	v := vendorOf(vendor)
	if v == nil {
		return Access{}, fmt.Errorf("this build of agentry cannot refresh a %s sign-in yet", vendor)
	}
	rt, err := s.readSecret(r, vendor)
	if err != nil {
		return Access{}, err
	}
	// The refresh and its write-back are not cancelled with the caller: a
	// rotating grant's old token is spent the moment the vendor answers,
	// so a run ending mid-refresh must still keep the new one. Bounded by
	// its own deadline instead.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	got, err := v.Refresh(rctx, *r, rt)
	if errors.Is(err, ErrRevoked) {
		r.Broken = err.Error()
		if serr := s.save(r); serr != nil {
			return Access{}, serr
		}
		return Access{}, &BrokenError{Vendor: vendor, Reason: r.Broken}
	}
	if err != nil {
		return Access{}, fmt.Errorf("refresh the %s sign-in: %w", vendor, err)
	}
	if got.AccessToken == "" {
		return Access{}, fmt.Errorf("refresh the %s sign-in: the vendor sent no access token", vendor)
	}
	if got.RefreshToken != "" && got.RefreshToken != rt {
		if err := s.writeSecret(r, vendor, got.RefreshToken); err != nil {
			return Access{}, fmt.Errorf("the vendor rotated the %s sign-in and the new one could not be kept (%w); sign in again (agentry setup)", vendor, err)
		}
	}
	now := s.stamp()
	r.RefreshedAt, r.LastUsedAt = now, now
	if err := s.save(r); err != nil {
		return Access{}, err
	}
	return Access{Token: got.AccessToken, ExpiresAt: got.ExpiresAt}, nil
}

// The revocation outcomes Remove reports.
const (
	RevokeDone       = "revoked at the vendor"
	RevokeNotOffered = "not offered by this build: forgotten here; revoke it in your account at the vendor"
	RevokeNothing    = "nothing to revoke: no grant was kept here"
	// RevokeClaudeDir: a Claude sign-in's grant is Claude Code's, in its
	// own directory, which this build neither logs out of nor deletes.
	RevokeClaudeDir = "forgotten here; Claude Code's own sign-in stays in its directory until it logs out there, which this build does not do yet: delete %s to end it"
)

// Remove forgets vendor's sign-in: it revokes the kept grant at the vendor
// where the vendor's flow can, deletes the secret and the record, and logs
// the outcome. A Claude sign-in's directory is the caller's to log out of
// and delete (Claude Code's own logout runs there; M5).
func (s *Store) Remove(ctx context.Context, vendor, why string) (string, error) {
	if err := checkVendor(vendor); err != nil {
		return "", err
	}
	unlock, err := s.lock(vendor)
	if err != nil {
		return "", err
	}
	r, err := s.Get(vendor)
	if err != nil {
		unlock()
		return "", err
	}
	outcome := RevokeNothing
	if r.ConfigDir != "" {
		outcome = fmt.Sprintf(RevokeClaudeDir, r.ConfigDir)
	}
	if r.SecretIn != "" {
		outcome = RevokeNotOffered
		if v := vendorOf(vendor); v != nil {
			if rt, err := s.readSecret(r, vendor); err != nil {
				outcome = "not attempted: " + err.Error()
			} else if err := v.Revoke(ctx, *r, rt); err != nil {
				outcome = "failed: " + err.Error()
			} else {
				outcome = RevokeDone
			}
		}
	}
	s.dropSecret(vendor)
	rmErr := storefile.RemoveIfThere(s.recordPath(vendor))
	unlock()
	s.audit(r, why, outcome)
	return outcome, rmErr
}

func (s *Store) save(r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return storefile.WriteAtomic(s.recordPath(r.Vendor), append(b, '\n'))
}

// secret is the kept half, as JSON so it can grow without a new account.
type secret struct {
	RefreshToken string `json:"refreshToken"`
}

func (s *Store) writeSecret(r *Record, vendor, refresh string) error {
	raw, err := json.Marshal(secret{RefreshToken: refresh})
	if err != nil {
		return err
	}
	if s.keys != nil {
		if err := s.keys.Set(keychainAccount(vendor), string(raw)); err == nil {
			r.SecretIn = secretInKeychain
			_ = os.Remove(s.secretPath(vendor))
			return nil
		}
	}
	if err := storefile.WriteAtomic(s.secretPath(vendor), raw); err != nil {
		return fmt.Errorf("keep the %s sign-in's refresh token: %w", vendor, err)
	}
	if s.keys != nil {
		_ = s.keys.Delete(keychainAccount(vendor))
	}
	r.SecretIn = secretInFile
	return nil
}

func (s *Store) readSecret(r *Record, vendor string) (string, error) {
	var raw string
	switch r.SecretIn {
	case secretInKeychain:
		if s.keys == nil {
			return "", fmt.Errorf("the %s sign-in's refresh token is in the OS keychain, which is not in use here", vendor)
		}
		v, err := s.keys.Get(keychainAccount(vendor))
		if err != nil {
			return "", fmt.Errorf("read the %s sign-in's refresh token from the OS keychain: %w", vendor, err)
		}
		raw = v
	case secretInFile:
		b, err := os.ReadFile(s.secretPath(vendor)) //nolint:gosec // our store layout
		if err != nil {
			return "", fmt.Errorf("read the %s sign-in's refresh token: %w", vendor, err)
		}
		raw = string(b)
	default:
		return "", fmt.Errorf("the %s sign-in keeps no refresh token", vendor)
	}
	var sec secret
	if err := json.Unmarshal([]byte(raw), &sec); err != nil || sec.RefreshToken == "" {
		return "", fmt.Errorf("the %s sign-in's kept secret is not readable", vendor)
	}
	return sec.RefreshToken, nil
}

func (s *Store) dropSecret(vendor string) {
	if s.keys != nil {
		_ = s.keys.Delete(keychainAccount(vendor))
	}
	_ = storefile.RemoveIfThere(s.secretPath(vendor))
}

// audit appends a removal to the store's log: what, why and the
// revocation's outcome, never a token or the account's label.
func (s *Store) audit(r *Record, why, outcome string) {
	line, _ := json.Marshal(map[string]string{
		"at": s.stamp(), "action": "removed", "why": why, "vendor": r.Vendor, "kind": r.Kind, "revocation": outcome,
	})
	f, err := os.OpenFile(filepath.Join(s.dir, "audit.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // our store layout
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// lock holds vendor's lock file until the returned func is called, waiting
// for another process holding it, so one writer at a time writes a
// rotated refresh token back.
func (s *Store) lock(vendor string) (func(), error) {
	return storefile.Lock(s.lockPath(vendor), "the "+vendor+" sign-in")
}

// RunGrant is what a run on this machine is given of a model sign-in: an
// access token for a token-file sign-in, with the Renew that refreshes it
// mid-run, or a config-directory sign-in's directory. Never the refresh
// token (I3).
type RunGrant struct {
	Kind      string
	Access    Access
	ConfigDir string
	// Renew refreshes a token-file sign-in before its token expires; nil
	// for a config directory, which the harness keeps itself.
	Renew func(ctx context.Context) (Access, error)
}

// ErrNoSignIn is a run whose plan spends a sign-in the store does not keep.
var ErrNoSignIn = errors.New("not signed in")

// ForRun is the grant a plan's model sign-in (spec.ModelSignInSpec's kind
// and delivery) is run on: the kind's vendor's, refreshed now for a token
// file. ErrNoSignIn (wrapped) when none is kept.
func (s *Store) ForRun(ctx context.Context, kind, delivery string) (*RunGrant, error) {
	vendor := VendorOf(kind)
	if vendor == "" {
		return nil, fmt.Errorf("%q is not a model sign-in kind", kind)
	}
	r, err := s.Get(vendor)
	if errors.Is(err, ErrNone) {
		return nil, fmt.Errorf("%w: this run spends %s and none is kept on this computer; sign in first (agentry setup)", ErrNoSignIn, policy.SignInSentence(kind))
	}
	if err != nil {
		return nil, err
	}
	g := &RunGrant{Kind: kind}
	switch delivery {
	case spec.ModelSignInConfigDir:
		if r.ConfigDir == "" {
			return nil, fmt.Errorf("%w: the %s sign-in names no directory; sign in again (agentry setup)", ErrNoSignIn, vendor)
		}
		g.ConfigDir = r.ConfigDir
	case spec.ModelSignInToken:
		if g.Access, err = s.Use(ctx, vendor); err != nil {
			return nil, err
		}
		g.Renew = func(ctx context.Context) (Access, error) { return s.Use(ctx, vendor) }
	default:
		return nil, fmt.Errorf("unknown model sign-in delivery %q", delivery)
	}
	return g, nil
}
