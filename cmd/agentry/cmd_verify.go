// `agentry verify`: download a pinned version, check its sha256 digest
// against the registry, and verify the DSSE attestation against the
// signing keys pinned for that registry.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/trust"
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
			"digest, and compare it to the registry's. Prints PASS/FAIL and\n" +
			"exits accordingly, and verifies the DSSE attestation against\n" +
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
			"is reported and exits 0 in both modes: self-host may run unsigned.\n" +
			"An unscanned version — one the registry admitted without a scan, a private\n" +
			"organization agent its administrator took off the pipeline — is reported\n" +
			"as its own outcome, \"unscanned, no attestation\", exits 0 by default and\n" +
			"non-zero under --strict.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, args[0], strict)
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false,
		"also fail when the attestation could not be verified (key fetch failure, key rotation) and when the version is unscanned")
	return cmd
}

// verifyReport is the --json output shape.
type verifyReport struct {
	Ref            string                   `json:"ref"`
	Version        string                   `json:"version"`
	RegistryDigest string                   `json:"registryDigest"`
	ComputedDigest string                   `json:"computedDigest"`
	DigestMatch    bool                     `json:"digestMatch"`
	Attestation    *trust.AttestationResult `json:"attestation,omitempty"`
	Yanked         bool                     `json:"yanked"`
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
	token, err := credentialFor(reg)
	if err != nil {
		return err
	}
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

	// Provenance: verify the DSSE attestation against the pinned key set
	// and bind it to the bytes we just downloaded — unless the registry
	// admitted the version without a scan, which is its own outcome
	// (unscanned, no attestation) and not a missing signature.
	att := attestationOf(cmd.Context(), reg, token, res, report.ComputedDigest)
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
		if att.State == trust.StateUnscanned {
			_, _ = fmt.Fprintln(out, "  attestation:     unscanned, no attestation (the registry admitted this version without a scan)")
		} else {
			_, _ = fmt.Fprintf(out, "  attestation:     %s (%s)\n", att.State, att.Detail)
		}
		if att.State == "verified" {
			_, _ = fmt.Fprintf(out, "  signed by:       key %s\n", att.KeyID)
			if note := att.RetiredNote(); note != "" {
				_, _ = fmt.Fprintf(out, "  note:            %s\n", note)
			}
			if att.RulesetVer != "" {
				_, _ = fmt.Fprintf(out, "  scanned with:    ruleset %s, judge %s\n", att.RulesetVer, orNone(att.JudgeModel))
			}
			if att.FirstUse {
				_, _ = fmt.Fprintln(out, "  note:            signing keys pinned on first use (TOFU)")
			}
		}
		if att.KeyRotated {
			if att.KeyChange.Rotation() {
				_, _ = fmt.Fprintf(out, "  possible key rotation: the registry %s since the pin; confirm it with the registry's operator, then re-pin deliberately to trust the new key\n", att.KeyChange)
			} else {
				_, _ = fmt.Fprintf(out, "  WARNING:         the registry's signing keys changed since the pin: %s\n", att.KeyChange)
			}
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
	// reported and tolerated (self-host may run unsigned).
	if att.State == "mismatch" {
		return fmt.Errorf("attestation verification failed for %s: %s", ref, att.Detail)
	}
	// "unverifiable" is the check failing, not the artifact. Default behavior
	// is unchanged (exit 0, state printed); --strict promotes it, so a CI job
	// that cannot reach the key endpoint reports that instead of passing.
	if strict && att.State == "unverifiable" {
		return fmt.Errorf("attestation unverifiable for %s: %s (--strict)", ref, att.Detail)
	}
	// Unscanned is reported as its own outcome and, like unsigned, exits 0
	// by default: verify describes, install and run decide. --strict fails
	// it, so a CI job that wants only scanned releases can say so.
	if strict && att.State == trust.StateUnscanned {
		return fmt.Errorf("%s is unscanned: the registry admitted it without a scan (--strict)", ref)
	}
	return nil
}
