// `agentry validate` failure modes (§11, D-12): the scaffold-PASS half
// lives in cmd_init_test.go; this file proves the command actually FAILS —
// manifest errors, S1 inventory findings, --strict warning promotion — and
// pins the printers' ordering (most severe first, errors before warnings).
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/rules"
)

func TestValidateMissingOrBrokenManifest(t *testing.T) {
	isolateEnv(t)
	_, _, err := runCLI(t, nil, "validate", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read agent.yaml") {
		t.Errorf("empty dir: %v", err)
	}

	dir := writeTree(t, map[string]string{"agent.yaml": "kind: [broken"})
	_, _, err = runCLI(t, nil, "validate", dir)
	if err == nil || !strings.Contains(err.Error(), "parse agent.yaml") {
		t.Errorf("broken yaml: %v", err)
	}
}

// TestValidateFlagsS1Findings: a content-sniffed ELF inside the include set
// is the submit-time rejection D-12 exists to surface locally — non-zero,
// with the finding printed in the shared format.
func TestValidateFlagsS1Findings(t *testing.T) {
	isolateEnv(t)
	files := skillFiles("demo", "")
	files["helper.md"] = string([]byte{0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01}) // ELF magic behind a .md name
	files["agent.yaml"] = strings.Replace(files["agent.yaml"],
		"include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, helper.md]", 1)
	dir := writeTree(t, files)

	stdout, _, err := runCLI(t, nil, "validate", dir)
	if err == nil || !strings.Contains(err.Error(), "inventory finding") {
		t.Fatalf("ELF in the include set passed: %v", err)
	}
	for _, want := range []string{"CRITICAL S1/binary-executable helper.md", "executables and shared libraries are blocked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("finding output lacks %q:\n%s", want, stdout)
		}
	}
}

// TestValidateStrictPromotesWarnings: warnings pass by default and fail
// under --strict — the split the exit-code contract promises.
func TestValidateStrictPromotesWarnings(t *testing.T) {
	isolateEnv(t)
	// "*" egress is the stable SeverityWarning (V5: policy-gated, not
	// rejected at validate time). README + LICENSE keep V7 quiet — the
	// warning must be the ONLY issue for the default/strict split to show.
	files := skillFiles("demo", "capabilities:\n  network:\n    egress: [\"*\"]\n")
	files["README.md"] = "# demo\n"
	files["LICENSE"] = "Apache-2.0 text placeholder\n"
	dir := writeTree(t, files)

	stdout, _, err := runCLI(t, nil, "validate", dir)
	if err != nil {
		t.Fatalf("warning failed a default validate: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "warning(s)") || !strings.Contains(stdout, "WARN ") {
		t.Errorf("warning not printed:\n%s", stdout)
	}

	_, _, err = runCLI(t, nil, "validate", dir, "--strict")
	if err == nil || !strings.Contains(err.Error(), "validate failed") {
		t.Errorf("--strict did not promote the warning: %v", err)
	}
}

// TestValidateRulesUpdateFailsClosed: a requested rules update that did not
// happen must fail the command — validating against the older bundle is
// exactly what the caller said they no longer trust.
func TestValidateRulesUpdateFailsClosed(t *testing.T) {
	isolateEnv(t)
	dir := writeTree(t, skillFiles("demo", ""))
	_, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "validate", dir, "--rules-update")
	if err == nil || !strings.Contains(err.Error(), "fetch rule bundle") {
		t.Errorf("failed rules update did not fail closed: %v", err)
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
	m := runFixtureManifest(t, "capabilities:\n  network:\n    egress: [\"*\"]\n") // V5 warning
	m.Version = "not-semver"                                                       // V3 error
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
