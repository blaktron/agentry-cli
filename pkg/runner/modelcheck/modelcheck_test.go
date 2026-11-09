package modelcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// endpoint is an OpenAI-compatible server of the person's own that lists
// models and records what it was asked with.
func endpoint(t *testing.T, models ...string) (*httptest.Server, *[]http.Header) {
	t.Helper()
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := []map[string]any{}
		for _, m := range models {
			data = append(data, map[string]any{"id": m, "max_model_len": 32768})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func custom(url, model string) policy.Model {
	return policy.Model{Provider: policy.ProviderCustom, ProviderID: "custom-lab", Name: model, APIKeyEnv: "LAB_KEY", BaseURL: url + "/v1", ProxyAllow: []string{"lab:1"}}
}

func refusal(t *testing.T, err error, kind string) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Kind != kind {
		t.Fatalf("error %v, want a %s refusal", err, kind)
	}
	return r
}

func TestAnEndpointOfYourOwnIsCheckedByItsListingAndFillsTheContextWindow(t *testing.T) {
	srv, _ := endpoint(t, "qwen3", "llama4")
	c := Checkers{ProbeContextWindow: setup.ProbeContextWindow}
	res, err := Check(context.Background(), c, custom(srv.URL, "qwen3"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ContextWindow != 32768 || res.Source == "" {
		t.Fatalf("result = %+v: want the window the endpoint lists", res)
	}
	_, err = Check(context.Background(), c, custom(srv.URL, "not-a-model"))
	r := refusal(t, err, NotOffered)
	if !strings.Contains(r.Reason, `does not offer "not-a-model"`) || !strings.Contains(r.Reason, "qwen3, llama4") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestAnUnreachableEndpointRefusesTheSaveWithTheReason(t *testing.T) {
	srv, _ := endpoint(t, "qwen3")
	url := srv.URL
	srv.Close()
	_, err := Check(context.Background(), Checkers{}, custom(url, "qwen3"))
	r := refusal(t, err, CannotCheck)
	if !strings.Contains(r.Reason, "could not check the model") || !strings.Contains(r.Reason, "list models") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

// The save check keeps the listing's rules: no redirect is followed, so
// the key never reaches the host a redirect names, and the key is scrubbed
// from every error.
func TestTheSaveCheckFollowsNoRedirectAndKeepsTheKeyOutOfItsErrors(t *testing.T) {
	const key = "sk-lab-secret-value-0123"
	elsewhere, seen := endpoint(t, "stolen")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", elsewhere.URL+"/v1/models?echo="+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusFound)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"moved, your key %s"}}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	c := Checkers{Key: func(policy.Model) (string, error) { return key, nil }}
	_, err := Check(context.Background(), c, custom(srv.URL, "stolen"))
	r := refusal(t, err, CannotCheck)
	if !strings.Contains(r.Reason, "302") || strings.Contains(r.Reason, key) {
		t.Fatalf("reason = %q", r.Reason)
	}
	if len(*seen) != 0 {
		t.Fatal("the redirect's target was asked")
	}
}

// Plain http off this computer is said, never refused.
func TestPlainHTTPIsWarnedAndNeverBlocked(t *testing.T) {
	m := custom("http://gpu-box.lan:8000", "qwen3")
	warning := setup.PlainHTTPWarning(m.BaseURL)
	c := Checkers{ListModels: func(_ context.Context, provider, base, _ string) (*setup.Catalog, error) {
		if provider != policy.ProviderCustom || base != m.BaseURL {
			t.Errorf("asked %s at %s", provider, base)
		}
		return &setup.Catalog{Models: []setup.Model{{ID: "qwen3"}}, Warning: warning}, nil
	}}
	res, err := Check(context.Background(), c, m)
	if err != nil || warning == "" || res.Warning != warning {
		t.Fatalf("result %+v, %v", res, err)
	}
}

func TestAKeyProviderWithNoKeyIsKeyMissing(t *testing.T) {
	m := policy.Model{Provider: "openai", ProviderID: "openai", Name: "gpt-5.5", APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring", ProxyAllow: []string{"api.openai.com:443"}}
	_, err := Check(context.Background(), Checkers{Key: func(policy.Model) (string, error) { return "", nil }}, m)
	r := refusal(t, err, KeyMissing)
	if !strings.Contains(r.Reason, "$OPENAI_API_KEY") || !strings.Contains(r.Reason, "provider:openai") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestATypedCataloguesIDMayCarryARoutingSuffix(t *testing.T) {
	list := func(context.Context, string, string, string) (*setup.Catalog, error) {
		return &setup.Catalog{Models: []setup.Model{{ID: "vendor/model"}}}, nil
	}
	c := Checkers{Key: func(policy.Model) (string, error) { return "k", nil }, ListModels: list}
	m := policy.Model{Provider: policy.ProviderOpenRouter, Name: "vendor/model:nitro", APIKeyEnv: "OPENROUTER_API_KEY", ProxyAllow: []string{"openrouter.ai:443"}}
	if _, err := Check(context.Background(), c, m); err != nil {
		t.Fatal(err)
	}
	m.Provider = "openai"
	if _, err := Check(context.Background(), c, m); err == nil {
		t.Fatal("a suffix was taken from a provider that lists every id it serves")
	}
}

func TestAChatGPTPlanIsCheckedByItsOwnModelList(t *testing.T) {
	m := policy.Model{Provider: "openai", Name: "gpt-plan", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindChatGPTPlan)}
	listed := func(context.Context) ([]string, error) { return []string{"gpt-plan", "gpt-plan-mini"}, nil }
	if _, err := Check(context.Background(), Checkers{ChatGPTModels: listed}, m); err != nil {
		t.Fatal(err)
	}
	m.Name = "gpt-other"
	r := refusal(t, func() error { _, err := Check(context.Background(), Checkers{ChatGPTModels: listed}, m); return err }(), NotOffered)
	if !strings.Contains(r.Reason, "gpt-plan, gpt-plan-mini") {
		t.Fatalf("reason = %q", r.Reason)
	}
	for err, kind := range map[error]string{
		&mcpauth.ChatGPTNotEligibleError{Detail: "not on a plan"}: NotEligible,
		fmt.Errorf("get: %w", modelsignin.ErrNone):                NeedsSignIn,
		errors.New("dial tcp: no route to host"):                  CannotCheck,
	} {
		_, got := Check(context.Background(), Checkers{ChatGPTModels: func(context.Context) ([]string, error) { return nil, err }}, m)
		refusal(t, got, kind)
	}
}

func TestACopilotPlanTakesAutoUnlistedAndChecksTheRest(t *testing.T) {
	allow, _ := policy.CopilotProxyAllow(policy.CopilotPlanBusiness)
	m := policy.Model{Provider: policy.ProviderCopilot, Name: CopilotAuto, APIKeyKind: policy.KeyKindCopilotSignIn, ProxyAllow: allow}
	asked := ""
	c := Checkers{CopilotModels: func(_ context.Context, plan string) ([]string, error) {
		asked = plan
		return []string{"claude-sonnet-5.5", "gpt-5.5"}, nil
	}}
	if _, err := Check(context.Background(), c, m); err != nil || asked != "" {
		t.Fatalf("auto: %v (asked %q)", err, asked)
	}
	m.Name = "gpt-5.5"
	if _, err := Check(context.Background(), c, m); err != nil || asked != policy.CopilotPlanBusiness {
		t.Fatalf("listed: %v (asked %q)", err, asked)
	}
	m.Name = "o-disabled"
	_, err := Check(context.Background(), c, m)
	refusal(t, err, NotOffered)
}

func TestAClaudeSubscriptionIsCheckedByOneMessage(t *testing.T) {
	m := policy.Model{Provider: "anthropic", Name: "sonnet", APIKeyKind: policy.KeyKindClaudeSignIn, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	answer := func(a ClaudeAnswer) Checkers {
		return Checkers{Claude: func(_ context.Context, model string) (ClaudeAnswer, error) {
			if model != "sonnet" {
				t.Errorf("asked %q", model)
			}
			return a, nil
		}}
	}
	res, err := Check(context.Background(), answer(ClaudeAnswer{OK: true, Resolved: "claude-sonnet-5-5"}), m)
	if err != nil || res.Resolved != "claude-sonnet-5-5" {
		t.Fatalf("%+v, %v", res, err)
	}
	const sentence = "There's an issue with the selected model (sonnet). It may not exist or you may not have access to it."
	_, err = Check(context.Background(), answer(ClaudeAnswer{Status: 404, Message: sentence}), m)
	if r := refusal(t, err, NotOffered); r.Reason != sentence {
		t.Fatalf("a 404 refuses with %q, not Claude Code's own sentence", r.Reason)
	}
	_, err = Check(context.Background(), answer(ClaudeAnswer{NotSignedIn: true}), m)
	refusal(t, err, NeedsSignIn)
	_, err = Check(context.Background(), answer(ClaudeAnswer{Status: 429, Message: "rate limited"}), m)
	if r := refusal(t, err, CannotCheck); !strings.Contains(r.Reason, "rate limited") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestClaudeCodesResultIsReadFromIsErrorNeverSubtype(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		want ClaudeAnswer
	}{
		"an answer": {`{"type":"result","subtype":"success","is_error":false,"result":"OK","modelUsage":{"claude-sonnet-5-5":{"inputTokens":352}}}`,
			ClaudeAnswer{OK: true, Resolved: "claude-sonnet-5-5"}},
		"a 404 that says success": {`{"type":"result","subtype":"success","is_error":true,"api_error_status":404,"terminal_reason":"api_error","result":"There's an issue with the selected model (x)."}`,
			ClaudeAnswer{Status: 404, Message: "There's an issue with the selected model (x)."}},
		"not signed in": {`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`,
			ClaudeAnswer{NotSignedIn: true, Message: "Not logged in · Please run /login"}},
		"after other lines": {"[claude-code:unrecognized_model] x\n" + `{"is_error":false,"modelUsage":{"claude-opus-5-5":{}}}`,
			ClaudeAnswer{OK: true, Resolved: "claude-opus-5-5"}},
	} {
		got, err := ParseClaudeResult([]byte(c.out))
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, c.want)
		}
	}
	if _, err := ParseClaudeResult([]byte("[claude-code:unrecognized_model] sonnet\n")); err == nil {
		t.Error("the stderr's warning was read as an answer")
	}
}

func TestClaudeArgsAreOneTurnWithNoToolsAndAShortSystemPrompt(t *testing.T) {
	got := strings.Join(ClaudeArgs("opus"), " ")
	for _, want := range []string{"-p --output-format json", "--max-turns 1", "--tools ", "--strict-mcp-config", "--no-session-persistence", "--system-prompt Reply with the single word OK.", "--model opus"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
}

func TestAgentryCreditsAreCheckedByOpeningASession(t *testing.T) {
	m := policy.Model{Provider: policy.ProviderAgentry, Name: setup.AgentryModelName, BaseURL: "https://registry.example" + policy.AgentryInferencePath, ProxyAllow: []string{"registry.example:443"}}
	opened := 0
	c := Checkers{Credits: func(context.Context, policy.Model) error { opened++; return nil }}
	if _, err := Check(context.Background(), c, m); err != nil || opened != 1 {
		t.Fatalf("%v, opened %d", err, opened)
	}
	c.Credits = func(context.Context, policy.Model) error { return errors.New("sign in first (agentry login)") }
	_, err := Check(context.Background(), c, m)
	if r := refusal(t, err, CannotCheck); !strings.Contains(r.Reason, "agentry login") {
		t.Fatalf("reason = %q", r.Reason)
	}
	m.Name = "gpt-5.5"
	_, err = Check(context.Background(), c, m)
	refusal(t, err, NotOffered)
}
