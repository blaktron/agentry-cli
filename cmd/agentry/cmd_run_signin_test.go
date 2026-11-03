package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// signInRunFixture: a runner home whose policy spends a ChatGPT plan
// through signintest's token adapter, a kept sign-in whose vendor issues
// probeSignInAccess for probeSignInRefresh, and a fake daemon scripted for
// a successful run.
func signInRunFixture(t *testing.T) (policyPath string, f *dockerx.Fake) {
	t.Helper()
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	signintest.Register()
	pol := policy.Default()
	signintest.Policy(pol, policy.KeyKindChatGPTPlan)
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	prevKeys := connectionKeys
	connectionKeys = func() connection.Keychain { return nil }
	t.Cleanup(func() { connectionKeys = prevKeys })
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.ChatGPT, &fakePlanVendor{}))
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.ChatGPT, modelsignin.Record{}, modelsignin.Grant{RefreshToken: probeSignInRefresh}); err != nil {
		t.Fatal(err)
	}
	f = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-probe": {},
		"-agent": {Stdout: signintest.Line(signintest.Event{Type: "text", Text: "done"}) + "\n" +
			signintest.Line(signintest.Event{Type: "usage", In: 10, Out: 5, Cost: 1.5}) + "\n"},
	}}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	return policyPath, f
}

// A hosted run's markers refuse a sign-in plan through `agentry run`
// itself: exit 11, before a harness image is pulled and before any
// container starts.
func TestAgentryRunRefusesASignInForAHostedRun(t *testing.T) {
	policyPath, f := signInRunFixture(t)
	conns := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(conns, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath, "--connections-file", conns)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
		t.Fatalf("%v", err)
	}
	// A run grant alone marks it, and so does the session protocol.
	t.Setenv(envRunGrant, "agyr_grant")
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
		t.Fatalf("with a run grant: %v", err)
	}
	t.Setenv(envRunGrant, "")
	withProtocolEvents(t, io.Discard, nil)
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/hello-chat"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath, "--mode", "interactive", "--session-protocol")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
		t.Fatalf("with the session protocol: %v", err)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Pull:") || strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "RunDetached") {
			t.Fatalf("a hosted sign-in run reached the daemon: %v", f.Ops())
		}
	}
}

// `agentry run --max-cost` is refused for a plan run before anything is
// installed.
func TestAgentryRunRefusesACostBudgetForAPlanRun(t *testing.T) {
	policyPath, f := signInRunFixture(t)
	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath, "--max-cost", "2")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "--max-cost cannot apply: "+policy.MaxCostSignInSentence) {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(strings.Join(f.Ops(), " "), "Pull:") {
		t.Fatalf("pulled before refusing: %v", f.Ops())
	}
	// A cap of zero is no cap: the run goes on to the end the fake daemon
	// gives it, and the root --json asks it for its envelope.
	stdout, _, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath, "--max-cost", "0", "--out", t.TempDir())
	wantClassified(t, err, engine.ExitBlocked)
	var env receipt.Envelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil || env.ExitClass != "blocked" {
		t.Errorf("--json run: stdout %q (%v)", stdout, jerr)
	}
}

// On this computer `agentry run` hands the run its sign-in's access token
// alone: a probe search of the runner home's run folders, the output
// folder and every docker call finds no refresh token, and no access
// token outside the run's own token file, which dies with the run.
func TestAgentryRunHandsItsSignInsAccessTokenAlone(t *testing.T) {
	policyPath, f := signInRunFixture(t)
	home, _ := runnerHome()
	var tokenFile string
	f.Foreground["-agent"].Before = func(r dockerx.RunSpec) {
		for _, m := range r.Mounts {
			if strings.HasSuffix(m.Source, "model-signin") {
				b, _ := os.ReadFile(filepath.Join(m.Source, "token")) //nolint:gosec // the test's run dir
				tokenFile = string(b)
			}
		}
	}
	out := t.TempDir()
	// The fake daemon runs no tools container, so the release's one-shot
	// run ends blocked (exit 18) after the agent ran: what is searched is
	// the run the agent had.
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--adapter", signintest.TokenAdapter, "--policy", policyPath, "--out", out, "--output-format", "json")
	wantClassified(t, err, engine.ExitBlocked)
	if tokenFile != probeSignInAccess {
		t.Fatalf("the agent's token file held %q", tokenFile)
	}
	if !strings.Contains(stdout, `"credentialKind": "chatgpt-plan"`) || !strings.Contains(stdout, `"costUsd": 0`) {
		t.Fatalf("envelope:\n%s", stdout)
	}
	// The envelope is the only document on stdout, a failure's own envelope
	// does not follow it; the live usage is narrated on stderr.
	dec := json.NewDecoder(strings.NewReader(stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || dec.More() {
		t.Errorf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "usage: input=10 output=5 total=15 turns=0\n") {
		t.Errorf("stderr does not narrate the usage:\n%s", stderr)
	}
	blob := stdout + stderr
	for _, c := range f.Calls {
		blob += c.Arg + " " + strings.Join(c.Run.Argv, " ")
		for k, v := range c.Run.Env {
			blob += " " + k + "=" + v
		}
	}
	if strings.Contains(blob, probeSignInRefresh) || strings.Contains(blob, probeSignInAccess) {
		t.Fatal("the output or a docker call carries a token")
	}
	for _, dir := range []string{out, filepath.Join(home, "installs"), filepath.Join(home, "runs")} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's dirs
			if strings.Contains(string(b), probeSignInRefresh) || strings.Contains(string(b), probeSignInAccess) {
				t.Errorf("%s holds a token", path)
			}
			return nil
		})
	}
}

// A hosted run's markers refuse a release that declares a target host
// through `agentry run` itself: exit 11, host-refused, before any image
// is pulled or container started.
func TestAgentryRunRefusesATargetHostForAHostedRun(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	if err := policy.Save(policyPath, policy.Default()); err != nil {
		t.Fatal(err)
	}
	f := &dockerx.Fake{}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	conns := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(conns, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/target-host"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--connections-file", conns, "--intake", "server=admin@203.0.113.4")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never reaches a target host") {
		t.Fatalf("%v", err)
	}
	// A hosted run's job token alone marks it too, and so does the host's
	// own marker.
	t.Setenv(envHosted, "1")
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/target-host"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--intake", "server=admin@203.0.113.4")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never reaches a target host") {
		t.Fatalf("with AGENTRY_HOSTED: %v", err)
	}
	t.Setenv(envHosted, "")
	t.Setenv(envJobToken, "job-token")
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/target-host"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--intake", "server=admin@203.0.113.4")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never reaches a target host") {
		t.Fatalf("with the job token: %v", err)
	}
	// A run grant alone marks it too.
	t.Setenv(envJobToken, "")
	t.Setenv(envRunGrant, "agyr_grant")
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/target-host"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--intake", "server=admin@203.0.113.4")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "a hosted run never reaches a target host") {
		t.Fatalf("with a run grant: %v", err)
	}
	t.Setenv(envRunGrant, "")
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Pull:") || strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "Build") {
			t.Fatalf("a hosted target-host run reached the daemon: %v", f.Ops())
		}
	}
	// A run on this computer is not refused: it gets as far as installing
	// what it needs.
	seedHostLinkImage(t, f, policyPath)
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/target-host"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--prepare")
	if err != nil || !strings.HasPrefix(stdout, "prepared fixtures/target-host@1.0.0 ") {
		t.Fatalf("a run on this computer: %q %v\n%s", stdout, err, stderr)
	}
}

// --events appends the harness's event stream to the named file as the run
// goes: what was there is kept, and a file the run makes is private to its
// owner.
func TestAgentryRunAppendsTheLiveEventsToTheNamedFile(t *testing.T) {
	policyPath, _ := signInRunFixture(t)
	pure := repoPath(t, "testdata/runner/pure-text")
	run := func(events string) {
		t.Helper()
		_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--adapter", signintest.TokenAdapter, "--policy", policyPath, "--events", events, "--out", t.TempDir())
		wantClassified(t, err, engine.ExitBlocked)
	}
	existing := filepath.Join(t.TempDir(), "events.ndjson")
	if err := os.WriteFile(existing, []byte("{\"earlier\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(existing)
	got, err := os.ReadFile(existing)
	if err != nil || !strings.HasPrefix(string(got), "{\"earlier\":true}\n") || !strings.Contains(string(got), `"text":"done"`) || !strings.Contains(string(got), `"type":"usage"`) {
		t.Errorf("the events file = %q (%v)", got, err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh.ndjson")
	run(fresh)
	if st, err := os.Stat(fresh); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("the events file the run made is %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(fresh); !strings.Contains(string(b), `"text":"done"`) {
		t.Errorf("the new events file = %q", b)
	}
}

// A profile that says not to keep it goes with the run, whichever way the
// run ends.
func TestAgentryRunRemovesAProfileThatIsNotKept(t *testing.T) {
	policyPath, _ := signInRunFixture(t)
	home, _ := runnerHome()
	pure := repoPath(t, "testdata/runner/pure-text")
	local, err := localrun.LocalMeta(pure)
	if err != nil {
		t.Fatal(err)
	}
	dir := profile.Dir(home, local.Namespace, local.Name, local.Version, installdir.DigestSuffix(local.Digest))
	if err := profile.New(local.Ref, local.Digest, true, nil).Save(dir); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--adapter", signintest.TokenAdapter, "--policy", policyPath, "--keep-profile", "false", "--out", t.TempDir())
	wantClassified(t, err, engine.ExitBlocked)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) || !strings.Contains(stderr, "MCP profile removed (run completed with keep-profile=false)\n") {
		t.Errorf("the profile after a run that was to drop it: %v\n%s", statErr, stderr)
	}
}
