package main

// Attestation verification (§8.8): fetch the registry's signing keys, pin
// them on first use (TOFU) with key-id continuity warnings, and verify the
// DSSE envelope offline against the archive digest. Verification uses the
// same library code the server signs with (internal/scan), so the formats
// cannot drift.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/attest"
)

// signingKey is one entry from /.well-known/agentry/signing-keys.json.
type signingKey struct {
	KeyID     string `json:"keyId" yaml:"keyId"`
	Algorithm string `json:"algorithm" yaml:"algorithm"`
	PublicKey string `json:"publicKeyPem" yaml:"publicKeyPem"`
}

type signingKeySet struct {
	Keys []signingKey `json:"keys" yaml:"keys"`
}

// AttestationResult is what verify/install report about provenance.
type AttestationResult struct {
	// State is one of: verified, unsigned, unverifiable, mismatch.
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

// fetchSigningKeys reads the registry's key set.
func fetchSigningKeys(ctx context.Context, registryURL string) (signingKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(registryURL, "/")+"/.well-known/agentry/signing-keys.json", nil)
	if err != nil {
		return signingKeySet{}, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return signingKeySet{}, fmt.Errorf("fetch signing keys: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return signingKeySet{}, fmt.Errorf("signing keys: HTTP %d", res.StatusCode)
	}
	var set signingKeySet
	if err := json.NewDecoder(res.Body).Decode(&set); err != nil {
		return signingKeySet{}, fmt.Errorf("decode signing keys: %w", err)
	}
	return set, nil
}

// pinnedKeysPath is where TOFU-pinned keys live, per registry host.
func pinnedKeysPath(registryURL string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	host := registryURL
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.ReplaceAll(strings.TrimRight(host, "/"), "/", "_")
	host = strings.ReplaceAll(host, ":", "_")
	return filepath.Join(dir, "keys", host+".yaml"), nil
}

// loadPinnedKeys reads the pinned set (empty when nothing is pinned yet).
func loadPinnedKeys(registryURL string) (signingKeySet, error) {
	path, err := pinnedKeysPath(registryURL)
	if err != nil {
		return signingKeySet{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path derives from config dir
	if err != nil {
		if os.IsNotExist(err) {
			return signingKeySet{}, nil
		}
		return signingKeySet{}, err
	}
	var set signingKeySet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return signingKeySet{}, fmt.Errorf("pinned keys: %w", err)
	}
	return set, nil
}

// savePinnedKeys writes the pinned set (0600).
func savePinnedKeys(registryURL string, set signingKeySet) error {
	path, err := pinnedKeysPath(registryURL)
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

// pinSigningKeysAtLogin implements the §11 login row: "pins signing keys
// (TOFU)". Pinning at login rather than at first verify is the whole point —
// login is the moment a human deliberately chose this registry and typed a
// credential for it, which is exactly when trust-on-first-use should be
// established. Pinning later, at the first `verify` or `install`, silently
// moves that decision to whichever download happens to come first.
//
// It returns a human-readable note describing what happened, and never fails
// the login: a registry with signing switched off publishes an empty key set,
// and that is a legitimate self-host configuration (§8.8), not a login error.
func pinSigningKeysAtLogin(ctx context.Context, registryURL string) string {
	existing, err := loadPinnedKeys(registryURL)
	if err != nil {
		return "could not read pinned signing keys: " + err.Error()
	}

	fetched, err := fetchSigningKeys(ctx, registryURL)
	if err != nil {
		return "could not fetch signing keys (" + err.Error() + "); they will be pinned on first verify"
	}
	if len(fetched.Keys) == 0 {
		return "the registry publishes no signing keys — releases from it are unsigned"
	}

	if len(existing.Keys) == 0 {
		if err := savePinnedKeys(registryURL, fetched); err != nil {
			return "could not pin signing keys: " + err.Error()
		}
		return fmt.Sprintf("pinned %d signing key(s) on first use (TOFU)", len(fetched.Keys))
	}
	if sameKeyIDs(existing, fetched) {
		return fmt.Sprintf("signing keys unchanged (%d pinned)", len(existing.Keys))
	}
	// A changed key set at login is reported, never silently adopted:
	// re-pinning on the strength of a fresh fetch would defeat the pin.
	path, _ := pinnedKeysPath(registryURL)
	return "WARNING: the registry advertises signing key ids that differ from the pinned set — " +
		"rotation or compromise. Verification will keep using the pinned keys; " +
		"delete " + path + " to re-pin deliberately"
}

// verifyAttestation fetches and verifies the attestation for a version.
// archiveDigest is the digest the client computed from the downloaded bytes,
// so a verified attestation binds the signature to what is actually on disk.
func verifyAttestation(ctx context.Context, registryURL, token, attestationURL, archiveDigest string) AttestationResult {
	if attestationURL == "" {
		return AttestationResult{State: "unsigned", Detail: "the registry published no attestation for this version"}
	}

	// Fetch the envelope.
	url := attestationURL
	if !strings.HasPrefix(url, "http") {
		url = strings.TrimRight(registryURL, "/") + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AttestationResult{State: "unverifiable", Detail: err.Error()}
	}
	if token != "" && sameHost(url, registryURL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return AttestationResult{State: "unverifiable", Detail: "fetch attestation: " + err.Error()}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return AttestationResult{State: "unsigned", Detail: "no attestation recorded for this version"}
	}
	if res.StatusCode != http.StatusOK {
		return AttestationResult{State: "unverifiable", Detail: fmt.Sprintf("attestation: HTTP %d", res.StatusCode)}
	}
	var env attest.DSSEEnvelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return AttestationResult{State: "unverifiable", Detail: "decode envelope: " + err.Error()}
	}

	// Resolve the trusted key set: pinned first (TOFU), then the registry's
	// published set (pinning it on first use).
	out := AttestationResult{}
	pinned, err := loadPinnedKeys(registryURL)
	if err != nil {
		return AttestationResult{State: "unverifiable", Detail: err.Error()}
	}
	trusted := pinned
	if len(trusted.Keys) == 0 {
		fetched, err := fetchSigningKeys(ctx, registryURL)
		if err != nil {
			return AttestationResult{State: "unverifiable", Detail: err.Error()}
		}
		if len(fetched.Keys) == 0 {
			return AttestationResult{State: "unverifiable", Detail: "registry publishes no signing keys"}
		}
		if err := savePinnedKeys(registryURL, fetched); err != nil {
			return AttestationResult{State: "unverifiable", Detail: "pin keys: " + err.Error()}
		}
		trusted = fetched
		out.FirstUse = true
	}

	// Try each pinned key; on failure, check whether the registry has
	// rotated (a key id we have not pinned) and report that distinctly
	// rather than silently trusting the new key.
	stmt, keyID, ok := tryVerify(env, trusted)
	if !ok {
		fetched, ferr := fetchSigningKeys(ctx, registryURL)
		if ferr == nil && !sameKeyIDs(trusted, fetched) {
			out.State = "unverifiable"
			out.KeyRotated = true
			out.Detail = "signature does not match any pinned key; the registry now advertises different key ids (rotation or compromise) — re-pin deliberately"
			return out
		}
		out.State = "mismatch"
		out.Detail = "no pinned key verifies this signature"
		return out
	}

	// The signature is valid — now bind it to the bytes we downloaded.
	wantDigest := strings.TrimPrefix(archiveDigest, "sha256:")
	if len(stmt.Subject) == 0 || stmt.Subject[0].Digest["sha256"] != wantDigest {
		out.State = "mismatch"
		out.Detail = "attestation subject digest does not match the downloaded archive"
		out.KeyID = keyID
		return out
	}

	out.State = "verified"
	out.KeyID = keyID
	out.Detail = "DSSE signature valid; subject digest matches the archive"
	if pred, err := decodePredicate(stmt); err == nil {
		out.Agent, out.Version, out.Source = pred.Agent, pred.Version, pred.Source
		out.RulesetVer, out.ReportDigest, out.JudgeModel = pred.ScanRun.RulesetVer, pred.ScanRun.ReportDigest, pred.ScanRun.JudgeModel
	}
	return out
}

func tryVerify(env attest.DSSEEnvelope, set signingKeySet) (attest.InTotoStatement, string, bool) {
	for _, k := range set.Keys {
		if stmt, err := attest.VerifyEnvelope(env, k.PublicKey); err == nil {
			return stmt, k.KeyID, true
		}
	}
	return attest.InTotoStatement{}, "", false
}

func sameKeyIDs(a, b signingKeySet) bool {
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

// orNone renders an empty string as "none" for human output.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
