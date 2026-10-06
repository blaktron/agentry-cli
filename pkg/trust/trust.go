// Package trust is the shared client-side provenance layer (§8.8): fetch a
// registry's signing keys, pin them on first use (TOFU) with key-id
// continuity warnings, and verify DSSE envelopes offline against an archive
// digest. Verification uses the same library code the server signs with
// (pkg/attest), so the formats cannot drift — and every client (CLI, desktop)
// shares the SAME pin store under ~/.config/agentry/keys/<host>.yaml, so a
// registry pinned by one client is pinned for all of them.
package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/attest"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// SigningKey is one entry from /.well-known/agentry/signing-keys.json.
// Status and RetiredAt are the registry's own record of the key
// (agentry#350): "active" for the key that signs today, "retired" (with
// the date) for one it signed with before and no longer does, empty for a
// verification-only key the registry never signed with. They are shown,
// never trusted: which keys verify is decided by the pinned PEMs alone.
type SigningKey struct {
	KeyID     string `json:"keyId" yaml:"keyId"`
	Algorithm string `json:"algorithm" yaml:"algorithm"`
	PublicKey string `json:"publicKeyPem" yaml:"publicKeyPem"`
	Status    string `json:"status,omitempty" yaml:"status,omitempty"`
	RetiredAt string `json:"retiredAt,omitempty" yaml:"retiredAt,omitempty"`
}

// Key statuses the registry advertises (agentry#350).
const (
	KeyStatusActive  = "active"
	KeyStatusRetired = "retired"
)

// Retired reports whether the registry recorded the key as retired.
func (k SigningKey) Retired() bool { return k.Status == KeyStatusRetired }

// samePublicKey compares two PEM encodings of a public key, ignoring the
// surrounding whitespace a JSON or YAML round trip may change.
func samePublicKey(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// KeySet is a registry's advertised (or pinned) signing key set.
type KeySet struct {
	Keys []SigningKey `json:"keys" yaml:"keys"`
}

// Attestation states reported by VerifyAttestation, and one the registry
// reports before any attestation is looked for.
const (
	StateVerified     = "verified"
	StateUnsigned     = "unsigned"
	StateUnverifiable = "unverifiable"
	StateMismatch     = "mismatch"
	// StateUnscanned is a version the registry admitted without a scan
	// (an organization's private agent its administrator took off the
	// pipeline; plan organization-subscriptions §4): no findings, no
	// report and no attestation, because nothing was attested. It is
	// its own outcome — not unsigned (a registry that signs nothing), and
	// not a scan with a known gap.
	StateUnscanned = "unscanned"
	// StateBuiltin is a package this binary embeds, run as part of the
	// application (plan knowledge.md §4.2, D6): not a release, so it has
	// no registry attestation and needs none, and it is never verified —
	// a client shows it as the application's own, never as a verified
	// release. Only localrun's Materialize of an embedded package sets it,
	// in memory; no record on disk carries it into a run.
	StateBuiltin = "builtin"
	// StateLocal is a package from a folder on this machine: a `run
	// --local` directory, or a QuickAgent installed from one and never
	// published (agentry-cli#491). Nothing attests to it, so every client
	// gates it as it gates unsigned.
	StateLocal = "local"
)

// Unscanned is the result for a version the registry says was admitted
// without a scan: the resolve answer carries `scanStatus: skipped`, and
// no attestation is asked for, because the absence is the fact and not a
// missing signature.
func Unscanned() AttestationResult {
	return AttestationResult{State: StateUnscanned, Detail: "the registry admitted this version without a scan: no findings, no report and no attestation stand behind it"}
}

// AttestationResult is what verify/install report about provenance.
type AttestationResult struct {
	// State is one of: verified, unsigned, unverifiable, mismatch, unscanned.
	State string `json:"state"`
	// Detail explains the state in one line.
	Detail string `json:"detail"`
	// KeyID that produced the accepted signature (when verified).
	KeyID string `json:"keyId,omitempty"`
	// Predicate summary fields (when verified).
	Agent        string `json:"agent,omitempty"`
	Version      string `json:"version,omitempty"`
	RulesetVer   string `json:"rulesetVersion,omitempty"`
	ReportDigest string `json:"reportDigest,omitempty"`
	JudgeModel   string `json:"judgeModel,omitempty"`
	Source       string `json:"source,omitempty"`
	// FirstUse is true when the key set was pinned by this invocation.
	FirstUse bool `json:"firstUse,omitempty"`
	// KeyRotated is true when the registry presented a key id we had not
	// pinned (a continuity warning, not necessarily an attack).
	KeyRotated bool `json:"keyRotated,omitempty"`
	// KeyChange says how the registry's advertised key set differs from
	// the pinned one, when a signature no pinned key verifies made us
	// look (agentry-cli#139).
	KeyChange *KeyChange `json:"keyChange,omitempty"`
	// KeyRetired is true when the key that verified the signature is one
	// the registry records as retired (agentry#350), and KeyRetiredAt is
	// the date it gives. Shown, not judged: a retired key's signature
	// verifies exactly as before.
	KeyRetired   bool   `json:"keyRetired,omitempty"`
	KeyRetiredAt string `json:"keyRetiredAt,omitempty"`
}

// RetiredNote is the one line a client shows for a signature a retired
// key made, or "" when the accepting key is not retired.
func (r AttestationResult) RetiredNote() string {
	if !r.KeyRetired {
		return ""
	}
	if r.KeyRetiredAt == "" {
		return "signed by a retired key (the registry gives no retirement date)"
	}
	return "signed by a retired key (retired " + retiredDate(r.KeyRetiredAt) + ")"
}

// retiredDate renders the registry's retiredAt for a terminal: the day of
// an RFC 3339 timestamp, else the value with anything but printable ASCII
// dropped and cut short, since the registry chose it.
func retiredDate(v string) string {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, v)
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return clean
}

// KeyChange is how a registry's advertised key set differs from the
// pinned one (agentry-cli#139). Under agentry#350 a rotation adds the new
// key and keeps every earlier one listed as retired, so an advertised set
// that is a strict superset of the pinned one is what a rotation looks
// like; a pinned key that disappears, or an id that now carries a
// different public key, is not.
type KeyChange struct {
	// Added are key ids the registry advertises that are not pinned.
	Added []string `json:"added,omitempty"`
	// Removed are pinned key ids the registry no longer advertises.
	Removed []string `json:"removed,omitempty"`
	// Replaced are key ids advertised with a public key other than the
	// pinned one.
	Replaced []string `json:"replaced,omitempty"`
}

// DiffKeys compares the pinned set with the advertised one.
func DiffKeys(pinned, advertised KeySet) KeyChange {
	var c KeyChange
	have := map[string]SigningKey{}
	for _, k := range pinned.Keys {
		have[k.KeyID] = k
	}
	seen, replaced := map[string]bool{}, map[string]bool{}
	for _, k := range advertised.Keys {
		p, pinnedID := have[k.KeyID]
		// A pinned id advertised with another public key, even beside
		// the pinned one (an id listed twice), is a replacement.
		if pinnedID && !samePublicKey(p.PublicKey, k.PublicKey) && !replaced[k.KeyID] {
			replaced[k.KeyID] = true
			c.Replaced = append(c.Replaced, k.KeyID)
		}
		if seen[k.KeyID] {
			continue
		}
		seen[k.KeyID] = true
		if !pinnedID {
			c.Added = append(c.Added, k.KeyID)
		}
	}
	for _, k := range pinned.Keys {
		if !seen[k.KeyID] {
			c.Removed = append(c.Removed, k.KeyID)
			seen[k.KeyID] = true
		}
	}
	return c
}

// Same reports no difference at all.
func (c KeyChange) Same() bool { return len(c.Added)+len(c.Removed)+len(c.Replaced) == 0 }

// Rotation reports the shape of a rotation: keys added, every pinned key
// still advertised with the same public key.
func (c KeyChange) Rotation() bool {
	return len(c.Added) > 0 && len(c.Removed) == 0 && len(c.Replaced) == 0
}

// String names what changed, e.g. "added key id(s) k2; removed key id(s) k1".
func (c KeyChange) String() string {
	var parts []string
	if len(c.Added) > 0 {
		parts = append(parts, "added key id(s) "+strings.Join(c.Added, ", "))
	}
	if len(c.Removed) > 0 {
		parts = append(parts, "removed key id(s) "+strings.Join(c.Removed, ", "))
	}
	if len(c.Replaced) > 0 {
		parts = append(parts, "a different public key under key id(s) "+strings.Join(c.Replaced, ", "))
	}
	if len(parts) == 0 {
		return "no change"
	}
	return strings.Join(parts, "; ")
}

// refreshStatus copies the registry's status and retirement date onto the
// pinned keys it still advertises with the same public key, and reports
// whether anything changed. Only this record moves: no key is added,
// removed or replaced, so what verifies is exactly what verified before.
func refreshStatus(pinned *KeySet, advertised KeySet) bool {
	byID := map[string]SigningKey{}
	for _, k := range advertised.Keys {
		byID[k.KeyID] = k
	}
	changed := false
	for i, p := range pinned.Keys {
		a, ok := byID[p.KeyID]
		if !ok || !samePublicKey(a.PublicKey, p.PublicKey) {
			continue
		}
		if p.Status != a.Status || p.RetiredAt != a.RetiredAt {
			pinned.Keys[i].Status, pinned.Keys[i].RetiredAt = a.Status, a.RetiredAt
			changed = true
		}
	}
	return changed
}

// retiredSummary names the pinned keys the registry records as retired,
// e.g. "key k1 retired 2026-09-20", or "" when none is.
func retiredSummary(set KeySet) string {
	var parts []string
	for _, k := range set.Keys {
		if k.Retired() {
			when := k.RetiredAt
			if when == "" {
				when = "(no date given)"
			}
			parts = append(parts, "key "+k.KeyID+" retired "+retiredDate(when))
		}
	}
	return strings.Join(parts, ", ")
}

// orDefault guards against a nil client without silently adopting the
// timeout-free http.DefaultClient semantics callers may not want; callers
// should pass their own client.
func orDefault(hc *http.Client) *http.Client {
	if hc == nil {
		return http.DefaultClient
	}
	return hc
}

// FetchKeys reads the registry's key set.
func FetchKeys(ctx context.Context, hc *http.Client, registryURL string) (KeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(registryURL, "/")+"/.well-known/agentry/signing-keys.json", nil)
	if err != nil {
		return KeySet{}, err
	}
	res, err := orDefault(hc).Do(req)
	if err != nil {
		return KeySet{}, fmt.Errorf("fetch signing keys: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return KeySet{}, fmt.Errorf("signing keys: HTTP %d", res.StatusCode)
	}
	var set KeySet
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return KeySet{}, fmt.Errorf("decode signing keys: %w", err)
	}
	return set, nil
}

// EnvPinRegistry names the registry whose pinned keys verify attestations
// in place of the one being talked to (agentry-cli#232). A driver that
// points --registry at a run-scoped prefix of a registry keeps the pins it
// made at login against the registry itself, instead of trusting whatever
// the prefix serves on first use under a pin file named after the prefix.
const EnvPinRegistry = "AGENTRY_PIN_REGISTRY"

// PinnedKeysPath is where TOFU-pinned keys live, per registry: the one
// named by AGENTRY_PIN_REGISTRY when set, else registryURL's own.
func PinnedKeysPath(registryURL string) (string, error) {
	if pin := os.Getenv(EnvPinRegistry); pin != "" {
		registryURL = pin
	}
	return ownPinnedKeysPath(registryURL)
}

// ownPinnedKeysPath is registryURL's own pin file, whatever the override
// says: login pins the registry it was pointed at, never another one.
func ownPinnedKeysPath(registryURL string) (string, error) {
	dir, err := configdir.Dir()
	if err != nil {
		return "", err
	}
	host := registryURL
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.ReplaceAll(strings.TrimRight(host, "/"), "/", "_")
	host = strings.ReplaceAll(host, ":", "_")
	return filepath.Join(dir, "keys", host+".yaml"), nil
}

// LoadPinned reads the pinned set (empty when nothing is pinned yet).
func LoadPinned(registryURL string) (KeySet, error) {
	path, err := PinnedKeysPath(registryURL)
	if err != nil {
		return KeySet{}, err
	}
	return loadPinnedFile(path)
}

// loadPinnedFile reads one pin file (empty when it does not exist).
func loadPinnedFile(path string) (KeySet, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path derives from config dir
	if err != nil {
		if os.IsNotExist(err) {
			return KeySet{}, nil
		}
		return KeySet{}, err
	}
	var set KeySet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return KeySet{}, fmt.Errorf("pinned keys: %w", err)
	}
	return set, nil
}

// SavePinned writes the pinned set (0600).
func SavePinned(registryURL string, set KeySet) error {
	path, err := PinnedKeysPath(registryURL)
	if err != nil {
		return err
	}
	return savePinnedFile(path, set)
}

// savePinnedFile writes one pin file (0600).
func savePinnedFile(path string, set KeySet) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(set)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// PinAtLogin implements the §11 login row: "pins signing keys (TOFU)".
// Pinning at login rather than at first verify is the whole point — login is
// the moment a human deliberately chose this registry and typed a credential
// for it, which is exactly when trust-on-first-use should be established.
// Pinning later, at the first `verify` or `install`, silently moves that
// decision to whichever download happens to come first.
//
// It returns a human-readable note describing what happened, and never fails
// the login: a registry with signing switched off publishes an empty key set,
// and that is a legitimate self-host configuration (§8.8), not a login error.
func PinAtLogin(ctx context.Context, hc *http.Client, registryURL string) string {
	return PinAtLoginResult(ctx, hc, registryURL).Note
}

// LoginPinResult reports pinning without putting remote errors or key material
// into a machine client's output. Note retains the terminal explanation.
type LoginPinResult struct {
	// Status is pinned, unchanged, unsigned, rotation, mismatch or unavailable.
	Status string
	Note   string
}

// PinAtLoginResult is PinAtLogin's structured counterpart for machine clients.
func PinAtLoginResult(ctx context.Context, hc *http.Client, registryURL string) LoginPinResult {
	// Login pins the registry it was pointed at, whatever
	// AGENTRY_PIN_REGISTRY names: the override chooses which pins verify,
	// and must never let a prefix's keys be written into another
	// registry's pin file (agentry-cli#232).
	own, err := ownPinnedKeysPath(registryURL)
	if err != nil {
		return LoginPinResult{"unavailable", "could not read pinned signing keys: " + err.Error()}
	}
	existing, err := loadPinnedFile(own)
	if err != nil {
		return LoginPinResult{"unavailable", "could not read pinned signing keys: " + err.Error()}
	}

	fetched, err := FetchKeys(ctx, hc, registryURL)
	if err != nil {
		return LoginPinResult{"unavailable", "could not fetch signing keys (" + err.Error() + "); they will be pinned on first verify"}
	}
	if len(fetched.Keys) == 0 {
		return LoginPinResult{"unsigned", "the registry publishes no signing keys — releases from it are unsigned"}
	}

	if len(existing.Keys) == 0 {
		if err := savePinnedFile(own, fetched); err != nil {
			return LoginPinResult{"unavailable", "could not pin signing keys: " + err.Error()}
		}
		return LoginPinResult{"pinned", fmt.Sprintf("pinned %d signing key(s) on first use (TOFU)", len(fetched.Keys))}
	}
	change := DiffKeys(existing, fetched)
	// The registry's status record for keys still pinned (a key it has
	// since retired) is taken when the set is unchanged or looks like a
	// rotation: it is shown, never used to decide what verifies. A set
	// that changed any other way is not trusted even for that.
	statusNote := ""
	if (change.Same() || change.Rotation()) && refreshStatus(&existing, fetched) {
		if err := savePinnedFile(own, existing); err != nil {
			statusNote = " (could not record the keys' status: " + err.Error() + ")"
		}
	}
	if retired := retiredSummary(existing); retired != "" {
		statusNote = "; " + retired + statusNote
	}
	if change.Same() {
		return LoginPinResult{"unchanged", fmt.Sprintf("signing keys unchanged (%d pinned)%s", len(existing.Keys), statusNote)}
	}
	// A changed key set at login is reported, never silently adopted:
	// re-pinning on the strength of a fresh fetch would defeat the pin.
	// A superset is reported as consistent with a rotation
	// (agentry-cli#139, DR-35), which is all the client can tell, and is
	// still not adopted: the new key is trusted only after a deliberate
	// re-pin.
	repin := "delete " + own + " and log in again to re-pin deliberately"
	if change.Rotation() {
		return LoginPinResult{"rotation", "possible key rotation: the registry " + change.String() + " and still advertises every pinned key" + statusNote + ", which is consistent with a key rotation. " +
			"Verification keeps using the pinned keys, so releases they signed still verify and a release signed only by the new key is refused until you re-pin. " +
			"Confirm the rotation with the registry's operator, then " + repin}
	}
	return LoginPinResult{"mismatch", "WARNING: the registry's signing keys no longer match the pinned set: " + change.String() + statusNote + ". " +
		"A rotation keeps every earlier key listed, so this is not one; treat it as a possible compromise and confirm it with the registry's operator. " +
		"Verification keeps using the pinned keys; only once the change is confirmed, " + repin}
}

// VerifyAttestation fetches and verifies the attestation for a version.
// archiveDigest is the digest the client computed from the downloaded bytes,
// so a verified attestation binds the signature to what is actually on disk.
// token, when non-empty, is attached as a bearer credential — but only to
// requests going to the registry's own host, never to a CDN (§9.4).
func VerifyAttestation(ctx context.Context, hc *http.Client, registryURL, token, attestationURL, archiveDigest string) AttestationResult {
	if attestationURL == "" {
		return AttestationResult{State: StateUnsigned, Detail: "the registry published no attestation for this version"}
	}

	// Fetch the envelope.
	u := attestationURL
	if !strings.HasPrefix(u, "http") {
		u = strings.TrimRight(registryURL, "/") + u
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return AttestationResult{State: StateUnverifiable, Detail: err.Error()}
	}
	if token != "" && sameHost(u, registryURL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := orDefault(hc).Do(req)
	if err != nil {
		return AttestationResult{State: StateUnverifiable, Detail: "fetch attestation: " + err.Error()}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return AttestationResult{State: StateUnsigned, Detail: "no attestation recorded for this version"}
	}
	if res.StatusCode != http.StatusOK {
		return AttestationResult{State: StateUnverifiable, Detail: fmt.Sprintf("attestation: HTTP %d", res.StatusCode)}
	}
	var env attest.DSSEEnvelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return AttestationResult{State: StateUnverifiable, Detail: "decode envelope: " + err.Error()}
	}

	// Resolve the trusted key set: pinned first (TOFU), then the registry's
	// published set (pinning it on first use).
	out := AttestationResult{}
	pinned, err := LoadPinned(registryURL)
	if err != nil {
		return AttestationResult{State: StateUnverifiable, Detail: err.Error()}
	}
	trusted := pinned
	if len(trusted.Keys) == 0 && os.Getenv(EnvPinRegistry) != "" {
		// A driver that names the registry whose pins verify must have
		// made them: trusting first use here would fetch keys from
		// --registry and write them into the named registry's pin file.
		return AttestationResult{State: StateUnverifiable, Detail: "no signing keys are pinned for " + os.Getenv(EnvPinRegistry) + " (AGENTRY_PIN_REGISTRY); log in to it first"}
	}
	if len(trusted.Keys) == 0 {
		fetched, err := FetchKeys(ctx, hc, registryURL)
		if err != nil {
			return AttestationResult{State: StateUnverifiable, Detail: err.Error()}
		}
		if len(fetched.Keys) == 0 {
			return AttestationResult{State: StateUnverifiable, Detail: "registry publishes no signing keys"}
		}
		if err := SavePinned(registryURL, fetched); err != nil {
			return AttestationResult{State: StateUnverifiable, Detail: "pin keys: " + err.Error()}
		}
		trusted = fetched
		out.FirstUse = true
	}

	// Try each pinned key; on failure, check whether the registry has
	// rotated (a key id we have not pinned) and report that distinctly
	// rather than silently trusting the new key.
	stmt, key, ok := tryVerify(env, trusted)
	if !ok {
		fetched, ferr := FetchKeys(ctx, hc, registryURL)
		if ferr == nil && !SameKeyIDs(trusted, fetched) {
			change := DiffKeys(trusted, fetched)
			out.State = StateUnverifiable
			out.KeyRotated = true
			out.KeyChange = &change
			if change.Rotation() {
				out.Detail = "signature does not match any pinned key; since the pin the registry " + change.String() +
					" and still advertises every pinned key, which is consistent with a key rotation — the new key is not trusted until you confirm the rotation with the registry's operator and re-pin deliberately"
			} else {
				out.Detail = "signature does not match any pinned key; the registry's key set changed (" + change.String() +
					"), which a rotation does not do — treat it as a possible compromise and re-pin deliberately only once the change is confirmed"
			}
			return out
		}
		out.State = StateMismatch
		out.Detail = "no pinned key verifies this signature"
		if ferr == nil {
			if change := DiffKeys(trusted, fetched); !change.Same() {
				out.KeyChange = &change
				out.Detail += " (the registry now advertises " + change.String() + ")"
			}
		}
		return out
	}
	keyID := key.KeyID
	out.KeyRetired, out.KeyRetiredAt = retiredStatus(ctx, hc, registryURL, key)

	// The signature is valid — now bind it to the bytes we downloaded.
	wantDigest := strings.TrimPrefix(archiveDigest, "sha256:")
	if len(stmt.Subject) == 0 || stmt.Subject[0].Digest["sha256"] != wantDigest {
		out.State = StateMismatch
		out.Detail = "attestation subject digest does not match the downloaded archive"
		out.KeyID = keyID
		return out
	}

	out.State = StateVerified
	out.KeyID = keyID
	out.Detail = "DSSE signature valid; subject digest matches the archive"
	if pred, err := decodePredicate(stmt); err == nil {
		out.Agent, out.Version, out.Source = pred.Agent, pred.Version, pred.Source
		out.RulesetVer, out.ReportDigest, out.JudgeModel = pred.ScanRun.RulesetVer, pred.ScanRun.ReportDigest, pred.ScanRun.JudgeModel
	}
	return out
}

// tryVerify returns the first pinned key that verifies env, trying the
// keys not recorded as retired first: an attestation a rotation
// countersigned carries both signatures, and is reported as signed by
// the active key, not the retired one.
func tryVerify(env attest.DSSEEnvelope, set KeySet) (attest.InTotoStatement, SigningKey, bool) {
	for _, retired := range []bool{false, true} {
		for _, k := range set.Keys {
			if k.Retired() != retired {
				continue
			}
			if stmt, err := attest.VerifyEnvelope(env, k.PublicKey); err == nil {
				return stmt, k, true
			}
		}
	}
	return attest.InTotoStatement{}, SigningKey{}, false
}

// retiredStatus is whether the key that verified a signature is retired,
// and since when. The registry's current record wins over the pin's (a
// key pinned while active may have retired since), taken only for the
// same id with the same public key; when the registry cannot be read, or
// AGENTRY_PIN_REGISTRY points the pins at another registry than the one
// being talked to, the pinned record answers.
func retiredStatus(ctx context.Context, hc *http.Client, registryURL string, key SigningKey) (bool, string) {
	if os.Getenv(EnvPinRegistry) == "" {
		if fetched, err := FetchKeys(ctx, hc, registryURL); err == nil {
			for _, k := range fetched.Keys {
				if k.KeyID == key.KeyID && samePublicKey(k.PublicKey, key.PublicKey) {
					return k.Retired(), k.RetiredAt
				}
			}
		}
	}
	return key.Retired(), key.RetiredAt
}

// SameKeyIDs reports whether two key sets advertise the same key ids.
func SameKeyIDs(a, b KeySet) bool {
	if len(a.Keys) != len(b.Keys) {
		return false
	}
	have := map[string]bool{}
	for _, k := range a.Keys {
		have[k.KeyID] = true
	}
	for _, k := range b.Keys {
		if !have[k.KeyID] {
			return false
		}
	}
	return true
}

// sameHost reports whether two URLs share a host, so the bearer token is
// only ever attached to the registry itself, never to a CDN (§9.4).
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host == ub.Host
}

// decodePredicate re-decodes the statement predicate into its typed form.
func decodePredicate(stmt attest.InTotoStatement) (attest.PublishPredicate, error) {
	raw, err := json.Marshal(stmt.Predicate)
	if err != nil {
		return attest.PublishPredicate{}, err
	}
	var pred attest.PublishPredicate
	if err := json.Unmarshal(raw, &pred); err != nil {
		return attest.PublishPredicate{}, err
	}
	return pred, nil
}
