// `agentry run` behavior that needs no docker: the §R8 input/output
// contract (task precedence, envelope printing, out-dir), the §R5
// materialize gates (local trees are unsigned; digest/pin/attestation
// mismatches are exit-10 verify failures; bad manifests are exit-12), the
// §R6 install cache, MCP-profile lifecycle plumbing (RUNNER-PLAN-2 §P2/§P3),
// and the --dry-run plan path with its determinism guarantee (§R4).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// repoPath resolves a path relative to the repo root (tests run with cwd =
// cmd/agentry, two levels down).
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// isolateRunnerHome points AGENTRY_RUNNER_HOME at a temp dir and returns it
// with a policy path inside, so run tests never touch ~/.agentry.
func isolateRunnerHome(t *testing.T) (home, policyPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	return home, filepath.Join(home, "policy.yaml")
}

// wantClassified asserts err carries the given §R8 exit class.
func wantClassified(t *testing.T, err error, code int) {
	t.Helper()
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) {
		t.Fatalf("error is not classified: %v", err)
	}
	if ce.Code != code {
		t.Fatalf("exit class = %d, want %d (%v)", ce.Code, code, err)
	}
}

func TestRunnerHomeOverride(t *testing.T) {
	t.Setenv("AGENTRY_RUNNER_HOME", "/custom/runner")
	h, err := runnerHome()
	if err != nil || h != "/custom/runner" {
		t.Errorf("runnerHome override = %q, %v", h, err)
	}
	t.Setenv("AGENTRY_RUNNER_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	h, err = runnerHome()
	if err != nil || h != filepath.Join("/tmp/fakehome", ".agentry", "runner") {
		t.Errorf("runnerHome default = %q, %v", h, err)
	}
}

func TestHomeImageDirAndDigestSuffix(t *testing.T) {
	if got := homeImageDir("/rh"); got != filepath.Join("/rh", "image") {
		t.Errorf("homeImageDir = %q", got)
	}
	if got := installdir.DigestSuffix("sha256:0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("digestSuffix long = %q", got)
	}
	if got := installdir.DigestSuffix("short"); got != "short" {
		t.Errorf("digestSuffix short = %q", got)
	}
}

func TestRunnerCLIVersionFeedsImageTag(t *testing.T) {
	if runnerimage.ContentVersion() == "" {
		t.Error("runnerCLIVersion is empty — the runner image tag would be unversioned")
	}
}

// TestLocalTreeDigest: the digest is the cache key for --local runs, so it
// must be deterministic, content-sensitive, and blind to .git noise.
// The workspace copy's skip rule (.git, agentry-out) is installdir.Write's,
// tested in pkg/runner/installdir.

func TestResolveOutDir(t *testing.T) {
	t.Chdir(t.TempDir())
	meta := &installdir.Meta{Namespace: "acme", Name: "demo"}

	dir, err := resolveOutDir("", meta, "runid1")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) || !strings.HasSuffix(dir, filepath.Join("agentry-out", "acme-demo-runid1")) {
		t.Errorf("default out dir = %q", dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Error("default out dir was not created")
	}

	flagDir := filepath.Join(t.TempDir(), "reports")
	dir, err = resolveOutDir(flagDir, meta, "runid2")
	if err != nil || dir != flagDir {
		t.Errorf("--out dir = %q, %v", dir, err)
	}
	if _, err := os.Stat(flagDir); err != nil {
		t.Error("--out dir was not created")
	}
}

// TestTaskReaderPrecedence: --input file > piped stdin > TTY nil (§R2), and
// a NAMED file that does not open fails loudly through the deferred reader.
func TestTaskReaderPrecedence(t *testing.T) {
	taskFile := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(taskFile, []byte(`{"job":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := stubCmd(t)
	cmd.SetIn(strings.NewReader("piped"))
	r := taskReader(cmd, taskFile)
	got, err := readAll(r)
	if err != nil || got != `{"job":1}` {
		t.Errorf("--input wins: %q, %v", got, err)
	}

	r = taskReader(cmd, "")
	got, err = readAll(r)
	if err != nil || got != "piped" {
		t.Errorf("piped stdin: %q, %v", got, err)
	}

	// A missing --input surfaces on first Read, not as a silent empty task.
	r = taskReader(cmd, filepath.Join(t.TempDir(), "absent.json"))
	if _, err := readAll(r); err == nil || !os.IsNotExist(errUnwrapAll(err)) {
		t.Errorf("missing task file error = %v", err)
	}

	// An interactive terminal sitting silent is not a task: TTY stdin
	// yields a nil reader (the engine treats nil as an empty task).
	if !stdinIsTTY(devNull(t)) {
		t.Skip("/dev/null is not a char device on this platform")
	}
	cmd = stubCmd(t)
	cmd.SetIn(devNull(t))
	if r := taskReader(cmd, ""); r != nil {
		t.Error("TTY stdin was used as task input")
	}
}

// readAll drains a task reader into a string.
func readAll(r interface{ Read([]byte) (int, error) }) (string, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.String(), err
}

// errUnwrapAll unwraps to the innermost error for os.IsNotExist checks.
func errUnwrapAll(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

func TestPrintEnvelopeFormats(t *testing.T) {
	env := &receipt.Envelope{
		Agentry: "run/v1", Status: "ok", RunID: "r1", Ref: "acme/demo@1.0.0",
		Result: json.RawMessage(`"the answer"`), OutDir: "/out/dir",
	}

	// Text mode: only the result text on stdout (Unix composition, §R8);
	// the pointer to reports goes to stderr.
	cmd := stubCmd(t)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	printEnvelope(cmd, env, "text")
	if out.String() != "the answer\n" {
		t.Errorf("text stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "/out/dir") {
		t.Errorf("text stderr lacks the out dir: %q", errOut.String())
	}

	// A structured (non-string) result prints as raw JSON.
	env.Result = json.RawMessage(`{"k":1}`)
	out.Reset()
	printEnvelope(cmd, env, "text")
	if out.String() != "{\"k\":1}\n" {
		t.Errorf("structured text stdout = %q", out.String())
	}

	// JSON mode: the whole envelope, machine-readable.
	out.Reset()
	printEnvelope(cmd, env, "json")
	var round receipt.Envelope
	if err := json.Unmarshal(out.Bytes(), &round); err != nil || round.Ref != "acme/demo@1.0.0" {
		t.Errorf("json stdout did not round-trip: %v\n%s", err, out.String())
	}
}

func TestPrintAdaptersListsDefault(t *testing.T) {
	stdout, _, err := runCLI(t, nil, "run", "--list-adapters")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "docker-agent (default)") || !strings.Contains(stdout, "planned (RUNNER-PLAN M-R4)") {
		t.Errorf("adapters listing = %q", stdout)
	}
}

func TestRunArgValidation(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)

	_, _, err := runCLI(t, nil, "run")
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("no ref and no --local: %v", err)
	}
	_, _, err = runCLI(t, nil, "run", "acme/demo", "--local", "dir")
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("ref plus --local: %v", err)
	}
	_, _, err = runCLI(t, nil, "run", "acme/demo", "--output-format", "yaml")
	if err == nil || !strings.Contains(err.Error(), "--output-format") {
		t.Errorf("bad output format: %v", err)
	}
}

func TestWriteAndLoadInstallMeta(t *testing.T) {
	src := writeTree(t, map[string]string{"agent.yaml": "x"})
	installDir := filepath.Join(t.TempDir(), "agents", "acme", "demo", "1.0.0-abc")
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:d1", Source: "local"}
	if err := installdir.Write(installDir, src, meta); err != nil {
		t.Fatal(err)
	}

	got, ok := installdir.Load(installDir, "sha256:d1")
	if !ok || got.Ref != meta.Ref || got.Source != "local" {
		t.Fatalf("loadInstallMeta = %+v, %v", got, ok)
	}
	// A digest that moved on (new publish, changed tree) must invalidate
	// the cache rather than run stale bytes.
	if _, ok := installdir.Load(installDir, "sha256:other"); ok {
		t.Error("stale digest reused the cached install")
	}
	// A cache entry whose workspace vanished is not an install.
	if err := os.RemoveAll(filepath.Join(installDir, "workspace")); err != nil {
		t.Fatal(err)
	}
	if _, ok := installdir.Load(installDir, "sha256:d1"); ok {
		t.Error("workspace-less install dir was reused")
	}
	if _, ok := installdir.Load(filepath.Join(t.TempDir(), "nope"), "sha256:d1"); ok {
		t.Error("missing install dir was reused")
	}
}

// TestMaterializeLocalGates: --local is unsigned by definition (exit 10
// without --allow-unsigned), an invalid manifest is a compile failure
// (exit 12), and a valid tree installs once then reuses its cache.
func TestMaterializeLocalGates(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)

	cmd := stubCmd(t)
	_, _, err := materializeLocal(cmd, repoPath(t, "testdata/runner/pure-text"), home, false)
	wantClassified(t, err, engine.ExitVerify)

	bad := writeTree(t, map[string]string{"agent.yaml": `schemaVersion: "2"
name: bad
namespace: acme
modes: [autonomous]
version: not-semver
summary: "x"
entrypoints:
  instruction: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
`, "SKILL.md": "# x\n"})
	_, _, err = materializeLocal(stubCmd(t), bad, home, true)
	wantClassified(t, err, engine.ExitCompile)

	// Happy path lands workspace + metadata under the runner home…
	meta, installDir, err := materializeLocal(stubCmd(t), repoPath(t, "testdata/runner/pure-text"), home, true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Source != "local" || meta.Attestation.State != "local" || meta.Ref != "fixtures/pure-text@1.0.0" {
		t.Errorf("local meta = %+v", meta)
	}
	if _, err := os.Stat(filepath.Join(installDir, "workspace", "agent.yaml")); err != nil {
		t.Error("workspace not materialized")
	}
	// …and the second call reuses the cache (same dir, no re-copy chatter).
	var errOut bytes.Buffer
	cmd2 := stubCmd(t)
	cmd2.SetErr(&errOut)
	meta2, installDir2, err := materializeLocal(cmd2, repoPath(t, "testdata/runner/pure-text"), home, true)
	if err != nil || installDir2 != installDir || meta2.Digest != meta.Digest {
		t.Fatalf("cache reuse: %v (dir %s vs %s)", err, installDir2, installDir)
	}
	if strings.Contains(errOut.String(), "local package") {
		t.Errorf("cache hit re-announced the install: %q", errOut.String())
	}
}

// TestRunLocalDryRunDeterministic: the M-R0 acceptance shape as a unit test
// — a --local --dry-run prints the compiled plan on stdout, needs no
// docker, and produces byte-identical plans on repeat runs (§R4:
// deterministic, hashable, printable).
func TestRunLocalDryRunDeterministic(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")

	args := []string{"run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath}
	stdout1, stderr1, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("dry run: %v (stderr %q)", err, stderr1)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(stdout1), &plan); err != nil {
		t.Fatalf("stdout is not the plan JSON: %v\n%s", err, stdout1)
	}
	if plan["ref"] != "fixtures/pure-text@1.0.0" || plan["source"] != "local" {
		t.Errorf("plan identity = %v / %v", plan["ref"], plan["source"])
	}
	if !strings.Contains(stderr1, "run plan sha256:") || !strings.Contains(stderr1, "plan dir:") {
		t.Errorf("stderr lacks the plan hash lines: %q", stderr1)
	}

	stdout2, _, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout1 != stdout2 {
		t.Error("two dry runs of the same tree printed different plans")
	}
}

// TestRunWildcardEgressPolicyRefusal: manifest egress "*" without policy
// consent is refused at compile with the policy exit class (§R4 Q-R1).
func TestRunWildcardEgressPolicyRefusal(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)

	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/wildcard"),
		"--allow-unsigned", "--dry-run", "--policy", policyPath, "--yes")
	wantClassified(t, err, engine.ExitPolicy)
}

func TestRunUnknownAdapterIsCLIError(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)

	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--allow-unsigned", "--dry-run", "--policy", policyPath, "--adapter", "nonesuch")
	wantClassified(t, err, engine.ExitCLI)
}

// TestRunRegistryDryRun: materializeRegistry end to end — resolve, download,
// digest check, attestation verify (real DSSE), cache reuse on the second
// run — without any docker, thanks to --dry-run.
func TestRunRegistryDryRun(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned

	args := []string{"--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath}
	stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("registry dry run: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "attestation:   verified") {
		t.Errorf("run narration lacks the verified line: %q", stderr)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan["source"] != "registry" {
		t.Errorf("plan = %v, %v", plan["source"], err)
	}

	// Second run: the verified install is reused, no re-download.
	_, stderr, err = runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "using verified install") {
		t.Errorf("second run did not reuse the cache: %q", stderr)
	}
}

// TestRunRegistryVerifyGates: pin mismatch, digest mismatch, and the
// unsigned-without-flag refusal are all §R8 exit-10 verify failures.
func TestRunRegistryVerifyGates(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	base := []string{"--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath}

	_, _, err := runCLI(t, strings.NewReader(""), base...)
	wantClassified(t, err, engine.ExitVerify) // unsigned, no --allow-unsigned

	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--allow-unsigned", "--pin", "sha256:beef")...)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "--pin mismatch") {
		t.Errorf("pin error = %v", err)
	}

	fv.digest = "sha256:" + strings.Repeat("ef", 32)
	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--allow-unsigned")...)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("digest error = %v", err)
	}
}

// runFixtureManifest parses a manifest fixture with the given secrets list
// for profile tests.
func runFixtureManifest(t *testing.T, secretsYAML string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "s"
entrypoints:
  instruction: SKILL.md
` + secretsYAML + `publish:
  include: [agent.yaml, SKILL.md]
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestEnsureProfilePaths: the non-guided branches of RUNNER-PLAN-2 §P2/§P3
// — flag validation, the policy-default keep choice, the explicit-keep
// record with no secrets, stored-profile reuse, and the non-TTY fallback
// note. The guided TTY conversation itself is pkg/runner/setup's tested
// code and is not faked here.
func TestEnsureProfilePaths(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	pol := policy.Default()
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:0123456789abcdef"}
	pure := runFixtureManifest(t, "")
	secretful := runFixtureManifest(t, "capabilities:\n  secrets: [API_TOKEN]\n")

	// Bad flag value: refused before anything is written.
	cmd := stubCmd(t)
	_, _, _, err := ensureProfile(cmd, home, meta, pure, pol, runOpts{keepProfile: "maybe"})
	if err == nil || !strings.Contains(err.Error(), "--keep-profile") {
		t.Fatalf("bad keep-profile flag: %v", err)
	}

	// No secrets, nothing decided: no profile exists or is created; keep
	// falls back to the policy default (true).
	prof, dir, keep, err := ensureProfile(stubCmd(t), home, meta, pure, pol, runOpts{})
	if err != nil || prof != nil || !keep {
		t.Fatalf("no-secrets path = %+v keep=%v err=%v", prof, keep, err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("a profile dir appeared for a secretless signature")
	}

	// --keep-profile=true with nothing to collect records the choice for
	// future schema growth (§P3).
	prof, dir, keep, err = ensureProfile(stubCmd(t), home, meta, pure, pol, runOpts{keepProfile: "true"})
	if err != nil || prof == nil || !keep || !prof.Keep {
		t.Fatalf("explicit-keep record = %+v keep=%v err=%v", prof, keep, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "profile.json")); statErr != nil {
		t.Error("explicit keep did not persist a profile")
	}

	// A stored profile's own keep choice wins when the flag is silent.
	stored := profile.New(meta.Ref, meta.Digest, false, map[string]string{"API_TOKEN": "v"})
	if err := stored.Save(dir); err != nil {
		t.Fatal(err)
	}
	prof, _, keep, err = ensureProfile(stubCmd(t), home, meta, secretful, pol, runOpts{})
	if err != nil || prof == nil || keep {
		t.Fatalf("stored keep=false not honored: %+v keep=%v err=%v", prof, keep, err)
	}
	// …and --keep-profile=false forces the remove choice.
	_, _, keep, err = ensureProfile(stubCmd(t), home, meta, secretful, pol, runOpts{keepProfile: "false"})
	if err != nil || keep {
		t.Fatalf("flag keep=false not honored: keep=%v err=%v", keep, err)
	}

	// Non-interactive first run with declared secrets: v1 semantics — a
	// note, no profile, no error (the engine still refuses later if the
	// secret is unresolvable).
	if err := profile.Remove(dir); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	cmd = stubCmd(t)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader("")) // piped: not a TTY
	prof, _, _, err = ensureProfile(cmd, home, meta, secretful, pol, runOpts{})
	if err != nil || prof != nil {
		t.Fatalf("non-TTY secretful first run = %+v, %v", prof, err)
	}
	if !strings.Contains(errOut.String(), "no MCP profile") || !strings.Contains(errOut.String(), "env source") {
		t.Errorf("non-TTY note = %q", errOut.String())
	}

	// --reconfigure-profile explicitly asks for the guided flow, which
	// cannot run without a terminal.
	cmd = stubCmd(t)
	cmd.SetIn(strings.NewReader(""))
	_, _, _, err = ensureProfile(cmd, home, meta, secretful, pol, runOpts{reconfigureProfile: true})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("reconfigure off a TTY: %v", err)
	}
}

// TestFinishProfileLifecycle: keep stamps lastUsedAt and persists; remove
// deletes the directory with the run; nil is a no-op (§P2 completion).
func TestFinishProfileLifecycle(t *testing.T) {
	var errOut bytes.Buffer
	finishProfile(&errOut, nil, t.TempDir(), true)
	if errOut.Len() != 0 {
		t.Errorf("nil profile wrote output: %q", errOut.String())
	}

	dir := filepath.Join(t.TempDir(), "p")
	prof := profile.New("acme/demo@1.0.0", "sha256:d", true, nil)
	if err := prof.Save(dir); err != nil {
		t.Fatal(err)
	}
	finishProfile(&errOut, prof, dir, true)
	kept, err := profile.Load(dir)
	if err != nil || kept == nil || kept.LastUsedAt == "" {
		t.Fatalf("kept profile = %+v, %v (lastUsedAt must be stamped)", kept, err)
	}

	errOut.Reset()
	finishProfile(&errOut, prof, dir, false)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("keep=false did not remove the profile")
	}
	if !strings.Contains(errOut.String(), "MCP profile removed") {
		t.Errorf("removal note = %q", errOut.String())
	}
}

// TestRunInteractiveUsageGates: an interactive run without a terminal, or
// with --output-format json, is a usage error (exit 2) before anything
// starts; --dry-run of the interactive plan still works without a terminal.
func TestRunInteractiveUsageGates(t *testing.T) {
	isolateEnv(t)
	_, pol := isolateRunnerHome(t)
	fixture := repoPath(t, "testdata/runner/full-caps") // declares both modes, and exec: [jq]
	catalogued := policy.Default()
	catalogued.Exec.Packages = map[string]string{"jq": "jq"}
	if err := policy.Save(pol, catalogued); err != nil {
		t.Fatal(err)
	}

	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("non-TTY interactive: %v", err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive", "--output-format", "json")
	wantClassified(t, err, engine.ExitUsage)

	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive", "--dry-run")
	if err != nil {
		t.Fatalf("interactive dry run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, `"mode": "interactive"`) || !strings.Contains(stderr, "mode: interactive (declared: autonomous,interactive)") {
		t.Errorf("dry run = %s\n%s", stdout, stderr)
	}
	// Both modes declared, no flag, no terminal: autonomous, and the
	// consent screen carries no interactive line.
	_, stderr, err = runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--dry-run")
	if err != nil || !strings.Contains(stderr, "mode: autonomous") || strings.Contains(stderr, "interactive: you can talk") {
		t.Errorf("default mode: %v\n%s", err, stderr)
	}
}

// A cached plan compiled by an older CLI is recompiled in place by
// ensurePlan — the --prepare and run paths — and reported; doctor names it
// as stale until then (agentry-cli#60).
func TestEnsurePlanRecompilesOldPlanVersionAndDoctorReportsIt(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	m := runFixtureManifest(t, "")
	pol := policy.Default()
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:0123456789abcdef", Source: "local"}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"SKILL.md": "# demo\n\nDo the thing.\n", "agent.yaml": "schemaVersion: \"2\"\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(installDir, "plan", "autonomous")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "plan.json"), []byte(`{"planVersion":"1","mode":"autonomous"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := &doctorReport{}
	doctorAgentState(rep, home, pol, m, meta, installDir)
	if !doctorHas(rep, "warn", "older CLI") {
		t.Errorf("doctor did not flag the stale plan: %+v", rep.Checks)
	}

	var errOut strings.Builder
	plan, planDir, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil {
		t.Fatalf("ensurePlan: %v", err)
	}
	if plan.PlanVersion != spec.PlanVersion {
		t.Errorf("plan version after recompile = %q, want %q", plan.PlanVersion, spec.PlanVersion)
	}
	if !strings.Contains(errOut.String(), "older CLI") {
		t.Errorf("no recompile notice: %q", errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(planDir, "plan.json"))
	if err != nil || cachedPlanVersion(b) != spec.PlanVersion {
		t.Errorf("on-disk plan version = %q (err %v), want %q", cachedPlanVersion(b), err, spec.PlanVersion)
	}

	rep = &doctorReport{}
	doctorAgentState(rep, home, pol, m, meta, installDir)
	if !doctorHas(rep, "ok", "compiled (plan version") {
		t.Errorf("doctor did not report the recompiled plan: %+v", rep.Checks)
	}
}

func doctorHas(rep *doctorReport, status, needle string) bool {
	for _, e := range rep.Checks {
		if e.Status == status && strings.Contains(e.Detail, needle) {
			return true
		}
	}
	return false
}

// TestEnsurePlanRecompilesForANewerCLIOrRunnerImage (agentry-cli#105): a
// plan the same schema version but compiled by another CLI build, or
// against another runner image, is recompiled — the compiler's output
// changed under it (the catalog env, a proxy switch), and reusing the old
// files would run yesterday's plan with today's binary.
func TestEnsurePlanRecompilesForANewerCLIOrRunnerImage(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	key, err := compile.IntegrityKey(home)
	if err != nil {
		t.Fatal(err)
	}
	// Each case below stands for a plan another build compiled under this
	// runner home, so each rewrite is sealed as that build's compile would
	// have been; an unsealed edit is refused (agentry-cli#266).
	rewrite := func(planDir string, edit func(string) string) {
		t.Helper()
		b, _ := os.ReadFile(filepath.Join(planDir, "plan.json")) //nolint:gosec // test plan dir
		if err := os.WriteFile(filepath.Join(planDir, "plan.json"), []byte(edit(string(b))), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := compile.Seal(key, planDir); err != nil {
			t.Fatal(err)
		}
	}
	m := runFixtureManifest(t, "")
	pol := policy.Default()
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:0123456789abcdef", Source: "local"}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"SKILL.md": "# demo\n\nDo the thing.\n", "agent.yaml": "schemaVersion: \"2\"\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var errOut strings.Builder
	first, planDir, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil {
		t.Fatal(err)
	}
	if first.CompiledBy == "" {
		t.Fatal("plan does not record the CLI that compiled it")
	}
	// The test binary is unstamped, so the plan names the binary, not only
	// the version every unstamped build shares (agentry-cli#144).
	if !strings.HasPrefix(first.CompiledBy, "0.0.0-dev+exe.") {
		t.Errorf("an unstamped build recorded %q, want 0.0.0-dev+exe.<digest>", first.CompiledBy)
	}
	// Same CLI, same image: reused.
	errOut.Reset()
	if _, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous); err != nil || strings.Contains(errOut.String(), "recompiling") {
		t.Fatalf("unchanged plan was not reused: %v %q", err, errOut.String())
	}
	// Another CLI build wrote it: recompiled, and said so.
	rewrite(planDir, func(s string) string { return strings.Replace(s, first.CompiledBy, "other-build", 1) })
	errOut.Reset()
	again, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil || again.CompiledBy != first.CompiledBy || !strings.Contains(errOut.String(), "recompiling") {
		t.Fatalf("plan from another CLI build was reused: %v compiledBy=%q %q", err, again.CompiledBy, errOut.String())
	}
	// A plan an older unstamped CLI cached records the bare 0.0.0-dev that
	// every unstamped build used to share: recompiled. This is how the hosted
	// worker kept running plans from before a compile change (agentry-cli#144).
	rewrite(planDir, func(s string) string { return strings.Replace(s, again.CompiledBy, "0.0.0-dev", 1) })
	errOut.Reset()
	again, _, err = ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil || again.CompiledBy != first.CompiledBy || !strings.Contains(errOut.String(), "recompiling") {
		t.Fatalf("plan from an older unstamped CLI was reused: %v compiledBy=%q %q", err, again.CompiledBy, errOut.String())
	}
	// Another runner image (the embedded build context changed): recompiled.
	rewrite(planDir, func(s string) string { return strings.ReplaceAll(s, again.Proxy.Image, "agentry-runner:older") })
	errOut.Reset()
	third, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil || third.Proxy.Image != again.Proxy.Image || !strings.Contains(errOut.String(), "runner image") {
		t.Fatalf("plan for another runner image was reused: %v image=%q %q", err, third.Proxy.Image, errOut.String())
	}
}

// TestEnsurePlanRefusesAPlanEditedAfterCompile (agentry-cli#266): a
// compiled plan is used only as compiled. plan.json edited after the
// compile, with its generated files left alone so their hashes still
// pass, refuses the run with the verify class instead of running the
// edit; a plan with no record this runner home can check (none, or one
// from another home's key) is recompiled, never trusted.
func TestEnsurePlanRefusesAPlanEditedAfterCompile(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	m := runFixtureManifest(t, "")
	pol := policy.Default()
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:0123456789abcdef", Source: "local"}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"SKILL.md": "# demo\n\nDo the thing.\n", "agent.yaml": "schemaVersion: \"2\"\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ensure := func() (*spec.Plan, string, string, error) {
		var errOut strings.Builder
		plan, planDir, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
		return plan, planDir, errOut.String(), err
	}
	first, planDir, _, err := ensure()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(planDir, compile.IntegrityFile)); err != nil {
		t.Fatalf("the compile wrote no integrity record: %v", err)
	}
	planPath := filepath.Join(planDir, "plan.json")
	compiled, err := os.ReadFile(planPath) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}

	// The edit a writable install dir allows: the contract widened in
	// plan.json alone, every field ensurePlan compares left as it was.
	widened, err := spec.ParsePlan(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if first.Capabilities.EgressMode != "none" {
		t.Fatalf("fixture: egress %q, want none", first.Capabilities.EgressMode)
	}
	widened.Capabilities.EgressMode = "all"
	editedBytes, err := widened.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	edited := string(editedBytes)
	if err := os.WriteFile(planPath, editedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	var classified *engine.ClassifiedError
	if plan, _, _, err := ensure(); err == nil {
		t.Fatalf("an edited plan.json was used: egress %q", plan.Capabilities.EgressMode)
	} else if !strings.Contains(err.Error(), "does not match its integrity record") || !errors.As(err, &classified) || classified.Code != engine.ExitVerify {
		t.Fatalf("edited plan: want the verify-class refusal, got %v", err)
	}
	if b, _ := os.ReadFile(planPath); string(b) != edited { //nolint:gosec // test plan dir
		t.Fatal("the refusal repaired the plan in place: the evidence is gone")
	}

	// No record: recompiled, not trusted.
	if err := os.Remove(filepath.Join(planDir, compile.IntegrityFile)); err != nil {
		t.Fatal(err)
	}
	plan, _, notice, err := ensure()
	if err != nil || !strings.Contains(notice, "no integrity record") || plan.Capabilities.EgressMode != "none" {
		t.Fatalf("a plan without a record: err %v notice %q", err, notice)
	}
	if b, _ := os.ReadFile(planPath); string(b) != string(compiled) { //nolint:gosec // test plan dir
		t.Fatal("the recompile did not restore the compiled plan")
	}

	// A record made under another home's key: recompiled, not refused.
	if err := os.WriteFile(planPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compile.Seal(bytes.Repeat([]byte{7}, 32), planDir); err != nil {
		t.Fatal(err)
	}
	plan, _, notice, err = ensure()
	if err != nil || !strings.Contains(notice, "no integrity record") || plan.Capabilities.EgressMode != "none" {
		t.Fatalf("a plan sealed by another home: err %v notice %q", err, notice)
	}

	// Unchanged: reused without a word.
	if _, _, notice, err := ensure(); err != nil || strings.Contains(notice, "recompiling") {
		t.Fatalf("an intact plan was not reused: %v %q", err, notice)
	}
}

// TestRunPlanAsksNothingRecordsNothing (agentry-cli#124): --plan --json is
// the consent-free preview. A first run of a contract with egress would
// prompt (and fail off a TTY) under --dry-run; --plan reports that a run
// would ask, records no consent, and writes nothing under the runner home.
func TestRunPlanAsksNothingRecordsNothing(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	caps := "capabilities:\n  network:\n    egress: [api.example.com]\n"
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", caps))
	fv.attMode = attSigned

	stdout, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan: %v (stderr %q)", err, stderr)
	}
	var plan runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
	}
	if plan.Ref != "acme/demo@1.0.0" || plan.Mode != "autonomous" || plan.Attestation != "verified" || !plan.Signed || plan.Refusal != "" {
		t.Fatalf("plan: %+v", plan)
	}
	if !plan.NeedsConfirmation || !strings.Contains(plan.Reason, "first run") {
		t.Fatalf("an egress contract needs confirmation: %+v", plan)
	}
	if len(plan.Plan) == 0 || len(plan.PlanSha256) != 64 || !strings.Contains(string(plan.Contract), "api.example.com") {
		t.Fatalf("plan body: sha %q contract %s", plan.PlanSha256, plan.Contract)
	}
	if strings.Contains(stderr, `type "yes"`) {
		t.Fatalf("the plan prompted: %q", stderr)
	}
	if st, _ := installed.Load(); len(st.Records) != 0 {
		t.Fatalf("--plan recorded consent: %+v", st.Records)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "agents")); len(entries) != 0 {
		t.Fatalf("--plan wrote into the runner cache: %v", entries)
	}

	// The dry run off a TTY refuses at the gate: that is the behaviour the
	// preview exists to avoid. (It materializes first, so it comes after
	// the cache assertion.)
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("dry run of an egress contract off a TTY should refuse at the gate: %v", err)
	}

	// Unsigned: reported with the refusal, not refused.
	fv.attMode = attUnsigned
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan of an unsigned release: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan.Attestation != "unsigned" || plan.Signed || !strings.Contains(plan.Refusal, "--allow-unsigned") {
		t.Fatalf("unsigned plan: %v %+v", err, plan)
	}

	// A signature no pinned key verifies: reported as the refusal a run
	// would meet — no flag lifts it — with the contract still shown.
	fv.attMode = attSigned
	fr.enableSigning(t) // a new key: the pinned one no longer verifies the signature
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan of an unverifiable release: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan.Signed || !strings.Contains(plan.Refusal, "a run refuses") || len(plan.Contract) == 0 {
		t.Fatalf("unverifiable plan: %v %+v", err, plan)
	}

	// Local: attestation local, the same refusal without the flag.
	pkg := repoPath(t, "testdata/runner/pure-text")
	stdout, _, err = runCLI(t, strings.NewReader(""), "--json", "run", "--local", pkg, "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("local plan: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan.Attestation != "local" || !strings.Contains(plan.Refusal, "--allow-unsigned") {
		t.Fatalf("local plan: %v %+v", err, plan)
	}
}

// TestRunCachedUnsignedInstallIsRegated (agentry-cli#124): the unsigned gate
// asks for --allow-unsigned on every run, not once per digest.
func TestRunCachedUnsignedInstallIsRegated(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attUnsigned

	first := []string{"--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--allow-unsigned", "--policy", policyPath}
	if _, _, err := runCLI(t, strings.NewReader(""), first...); err != nil {
		t.Fatalf("first run with the flag: %v", err)
	}
	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--allow-unsigned") || !strings.Contains(err.Error(), "cached install") {
		t.Fatalf("a cached unsigned install ran without the flag: %v (stderr %q)", err, stderr)
	}
	wantClassified(t, err, engine.ExitVerify)
}

// TestRunRegistryRefusesAnInvalidManifest: a release downloaded from a
// registry is validated exactly as a --local tree is, before it is cached.
// The fake registry applies no publish-time rules, so it stands in for a
// compromised or out-of-date one (security review 2026-09-20 §4.2, C3).
func TestRunRegistryRefusesAnInvalidManifest(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "escape", "autonomous", "s").addVersion(t, "1.0.0",
		skillFiles("escape", "capabilities:\n  filesystem:\n    write: { paths: [\"../../../../escape\"] }\n"))
	fv.attMode = attSigned

	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/escape@1.0.0", "--dry-run", "--policy", policyPath)
	wantClassified(t, err, engine.ExitCompile)
	if !strings.Contains(err.Error(), "manifest invalid") && !strings.Contains(stderr, "manifest invalid") {
		t.Fatalf("the refusal should name the manifest rule: %v (stderr %q)", err, stderr)
	}
	// Nothing was cached: a refused release is not a verified install.
	if entries, _ := os.ReadDir(filepath.Join(home, "agents", "acme", "escape")); len(entries) != 0 {
		t.Fatalf("a refused release was cached: %v", entries)
	}
}

// TestRunUnscannedIsItsOwnConsent: a release the registry admitted without
// a scan (M22; the core's M21) is refused by run without --allow-unscanned — and
// --allow-unsigned does not stand in for it — on the first run and on
// every run from the cache; the receipt names the consent that admitted
// it; --plan reports the verdict and the refusal instead of stopping.
func TestRunUnscannedIsItsOwnConsent(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "internal", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("internal", ""))
	fv.unscanned = true

	_, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--allow-unscanned") || !strings.Contains(err.Error(), "without a scan") {
		t.Fatalf("an unscanned release ran without the flag: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unsigned", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--allow-unscanned") {
		t.Fatalf("--allow-unsigned admitted an unscanned release: %v", err)
	}

	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unscanned", "--policy", policyPath)
	if err != nil {
		t.Fatalf("run with the flag: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "warning: unscanned") || !strings.Contains(stderr, "because --allow-unscanned was given") {
		t.Errorf("an allowed unscanned run must still warn: %q", stderr)
	}
	// The cache remembers the verdict, not the consent: asked again, and
	// the cache file itself holds no consent.
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--allow-unscanned") || !strings.Contains(err.Error(), "cached install") {
		t.Fatalf("a cached unscanned install ran without the flag: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)
	installs, _ := filepath.Glob(filepath.Join(home, "agents", "acme", "internal", "*", "install.json"))
	if len(installs) != 1 {
		t.Fatalf("install.json files = %v, want one", installs)
	}
	if cached := cachedInstall(t, installs[0]); cached.Attestation.State != "unscanned" || cached.Attestation.Consent != "" {
		t.Fatalf("the cache must hold the verdict and no consent: %+v", cached.Attestation)
	}
	// A cached run with the flag says what it is doing, every time.
	_, stderr, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unscanned", "--policy", policyPath)
	if err != nil || !strings.Contains(stderr, "warning: unscanned") || !strings.Contains(stderr, "cached install") {
		t.Fatalf("a cached unscanned run with the flag: %v (stderr %q)", err, stderr)
	}
	if strings.Contains(stderr, "using verified install") {
		t.Fatalf("a cached unscanned run called itself verified: %q", stderr)
	}

	// --plan: the verdict reported, the refusal named, nothing asked.
	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/internal@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var plan runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan json: %v\n%s", err, stdout)
	}
	if plan.Attestation != "unscanned" || plan.Signed || !strings.Contains(plan.Refusal, "--allow-unscanned") {
		t.Fatalf("plan = attestation %q signed %t refusal %q", plan.Attestation, plan.Signed, plan.Refusal)
	}
}

// TestReceiptRecordsTheConsent: the receipt's attestation carries the flag
// that admitted a release the gate would have refused — evidence of who
// accepted what — and nothing for a verified one.
func TestReceiptRecordsTheConsent(t *testing.T) {
	cached := &installdir.Meta{Ref: "acme/internal@1.0.0", Attestation: receipt.Attestation{State: "unscanned"}}
	if err := cachedAttestationGate(cached, false, false); err == nil {
		t.Fatal("a cached unscanned install passed without the flag")
	}
	if err := cachedAttestationGate(cached, true, false); err == nil {
		t.Fatal("--allow-unsigned passed a cached unscanned install")
	}
	if err := cachedAttestationGate(cached, false, true); err != nil {
		t.Fatalf("with the flag: %v", err)
	}
	if cached.Attestation.Consent != receipt.ConsentAllowUnscanned {
		t.Fatalf("consent = %q, want %q", cached.Attestation.Consent, receipt.ConsentAllowUnscanned)
	}
	unsigned := &installdir.Meta{Ref: "acme/demo@1.0.0", Attestation: receipt.Attestation{State: "unsigned"}}
	if err := cachedAttestationGate(unsigned, true, false); err != nil || unsigned.Attestation.Consent != receipt.ConsentAllowUnsigned {
		t.Fatalf("unsigned: %v, consent %q", err, unsigned.Attestation.Consent)
	}
	verified := &installdir.Meta{Ref: "acme/demo@1.0.0", Attestation: receipt.Attestation{State: "verified", Verified: true}}
	if err := cachedAttestationGate(verified, false, false); err != nil || verified.Attestation.Consent != "" {
		t.Fatalf("verified: %v, consent %q", err, verified.Attestation.Consent)
	}
	// On the wire, as a client reads it.
	raw, _ := json.Marshal(receipt.Attestation{State: "unscanned", Consent: receipt.ConsentAllowUnscanned})
	if string(raw) != `{"state":"unscanned","verified":false,"consent":"--allow-unscanned"}` {
		t.Fatalf("attestation json = %s", raw)
	}
	raw, _ = json.Marshal(receipt.Attestation{State: "verified", KeyID: "k1", Verified: true})
	if strings.Contains(string(raw), "consent") {
		t.Fatalf("a verified attestation carries a consent: %s", raw)
	}
	if r := attestationRefusal(receipt.Attestation{State: "unscanned"}, true, false); !strings.Contains(r, "--allow-unscanned") {
		t.Fatalf("plan refusal = %q", r)
	}
	if r := attestationRefusal(receipt.Attestation{State: "unscanned"}, false, true); r != "" {
		t.Fatalf("plan refusal with the flag = %q", r)
	}
}

// TestRunStaleCacheDefersToTheRegistry: an install cached as unsigned by
// a CLI that did not know the unscanned state, against a registry that
// now says the version was admitted without a scan, is unscanned — the
// registry's word today outranks the cache's memory — so --allow-unsigned
// no longer admits it and the receipt would name the right consent.
func TestRunStaleCacheDefersToTheRegistry(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "internal", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("internal", ""))
	// Cached by the older CLI: the registry then said nothing of scans.
	fv.attMode = attUnsigned
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unsigned", "--policy", policyPath); err != nil {
		t.Fatalf("the older run: %v", err)
	}
	installs, _ := filepath.Glob(filepath.Join(home, "agents", "acme", "internal", "*", "install.json"))
	if len(installs) != 1 {
		t.Fatalf("install.json files = %v", installs)
	}
	if cached := cachedInstall(t, installs[0]); cached.Attestation.State != "unsigned" {
		t.Fatalf("the cache does not say unsigned: %+v", cached.Attestation)
	}
	// The registry now says the version was admitted without a scan.
	fv.unscanned = true
	_, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unsigned", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--allow-unscanned") {
		t.Fatalf("--allow-unsigned admitted a cached release the registry now calls unscanned: %v", err)
	}
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/internal@1.0.0", "--dry-run", "--allow-unscanned", "--policy", policyPath); err != nil || !strings.Contains(stderr, "warning: unscanned") {
		t.Fatalf("with the right flag: %v (stderr %q)", err, stderr)
	}
}

// cachedInstall reads an install.json the run cache wrote.
func cachedInstall(t *testing.T, path string) installdir.Meta {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta installdir.Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("install.json: %v\n%s", err, raw)
	}
	return meta
}

// TestEnsurePlanRecompilesUnderAnotherPolicy (agentry-cli#152): a cached
// plan is reused only when it was compiled against the policy the run
// read, so the receipt's policy.sha256 (the plan's policySha256) is
// always the digest of the file the receipt's policy.path names.
func TestEnsurePlanRecompilesUnderAnotherPolicy(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	m := runFixtureManifest(t, "")
	pol := policy.Default()
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:0123456789abcdef", Source: "local"}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"SKILL.md": "# demo\n\nDo the thing.\n", "agent.yaml": "schemaVersion: \"2\"\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var errOut strings.Builder
	first, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-a", "docker-agent", nil, manifest.ModeAutonomous)
	if err != nil || first.PolicySHA256 != "policy-a" {
		t.Fatalf("first: %v %q", err, first.PolicySHA256)
	}
	errOut.Reset()
	again, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-b", "docker-agent", nil, manifest.ModeAutonomous)
	// A reused plan would still say policy-a.
	if err != nil || again.PolicySHA256 != "policy-b" {
		t.Fatalf("a plan compiled under another policy was reused: %v policySha256=%q", err, again.PolicySHA256)
	}
}
