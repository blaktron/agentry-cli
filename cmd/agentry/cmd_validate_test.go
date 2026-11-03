// `agentry validate` failure modes: the scaffold-PASS half lives in
// cmd_init_test.go; this file proves the command actually FAILS — manifest
// errors, S1 inventory findings, --strict warning promotion. The printers'
// ordering is pinned in pkg/validate.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/validate"
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
// is the submit-time rejection validate exists to surface locally — non-zero,
// with the finding printed in the shared format.
func TestValidateFlagsS1Findings(t *testing.T) {
	isolateEnv(t)
	files := skillFiles("demo", "")
	files["helper.md"] = string([]byte{0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01}) // ELF magic behind a .md name
	files["agent.yaml"] = strings.Replace(files["agent.yaml"],
		"include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, helper.md]", 1)
	dir := writeTree(t, files)

	stdout, stderr, err := runCLI(t, nil, "validate", dir)
	if err == nil || !strings.Contains(err.Error(), "inventory finding") {
		t.Fatalf("ELF in the include set passed: %v", err)
	}
	for _, want := range []string{"CRITICAL S1/binary-executable helper.md", "executables and shared libraries are blocked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("finding output lacks %q:\n%s", want, stdout)
		}
	}
	// The failure counts what failed, and a failed check points nowhere next.
	const failed = "validate failed: 2 error(s), 1 warning(s), 1 inventory finding(s), 0 scan finding(s)"
	if err.Error() != failed || strings.Contains(stderr, "next steps") || strings.Contains(stdout, "validate: PASSED") {
		t.Errorf("err = %v, stderr %q", err, stderr)
	}

	// With --json the report is on stdout and the narration on stderr; the
	// exit still says the check failed.
	jout, jerr, err := runCLI(t, nil, "--json", "validate", dir)
	if err == nil || err.Error() != failed {
		t.Fatalf("--json: err = %v", err)
	}
	var rep validateOutput
	if uerr := json.Unmarshal([]byte(jout), &rep); uerr != nil || rep.Passed || rep.Counts.InventoryFailing != 1 || len(rep.Inventory) != 1 || rep.Inventory[0].FilePath != "helper.md" {
		t.Fatalf("--json report: %v %+v\n%s", uerr, rep, jout)
	}
	if !strings.Contains(jerr, "inventory: 1 finding(s)") || strings.Contains(jout, "inventory:") {
		t.Errorf("the narration is not on stderr alone:\nstdout %s\nstderr %s", jout, jerr)
	}
}

// TestValidateAppliesTextTotalQuota: the text total (V8) is the
// registry's submit-time rule, and validate promises the same rules
// offline — two 600 KiB prose files must fail here, not at upload.
func TestValidateAppliesTextTotalQuota(t *testing.T) {
	isolateEnv(t)
	files := skillFiles("demo", "")
	files["notes/a.md"] = "# a\n\n" + strings.Repeat("prose ", 100*1024)
	files["notes/b.md"] = "# b\n\n" + strings.Repeat("prose ", 100*1024)
	files["agent.yaml"] = strings.Replace(files["agent.yaml"],
		"include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, notes/a.md, notes/b.md]", 1)
	dir := writeTree(t, files)

	stdout, _, err := runCLI(t, nil, "validate", dir)
	if err == nil {
		t.Fatalf("1.2 MiB of prose passed validate:\n%s", stdout)
	}
	for _, want := range []string{"V8", "text files total", "maximum scanned prose"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
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
	dir := writeTree(t, publishableSkillFiles("demo", "capabilities:\n  network:\n    egress: [\"*\"]\n"))

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

// TestValidateAcceptsDeclaredServerEnv: the vendored spec carries the
// manifest's plain env map on a declared server, so validate passes it
// offline — and refuses the one collision the spec refuses, a name that
// is both a secret and env.
func TestValidateAcceptsDeclaredServerEnv(t *testing.T) {
	isolateEnv(t)
	caps := `capabilities:
  mcpServers:
    - npm: github-mcp-server-js
      version: 0.1.7
      tools: [list_issues]
      secrets: [GITHUB_TOKEN]
      env:
        GITHUB_PERMISSION: read-only
      egress: [api.github.com]
`
	dir := writeTree(t, publishableSkillFiles("demo", caps))
	stdout, stderr, err := runCLI(t, nil, "validate", dir)
	if err != nil || !strings.Contains(stdout, "validate: PASSED") {
		t.Fatalf("env refused offline: %v\n%s", err, stdout)
	}
	// A passing --json check puts the report on stdout and the verdict line
	// beside the narration.
	jout, jerr, err := runCLI(t, nil, "--json", "validate", dir)
	var rep validateOutput
	if err != nil || json.Unmarshal([]byte(jout), &rep) != nil || !rep.Passed || strings.Contains(jout, "validate: PASSED") || !strings.Contains(jerr, "validate: PASSED") {
		t.Fatalf("--json of a passing check: %v\nstdout %s\nstderr %s", err, jout, jerr)
	}
	// A clean validate ends with what comes next, on stderr, for the
	// directory as it was given.
	if !strings.Contains(stderr, "next steps:\n  1. agentry run --local "+dir+" --allow-unsigned\n") || !strings.Contains(stderr, "2. agentry publish "+dir+"\n") {
		t.Errorf("next steps = %q", stderr)
	}
	bad := strings.Replace(caps, "        GITHUB_PERMISSION: read-only\n", "        GITHUB_PERMISSION: read-only\n        GITHUB_TOKEN: plain\n", 1)
	stdout, _, err = runCLI(t, nil, "validate", writeTree(t, publishableSkillFiles("demo", bad)))
	if err == nil || !strings.Contains(stdout, "configure it as a secret or as env, not both") {
		t.Fatalf("secret/env collision passed: %v\n%s", err, stdout)
	}
}

// publishableSkillFiles is skillFiles plus the README and LICENSE V7 wants
// for a public publish, so a validate test sees only the issue it is about.
func publishableSkillFiles(name, capsYAML string) map[string]string {
	files := skillFiles(name, capsYAML)
	files["README.md"] = "# " + name + "\n"
	files["LICENSE"] = "Apache-2.0 text placeholder\n"
	files["agent.yaml"] = strings.Replace(files["agent.yaml"], "include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, README.md, LICENSE]", 1)
	return files
}

// TestValidateHelpNamesTheRulesThatRun: the help text names the same
// rules the check runs, not a range typed in by hand.
func TestValidateHelpNamesTheRulesThatRun(t *testing.T) {
	cmd := newValidateCmd()
	span := "rules " + validate.ManifestRules()
	if !strings.Contains(cmd.Short, span+" ") || !strings.Contains(cmd.Long, span+",") {
		t.Fatalf("help does not name %q:\n%s\n%s", span, cmd.Short, cmd.Long)
	}
}
