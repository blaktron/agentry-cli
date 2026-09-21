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

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/attest"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// SigningKey is one entry from /.well-known/agentry/signing-keys.json.
type SigningKey struct {
	KeyID     string `json:"keyId" yaml:"keyId"`
	Algorithm string `json:"algorithm" yaml:"algorithm"`
	PublicKey string `json:"publicKeyPem" yaml:"publicKeyPem"`
}

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

// PinnedKeysPath is where TOFU-pinned keys live, per registry host.
func PinnedKeysPath(registryURL string) (string, error) {
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
	existing, err := LoadPinned(registryURL)
	if err != nil {
		return "could not read pinned signing keys: " + err.Error()
	}

	fetched, err := FetchKeys(ctx, hc, registryURL)
	if err != nil {
		return "could not fetch signing keys (" + err.Error() + "); they will be pinned on first verify"
	}
	if len(fetched.Keys) == 0 {
		return "the registry publishes no signing keys — releases from it are unsigned"
	}

	if len(existing.Keys) == 0 {
		if err := SavePinned(registryURL, fetched); err != nil {
			return "could not pin signing keys: " + err.Error()
		}
		return fmt.Sprintf("pinned %d signing key(s) on first use (TOFU)", len(fetched.Keys))
	}
	if SameKeyIDs(existing, fetched) {
		return fmt.Sprintf("signing keys unchanged (%d pinned)", len(existing.Keys))
	}
	// A changed key set at login is reported, never silently adopted:
	// re-pinning on the strength of a fresh fetch would defeat the pin.
	path, _ := PinnedKeysPath(registryURL)
	return "WARNING: the registry advertises signing key ids that differ from the pinned set — " +
		"rotation or compromise. Verification will keep using the pinned keys; " +
		"delete " + path + " to re-pin deliberately"
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
	stmt, keyID, ok := tryVerify(env, trusted)
	if !ok {
		fetched, ferr := FetchKeys(ctx, hc, registryURL)
		if ferr == nil && !SameKeyIDs(trusted, fetched) {
			out.State = StateUnverifiable
			out.KeyRotated = true
			out.Detail = "signature does not match any pinned key; the registry now advertises different key ids (rotation or compromise) — re-pin deliberately"
			return out
		}
		out.State = StateMismatch
		out.Detail = "no pinned key verifies this signature"
		return out
	}

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

func tryVerify(env attest.DSSEEnvelope, set KeySet) (attest.InTotoStatement, string, bool) {
	for _, k := range set.Keys {
		if stmt, err := attest.VerifyEnvelope(env, k.PublicKey); err == nil {
			return stmt, k.KeyID, true
		}
	}
	return attest.InTotoStatement{}, "", false
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
