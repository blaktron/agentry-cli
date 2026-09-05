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
	"github.com/blaktron/agentry-cli/runnerimage"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

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
	if got := digestSuffix("sha256:0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("digestSuffix long = %q", got)
	}
	if got := digestSuffix("short"); got != "short" {
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
func TestLocalTreeDigest(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"agent.yaml": "x", "sub/a.md": "aa",
		".git/HEAD":              "ref: refs/heads/main",
		"agentry-out/old/r.json": "{}",
	})
	d1, err := localTreeDigest(dir)
	if err != nil || !strings.HasPrefix(d1, "sha256:") {
		t.Fatalf("digest = %q, %v", d1, err)
	}
	d2, _ := localTreeDigest(dir)
	if d1 != d2 {
		t.Error("digest is not deterministic")
	}

	// .git and agentry-out do not participate: a fetch or an old run must
	// not invalidate the install cache.
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, _ := localTreeDigest(dir)
	if d3 != d1 {
		t.Error(".git content changed the tree digest")
	}

	if err := os.WriteFile(filepath.Join(dir, "sub", "a.md"), []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}
	d4, _ := localTreeDigest(dir)
	if d4 == d1 {
		t.Error("package content change did not change the digest")
	}
}

func TestCopyWorkspaceSkipsVCSAndOutputs(t *testing.T) {
	src := writeTree(t, map[string]string{
		"agent.yaml": "y", "prompts/main.md": "p",
		".git/config":         "x",
		"agentry-out/r/x.txt": "x",
	})
	dest := filepath.Join(t.TempDir(), "ws")
	if err := copyWorkspace(src, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "prompts", "main.md")); err != nil {
		t.Error("workspace file missing after copy")
	}
	for _, skipped := range []string{".git", "agentry-out"} {
		if _, err := os.Stat(filepath.Join(dest, skipped)); !os.IsNotExist(err) {
			t.Errorf("%s was copied into the workspace", skipped)
		}
	}
}

func TestResolveOutDir(t *testing.T) {
	t.Chdir(t.TempDir())
	meta := &installMeta{Namespace: "acme", Name: "demo"}

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
	meta := &installMeta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
		Version: "1.0.0", Digest: "sha256:d1", Source: "local"}
	if err := writeInstall(installDir, src, meta); err != nil {
		t.Fatal(err)
	}

	got, ok := loadInstallMeta(installDir, "sha256:d1")
	if !ok || got.Ref != meta.Ref || got.Source != "local" {
		t.Fatalf("loadInstallMeta = %+v, %v", got, ok)
	}
	// A digest that moved on (new publish, changed tree) must invalidate
	// the cache rather than run stale bytes.
	if _, ok := loadInstallMeta(installDir, "sha256:other"); ok {
		t.Error("stale digest reused the cached install")
	}
	// A cache entry whose workspace vanished is not an install.
	if err := os.RemoveAll(filepath.Join(installDir, "workspace")); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadInstallMeta(installDir, "sha256:d1"); ok {
		t.Error("workspace-less install dir was reused")
	}
	if _, ok := loadInstallMeta(filepath.Join(t.TempDir(), "nope"), "sha256:d1"); ok {
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

	bad := writeTree(t, map[string]string{"agent.yaml": `schemaVersion: "1"
name: bad
namespace: acme
kind: skill
version: not-semver
summary: "x"
entrypoints:
  skill: SKILL.md
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

func TestManifestFromWorkspace(t *testing.T) {
	m, err := manifestFromWorkspace(repoPath(t, "testdata/runner/pure-text"))
	if err != nil || m.Name != "pure-text" {
		t.Fatalf("manifestFromWorkspace = %+v, %v", m, err)
	}
	if _, err := manifestFromWorkspace(t.TempDir()); err == nil || !strings.Contains(err.Error(), "read agent.yaml") {
		t.Errorf("missing agent.yaml: %v", err)
	}
	bad := writeTree(t, map[string]string{"agent.yaml": ":\nnot yaml: ["})
	if _, err := manifestFromWorkspace(bad); err == nil || !strings.Contains(err.Error(), "parse agent.yaml") {
		t.Errorf("unparseable agent.yaml: %v", err)
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
	m, err := manifest.Parse([]byte(`schemaVersion: "1"
name: demo
namespace: acme
kind: skill
version: 1.0.0
summary: "s"
entrypoints:
  skill: SKILL.md
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
	meta := &installMeta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo",
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
