// `agentry verify` (§11): digest PASS/FAIL drives the exit code for CI, a
// present-but-invalid attestation always fails, an absent one is tolerated
// (§8.8 self-host), and --strict promotes "we could not check" to a failure
// so it can never impersonate "we checked and it was fine".
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyRequiresExactPin(t *testing.T) {
	isolateEnv(t)
	for _, ref := range []string{"acme/demo", "acme/demo@^1.2"} {
		_, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "verify", ref)
		if err == nil || !strings.Contains(err.Error(), "exact version pin") {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

func TestVerifyUnsignedPasses(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0")
	if err != nil {
		t.Fatalf("unsigned verify must exit 0 (§8.8 self-host): %v", err)
	}
	for _, want := range []string{"digest: PASS", "attestation:     unsigned"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verify output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestVerifyVerifiedAttestation(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"digest: PASS",
		"attestation:     verified",
		"signed by:       key " + fr.keyID,
		"scanned with:    ruleset 1.0.0, judge judge-1",
		"note:            signing keys pinned on first use (TOFU)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verified output lacks %q:\n%s", want, stdout)
		}
	}
}

// TestVerifyDigestMismatchFails: bytes that hash differently from the
// registry's claim are the §9.4 hard failure regardless of signatures.
func TestVerifyDigestMismatchFails(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.digest = "sha256:" + strings.Repeat("11", 32)
	fv.yanked = true // the report must surface yanked too

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0")
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("digest mismatch passed: %v", err)
	}
	if !strings.Contains(stdout, "digest: FAIL") || !strings.Contains(stdout, "yanked:          yes") {
		t.Errorf("verify output = %s", stdout)
	}
}

// TestVerifyAttestationMismatchFails: a valid signature over the wrong
// archive is evidence AGAINST the artifact — non-zero in both modes.
func TestVerifyAttestationMismatchFails(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned
	fv.attSubject = "sha256:" + strings.Repeat("22", 32)

	_, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0")
	if err == nil || !strings.Contains(err.Error(), "attestation verification failed") {
		t.Fatalf("mismatched attestation passed: %v", err)
	}
}

// TestVerifyStrictPromotesUnverifiable: a 500 from the attestation endpoint
// is a fact about the CHECK — exit 0 by default, non-zero under --strict.
func TestVerifyStrictPromotesUnverifiable(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attError

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0")
	if err != nil {
		t.Fatalf("unverifiable must pass by default (a DNS blip is not a compromise): %v", err)
	}
	if !strings.Contains(stdout, "attestation:     unverifiable") {
		t.Errorf("verify output = %s", stdout)
	}

	_, _, err = runCLI(t, nil, "--registry", fr.url(), "verify", "acme/demo@1.0.0", "--strict")
	if err == nil || !strings.Contains(err.Error(), "unverifiable") || !strings.Contains(err.Error(), "--strict") {
		t.Fatalf("--strict did not promote unverifiable: %v", err)
	}
}

func TestVerifyJSONReport(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "verify", "acme/demo@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var report verifyReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("--json verify is not JSON: %v\n%s", err, stdout)
	}
	if !report.DigestMatch || report.Attestation == nil || report.Attestation.State != "unsigned" {
		t.Errorf("report = %+v", report)
	}
	if report.ComputedDigest != report.RegistryDigest {
		t.Error("digests disagree in a passing report")
	}
}

// TestVerifyReportsUnscanned: a version the registry admitted without a
// scan is reported as its own outcome — "unscanned, no attestation", never
// "unsigned" — exits 0 by default and non-zero under --strict.
func TestVerifyReportsUnscanned(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "internal", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("internal", ""))
	fv.unscanned = true

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "verify", "acme/internal@1.0.0")
	if err != nil {
		t.Fatalf("verify of an unscanned version must exit 0 by default: %v", err)
	}
	if !strings.Contains(stdout, "attestation:     unscanned, no attestation") || !strings.Contains(stdout, "digest: PASS") {
		t.Errorf("verify output lacks the unscanned outcome:\n%s", stdout)
	}
	if strings.Contains(stdout, "unsigned") {
		t.Errorf("verify conflates unscanned with unsigned:\n%s", stdout)
	}

	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "verify", "acme/internal@1.0.0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Attestation struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"attestation"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if rep.Attestation.State != "unscanned" || !strings.Contains(rep.Attestation.Detail, "without a scan") {
		t.Fatalf("json attestation = %+v", rep.Attestation)
	}

	_, _, err = runCLI(t, nil, "--registry", fr.url(), "verify", "acme/internal@1.0.0", "--strict")
	if err == nil || !strings.Contains(err.Error(), "unscanned") {
		t.Fatalf("--strict must fail an unscanned version: %v", err)
	}
}
