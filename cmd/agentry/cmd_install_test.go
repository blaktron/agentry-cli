package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

// TestResolveInstallPathsManagedFlag: only a directory the CLI chose is
// cleared on reinstall.
func TestResolveInstallPathsManagedFlag(t *testing.T) {
	for _, target := range validTargets {
		withDir, err := resolveInstallPaths(target, "/user/chosen", "helper")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if withDir.managed {
			t.Errorf("%s: a --dir destination is marked managed and would be cleared", target)
		}
		def, err := resolveInstallPaths(target, "", "helper")
		if err != nil {
			t.Fatalf("%s default: %v", target, err)
		}
		if !def.managed {
			t.Errorf("%s: the default destination is not managed and stale files would linger", target)
		}
	}
	if _, err := resolveInstallPaths("nope", "", "helper"); err == nil {
		t.Error("an unknown target resolved to a path")
	}
}

// TestValidateTargetTable: a typo in --target must fail before any network
// call, naming the closed §5.6 set.
func TestValidateTargetTable(t *testing.T) {
	if err := validateTarget(""); err != nil {
		t.Errorf("empty target (resolve from kind later) errored: %v", err)
	}
	for _, ok := range validTargets {
		if err := validateTarget(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	err := validateTarget("vscode")
	if err == nil || !strings.Contains(err.Error(), "claude-skill") {
		t.Errorf("unknown target error must name the valid set: %v", err)
	}
}

// TestResolveProviderTarget: --provider is sugar for --target (RUNNER-PLAN-2
// §P5) and must stay mutually exclusive with it.
func TestResolveProviderTarget(t *testing.T) {
	tests := []struct {
		provider, target string
		wantTarget       string
		wantErr          string
	}{
		{"", "", "", ""},
		{"claude", "", manifest.InterfaceClaudeSkill, ""},
		{"CODEX", "", manifest.InterfaceAgentsMD, ""}, // case-insensitive
		{"openclaw", "", manifest.InterfaceClaudeSkill, ""},
		{"emacs", "", "", "unknown --provider"},
		{"claude", "mcp", "", "mutually exclusive"},
	}
	for _, tt := range tests {
		opts := installOpts{provider: tt.provider, target: tt.target}
		err := resolveProviderTarget(&opts)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("provider %q target %q: err = %v, want %q", tt.provider, tt.target, err, tt.wantErr)
			}
			continue
		}
		if err != nil || opts.target != tt.wantTarget {
			t.Errorf("provider %q: target = %q, %v; want %q", tt.provider, opts.target, err, tt.wantTarget)
		}
	}
	// The unknown-provider message lists the accepted names, sorted, so the
	// fix is in the error itself.
	opts := installOpts{provider: "emacs"}
	if err := resolveProviderTarget(&opts); err == nil || !strings.Contains(err.Error(), "claude, codex, openclaw") {
		t.Errorf("unknown provider error must list providers sorted: %v", err)
	}
}

// TestPrepareDest: only a CLI-managed destination is cleared; a --dir the
// user named is merged into, never wiped.
func TestPrepareDest(t *testing.T) {
	managed := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(managed, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDest(installPaths{dest: managed, managed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("managed dest was not cleared")
	}

	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDest(installPaths{dest: managed, managed: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("user-named dest was cleared")
	}
}

// TestWarnUndeclaredTarget: runtime.interfaces, when present, is the §5.2
// declaration of what §5.6 may emit — installing outside it warns.
func TestWarnUndeclaredTarget(t *testing.T) {
	var buf bytes.Buffer
	m := &manifest.Manifest{}
	warnUndeclaredTarget(&buf, m, "mcp")
	if buf.Len() != 0 {
		t.Errorf("no interfaces declared must stay silent: %q", buf.String())
	}
	m.Runtime.Interfaces = []string{"claude-skill", "mcp"}
	warnUndeclaredTarget(&buf, m, "mcp")
	if buf.Len() != 0 {
		t.Errorf("declared target must stay silent: %q", buf.String())
	}
	warnUndeclaredTarget(&buf, m, "agents-md")
	if !strings.Contains(buf.String(), `does not list target "agents-md"`) {
		t.Errorf("undeclared target warning = %q", buf.String())
	}
}

// TestStdinIsTTY: the TTY check gates every consent prompt, so its verdicts
// on the reader shapes tests can produce must be exact. A real typed-yes
// needs a char device; /dev/null is one (EOF on read), a pipe or buffer is
// not.
func TestStdinIsTTY(t *testing.T) {
	if stdinIsTTY(strings.NewReader("yes\n")) {
		t.Error("a bytes reader is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if stdinIsTTY(f) {
		t.Error("a regular file is not a terminal")
	}
	if !stdinIsTTY(devNull(t)) {
		t.Skip("/dev/null is not a char device on this platform")
	}
}

// TestPromptYes: off a TTY the gate refuses rather than reading — EOF is
// not consent — and a char-device stdin that yields no line is a read
// error, not a silent accept. (Typed-"yes" acceptance needs a real pty and
// is three visible lines of string comparison.)
func TestPromptYes(t *testing.T) {
	cmd := stubCmd(t)
	cmd.SetIn(strings.NewReader("yes\n")) // piped "yes" is still not a TTY
	err := promptYes(cmd, "continue? ", "not a terminal — pass --yes", "refused")
	if err == nil || err.Error() != "not a terminal — pass --yes" {
		t.Errorf("non-TTY promptYes = %v", err)
	}

	if !stdinIsTTY(devNull(t)) {
		t.Skip("/dev/null is not a char device on this platform")
	}
	cmd = stubCmd(t)
	cmd.SetIn(devNull(t))
	err = promptYes(cmd, "continue? ", "not a terminal", "refused")
	if err == nil || !strings.Contains(err.Error(), "read confirmation") {
		t.Errorf("EOF on a char device = %v, want a read error", err)
	}
}

// TestConfirmCapabilities: the §11 gate — egress/exec asks, --yes skips,
// and a non-interactive run without --yes refuses with the flag named.
func TestConfirmCapabilities(t *testing.T) {
	egress := parseCaps(t, "capabilities:\n  network:\n    egress: [api.example.com]\n")

	if err := confirmCapabilities(stubCmd(t), egress, true); err != nil {
		t.Errorf("--yes must skip the prompt: %v", err)
	}
	if err := confirmCapabilities(stubCmd(t), manifest.Capabilities{}, false); err != nil {
		t.Errorf("pure-text caps must not prompt: %v", err)
	}
	err := confirmCapabilities(stubCmd(t), egress, false)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-TTY egress confirm = %v, want a refusal naming --yes", err)
	}
}

// TestCopyTreePreservesModes: reinstalling an agent with an executable
// helper script must keep it executable (the archive normalizes modes,
// D-15; the copy must not undo that).
func TestCopyTreePreservesModes(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "doc.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "sub", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("copied mode = %o, want 755", st.Mode().Perm())
	}
	files := treeFiles(dest)
	if !files["sub/run.sh"] || !files["doc.md"] || len(files) != 2 {
		t.Errorf("treeFiles = %v", files)
	}
}

// TestInstallClaudeSkillEndToEnd: the full §9.4 flow against the fake
// registry — resolve, download, digest check, unsigned gate, export with
// embedded-mode allowed-tools, install-state record, and the
// reinstall-clears-managed-dir rule with an "unchanged" diff.
func TestInstallClaudeSkillEndToEnd(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	files := skillFiles("demo", "capabilities:\n  filesystem:\n    read: workspace\n")
	fr.addAgent("acme", "demo", "autonomous", "reads things").addVersion(t, "1.0.0", files)

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	for _, want := range []string{"acme/demo@1.0.0 (autonomous)", "filesystem.read: workspace", "installed acme/demo@1.0.0 as Claude skill"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}

	home, _ := os.UserHomeDir()
	dest := filepath.Join(home, ".claude", "skills", "demo")
	skill, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Embedded mode (RUNNER-PLAN §R5): fs.read compiles to the host's
	// Read/Grep/Glob grants inside the frontmatter.
	if !strings.Contains(string(skill), "allowed-tools: Read, Grep, Glob") {
		t.Errorf("SKILL.md lacks the compiled allowed-tools line:\n%s", skill)
	}

	// Reinstall: the CLI-managed directory is cleared (stale files from an
	// older version must not linger) and the unchanged contract says so
	// without prompting.
	stale := filepath.Join(dest, "stale-from-old-version.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !strings.Contains(stdout, "capability diff vs 1.0.0: unchanged") {
		t.Errorf("reinstall did not report the unchanged diff:\n%s", stdout)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("reinstall left a stale file in the managed directory")
	}
}

// TestInstallDirIsMergedNotCleared: a --dir destination belongs to the
// user; reinstalling must merge, not wipe (the help text promises this).
func TestInstallDirIsMergedNotCleared(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	dir := t.TempDir()

	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "demo", "my-notes.txt")
	if err := os.WriteFile(mine, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error("reinstall cleared a user-owned --dir destination")
	}
}

// promptAgentFiles is a minimal schema v2 package that lists agents-md
// first, so it installs there by default.
func promptAgentFiles(name string) map[string]string {
	return map[string]string{
		"agent.yaml": `schemaVersion: "2"
name: ` + name + `
namespace: acme
modes: [autonomous, interactive]
version: 1.0.0
summary: "test prompt agent"
license: Apache-2.0
runtime:
  interfaces: [agents-md, claude-skill]
capabilities:
  exec:
    commands: [jq]
publish:
  include: [agent.yaml, AGENT.md, prompts/**]
`,
		"AGENT.md":        "# main\n\nDo it.\n",
		"prompts/main.md": "# main\n\nDo it.\n",
	}
}

// TestInstallAgentsMDTarget: the agents-md target extracts and upserts a
// section in ./AGENTS.md carrying the host-tools note (RUNNER-PLAN §R5).
// exec needs consent, so the non-interactive run must pass --yes.
func TestInstallAgentsMDTarget(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir())
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "pa", "autonomous", "s").addVersion(t, "1.0.0", promptAgentFiles("pa"))

	// Declared exec without --yes off a TTY (piped stdin): the gate holds.
	_, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "install", "acme/pa@1.0.0", "--allow-unsigned")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("exec caps installed without consent: %v", err)
	}

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/pa@1.0.0", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "as prompt agent") {
		t.Errorf("stdout = %s", stdout)
	}
	agents, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "acme/pa") || !strings.Contains(string(agents), "Bash(jq:*)") {
		t.Errorf("AGENTS.md lacks the section or its tools note:\n%s", agents)
	}
	if _, err := os.Stat(filepath.Join("agents", "pa", "prompts", "main.md")); err != nil {
		t.Fatalf("extracted prompt missing: %v", err)
	}
}

// TestInstallDigestMismatchAborts: the registry's advertised digest is the
// identity (D-15); bytes that hash differently must never reach disk.
func TestInstallDigestMismatchAborts(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.digest = "sha256:" + strings.Repeat("ab", 32)

	_, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("mismatched digest installed: %v", err)
	}
	home, _ := os.UserHomeDir()
	if _, statErr := os.Stat(filepath.Join(home, ".claude", "skills", "demo")); !os.IsNotExist(statErr) {
		t.Error("a mismatched archive left files behind")
	}
}

// TestInstallUnsignedGate: no attestation published → refuse without
// --allow-unsigned (in CI too), proceed with it under a warning (§8.8).
// The fixture omits attestationUrl entirely, so this also proves the
// canonical-path fallback probe concludes "unsigned" via the 404.
func TestInstallUnsignedGate(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	_, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0")
	if err == nil || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("unsigned release installed without the flag: %v", err)
	}

	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning: unsigned") {
		t.Errorf("allow-unsigned install must still warn: %q", stderr)
	}
}

// TestInstallVerifiedAttestation: a real DSSE envelope over the real
// archive digest verifies against the registry's published key, TOFU-pinned
// on first use — the full §8.8 happy path, no seams.
func TestInstallVerifiedAttestation(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0")
	if err != nil {
		t.Fatalf("verified install failed: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "attestation:   verified (key "+fr.keyID+")") {
		t.Errorf("stdout lacks the verified line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "scanned with ruleset 1.0.0, judge judge-1") {
		t.Errorf("stdout lacks the scan provenance line:\n%s", stdout)
	}
	if !strings.Contains(stderr, "pinned on first use (TOFU)") {
		t.Errorf("first verified install must disclose the TOFU pin: %q", stderr)
	}
}

// TestInstallAttestationMismatchAborts: a VALID signature over the WRONG
// archive is worse evidence than no signature — the install must abort,
// with no --allow-unsigned escape (that flag is for absent attestations).
func TestInstallAttestationMismatchAborts(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned
	fv.attSubject = "sha256:" + strings.Repeat("cd", 32) // signs some other archive

	_, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err == nil || !strings.Contains(err.Error(), "attestation mismatch") || !strings.Contains(err.Error(), "refusing to install") {
		t.Fatalf("subject-mismatched attestation was accepted: %v", err)
	}
}

// TestInstallCapabilityDiffOnUpgrade: reinstalls compare against what this
// machine last accepted — a widened contract re-asks (or needs --yes), a
// narrowed one reports and proceeds (§11).
func TestInstallCapabilityDiffOnUpgrade(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fa := fr.addAgent("acme", "demo", "autonomous", "s")
	fa.addVersion(t, "1.0.0", skillFiles("demo", ""))
	wideCaps := "capabilities:\n  network:\n    egress: [api.example.com]\n"
	wide := skillFiles("demo", wideCaps)
	wide["agent.yaml"] = strings.Replace(wide["agent.yaml"], "version: 1.0.0", "version: 2.0.0", 1)
	fa.addVersion(t, "2.0.0", wide)

	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
		t.Fatal(err)
	}

	// 1.0.0 → 2.0.0 adds egress: widened, so a non-interactive (piped
	// stdin) install without --yes must refuse and name the flag.
	_, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "install", "acme/demo@2.0.0", "--allow-unsigned")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("widened contract installed without consent: %v", err)
	}
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@2.0.0", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "capability diff vs 1.0.0: WIDENED") {
		t.Errorf("widened diff not shown:\n%s", stdout)
	}

	// 2.0.0 → 1.0.0 drops egress: narrowed installs without asking.
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "capability diff vs 2.0.0: narrowed") {
		t.Errorf("narrowed diff not shown:\n%s", stdout)
	}
}

// TestInstallYankedWarns: an exact pin of a yanked version installs (§5.5)
// but the warning must reach stderr.
func TestInstallYankedWarns(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.yanked = true

	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "YANKED") {
		t.Errorf("yanked install did not warn: %q", stderr)
	}
}

// TestInstallProviderPicksTarget: --provider openclaw rides the
// claude-skill path end to end (RUNNER-PLAN-2 §P5).
func TestInstallProviderPicksTarget(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--provider", "openclaw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "as Claude skill") {
		t.Errorf("openclaw provider did not export a Claude skill:\n%s", stdout)
	}

	// Flag mistakes fail before any network call.
	if _, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "install", "acme/demo@1.0.0", "--provider", "emacs"); err == nil {
		t.Error("unknown provider was accepted")
	}
	if _, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "install", "acme/demo@1.0.0", "--target", "bogus"); err == nil {
		t.Error("unknown target was accepted")
	}
	if _, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "install", "not-a-ref"); err == nil {
		t.Error("unparseable ref was accepted")
	}
}
