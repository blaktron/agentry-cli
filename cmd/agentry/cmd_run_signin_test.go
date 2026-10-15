package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
	if strings.Contains(strings.Join(f.Ops(), " "), "Pull:") {
		t.Fatalf("pulled before refusing: %v", f.Ops())
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
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Pull:") || strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "Build") {
			t.Fatalf("a hosted target-host run reached the daemon: %v", f.Ops())
		}
	}
}
