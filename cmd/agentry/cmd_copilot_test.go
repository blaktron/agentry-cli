package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// copilotFixture points the CLI's GitHub sign-in and the store's Copilot
// vendor at a fake GitHub, with the browser opening nothing.
func copilotFixture(t *testing.T) *mcpauthtest.GitHub {
	t.Helper()
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	t.Setenv("AGENTRY_NO_BROWSER", "1")
	t.Setenv("AGENTRY_GITHUB_APP_CLIENT_ID", mcpauthtest.GitHubClientID)
	t.Setenv("AGENTRY_GITHUB_OAUTH_APP_CLIENT_ID", mcpauthtest.GitHubClientID)
	v := mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	prevHTTP, prevKeys, prevEnd := vendorHTTP, connectionKeys, githubEndpoints
	vendorHTTP = func() *http.Client { return flow.HTTP }
	connectionKeys = func() connection.Keychain { return nil }
	githubEndpoints = func(g *mcpauth.GitHub) {
		g.DeviceCodeURL, g.TokenURL, g.DevicePage, g.API, g.Sleep = flow.DeviceCodeURL, flow.TokenURL, flow.DevicePage, flow.API, flow.Sleep
	}
	t.Cleanup(func() { vendorHTTP, connectionKeys, githubEndpoints = prevHTTP, prevKeys, prevEnd })
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.Copilot, modelsignin.CopilotVendor{Flow: flow}))
	return v
}

// `model-signins add copilot` shows the code and GitHub's page, keeps the
// sign-in, prints no token; `remove copilot` revokes the grant at GitHub.
func TestModelSignInsAddCopilotThenRemoveRevokesIt(t *testing.T) {
	v := copilotFixture(t)
	out, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "copilot", "--json")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	var got signedInJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Vendor != "copilot" || got.Kind != policy.KeyKindCopilotSignIn || got.Account != "octo-fixture" {
		t.Fatalf("add = %s (%v)", out, err)
	}
	if !strings.Contains(errOut, "enter the code WDJB-MJHT at") || !strings.Contains(errOut, "/login/device") {
		t.Errorf("the code was not shown:\n%s", errOut)
	}
	if strings.Contains(out+errOut, "PROBE") {
		t.Fatal("a token was printed")
	}
	if v.DeviceRequests[0].Has("scope") {
		t.Fatalf("the GitHub App's sign-in asked a scope: %v", v.DeviceRequests[0])
	}
	out, _, err = runCLI(t, nil, "model-signins", "remove", "copilot")
	if err != nil || !strings.Contains(out, "revocation: "+modelsignin.RevokeDone) || !strings.Contains(out, "stays in your authorized apps") {
		t.Fatalf("remove = %q (%v)", out, err)
	}
	if len(v.Revoked) != 3 {
		t.Fatalf("revoked %v", v.Revoked)
	}
	for _, c := range v.Revoked {
		if v.Live(c) || v.LiveAccess(c) {
			t.Fatalf("%s is still live at GitHub", c)
		}
	}
}

// --oauth-app signs in through Agentry's OAuth App, with its scope, and
// the list says which account; --oauth-app is for copilot alone.
func TestModelSignInsAddCopilotThroughTheOAuthApp(t *testing.T) {
	v := copilotFixture(t)
	if _, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "copilot", "--oauth-app"); err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	if v.DeviceRequests[0].Get("scope") != mcpauth.GitHubOAuthScope {
		t.Fatalf("scope %v", v.DeviceRequests[0])
	}
	out, _, err := runCLI(t, nil, "model-signins", "list")
	if err != nil || !strings.Contains(out, "copilot as octo-fixture (through Agentry's GitHub OAuth App)") {
		t.Fatalf("list = %q (%v)", out, err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "chatgpt", "--oauth-app"); err == nil {
		t.Fatal("--oauth-app beside chatgpt")
	}
	if slices.ContainsFunc(v.TokenRequests, func(f url.Values) bool { return f.Has("client_secret") }) {
		t.Fatal("a secret was sent")
	}
}

// On this computer `agentry run` on a Copilot plan runs the Copilot image,
// the default for the kind, and hands Copilot CLI its sign-in's access
// token through the session callback alone (I3, I4): the agent container
// mounts nothing and carries no token in its environment, and a probe
// search of the output, the run folders and every docker call finds
// neither the refresh token nor the access token.
func TestAgentryRunHandsCopilotItsAccessTokenThroughTheCallbackAlone(t *testing.T) {
	isolateEnv(t)
	v := copilotFixture(t)
	home, policyPath := isolateRunnerHome(t)
	pol := policy.Default()
	pol.Model = policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindCopilotSignIn)}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	_, refresh := v.Issue(mcpauthtest.GitHubClientID)
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.Copilot, modelsignin.Record{ClientID: mcpauthtest.GitHubClientID, App: mcpauth.GitHubApp},
		modelsignin.Grant{RefreshToken: refresh}); err != nil {
		t.Fatal(err)
	}
	img, _ := pol.Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	srv := &copilottest.Server{}
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{img.Ref: true},
		Labels:         map[string]map[string]string{img.Ref: {harnessbuild.LabelArtifact: img.Artifact}},
		Foreground: map[string]*dockerx.FakeForeground{
			// A session's driver is told the tools the gateway serves.
			"-probe": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
				if slices.Contains(r.Argv, "--list-tools") {
					_, _ = io.WriteString(stdout, `{"tools":["read_input"]}`+"\n")
				}
				return 0, nil
			}},
			"-agent": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
				return 0, srv.Serve(stdin, stdout)
			}},
		},
	}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	out := t.TempDir()
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--out", out, "--output-format", "json")
	// The fake daemon runs no tools container, so the release's one-shot
	// run ends blocked (exit 18) after the agent ran.
	wantClassified(t, err, engine.ExitBlocked)
	_, _, toks, _, _ := srv.Snapshot()
	if len(toks) != 1 || !strings.HasPrefix(toks[0]["accessToken"].(string), "ghu_PROBEACCESS") {
		t.Fatalf("tokens handed %v\n%s", toks, stderr)
	}
	handed := toks[0]["accessToken"].(string)
	if !strings.Contains(stdout, `"credentialKind": "copilot-signin"`) || !strings.Contains(stdout, `"adapter": "copilot-image"`) ||
		!strings.Contains(stdout, "pinned by checksum, not signed") {
		t.Fatalf("envelope:\n%s", stdout)
	}
	blob := stdout + stderr
	for _, c := range f.Calls {
		blob += c.Arg + " " + strings.Join(c.Run.Argv, " ")
		for k, v := range c.Run.Env {
			blob += " " + k + "=" + v
		}
		if strings.HasSuffix(c.Run.Name, "-agent") && len(c.Run.Mounts) != 0 {
			t.Fatalf("the agent container mounts %+v", c.Run.Mounts)
		}
	}
	if strings.Contains(blob, "ghr_PROBE") || strings.Contains(blob, handed) {
		t.Fatal("the output or a docker call carries a token")
	}
	for _, dir := range []string{out, filepath.Join(home, "installs"), filepath.Join(home, "runs")} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's dirs
			if strings.Contains(string(b), "ghr_PROBE") || strings.Contains(string(b), handed) {
				t.Errorf("%s holds a token", path)
			}
			return nil
		})
	}
}
