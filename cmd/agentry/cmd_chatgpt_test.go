package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// chatgptFixture points the CLI's ChatGPT sign-in, refresh and revocation
// at a fake OpenAI, with a browser that approves.
func chatgptFixture(t *testing.T) *mcpauthtest.ChatGPT {
	t.Helper()
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	t.Setenv("AGENTRY_NO_BROWSER", "")
	v := mcpauthtest.NewChatGPT(t)
	flow := v.Flow()
	prevHTTP, prevKeys, prevOpen, prevEnd := vendorHTTP, connectionKeys, openBrowser, chatgptEndpoints
	vendorHTTP = func() *http.Client { return flow.HTTP }
	connectionKeys = func() connection.Keychain { return nil }
	openBrowser = func(u string) error { flow.Present(u); return nil }
	chatgptEndpoints = func(g *mcpauth.ChatGPT) {
		g.Issuer, g.AuthorizeURL, g.TokenURL, g.DiscoveryURL, g.APIBase = flow.Issuer, flow.AuthorizeURL, flow.TokenURL, flow.DiscoveryURL, flow.APIBase
	}
	t.Cleanup(func() {
		vendorHTTP, connectionKeys, openBrowser, chatgptEndpoints = prevHTTP, prevKeys, prevOpen, prevEnd
	})
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.ChatGPT, modelsignin.ChatGPTVendor{Flow: flow}))
	return v
}

func TestModelSignInsAddChatGPTThenRemoveRevokesIt(t *testing.T) {
	v := chatgptFixture(t)
	out, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "chatgpt", "--json")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, errOut)
	}
	var got signedInJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Vendor != "chatgpt" || got.Kind != policy.KeyKindChatGPTPlan ||
		got.Account != "person@example.com" || strings.Join(got.Models, ",") != "gpt-fixture,gpt-fixture-mini" {
		t.Fatalf("add = %s (%v)", out, err)
	}
	if !strings.Contains(errOut, "Sign in with ChatGPT in your browser:") {
		t.Errorf("the address was not shown:\n%s", errOut)
	}
	if strings.Contains(out+errOut, "PROBE-") {
		t.Fatal("a token was printed")
	}
	live := v.Issued[len(v.Issued)-1]
	out, _, err = runCLI(t, nil, "model-signins", "remove", "chatgpt")
	if err != nil || !strings.Contains(out, "revocation: "+modelsignin.RevokeDone) {
		t.Fatalf("remove = %q (%v)", out, err)
	}
	if v.Live(live) {
		t.Fatal("remove did not revoke the grant at OpenAI")
	}
}

func TestModelSignInsAddChatGPTKeepsNothingForAnIneligibleAccount(t *testing.T) {
	v := chatgptFixture(t)
	v.NotEligible = true
	_, _, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "chatgpt")
	if err == nil || !strings.Contains(err.Error(), "nothing kept") || !strings.Contains(err.Error(), "OpenAI API key") {
		t.Fatalf("err %v", err)
	}
	if out, _, _ := runCLI(t, nil, "model-signins", "list"); !strings.Contains(out, "no model sign-ins kept") {
		t.Fatalf("list after an ineligible sign-in: %s", out)
	}
}

func TestSetupWithAChatGPTPlanWritesThePolicy(t *testing.T) {
	chatgptFixture(t)
	policyPath := t.TempDir() + "/policy.yaml"
	_, errOut, err := runCLI(t, strings.NewReader(""), "setup", "--provider", "openai", "--api-key-kind", "chatgpt-plan", "--yes", "--skip-images", "--policy", policyPath)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, errOut)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != "openai" || m.Name != "gpt-fixture" || m.APIKeyKind != policy.KeyKindChatGPTPlan || m.APIKeyEnv != "" ||
		strings.Join(m.ProxyAllow, ",") != "api.openai.com:443" {
		t.Fatalf("policy model %+v", m)
	}
	if _, err := modelsignin.Open(mustRunnerHome(t), nil).Get(modelsignin.ChatGPT); err != nil {
		t.Fatalf("setup kept no sign-in: %v", err)
	}
}

func mustRunnerHome(t *testing.T) string {
	t.Helper()
	h, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// appServerOneTurn answers codex's app-server handshake and one turn, as
// the Codex image's codex would on a plan.
func appServerOneTurn(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	sc := bufio.NewScanner(stdin)
	say := func(v any) { b, _ := json.Marshal(v); _, _ = stdout.Write(append(b, '\n')) }
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			say(map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "codex"}})
		case "thread/start":
			say(map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "t1"}}})
		case "turn/start":
			say(map[string]any{"id": m.ID, "result": map[string]any{"turn": map[string]any{"id": "u1", "status": "inProgress"}}})
			say(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "t1", "turnId": "u1", "item": map[string]any{"type": "agentMessage", "id": "m1", "text": "done"}}})
			say(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "u1", "status": "completed"}}})
		}
	}
	return 0, nil
}

// A whole `agentry run` on a ChatGPT plan, signed in through OpenAI's flow
// (a fake): the run is the Codex image's, and nothing of the grant but an
// access token reaches it (I3) — a probe search of the run folders, the
// output and every docker call finds no refresh token, no ID token and not
// the client OpenAI issued, and the access token only in the run's own
// token file, which dies with the run.
func TestAChatGPTPlanRunIsHandedItsAccessTokenAlone(t *testing.T) {
	v := chatgptFixture(t)
	home := mustRunnerHome(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if _, errOut, err := runCLI(t, strings.NewReader(""), "setup", "--provider", "openai", "--api-key-kind", "chatgpt-plan", "--yes", "--skip-images", "--policy", policyPath); err != nil {
		t.Fatalf("setup: %v\n%s", err, errOut)
	}
	var tokenFile, image string
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		// The gateway's tool listing, which a one-turn run reads before the
		// harness starts (the tools its driver may approve).
		"-probe": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
			if slices.Contains(r.Argv, "--list-tools") {
				_, _ = io.WriteString(stdout, `{"tools":[]}`+"\n")
			}
			return 0, nil
		}},
		"-agent": {Handler: appServerOneTurn, Before: func(r dockerx.RunSpec) {
			image = r.Image
			for _, m := range r.Mounts {
				if strings.HasSuffix(m.Source, "model-signin") {
					b, _ := os.ReadFile(filepath.Join(m.Source, "token")) //nolint:gosec // the test's run dir
					tokenFile = string(b)
				}
			}
		}},
	}}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	out := t.TempDir()
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes",
		"--policy", policyPath, "--out", out, "--output-format", "json")
	// The fake daemon runs no tools container, so the one-shot run ends
	// blocked (exit 18) after the agent ran, as the key run's test does.
	wantClassified(t, err, engine.ExitBlocked)
	def, _ := policy.Default().Images.HarnessImageFor("codex-image")
	if image != def.Ref || !strings.HasPrefix(tokenFile, v.AccessPrefix) {
		t.Fatalf("image %q, token file %q", image, tokenFile)
	}
	if !strings.Contains(stdout, `"credentialKind": "chatgpt-plan"`) || !strings.Contains(stdout, `"adapter": "codex-image"`) {
		t.Fatalf("envelope:\n%s", stdout)
	}
	probes := []string{v.RefreshPrefix, "eyJhbGciOiJSUzI1NiJ9.", v.IssuedClient}
	blob := stdout + stderr
	for _, c := range f.Calls {
		blob += c.Arg + " " + c.Run.Entrypoint + " " + strings.Join(c.Run.Argv, " ")
		for k, val := range c.Run.Env {
			blob += " " + k + "=" + val
		}
	}
	for _, p := range append(probes, tokenFile) {
		if strings.Contains(blob, p) {
			t.Fatalf("the output or a docker call carries %q", p)
		}
	}
	for _, dir := range []string{out, filepath.Join(home, "installs"), filepath.Join(home, "runs")} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's dirs
			for _, p := range append(probes, tokenFile) {
				if strings.Contains(string(b), p) {
					t.Errorf("%s holds %q", path, p)
				}
			}
			return nil
		})
	}
}
