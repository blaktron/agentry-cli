// The printers' ordering (most severe first, errors before warnings) and
// the check's own contract: the embedded bundle when a client names no
// loader, and a loader that fails fails the check (DEV-011).
package validate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/rules"
)

func fixtureManifest(t *testing.T, capsYAML string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "s"
entrypoints:
  instruction: SKILL.md
` + capsYAML + `publish:
  include: [agent.yaml, SKILL.md]
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// workspace writes a publishable package: a manifest, its instruction, and
// the README and LICENSE V7 wants for a public publish.
func workspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"agent.yaml": `schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "Says hello."
license: Apache-2.0
entrypoints:
  instruction: SKILL.md
publish:
  include: [agent.yaml, SKILL.md, README.md, LICENSE]
`,
		"SKILL.md":  "---\nname: demo\ndescription: Says hello.\n---\n\n# Demo\n\nSay hello.\n",
		"README.md": "# demo\n",
		"LICENSE":   "Apache-2.0 text placeholder\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestWorkspaceUsesTheEmbeddedBundleUnlessToldOtherwise: a client that
// names no loader (the desktop) validates under the bundle this binary
// embeds; one whose loader fails gets no report that passed.
func TestWorkspaceUsesTheEmbeddedBundleUnlessToldOtherwise(t *testing.T) {
	dir := workspace(t)
	r, err := Workspace(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Bundle.Source != "embedded" || r.Bundle.Version == "" || r.FileCount != 4 {
		t.Fatalf("report %+v", r)
	}
	if !r.Passed {
		t.Fatalf("the fixture does not pass: %+v", r)
	}
	r, err = Workspace(dir, Options{Rules: func() (*rules.Bundle, string, error) { return nil, "", errors.New("no bundle") }})
	if err == nil || r.Passed {
		t.Fatalf("a bundle that did not load: passed %v, %v", r.Passed, err)
	}
}

func TestPrintLocalFindingsOrdersBySeverity(t *testing.T) {
	var buf bytes.Buffer
	printLocalFindings(&buf, []localFinding{
		{Scanner: "S1/non-utf8", Severity: "low", Title: "Non-UTF-8 text file", FilePath: "z.txt", Detail: "d1"},
		{Scanner: "S1/binary-executable", Severity: "critical", Title: "Executable binary in package", FilePath: "a.md", Detail: "d2"},
		{Scanner: "S1/opaque-binary", Severity: "info", Title: "Opaque binary file", FilePath: "b.bin"},
	})
	out := buf.String()
	critical := strings.Index(out, "CRITICAL")
	low := strings.Index(out, "LOW")
	info := strings.Index(out, "INFO")
	ordered := critical >= 0 && critical < low && low < info
	if !ordered {
		t.Errorf("severity ordering wrong:\n%s", out)
	}
	if !strings.Contains(out, "           d2\n") {
		t.Errorf("detail line missing:\n%s", out)
	}
}

// TestSortFindingsAndPrintFinding: S3/S4 findings order by severity, then
// path, then line, then rule — and the printer carries location + excerpt.
func TestSortFindingsAndPrintFinding(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: "S4/b", Severity: rules.SeverityLow, FilePath: "b.md", LineStart: 9, Title: "t1"},
		{RuleID: "S4/a", Severity: rules.SeverityCritical, FilePath: "b.md", LineStart: 3, Title: "t2", Excerpt: "evil()"},
		{RuleID: "S4/c", Severity: rules.SeverityCritical, FilePath: "a.md", LineStart: 5, Title: "t3"},
		{RuleID: "S4/d", Severity: rules.SeverityCritical, FilePath: "b.md", LineStart: 1, Title: "t4"},
	}
	sortFindings(findings)
	gotOrder := []string{findings[0].RuleID, findings[1].RuleID, findings[2].RuleID, findings[3].RuleID}
	wantOrder := []string{"S4/c", "S4/d", "S4/a", "S4/b"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
		}
	}

	var buf bytes.Buffer
	printFinding(&buf, findings[2])
	if !strings.Contains(buf.String(), "CRITICAL S4/a b.md:3 — t2") || !strings.Contains(buf.String(), "evil()") {
		t.Errorf("printFinding = %q", buf.String())
	}
	// Line 0 means "whole file": no :0 suffix.
	buf.Reset()
	printFinding(&buf, rules.Finding{RuleID: "S3/x", Severity: rules.SeverityHigh, FilePath: "n.md", Title: "t"})
	if strings.Contains(buf.String(), "n.md:0") {
		t.Errorf("zero line rendered as a location: %q", buf.String())
	}
}

// TestPrintManifestIssuesOrdersErrorsFirst: errors precede warnings so the
// blocking problems are the first thing on screen — proven with REAL typed
// issues from the validator (a warning listed first on purpose).
func TestPrintManifestIssuesOrdersErrorsFirst(t *testing.T) {
	m := fixtureManifest(t, "capabilities:\n  network:\n    egress: [\"*\"]\n") // V5 warning
	m.Version = "not-semver"                                                    // V3 error
	issues := manifest.Validate(m)
	var haveErr, haveWarn bool
	for _, issue := range issues {
		if manifest.IsWarning(issue) {
			haveWarn = true
		} else {
			haveErr = true
		}
	}
	if !haveErr || !haveWarn {
		t.Fatalf("fixture must yield both classes, got %v", issues)
	}

	// Feed the printer warning-first to prove it reorders.
	ordered := append([]error(nil), issues...)
	for i, issue := range ordered {
		if manifest.IsWarning(issue) && i > 0 {
			ordered[0], ordered[i] = ordered[i], ordered[0]
			break
		}
	}
	var buf bytes.Buffer
	printManifestIssues(&buf, ordered)
	out := buf.String()
	errIdx := strings.Index(out, "ERROR")
	warnIdx := strings.Index(out, "WARN ")
	if errIdx < 0 || warnIdx < 0 || errIdx > warnIdx {
		t.Errorf("issue ordering:\n%s", out)
	}
	// Typed issues render rule + path context, not bare text.
	if !strings.Contains(out, "V3") {
		t.Errorf("rule id missing from output:\n%s", out)
	}
}
