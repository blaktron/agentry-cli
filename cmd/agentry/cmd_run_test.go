// `agentry run` behavior that needs no docker: the input/output contract
// (task precedence, envelope printing, out-dir), the materialize gates
// (local trees are unsigned; digest/pin/attestation mismatches are
// exit-10 verify failures; bad manifests are exit-12), the install cache,
// MCP-profile lifecycle plumbing, and the --dry-run plan path with its
// determinism guarantee.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/spf13/cobra"
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
	// A run checks the engine before it installs anything; a test that
	// stops before the engine must not depend on the machine's own.
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return &dockerx.Fake{} }
	t.Cleanup(func() { newDockerClient = prev })
	return home, filepath.Join(home, "policy.yaml")
}

// wantClassified asserts err carries the given exit class.
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

// TestTaskReaderPrecedence: --input file > piped stdin > TTY nil, and a
// NAMED file that does not open fails loudly through the deferred reader.
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
	// yields a nil reader (the engine treats nil as an empty task). There is
	// no pty here, so a terminal is any open file the seam says is one.
	prevTTY := stdinIsTTY
	stdinIsTTY = func(in io.Reader) bool { _, ok := in.(*os.File); return ok }
	t.Cleanup(func() { stdinIsTTY = prevTTY })
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

	// Text mode: only the result text on stdout (Unix composition);
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
	if !strings.Contains(errOut.String(), "run finished") || !strings.Contains(errOut.String(), "receipts: /out/dir/receipts") {
		t.Errorf("json mode keeps the finished run's summary on stderr: %q", errOut.String())
	}
	errOut.Reset()
	out.Reset()
	printEnvelope(cmd, env, "json")
	if !strings.Contains(errOut.String(), "run finished") {
		t.Errorf("json mode, alone: the finished run's summary on stderr: %q", errOut.String())
	}

	// An empty result prints nothing in text mode; a run that failed prints no
	// summary of a finished one, in either mode.
	out.Reset()
	errOut.Reset()
	printEnvelope(cmd, &receipt.Envelope{Status: "ok", Result: json.RawMessage(`""`), OutDir: "/out/dir"}, "text")
	if out.Len() != 0 || !strings.Contains(errOut.String(), "run finished") {
		t.Errorf("an empty result: stdout %q, stderr %q", out.String(), errOut.String())
	}
	for _, format := range []string{"text", "json"} {
		out.Reset()
		errOut.Reset()
		printEnvelope(cmd, &receipt.Envelope{Status: "error", ExitCode: engine.ExitHarness, Result: json.RawMessage(`"partial"`), OutDir: "/out/dir"}, format)
		if out.Len() == 0 || strings.Contains(errOut.String(), "run finished") {
			t.Errorf("%s mode, a failed run: stdout %q, stderr %q", format, out.String(), errOut.String())
		}
	}
}

func TestPrintAdaptersListsDefault(t *testing.T) {
	stdout, _, err := runCLI(t, nil, "run", "--list-adapters")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "docker-agent (default)") || !strings.Contains(stdout, "planned: goose") {
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

	// A command-line mistake is returned as it is, for main to print: it is
	// not a run that failed, and gets no failure report.
	for _, args := range [][]string{{"run"}, {"run", "acme/demo", "--local", "dir"}, {"run", "acme/demo", "--output-format", "yaml"}} {
		_, stderr, err := runCLI(t, nil, args...)
		var reported *reportedError
		if err == nil || errors.As(err, &reported) || strings.Contains(stderr, "run failed") {
			t.Errorf("%v: err %v (reported %v), stderr %q", args, err, reported != nil, stderr)
		}
	}
	// Flags that exclude each other are refused before anything is read.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "acme/demo", "--no-destination", "--destination", "d"}, "--no-destination and --destination exclude each other"},
		{[]string{"run", "acme/demo", "--destinations-file", "f.json", "--destination", "d"}, "--destinations-file excludes --destination and --no-destination"},
		{[]string{"run", "acme/demo", "--destinations-file", "f.json", "--no-destination"}, "--destinations-file excludes --destination and --no-destination"},
		{[]string{"run", "acme/demo", "--installed"}, "--installed runs one exact version at --pin: name it as ns/name@version"},
		{[]string{"run", "acme/demo@^1.0", "--installed", "--pin", "sha256:abc"}, "--installed runs one exact version at --pin: name it as ns/name@version"},
	} {
		_, _, err := runCLI(t, nil, c.args...)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
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
	_, _, err = materializeLocal(stubCmd(t), writeTree(t, map[string]string{"agent.yaml": "name: [unclosed\n"}), home, true)
	wantClassified(t, err, engine.ExitCompile)

	// Happy path lands workspace + metadata under the runner home…
	var firstErr bytes.Buffer
	first := stubCmd(t)
	first.SetErr(&firstErr)
	meta, installDir, err := materializeLocal(first, repoPath(t, "testdata/runner/pure-text"), home, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := "local package fixtures/pure-text@1.0.0 (" + installdir.DigestSuffix(meta.Digest) + ")\n"; firstErr.String() != want {
		t.Errorf("the first install said %q, want %q", firstErr.String(), want)
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

// TestRunLocalDryRunDeterministic: the acceptance shape as a unit test — a
// --local --dry-run prints the compiled plan on stdout, needs no docker, and
// produces byte-identical plans on repeat runs (deterministic, hashable,
// printable).
func TestRunLocalDryRunDeterministic(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")

	args := []string{"--json", "run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath}
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
	// A release that declares no server has no registry verdict to record.
	if enforcement, _ := plan["enforcement"].(map[string]any); enforcement == nil || enforcement["mcpServers.vulnerabilities"] != nil {
		t.Errorf("enforcement = %v", plan["enforcement"])
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

	// --pin holds the directory to the digest it was pinned at: the same tree
	// runs, another does not.
	digest, _ := plan["digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("plan digest = %v", plan["digest"])
	}
	if _, _, err := runCLI(t, strings.NewReader(""), append(args, "--pin", digest)...); err != nil {
		t.Errorf("a directory at the digest it was pinned at: %v", err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), append(args, "--pin", "sha256:beef")...)
	wantClassified(t, err, engine.ExitVerify)
	if want := "--pin mismatch: the directory " + pkg + " is now " + digest + ", pin requires sha256:beef"; err.Error() != want {
		t.Errorf("pin error = %q, want %q", err, want)
	}
}

// TestRunWildcardEgressPolicyRefusal: manifest egress "*" without policy
// consent is refused at compile with the policy exit class.
func TestRunWildcardEgressPolicyRefusal(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)

	_, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/wildcard"),
		"--allow-unsigned", "--dry-run", "--policy", policyPath, "--yes")
	wantClassified(t, err, engine.ExitPolicy)
	// A warning the manifest rules give does not stop the run; it is shown.
	if !strings.Contains(stderr, "warning: ") {
		t.Errorf("stderr shows no manifest warning: %q", stderr)
	}
}

// A run that fails before the engine writes an envelope still prints one
// under --output-format json: the exit class and the step it stopped in,
// which is all a hosted run's host may keep. In text mode stdout stays
// empty.
func TestRunFailingBeforeTheEngineNamesItsStage(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	for _, c := range []struct {
		name   string
		args   []string
		code   int
		stage  string
		reason *receipt.Reason
	}{
		{"an unsigned local release", []string{"--local", repoPath(t, "testdata/runner/pure-text")}, engine.ExitVerify, receipt.StageVerify, nil},
		{"an undeclared adapter", []string{"--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--adapter", "nonesuch"}, engine.ExitCLI, receipt.StageCompile, nil},
		// The reason says why in fixed words, naming the declared command
		// this machine does not provide.
		{"a command the policy does not provide", []string{"--local", repoPath(t, "testdata/runner/exec-git"), "--allow-unsigned"}, engine.ExitPolicy, receipt.StageCompile,
			&receipt.Reason{Code: receipt.ReasonExecNotProvided, Names: []string{"git"}}},
	} {
		args := append([]string{"run"}, c.args...)
		args = append(args, "--policy", policyPath, "--yes", "--output-format", "json")
		stdout, _, err := runCLI(t, strings.NewReader(""), args...)
		wantClassified(t, err, c.code)
		var env receipt.Envelope
		if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
			t.Fatalf("%s: stdout is not an envelope: %v\n%s", c.name, jerr, stdout)
		}
		if env.Status != "error" || env.ExitCode != c.code || env.ExitClass != receipt.ExitClassName(c.code) || env.Stage != c.stage || env.Error == "" {
			t.Errorf("%s: envelope status %q code %d class %q stage %q error %q", c.name, env.Status, env.ExitCode, env.ExitClass, env.Stage, env.Error)
		}
		if !reflect.DeepEqual(env.Reason, c.reason) {
			t.Errorf("%s: reason %+v, want %+v", c.name, env.Reason, c.reason)
		}
		// Past verification the release is named with what verified it,
		// so a later failure does not read as an unverified release.
		if c.stage != receipt.StageVerify && (env.Digest == "" || env.Attestation.State != "local") {
			t.Errorf("%s: digest %q attestation %+v, want the verified local release", c.name, env.Digest, env.Attestation)
		}
		textArgs := args[:len(args)-2]
		// The root --json prints the same failure envelope.
		rootOut, _, rootErr := runCLI(t, strings.NewReader(""), append([]string{"--json"}, textArgs...)...)
		wantClassified(t, rootErr, c.code)
		var rootEnv receipt.Envelope
		if jerr := json.Unmarshal([]byte(rootOut), &rootEnv); jerr != nil {
			t.Fatalf("%s: under the root --json stdout is not an envelope: %v\n%s", c.name, jerr, rootOut)
		}
		if !reflect.DeepEqual(rootEnv, env) {
			t.Errorf("%s: the root --json printed %+v, want the --output-format json envelope %+v", c.name, rootEnv, env)
		}
		out, textErr, _ := runCLI(t, strings.NewReader(""), textArgs...)
		if out != "" {
			t.Errorf("%s: text mode printed %q", c.name, out)
		}
		if want := "agentry: run failed: " + receipt.ExitClassName(c.code) + ": "; !strings.Contains(textErr, want) {
			t.Errorf("%s: text mode explained the failure as %q, want %q", c.name, textErr, want)
		}
		// The envelope's shape: the keys a driver reads are there, with the
		// empty values a failure has, not null.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for key, want := range map[string]string{"agentry": `"` + receipt.EnvelopeVersion + `"`, "status": `"error"`, "result": `""`, "capabilities_enforced": `{}`} {
			if string(raw[key]) != want {
				t.Errorf("%s: envelope %s = %s, want %s", c.name, key, raw[key], want)
			}
		}
	}
}

// An engine that is not there is the first thing a run says, before anything
// is installed for it; a plan or a dry run needs none. A failure to install
// the images is a gateway failure, and ends the stage line it was on.
func TestRunNeedsTheEngineBeforeItInstallsAnything(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	down := &dockerx.Fake{AvailableErr: errors.New("the daemon is down")}
	newDockerClient = func() dockerx.Client { return down }
	pure := repoPath(t, "testdata/runner/pure-text")

	stdout, _, err := runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--policy", policyPath, "--output-format", "json")
	wantClassified(t, err, engine.ExitGateway)
	var env receipt.Envelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil || env.Stage != receipt.StageImages || env.ExitClass != "gateway" || !strings.Contains(env.Error, "the daemon is down") {
		t.Errorf("envelope: stage %q class %q error %q (%v)", env.Stage, env.ExitClass, env.Error, jerr)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "agents")); len(entries) != 0 {
		t.Errorf("a run that cannot start installed %v", entries)
	}
	for _, flag := range []string{"--dry-run", "--plan"} {
		if _, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--policy", policyPath, flag); err != nil {
			t.Errorf("%s needs no engine: %v\n%s", flag, err, stderr)
		}
	}

	// Installing the runner image fails: the stage line ends as failed.
	pol, _, err := policy.EnsureDefault(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	broken := &dockerx.Fake{FailBuild: map[string]error{pol.RunnerImageTag(runnerimage.ContentVersion()): errors.New("build broke")}}
	newDockerClient = func() dockerx.Client { return broken }
	prevTerminal := terminalWriter
	terminalWriter = func(io.Writer) bool { return true }
	t.Cleanup(func() { terminalWriter = prevTerminal })
	_, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--policy", policyPath)
	wantClassified(t, err, engine.ExitGateway)
	if !strings.Contains(err.Error(), "build broke") || !regexp.MustCompile(`from embedded context[^\n]*\(first use\)… (\x1b\[31m)?failed(\x1b\[0m)? after \d`).MatchString(stderr) {
		t.Errorf("a failed image install: %v\n%s", err, stderr)
	}
}

// A failure before the engine names the release a person asked for by its ref.
func TestRunFailingBeforeTheEngineNamesTheRefItWasAskedFor(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "")).attMode = attUnsigned
	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--policy", policyPath, "--yes", "--output-format", "json")
	wantClassified(t, err, engine.ExitVerify)
	var env receipt.Envelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil || env.Ref != "acme/demo@1.0.0" || env.Stage != receipt.StageVerify {
		t.Errorf("envelope: ref %q stage %q (%v)", env.Ref, env.Stage, jerr)
	}
}

// A run cancelled before the engine says it was interrupted, under the cli
// class, as the engine's own envelope does: a driver records an
// interruption, not the refusal the step's class would read as.
func TestAnInterruptedRunBeforeTheEngineSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	p := &runProgress{stage: receipt.StageMCPServers}
	printFailure(ctx, cmd, p, engine.Classify(engine.ExitVerify, context.Canceled))
	var env receipt.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, out.String())
	}
	if env.Error != "interrupted" || env.ExitCode != engine.ExitCLI || env.Stage != receipt.StageMCPServers {
		t.Errorf("envelope error %q code %d stage %q", env.Error, env.ExitCode, env.Stage)
	}
}

// A refusal that reaches the CLI's own envelope names the inputs its error
// names, and counts one whose path the error does not show (a masked one).
func TestARefusalBeforeTheEnginesEnvelopeNamesWhatItsErrorNames(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := receipt.WithRefusedInputs(receipt.WithReason(errors.New("the run was refused before the agent started: ClamAV found malware in eicar.com (Eicar-Test-Signature), [redacted]"), receipt.ReasonInputRefused, receipt.InputMalware),
		[]receipt.RefusedInput{{Path: "eicar.com", By: receipt.RefusedByClamAV, Signature: "Eicar-Test-Signature"}, {Path: "secret-value.com", By: receipt.RefusedByClamAV}})
	printFailure(context.Background(), cmd, &runProgress{}, engine.Classify(engine.ExitPolicy, err))
	var env receipt.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, out.String())
	}
	if len(env.RefusedInputs) != 1 || env.RefusedInputs[0].Path != "eicar.com" || env.RefusedInputsMore != 1 {
		t.Fatalf("refused inputs %+v and %d more", env.RefusedInputs, env.RefusedInputsMore)
	}
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

	args := []string{"--json", "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath}
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

	// A runhost's service runs the release it installed, at its digest, asking
	// no registry; its harness is a container, never a process of the host.
	installedRun := []string{"--json", "--registry", fr.url(), "run", "acme/demo@1.0.0", "--installed", "--pin", fv.digest, "--dry-run", "--policy", policyPath}
	if _, stderr, err := runCLI(t, strings.NewReader(""), installedRun...); err != nil || !strings.Contains(stderr, "using verified install acme/demo@1.0.0") {
		t.Fatalf("an installed release: %v\n%s", err, stderr)
	}
	_, _, err = runCLI(t, strings.NewReader(""), append(installedRun, "--adapter", adapter.ClaudeAdapterName)...)
	wantClassified(t, err, engine.ExitPolicy)
	if want := `a runhost runs its harness in a container under gVisor; the adapter "claude" runs one as a host process`; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--installed", "--pin", "sha256:"+strings.Repeat("ab", 32), "--dry-run", "--policy", policyPath)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "acme/demo@1.0.0 is not installed at sha256:") {
		t.Errorf("err = %v", err)
	}
}

// TestRunRegistryVerifyGates: pin mismatch, digest mismatch, and the
// unsigned-without-flag refusal are all exit-10 verify failures.
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

// TestEnsureProfilePaths: the non-guided branches of the MCP profile step —
// flag validation, the policy-default keep choice, the explicit-keep record
// with no secrets, stored-profile reuse, and the non-TTY fallback note. The
// guided TTY conversation itself is pkg/runner/setup's tested code and is
// not faked here.
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
	// future schema growth.
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
	prof, _, keep, err = ensureProfile(cmd, home, meta, secretful, pol, runOpts{})
	if err != nil || prof != nil {
		t.Fatalf("non-TTY secretful first run = %+v, %v", prof, err)
	}
	if !keep {
		t.Error("with no profile and no choice, the policy's default (keep) is the answer")
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

	// Through the run command the same refusal is a policy refusal.
	_, policyPath := isolateRunnerHome(t)
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes", "--policy", policyPath, "--reconfigure-profile")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "--reconfigure-profile needs an interactive terminal") {
		t.Errorf("err = %v", err)
	}
}

// TestFinishProfileLifecycle: keep stamps lastUsedAt and persists; remove
// deletes the directory with the run; nil is a no-op.
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

	// A profile not yet marked kept is, once a run keeps it.
	unmarked := filepath.Join(t.TempDir(), "q")
	notKept := profile.New("acme/demo@1.0.0", "sha256:d", false, nil)
	if err := notKept.Save(unmarked); err != nil {
		t.Fatal(err)
	}
	finishProfile(&errOut, notKept, unmarked, true)
	if again, err := profile.Load(unmarked); err != nil || again == nil || !again.Keep {
		t.Errorf("a profile a run kept = %+v, %v", again, err)
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
	home, pol := isolateRunnerHome(t)
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

	// On a terminal the chat is allowed, but its transcript is the record: no
	// envelope on stdout, and no event stream beside it.
	prevTTY := stdinIsTTY
	stdinIsTTY = func(io.Reader) bool { return true }
	for _, c := range []struct{ flag, value, want string }{
		{"--output-format", "json", "--output-format json is not available for an interactive run"},
		{"--events", filepath.Join(t.TempDir(), "events.ndjson"), "--events is not available for an interactive run"},
	} {
		_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive", c.flag, c.value)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s on a terminal: %v", c.flag, err)
		}
	}
	stdinIsTTY = prevTTY

	stdout, stderr, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive", "--dry-run")
	if err != nil {
		t.Fatalf("interactive dry run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, `"mode": "interactive"`) || !strings.Contains(stderr, "mode: interactive (declared: autonomous,interactive)") {
		t.Errorf("dry run = %s\n%s", stdout, stderr)
	}
	// The consent screen of a session carries the interactive line, and a
	// consent given is recorded.
	if !strings.Contains(stderr, "\n  interactive: you can talk to this agent and give it files; you cannot give it more access than listed above\n") {
		t.Errorf("the consent screen of an interactive run:\n%s", stderr)
	}
	if st, _ := installed.Load(); st.Find(installdir.ConsentKey(home, "fixtures", "full-caps")) == nil {
		t.Errorf("the consent given with --yes was not recorded: %+v", st.Records)
	}
	// A mode that is none is a usage error; a release that is a persistent
	// agent is one a run cannot be; a plan of an interactive release needs no
	// terminal.
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "chatty", "--dry-run")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), `unknown --mode "chatty"`) {
		t.Errorf("a bad mode: %v", err)
	}
	persistent := skillFiles("persist", "")
	persistent["agent.yaml"] = strings.Replace(persistent["agent.yaml"], "modes: [autonomous]", "modes: [persistent]", 1)
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", writeTree(t, persistent), "--allow-unsigned", "--yes", "--policy", pol, "--dry-run")
	wantClassified(t, err, engine.ExitUsage)
	if want := "acme/persist runs as a persistent agent, not as a run: install it with agentry agent install acme/persist, then start it with agentry agent start"; err.Error() != want {
		t.Errorf("a persistent release: %q, want %q", err, want)
	}
	planned, _, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", fixture, "--allow-unsigned", "--policy", pol, "--mode", "interactive", "--plan")
	if err != nil || !strings.Contains(planned, `"mode":"interactive"`) {
		t.Errorf("a plan of an interactive release: %v\n%s", err, planned)
	}
	// Both modes declared, no flag, no terminal: autonomous, and the
	// consent screen carries no interactive line.
	_, stderr, err = runCLI(t, strings.NewReader(""), "--json", "run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--dry-run")
	if err != nil || !strings.Contains(stderr, "mode: autonomous") || strings.Contains(stderr, "interactive: you can talk") {
		t.Errorf("default mode: %v\n%s", err, stderr)
	}
}

// A cached plan compiled by an older CLI is recompiled in place by
// ensurePlan — the --prepare and run paths — and reported; doctor names it
// as stale until then.
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

// TestEnsurePlanRecompilesForANewerCLIOrRunnerImage: a plan the same
// schema version but compiled by another CLI build, or against another
// runner image, is recompiled — the compiler's output changed under it
// (the catalog env, a proxy switch), and reusing the old files would run
// yesterday's plan with today's binary.
func TestEnsurePlanRecompilesForANewerCLIOrRunnerImage(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	key, err := compile.IntegrityKey(home)
	if err != nil {
		t.Fatal(err)
	}
	// Each case below stands for a plan another build compiled under this
	// runner home, so each rewrite is sealed as that build's compile would
	// have been; an unsealed edit is refused.
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
	// the version every unstamped build shares.
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
	// every unstamped build shares: recompiled, so a hosted worker does not
	// keep running plans from before a compile change.
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
	// A plan sealed for another release, run mode, adapter or schema is not
	// this run's: it is compiled again, whatever else it holds.
	for _, c := range []struct {
		name string
		edit func(plan map[string]any)
	}{
		{"another release", func(p map[string]any) { p["digest"] = "sha256:ffffffffffffffff" }},
		{"another mode", func(p map[string]any) { p["mode"] = "interactive" }},
		{"another adapter", func(p map[string]any) { p["harness"].(map[string]any)["adapter"] = "other-harness" }},
		{"another schema", func(p map[string]any) { p["planVersion"] = "1" }},
	} {
		rewrite(planDir, func(s string) string {
			var plan map[string]any
			if err := json.Unmarshal([]byte(s), &plan); err != nil {
				t.Fatal(err)
			}
			c.edit(plan)
			b, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		})
		errOut.Reset()
		fresh, _, err := ensurePlan(&errOut, home, m, meta, installDir, ws, pol, "policy-sha", "docker-agent", nil, manifest.ModeAutonomous)
		if err != nil || fresh.Digest != meta.Digest || fresh.Mode != string(manifest.ModeAutonomous) || fresh.Harness.Adapter != "docker-agent" || fresh.PlanVersion != spec.PlanVersion {
			t.Errorf("%s: the plan was reused: %v digest %q mode %q adapter %q version %q", c.name, err, fresh.Digest, fresh.Mode, fresh.Harness.Adapter, fresh.PlanVersion)
		}
	}
}

// TestEnsurePlanRefusesAPlanEditedAfterCompile: a compiled plan is
// used only as compiled. plan.json edited after the compile, with its
// generated files left alone so their hashes still pass, refuses the
// run with the verify class instead of running the edit; a plan with
// no record this runner home can check (none, or one from another
// home's key) is recompiled, never trusted.
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

// TestRunPlanAsksNothingRecordsNothing: --plan --json is the consent-free
// preview. A first run of a contract with egress would prompt (and fail
// off a TTY) under --dry-run; --plan reports that a run would ask, records
// no consent, and writes nothing under the runner home.
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
	if plan.Version != "1.0.0" || plan.Digest != fv.digest || !slices.Equal(plan.Modes, []string{"autonomous"}) || plan.Summary != "test fixture skill" || plan.KeyID == "" ||
		plan.ContractDigest == "" || plan.Integrity != integrityNotInstalled || len(plan.Refusals) != 0 {
		t.Fatalf("what the plan says of the release: version %q digest %q modes %v summary %q key %q contract digest %q integrity %q refusals %v",
			plan.Version, plan.Digest, plan.Modes, plan.Summary, plan.KeyID, plan.ContractDigest, plan.Integrity, plan.Refusals)
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

	// In words, the same preview.
	words, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan in words: %v", err)
	}
	if want := "plan: acme/demo@1.0.0 (autonomous) plan sha256 " + plan.PlanSha256 + "\nattestation: verified\ninstalled: not installed — a run fetches it\n" +
		"would ask: " + plan.Reason + "\nnothing asked, nothing recorded (--plan)\n"; words != want {
		t.Fatalf("plan in words:\n%s\nwant:\n%s", words, want)
	}

	// The dry run off a TTY refuses at the gate: that is the behaviour the
	// preview exists to avoid. (It materializes first, so it comes after
	// the cache assertion.)
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("dry run of an egress contract off a TTY should refuse at the gate: %v", err)
	}
	wantClassified(t, err, engine.ExitPolicy)

	// Unsigned: reported with the refusal, not refused.
	fv.attMode = attUnsigned
	stdout, planErr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan of an unsigned release: %v", err)
	}
	if !strings.Contains(planErr, "attestation:   unsigned — a run of acme/demo@1.0.0 proceeds only with --allow-unsigned\n") {
		t.Errorf("the plan of an unsigned release says on stderr:\n%s", planErr)
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || plan.Attestation != "unsigned" || plan.Signed || !strings.Contains(plan.Refusal, "--allow-unsigned") {
		t.Fatalf("unsigned plan: %v %+v", err, plan)
	}
	// The dry run above laid the release down, and nothing has touched it.
	var unsigned runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &unsigned); err != nil || unsigned.Integrity != integrityUnchanged || len(unsigned.Refusals) != 1 ||
		unsigned.Refusals[0].Lift != receipt.ConsentAllowUnsigned || unsigned.Refusals[0].Kind != refusalAttestation {
		t.Fatalf("unsigned plan of an installed release: %v integrity %q, refusals %+v", err, unsigned.Integrity, unsigned.Refusals)
	}
	words, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil || !strings.Contains(words, "\ninstalled: unchanged since it was installed\nwould ask: ") || !strings.Contains(words, "\nwould refuse: "+plan.Refusal+"\nnothing asked, nothing recorded (--plan)\n") {
		t.Fatalf("plan in words of an installed unsigned release: %v\n%s", err, words)
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
	var unverifiable runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &unverifiable); err != nil || len(unverifiable.Refusals) != 1 || unverifiable.Refusals[0].Lift != "" || unverifiable.Refusals[0].Kind != refusalAttestation {
		t.Fatalf("no flag lifts a signature no pinned key verifies: %v %+v", err, unverifiable.Refusals)
	}

	// The release laid down is checked against its record: a file added after
	// it was laid down is reported, and a refusal no flag lifts.
	releaseDir := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest)
	if err := os.WriteFile(filepath.Join(releaseDir, "workspace", "added-later.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	var changedPlan runPlanOutput
	if err != nil || json.Unmarshal([]byte(stdout), &changedPlan) != nil || changedPlan.Integrity != integrityChanged || len(changedPlan.Refusals) != 2 ||
		changedPlan.Refusals[0].Kind != refusalInstalledChanged || changedPlan.Refusals[0].Lift != "" || changedPlan.Refusals[1].Kind != refusalAttestation {
		t.Fatalf("plan of a release that changed after it was installed: %v %q %+v", err, changedPlan.Integrity, changedPlan.Refusals)
	}
	if err := os.Remove(filepath.Join(releaseDir, "workspace", "added-later.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(releaseDir, installdir.RecordFile)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	var noRecord runPlanOutput
	if err != nil || json.Unmarshal([]byte(stdout), &noRecord) != nil || noRecord.Integrity != integrityNoRecord || len(noRecord.Refusals) != 1 || noRecord.Refusals[0].Kind != refusalAttestation {
		t.Fatalf("plan of a release installed with no record: %v %q %+v", err, noRecord.Integrity, noRecord.Refusals)
	}

	// What the person accepted before decides what a run would ask: a version
	// that widens it asks, one that does not asks nothing.
	key := installdir.ConsentKey(home, "acme", "demo")
	var accepted installed.State
	accepted.Reconsent(key, "0.9.0", installed.Snapshot(manifest.Capabilities{}), "sha256:old")
	if err := installed.Save(accepted); err != nil {
		t.Fatal(err)
	}
	var widens runPlanOutput
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan of a version that widens: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &widens); err != nil || widens.Installed == nil || widens.Installed.Version != "0.9.0" || !widens.Diff.HasWidened() ||
		!widens.NeedsConfirmation || widens.Reason != "this version widens the declared capabilities" {
		t.Fatalf("plan of a version that widens what was accepted: %v %+v", err, widens.runPlanConsent)
	}
	accepted = installed.State{}
	declared, err := manifest.Parse([]byte(skillFiles("demo", caps)["agent.yaml"]))
	if err != nil {
		t.Fatal(err)
	}
	accepted.Reconsent(key, "0.9.0", installed.Snapshot(declared.Contract()), "sha256:old")
	if err := installed.Save(accepted); err != nil {
		t.Fatal(err)
	}
	var same runPlanOutput
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan of a version that widens nothing: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &same); err != nil || same.Installed == nil || same.Diff.HasWidened() || same.NeedsConfirmation || same.Reason != "" {
		t.Fatalf("plan of a version that widens nothing: %v %+v", err, same.runPlanConsent)
	}
	if err := installed.Save(installed.State{}); err != nil {
		t.Fatal(err)
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
	// A pure-text release has nothing to ask about, and a local directory is
	// its own source.
	var local runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &local); err != nil || local.NeedsConfirmation || local.Reason != "" || local.Integrity != integrityLocal || local.Digest == "" || local.Version != "1.0.0" {
		t.Fatalf("local plan: %v needs confirmation %v (%q), integrity %q, digest %q, version %q", err, local.NeedsConfirmation, local.Reason, local.Integrity, local.Digest, local.Version)
	}
	// In words: nothing would be asked of a release that declares nothing.
	words, _, err = runCLI(t, strings.NewReader(""), "run", "--local", pkg, "--plan", "--policy", policyPath)
	if want := "plan: fixtures/pure-text@1.0.0 (autonomous) plan sha256 " + local.PlanSha256 + "\nattestation: local\ninstalled: not checked — a local directory is its own source\n" +
		"would refuse: a local workspace has no attestation; a run needs --allow-unsigned\nnothing asked, nothing recorded (--plan)\n"; err != nil || words != want {
		t.Fatalf("plan in words of a local release: %v\n%s\nwant:\n%s", err, words, want)
	}
}

// TestRunCachedUnsignedInstallIsRegated: the unsigned gate asks
// for --allow-unsigned on every run, not once per digest.
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
// compromised or out-of-date one.
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

	// An archive that matches its digest and cannot be unpacked is a failure to
	// verify the release.
	junk := []byte("this is not an archive")
	broken := fr.addAgent("acme", "broken", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("broken", ""))
	broken.archive, broken.digest, broken.attMode = junk, sha256Digest(junk), attUnsigned
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/broken@1.0.0", "--dry-run", "--allow-unsigned", "--policy", policyPath)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "extract archive: ") {
		t.Errorf("an archive that cannot be unpacked: %v", err)
	}

	// A copy an older CLI cached without these rules is held to them at run.
	src := writeTree(t, skillFiles("escape", "capabilities:\n  filesystem:\n    write: { paths: [\"../../../../escape\"] }\n"))
	cachedMeta := &installdir.Meta{Ref: "acme/escape@1.0.0", Namespace: "acme", Name: "escape", Version: "1.0.0", Digest: fv.digest, Source: "registry",
		Attestation: receipt.Attestation{State: "verified", Verified: true}}
	if err := installdir.Write(installdir.ReleaseDir(home, "acme", "escape", "1.0.0", fv.digest), src, cachedMeta); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/escape@1.0.0", "--dry-run", "--policy", policyPath)
	wantClassified(t, err, engine.ExitCompile)
	if !strings.Contains(err.Error(), "acme/escape@1.0.0 manifest invalid:\n  ") || !strings.Contains(stderr, "using verified install acme/escape@1.0.0") {
		t.Fatalf("a cached release with an invalid manifest: %v (stderr %q)", err, stderr)
	}
}

// TestRunUnscannedIsItsOwnConsent: a release the registry admitted without
// a scan is refused by run without --allow-unscanned — and
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

	// A local release is admitted like an unsigned one, with its flag.
	local := &installdir.Meta{Ref: "acme/demo@1.0.0", Attestation: receipt.Attestation{State: "local"}}
	if err := cachedAttestationGate(local, false, true); err == nil || !strings.Contains(err.Error(), "acme/demo@1.0.0 is local (cached install); re-run with --allow-unsigned") {
		t.Fatalf("a cached local install without the flag: %v", err)
	}
	if err := cachedAttestationGate(local, true, false); err != nil || local.Attestation.Consent != receipt.ConsentAllowUnsigned {
		t.Fatalf("a cached local install with the flag: %v, consent %q", err, local.Attestation.Consent)
	}
	// What a plan says a run would stop on, and the flag that lifts it.
	for _, c := range []struct {
		att                        receipt.Attestation
		allowUnsigned, allowUnscan bool
		refusal, lift              string
	}{
		{receipt.Attestation{State: "verified"}, false, false, "", ""},
		{receipt.Attestation{State: "unsigned"}, false, false, "the release is unsigned; a run needs --allow-unsigned", receipt.ConsentAllowUnsigned},
		{receipt.Attestation{State: "unsigned"}, true, false, "", receipt.ConsentAllowUnsigned},
		{receipt.Attestation{State: "local"}, false, false, "the release is local; a run needs --allow-unsigned", receipt.ConsentAllowUnsigned},
		{receipt.Attestation{State: "local"}, true, false, "", receipt.ConsentAllowUnsigned},
		{receipt.Attestation{State: "unscanned"}, false, false, "the release is unscanned — the registry admitted it without a scan; a run needs --allow-unscanned", receipt.ConsentAllowUnscanned},
		{receipt.Attestation{State: "unscanned"}, true, false, "the release is unscanned — the registry admitted it without a scan; a run needs --allow-unscanned", receipt.ConsentAllowUnscanned},
		{receipt.Attestation{State: "mismatch"}, true, true, "attestation mismatch — a run refuses", ""},
	} {
		if got := attestationRefusal(c.att, c.allowUnsigned, c.allowUnscan); got != c.refusal {
			t.Errorf("%s (unsigned %v, unscanned %v): refusal %q, want %q", c.att.State, c.allowUnsigned, c.allowUnscan, got, c.refusal)
		}
		if got := attestationLift(c.att); got != c.lift {
			t.Errorf("%s: lift %q, want %q", c.att.State, got, c.lift)
		}
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

// TestEnsurePlanRecompilesUnderAnotherPolicy: a cached plan is reused
// only when it was compiled against the policy the run read, so the
// receipt's policy.sha256 (the plan's policySha256) is always the
// digest of the file the receipt's policy.path names.
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

// TestRunSaysWhatItFoundOfTheSignature: the first verified run says the key
// that verified it and that the key was pinned on first use; a version the
// registry yanked runs only for an exact pin, and says so; a signature no
// pinned key verifies refuses the run, with the reason.
func TestRunSaysWhatItFoundOfTheSignature(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned
	args := []string{"--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath}

	_, stderr, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr)
	}
	if !regexp.MustCompile(`attestation:   verified \(key [^)]+\)\n`).MatchString(stderr) || !strings.Contains(stderr, "note: signing keys pinned on first use (TOFU)") {
		t.Errorf("the first run did not say what verified it:\n%s", stderr)
	}

	fv.yanked = true
	_, stderr, err = runCLI(t, strings.NewReader(""), args...)
	if err != nil || !strings.Contains(stderr, "warning: acme/demo@1.0.0 is YANKED — running only because the version was pinned exactly\n") {
		t.Errorf("a yanked version: %v\n%s", err, stderr)
	}
	fv.yanked = false

	// A new key: the one pinned no longer verifies the signature. A cached
	// install is not asked again, so the release is fetched fresh.
	other := fr.addAgent("acme", "other", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("other", ""))
	other.attMode = attSigned
	fr.enableSigning(t)
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/other@1.0.0", "--dry-run", "--policy", policyPath)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.HasPrefix(err.Error(), "attestation unverifiable for acme/other@1.0.0: ") || !strings.HasSuffix(err.Error(), " — refusing to run") {
		t.Errorf("a signature no pinned key verifies: %v", err)
	}
}

// TestRunReceiptNamesTheConsentThatAdmittedARegistryRelease: a run to its end
// reports, in the envelope, how its release was admitted: the key of a verified
// one, the flag that admitted an unsigned or an unscanned one.
func TestRunReceiptNamesTheConsentThatAdmittedARegistryRelease(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	signintest.Register()
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-probe": {},
		"-agent": {Stdout: signintest.Line(signintest.Event{Type: "text", Text: "done"}) + "\n"},
	}}
	newDockerClient = func() dockerx.Client { return f }
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	signed := fr.addAgent("acme", "signed", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("signed", ""))
	signed.attMode = attSigned
	unsigned := fr.addAgent("acme", "unsigned", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("unsigned", ""))
	unsigned.attMode = attUnsigned
	unscanned := fr.addAgent("acme", "unscanned", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("unscanned", ""))
	unscanned.unscanned = true
	for _, c := range []struct {
		ref, flag, state, consent string
		verified                  bool
	}{
		{"acme/signed@1.0.0", "", "verified", "", true},
		{"acme/unsigned@1.0.0", "--allow-unsigned", "unsigned", "--allow-unsigned", false},
		{"acme/unscanned@1.0.0", "--allow-unscanned", "unscanned", "--allow-unscanned", false},
	} {
		// Twice: the second run is from the cache, which holds the verdict
		// and never the consent.
		for _, round := range []string{"first run", "cached run"} {
			args := []string{"--registry", fr.url(), "run", c.ref, "--yes", "--adapter", signintest.TokenAdapter, "--policy", policyPath, "--output-format", "json", "--out", t.TempDir()}
			if c.flag != "" {
				args = append(args, c.flag)
			}
			stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
			wantClassified(t, err, engine.ExitBlocked)
			var env receipt.Envelope
			if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
				t.Fatalf("%s %s: %v\n%s\n%s", c.ref, round, jerr, stdout, stderr)
			}
			a := env.Attestation
			if env.Ref != c.ref || a.State != c.state || a.Consent != c.consent || a.Verified != c.verified || (c.verified && a.KeyID == "") || (!c.verified && a.KeyID != "") {
				t.Errorf("%s %s: ref %q attestation %+v, want %s consent %q verified %v", c.ref, round, env.Ref, a, c.state, c.consent, c.verified)
			}
		}
	}
}
