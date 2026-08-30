// `agentry verify` (§11): download a pinned version, check its sha256 digest
// against the registry (§9.4 step 3), and verify the DSSE attestation (§8.8)
// against the signing keys pinned for that registry.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"
)

// exactVersionRe mirrors the API's VersionString pattern: verify only works
// on exact pins.
var exactVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$`)

// newVerifyCmd builds the verify command.
func newVerifyCmd() *cobra.Command {
	var strict bool
	cmd := &cobra.Command{
		Use:   "verify <ns/name@version>",
		Short: "Verify a published version's archive digest and attestation (exit code for CI)",
		Long: "Resolve an exact version, download its archive, recompute the sha256\n" +
			"digest, and compare it to the registry's (§9.4). Prints PASS/FAIL and\n" +
			"exits accordingly, and verifies the DSSE attestation (§8.8) against\n" +
			"the registry's signing keys, pinned on first use.\n\n" +
			"Exit codes. A digest mismatch or a signature that fails to verify\n" +
			"(\"mismatch\") is always non-zero. An UNVERIFIABLE attestation — the key\n" +
			"endpoint is unreachable, or the registry has rotated to key ids this\n" +
			"machine has not pinned — exits 0 by default, because it describes a\n" +
			"problem with the check rather than evidence against the artifact, and a\n" +
			"transient DNS failure should not look like a compromised release.\n\n" +
			"--strict makes unverifiable non-zero as well. Use it in CI, where\n" +
			"\"we could not check\" and \"we checked and it was fine\" must not be the\n" +
			"same green build. An unsigned version (no attestation published at all)\n" +
			"is reported and exits 0 in both modes: self-host may run unsigned (§8.8).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, args[0], strict)
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false,
		"also fail when the attestation could not be verified (key fetch failure, key rotation)")
	return cmd
}

// verifyReport is the --json output shape.
type verifyReport struct {
	Ref            string             `json:"ref"`
	Version        string             `json:"version"`
	RegistryDigest string             `json:"registryDigest"`
	ComputedDigest string             `json:"computedDigest"`
	DigestMatch    bool               `json:"digestMatch"`
	Attestation    *AttestationResult `json:"attestation,omitempty"`
	Yanked         bool               `json:"yanked"`
}

// runVerify executes the verification.
func runVerify(cmd *cobra.Command, ref string, strict bool) error {
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		return err
	}
	if !exactVersionRe.MatchString(parsed.Range) {
		return fmt.Errorf("verify requires an exact version pin, e.g. %s/%s@1.2.3", parsed.Namespace, parsed.Name)
	}

	reg := registryURL()
	token := credentialFor(reg)
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	res, err := resolveRef(cmd.Context(), client, ref, false)
	if err != nil {
		return err
	}
	archive, err := downloadArchive(cmd.Context(), reg, token, res)
	if err != nil {
		return err
	}

	report := verifyReport{
		Ref:            ref,
		Version:        res.Version,
		RegistryDigest: res.Digest,
		ComputedDigest: sha256Digest(archive),
		Yanked:         res.Yanked,
	}
	report.DigestMatch = report.ComputedDigest == report.RegistryDigest

	// Provenance (§8.8): verify the DSSE attestation against the pinned key
	// set and bind it to the bytes we just downloaded.
	attURL := ""
	if res.AttestationUrl != nil {
		attURL = *res.AttestationUrl
	}
	if attURL == "" {
		attURL = fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/attestation", res.Namespace, res.Name, res.Version)
	}
	att := verifyAttestation(cmd.Context(), reg, token, attURL, report.ComputedDigest)
	report.Attestation = &att

	out := cmd.OutOrStdout()
	if flags.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
	} else {
		_, _ = fmt.Fprintf(out, "%s/%s@%s\n", res.Namespace, res.Name, res.Version)
		_, _ = fmt.Fprintf(out, "  registry digest: %s\n", report.RegistryDigest)
		_, _ = fmt.Fprintf(out, "  computed digest: %s\n", report.ComputedDigest)
		_, _ = fmt.Fprintf(out, "  attestation:     %s (%s)\n", att.State, att.Detail)
		if att.State == "verified" {
			_, _ = fmt.Fprintf(out, "  signed by:       key %s\n", att.KeyID)
			if att.RulesetVer != "" {
				_, _ = fmt.Fprintf(out, "  scanned with:    ruleset %s, judge %s\n", att.RulesetVer, orNone(att.JudgeModel))
			}
			if att.FirstUse {
				_, _ = fmt.Fprintln(out, "  note:            signing keys pinned on first use (TOFU)")
			}
		}
		if att.KeyRotated {
			_, _ = fmt.Fprintln(out, "  WARNING:         the registry advertises new signing key ids")
		}
		if report.Yanked {
			_, _ = fmt.Fprintln(out, "  yanked:          yes")
		}
		if report.DigestMatch {
			_, _ = fmt.Fprintln(out, "digest: PASS")
		} else {
			_, _ = fmt.Fprintln(out, "digest: FAIL")
		}
	}
	if !report.DigestMatch {
		return fmt.Errorf("digest mismatch for %s", ref)
	}
	// A present-but-invalid attestation is a hard failure; an absent one is
	// reported and tolerated (self-host may run unsigned, §8.8).
	if att.State == "mismatch" {
		return fmt.Errorf("attestation verification failed for %s: %s", ref, att.Detail)
	}
	// "unverifiable" is the check failing, not the artifact. Default behavior
	// is unchanged (exit 0, state printed); --strict promotes it, so a CI job
	// that cannot reach the key endpoint reports that instead of passing.
	if strict && att.State == "unverifiable" {
		return fmt.Errorf("attestation unverifiable for %s: %s (--strict)", ref, att.Detail)
	}
	return nil
}
