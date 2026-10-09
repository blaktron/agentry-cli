package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
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
