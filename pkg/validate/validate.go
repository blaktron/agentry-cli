// Package validate is `agentry validate`'s workspace check (§11, D-12):
// offline parity with the server's submit validation — manifest rules
// V1–V10 via pkg/manifest, the S1 inventory subset (localscan.go), and the
// S3/S4 scan via pkg/rules, run over the files publish would actually ship.
// It left the command's main package so that the Drafter's post-step
// (pkg/drafter) checks a draft with the same code in every client
// (agentry-cli#478): `agentry validate`, `agentry draft` and the desktop's
// draft on this computer write one report, the `validate --json` shape.
package validate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/rules"
	"github.com/blaktron/agentry/pkg/ruleset"
)

// Report is the `validate --json` shape (agentry-cli#102): the rules and
// scan findings as data, and the verdict `publish` would reach. A draft's
// draft.json carries it as its validation.
type Report struct {
	Passed bool `json:"passed"`
	// Manifest is what agent.yaml said about itself.
	Manifest Manifest `json:"manifest"`
	// Issues are the manifest rules' results, errors and warnings.
	Issues []Issue `json:"issues"`
	// Inventory is the S1 subset over the resolved include set.
	Inventory []InventoryFinding `json:"inventory"`
	// Findings are the S3/S4 scanner hits.
	Findings []Finding `json:"findings"`
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

// Manifest is the report's header: what agent.yaml said about itself.
type Manifest struct {
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	SchemaVersion int      `json:"schemaVersion"`
	Modes         []string `json:"modes"`
}

// Issue is one manifest rule's result.
type Issue struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// InventoryFinding is one S1-subset result.
type InventoryFinding struct {
	Scanner  string `json:"scanner"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	FilePath string `json:"filePath"`
	Detail   string `json:"detail,omitempty"`
}

// Finding is one S3/S4 scanner hit.
type Finding struct {
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

// RulesLoader loads the S4 signature bundle and says where it came from.
type RulesLoader func() (*rules.Bundle, string, error)

// Options are how a workspace is checked.
type Options struct {
	// Strict treats manifest warnings as errors.
	Strict bool
	// Out is the narration, ErrOut its notes; nil discards them.
	Out, ErrOut io.Writer
	// Rules loads the S4 signature bundle and says where it came from; nil
	// is the bundle embedded in this binary (Embedded). A loader that fails
	// fails the check: it never passes content S4 did not examine
	// (DEV-011).
	Rules RulesLoader
}

// Embedded is the S4 bundle embedded in this binary, the floor every client
// validates under.
func Embedded() (*rules.Bundle, string, error) {
	b, err := ruleset.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load embedded rule bundle: %w", err)
	}
	return b, "embedded", nil
}

// Workspace is the validation itself, narrated to o.Out (notes to
// o.ErrOut): the report `validate --json` prints, and what a draft's
// post-step writes into the draft. An error is a validation that could not
// run (no manifest, a rule bundle that will not load), never a failed one.
func Workspace(dir string, o Options) (Report, error) {
	out, errOut := o.Out, o.ErrOut
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	loadRules := o.Rules
	if loadRules == nil {
		loadRules = Embedded
	}
	strict := o.Strict
	report := Report{Strict: strict, Issues: []Issue{}, Inventory: []InventoryFinding{}, Findings: []Finding{}, ReadErrors: []string{}}
	m, err := ReadManifest(dir)
	if err != nil {
		return report, err
	}
	if m.Major() == 1 {
		_, _ = fmt.Fprintln(errOut, "note: schemaVersion 1 manifest (kind-classified); it still validates and publishes as autonomous-only. modes, intake and interactive need schemaVersion \"2\" — agentry init --modes scaffolds one.")
	}

	files, err := archive.WalkWorkspace(dir)
	if err != nil {
		return report, fmt.Errorf("walk workspace %s: %w", dir, err)
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
	report.Manifest = Manifest{Namespace: m.Namespace, Name: m.Name, Version: m.Version, SchemaVersion: m.Major(), Modes: modeStrings(m.SupportedModes())}
	var errCount, warnCount int
	for _, issue := range issues {
		sev := "error"
		if manifest.IsWarning(issue) {
			warnCount++
			sev = "warning"
		} else {
			errCount++
		}
		vi := Issue{Severity: sev, Message: issue.Error()}
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
	s1 := scanS1(scanFiles)
	s1Failing := failingFindings(s1)
	for _, f := range s1 {
		report.Inventory = append(report.Inventory, InventoryFinding(f))
	}
	if len(s1) == 0 {
		_, _ = fmt.Fprintf(out, "inventory: clean — %d file(s), S1 (binaries, nested archives, encoding)\n", len(scanFiles))
	} else {
		_, _ = fmt.Fprintf(out, "inventory: %d finding(s) over %d file(s)\n", len(s1), len(scanFiles))
		printLocalFindings(out, s1)
	}

	bundle, source, err := loadRules()
	if err != nil {
		// Fail closed: never report PASSED on content S4 did not examine.
		return report, err
	}
	findings := rules.Scan(bundle, rulesFiles(scanFiles))
	sortFindings(findings)
	report.Bundle.Version, report.Bundle.Source = bundle.Version(), source
	for _, f := range findings {
		report.Findings = append(report.Findings, Finding{
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
	return report, nil
}

// ReadManifest loads and parses dir/agent.yaml.
func ReadManifest(dir string) (*manifest.Manifest, error) {
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

// modeStrings converts a mode set for the report.
func modeStrings(modes []manifest.Mode) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
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
