// `agentry validate` (§11, D-12): offline parity with the server's submit
// validation — manifest rules V1–V8 via pkg/manifest, the S1 inventory
// subset and the S3/S4 scan via pkg/rules, run over the files that publish
// would actually ship. The check itself is pkg/validate (agentry-cli#478);
// the command adds the rule bundle's precedence and the --rules-update fetch.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/validate"
	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/rules"
	"github.com/blaktron/agentry/pkg/ruleset"
)

// newValidateCmd builds the validate command.
func newValidateCmd() *cobra.Command {
	var strict, rulesUpdate bool
	cmd := &cobra.Command{
		Use:   "validate [dir]",
		Short: "Validate a workspace offline (manifest rules V1-V10 + S1/S3/S4 scan)",
		Long: "Validate a workspace offline with the same code the registry runs at submit\n" +
			"time (D-12 local/remote parity), over the files publish.include would ship:\n\n" +
			"  manifest  rules V1-V9, in a public-publish context so V7 runs\n" +
			"  S1        content-sniffed executables and nested archives (critical),\n" +
			"            non-UTF-8 text (low) — the checks that used to reject a\n" +
			"            workspace at submit after it passed locally\n" +
			"  S3/S4     hidden-content detectors and the injection-signature engine\n\n" +
			"The S4 rule bundle is EMBEDDED in this binary, so validation never depends\n" +
			"on where it runs. `--rules-update` fetches the registry's current bundle and\n" +
			"caches it under ~/.config/agentry/rules/<semver>/; the newest cached bundle\n" +
			"is then preferred over the embedded one. The fetched bundle is pinned by\n" +
			"digest and its DSSE signature is verified against the registry's pinned\n" +
			"keys (an absent signature is reported and tolerated, an invalid one is\n" +
			"fatal).\n" +
			"$AGENTRY_RULES_DIR (or ./rules) overrides both, for rule development.\n\n" +
			"An override bundle that will not load is a hard ERROR, never a skipped scan:\n" +
			"reporting PASSED over content the signature engine never examined is the\n" +
			"failure mode §7 forbids on the server, so it is forbidden here too (DEV-011).\n" +
			"A cached --rules-update bundle that will not load is skipped for the\n" +
			"embedded one, which always loads.\n\n" +
			"Exit code: 0 clean; 1 on manifest errors or scan findings. Manifest warnings\n" +
			"fail only with --strict.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runValidate(cmd, dir, strict, rulesUpdate)
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "treat manifest warnings as errors")
	cmd.Flags().BoolVar(&rulesUpdate, "rules-update", false,
		"fetch the registry's current rule bundle, verify its digest, and cache it before scanning")
	return cmd
}

// validateOutput is the `validate --json` report (pkg/validate).
type validateOutput = validate.Report

// runValidate executes the offline validation over dir.
func runValidate(cmd *cobra.Command, dir string, strict, rulesUpdate bool) error {
	out := cmd.OutOrStdout()
	if flags.jsonOut {
		out = cmd.ErrOrStderr() // §R8: the report on stdout, the narration on stderr
	}
	if rulesUpdate {
		if _, err := fetchRulesBundle(cmd.Context(), cmd.ErrOrStderr(), registryURL()); err != nil {
			// Fail closed: a requested rules update that did not happen must
			// not silently validate against the older bundle, because the
			// whole reason to ask for it was distrust of the older bundle.
			return err
		}
	}
	report, err := validateWorkspace(out, cmd.ErrOrStderr(), dir, strict)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		// The report is the answer either way; the exit code still says
		// whether it passed, as it does for doctor.
		if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
	}
	if !report.Passed {
		c := report.Counts
		return fmt.Errorf("validate failed: %d error(s), %d warning(s), %d inventory finding(s), %d scan finding(s)",
			c.Errors, c.Warnings, c.InventoryFailing, c.Findings)
	}
	_, _ = fmt.Fprintln(out, "validate: PASSED")
	return nil
}

// validateWorkspace is the validation itself (pkg/validate), narrated to
// out (notes to errOut), under the bundle loadRulesBundle resolves: the
// report `validate --json` prints. An error is a validation that could not
// run (no manifest, a rule bundle that will not load), never a failed one.
func validateWorkspace(out, errOut io.Writer, dir string, strict bool) (validateOutput, error) {
	return validate.Workspace(dir, validate.Options{Strict: strict, Out: out, ErrOut: errOut, Rules: loadRulesBundle})
}

// readWorkspaceManifest loads and parses dir/agent.yaml.
func readWorkspaceManifest(dir string) (*manifest.Manifest, error) { return validate.ReadManifest(dir) }

// loadRulesBundle resolves the S4 signature bundle, in precedence order:
//
//  1. $AGENTRY_RULES_DIR — rule development against an unreleased bundle
//  2. ./rules            — the same, from a rules checkout
//  3. the newest bundle cached by `--rules-update`
//  4. the bundle EMBEDDED in this binary
//
// The embedded bundle is the floor, not the ceiling: it means local/remote
// parity (D-12) never depends on where the CLI is run from, while a cached
// update means it does not depend on how recently the CLI was released
// either. It returns the bundle and a short description of where it came
// from, because "which rules did this pass under" is the first question
// anyone asks about a clean validate.
//
// A malformed override or cache entry from (1) or (2) is a hard error, not a
// warning: silently skipping S4 would report "validate: PASSED" on content
// the signature engine never examined, which is the failure mode §7 forbids
// on the server (never pass on scanner failure) and DEV-011 forbids here.
func loadRulesBundle() (*rules.Bundle, string, error) {
	if d := os.Getenv(envRulesDir); d != "" {
		b, err := rules.LoadBundle(os.DirFS(d), ".")
		if err != nil {
			return nil, "", fmt.Errorf("load rule bundle from %s (%s): %w", d, envRulesDir, err)
		}
		return b, envRulesDir, nil
	}
	if st, err := os.Stat("rules"); err == nil && st.IsDir() {
		b, err := rules.LoadBundle(os.DirFS("rules"), ".")
		if err != nil {
			return nil, "", fmt.Errorf("load rule bundle from ./rules: %w", err)
		}
		return b, "./rules", nil
	}
	if b, _ := newestCachedBundle(); b != nil {
		return b, "cached via --rules-update", nil
	}
	b, err := ruleset.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load embedded rule bundle: %w", err)
	}
	return b, "embedded", nil
}
