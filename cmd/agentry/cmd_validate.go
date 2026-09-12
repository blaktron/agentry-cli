// `agentry validate` (§11, D-12): offline parity with the server's submit
// validation — manifest rules V1–V8 via pkg/manifest, the S1 inventory
// subset via localscan.go, and the S3/S4 scan via pkg/rules, run over the
// files that publish would actually ship.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/rules"
	"github.com/blaktron/agentry/pkg/ruleset"
)

// newValidateCmd builds the validate command.
func newValidateCmd() *cobra.Command {
	var strict, rulesUpdate bool
	cmd := &cobra.Command{
		Use:   "validate [dir]",
		Short: "Validate a workspace offline (manifest rules V1-V9 + S1/S3/S4 scan)",
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
			"digest — bundle-level signatures do not exist yet (DEVIATIONS DEV-024).\n" +
			"$AGENTRY_RULES_DIR (or ./rules) overrides both, for rule development.\n\n" +
			"A bundle that will not load is a hard ERROR, never a skipped scan: reporting\n" +
			"PASSED over content the signature engine never examined is the failure mode\n" +
			"§7 forbids on the server, so it is forbidden here too (DEV-011).\n\n" +
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

// runValidate executes the offline validation over dir.
// validateOutput is the `validate --json` shape (agentry-cli#102): the
// rules and scan findings as data, and the verdict `publish` would reach.
type validateOutput struct {
	Passed bool `json:"passed"`
	// Manifest is what agent.yaml said about itself.
	Manifest validateManifest `json:"manifest"`
	// Issues are the manifest rules V1–V9, errors and warnings.
	Issues []validateIssue `json:"issues"`
	// Inventory is the S1 subset over the resolved include set.
	Inventory []validateLocalFinding `json:"inventory"`
	// Findings are the S3/S4 scanner hits.
	Findings []validateFinding `json:"findings"`
	// ReadErrors are include-set files that could not be read.
	ReadErrors []string `json:"readErrors"`
	FileCount  int      `json:"fileCount"`
	Bundle     struct {
		Version string `json:"version"`
		Source  string `json:"source"`
	} `json:"bundle"`
	Counts struct {
		Errors           int `json:"errors"`
		Warnings         int `json:"warnings"`
		InventoryFailing int `json:"inventoryFailing"`
		Findings         int `json:"findings"`
	} `json:"counts"`
	Strict bool `json:"strict"`
}

type validateManifest struct {
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	SchemaVersion int      `json:"schemaVersion"`
	Modes         []string `json:"modes"`
}

type validateIssue struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

type validateLocalFinding struct {
	Scanner  string `json:"scanner"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	FilePath string `json:"filePath"`
	Detail   string `json:"detail,omitempty"`
}

type validateFinding struct {
	RuleID     string            `json:"ruleId"`
	Scanner    string            `json:"scanner"`
	Severity   string            `json:"severity"`
	Confidence string            `json:"confidence"`
	Title      string            `json:"title"`
	FilePath   string            `json:"filePath"`
	LineStart  int               `json:"lineStart,omitempty"`
	LineEnd    int               `json:"lineEnd,omitempty"`
	Excerpt    string            `json:"excerpt,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
}

func runValidate(cmd *cobra.Command, dir string, strict, rulesUpdate bool) error {
	out := cmd.OutOrStdout()
	if flags.jsonOut {
		out = cmd.ErrOrStderr() // §R8: the report on stdout, the narration on stderr
	}
	report := validateOutput{Strict: strict, Issues: []validateIssue{}, Inventory: []validateLocalFinding{}, Findings: []validateFinding{}, ReadErrors: []string{}}

	if rulesUpdate {
		if _, err := fetchRulesBundle(cmd.Context(), cmd.ErrOrStderr(), registryURL()); err != nil {
			// Fail closed: a requested rules update that did not happen must
			// not silently validate against the older bundle, because the
			// whole reason to ask for it was distrust of the older bundle.
			return err
		}
	}

	m, err := readWorkspaceManifest(dir)
	if err != nil {
		return err
	}
	if m.Major() == 1 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: schemaVersion 1 manifest (kind-classified); it still validates and publishes as autonomous-only. modes, intake and interactive need schemaVersion \"2\" — agentry init --modes scaffolds one.")
	}

	files, err := archive.WalkWorkspace(dir)
	if err != nil {
		return fmt.Errorf("walk workspace %s: %w", dir, err)
	}

	// Server parity context: namespace/name from the manifest itself (the
	// CLI cannot know the target repo), Public so V7 runs, no tag (upload
	// source semantics), no existing versions.
	issues := manifest.ValidateInContext(m, manifest.Context{
		Namespace: m.Namespace,
		Name:      m.Name,
		Public:    true,
		Files:     files,
		// V9 compares the skill entrypoint's frontmatter with the manifest;
		// the path is repo-relative and validated by V4 before it is read.
		ReadFile: func(p string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))) //nolint:gosec // validated repo-relative path in the workspace
		},
	})
	report.Manifest = validateManifest{Namespace: m.Namespace, Name: m.Name, Version: m.Version, SchemaVersion: m.Major(), Modes: modeStrings(m.SupportedModes())}
	var errCount, warnCount int
	for _, issue := range issues {
		sev := "error"
		if manifest.IsWarning(issue) {
			warnCount++
			sev = "warning"
		} else {
			errCount++
		}
		vi := validateIssue{Severity: sev, Message: issue.Error()}
		if is, ok := manifest.AsIssue(issue); ok {
			vi.Rule, vi.Path, vi.Message = is.Rule, is.Path, is.Message
		}
		report.Issues = append(report.Issues, vi)
	}
	if len(issues) == 0 {
		_, _ = fmt.Fprintln(out, "manifest: OK (rules V1-V9, public-publish context)")
	} else {
		_, _ = fmt.Fprintf(out, "manifest: %d error(s), %d warning(s)\n", errCount, warnCount)
		printManifestIssues(out, issues)
	}

	// Scan exactly what publish would ship: the resolved include set.
	scanFiles, readErrs := loadScanFiles(dir, m, files)
	errCount += len(readErrs)
	for _, e := range readErrs {
		_, _ = fmt.Fprintf(out, "  ERROR read: %v\n", e)
		report.ReadErrors = append(report.ReadErrors, e.Error())
	}
	report.FileCount = len(scanFiles)

	// S1 subset (D-12): the inventory checks that used to reject a workspace
	// at submit after it had passed locally.
	s1 := scanS1Local(scanFiles)
	s1Failing := localFailingFindings(s1)
	for _, f := range s1 {
		report.Inventory = append(report.Inventory, validateLocalFinding(f))
	}
	if len(s1) == 0 {
		_, _ = fmt.Fprintf(out, "inventory: clean — %d file(s), S1 (binaries, nested archives, encoding)\n", len(scanFiles))
	} else {
		_, _ = fmt.Fprintf(out, "inventory: %d finding(s) over %d file(s)\n", len(s1), len(scanFiles))
		printLocalFindings(out, s1)
	}

	bundle, source, err := loadRulesBundle()
	if err != nil {
		// Fail closed: never report PASSED on content S4 did not examine.
		return err
	}
	findings := rules.Scan(bundle, rulesFiles(scanFiles))
	sortFindings(findings)
	report.Bundle.Version, report.Bundle.Source = bundle.Version(), source
	for _, f := range findings {
		report.Findings = append(report.Findings, validateFinding{
			RuleID: f.RuleID, Scanner: f.Scanner, Severity: string(f.Severity), Confidence: string(f.Confidence),
			Title: f.Title, FilePath: f.FilePath, LineStart: f.LineStart, LineEnd: f.LineEnd, Excerpt: f.Excerpt, Detail: f.Detail,
		})
	}

	switch {
	case len(findings) == 0:
		_, _ = fmt.Fprintf(out, "scan: clean — %d file(s), S3 + S4 (bundle %s, %s)\n", len(scanFiles), bundle.Version(), source)
	default:
		_, _ = fmt.Fprintf(out, "scan: %d finding(s) over %d file(s) (bundle %s, %s)\n",
			len(findings), len(scanFiles), bundle.Version(), source)
		for _, f := range findings {
			printFinding(out, f)
		}
	}
	failed := errCount > 0 || len(findings) > 0 || s1Failing > 0 || (strict && warnCount > 0)
	report.Passed = !failed
	report.Counts.Errors, report.Counts.Warnings, report.Counts.InventoryFailing, report.Counts.Findings = errCount, warnCount, s1Failing, len(findings)
	if flags.jsonOut {
		// The report is the answer either way; the exit code still says
		// whether it passed, as it does for doctor.
		if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
	}
	if failed {
		return fmt.Errorf("validate failed: %d error(s), %d warning(s), %d inventory finding(s), %d scan finding(s)",
			errCount, warnCount, s1Failing, len(findings))
	}
	_, _ = fmt.Fprintln(out, "validate: PASSED")
	return nil
}

// readWorkspaceManifest loads and parses dir/agent.yaml.
func readWorkspaceManifest(dir string) (*manifest.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml")) //nolint:gosec // user-named workspace
	if err != nil {
		return nil, fmt.Errorf("read agent.yaml: %w", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse agent.yaml: %w", err)
	}
	return m, nil
}

// loadScanFiles reads the resolved include set (what the archive would
// contain) once, for both the S1 inventory and the S3/S4 engine; symlinks are
// skipped (V4 already rejects them).
func loadScanFiles(dir string, m *manifest.Manifest, files []manifest.FileInfo) ([]localFile, []error) {
	include := manifest.ResolveInclude(m.Publish.Include, files)
	scanFiles := make([]localFile, 0, len(include))
	var errs []error
	for _, fi := range include {
		if fi.Symlink {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(fi.Path))) //nolint:gosec // paths come from WalkWorkspace
		if err != nil {
			errs = append(errs, err)
			continue
		}
		scanFiles = append(scanFiles, localFile{Path: fi.Path, Content: content})
	}
	return scanFiles, errs
}

// rulesFiles adapts the shared read set to the pkg/rules input shape.
func rulesFiles(files []localFile) []rules.File {
	out := make([]rules.File, len(files))
	for i, f := range files {
		out[i] = rules.File{Path: f.Path, Content: f.Content}
	}
	return out
}

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

// printManifestIssues prints validation issues, errors before warnings.
func printManifestIssues(w io.Writer, issues []error) {
	ordered := append([]error(nil), issues...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return !manifest.IsWarning(ordered[i]) && manifest.IsWarning(ordered[j])
	})
	for _, err := range ordered {
		label := "ERROR"
		if manifest.IsWarning(err) {
			label = "WARN "
		}
		if issue, ok := manifest.AsIssue(err); ok {
			loc := issue.Rule
			if issue.Path != "" {
				loc += " (" + issue.Path + ")"
			}
			_, _ = fmt.Fprintf(w, "  %s %s: %s\n", label, loc, issue.Message)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s %v\n", label, err)
	}
}

// severityRank orders finding severities most-severe-first for display.
var severityRank = map[rules.Severity]int{
	rules.SeverityCritical: 0,
	rules.SeverityHigh:     1,
	rules.SeverityMedium:   2,
	rules.SeverityLow:      3,
	rules.SeverityInfo:     4,
}

// sortFindings sorts findings by severity, then path, then line, then rule.
func sortFindings(findings []rules.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.LineStart != b.LineStart {
			return a.LineStart < b.LineStart
		}
		return a.RuleID < b.RuleID
	})
}

// printFinding prints one scan finding with its excerpt.
func printFinding(w io.Writer, f rules.Finding) {
	loc := f.FilePath
	if f.LineStart > 0 {
		loc = fmt.Sprintf("%s:%d", f.FilePath, f.LineStart)
	}
	_, _ = fmt.Fprintf(w, "  %-8s %s %s — %s\n", strings.ToUpper(string(f.Severity)), f.RuleID, loc, f.Title)
	if f.Excerpt != "" {
		_, _ = fmt.Fprintf(w, "           %s\n", f.Excerpt)
	}
}
