package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The connect providers render as docker-agent reads them: OpenRouter and
// Hugging Face through docker-agent's aliases, which supply the endpoint,
// the key under the variable each alias reads; Poe as a named provider with
// Chat Completions set explicitly, its endpoint and its key variable
// (token_key) on the provider. Each agent.yaml is held to a golden
// (testdata/providers; -update rewrites them).
func TestDockerAgentRendersTheConnectProviders(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		provider, model, policyVar, wantVar string
		allow                               string
	}{
		{policy.ProviderOpenRouter, "anthropic/claude-sonnet-4.5", "OPENROUTER_API_KEY", "OPENROUTER_API_KEY", "openrouter.ai:443"},
		{policy.ProviderPoe, "Claude-Sonnet-4.6", "POE_API_KEY", "POE_API_KEY", "api.poe.com:443"},
		{policy.ProviderHuggingFace, "openai/gpt-oss-120b:groq", "HF_TOKEN", "HF_TOKEN", "router.huggingface.co:443"},
		// A host variable of another name still reaches the alias's.
		{policy.ProviderHuggingFace, "openai/gpt-oss-120b:groq", "HUGGINGFACE_API_KEY", "HF_TOKEN", "router.huggingface.co:443"},
	}
	for _, c := range cases {
		in := testInput()
		in.Model = policy.Model{Provider: c.provider, Name: c.model, APIKeyEnv: c.policyVar, APIKeySource: modelkey.SourceKeyring, ProxyAllow: []string{c.allow}}
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
		var name, from, account string
		for _, e := range res.Agent.Env {
			if e.FromHostEnv != "" {
				name, from, account = e.Name, e.FromHostEnv, e.FromKeyring
			}
		}
		if name != c.wantVar || from != c.policyVar || account != modelkey.Account(c.provider) {
			t.Errorf("%s: key travels as %s from $%s, else %s; want %s from $%s, else %s", c.provider, name, from, account, c.wantVar, c.policyVar, modelkey.Account(c.provider))
		}
		if c.policyVar != c.wantVar {
			continue
		}
		golden(t, filepath.Join("testdata", "providers", c.provider+".yaml"), res.Files["agent.yaml"])
	}
}

// Poe's endpoint may be the policy's (a test's, a mirror's); the API stays
// Chat Completions.
func TestDockerAgentRendersPoesBaseURLFromThePolicy(t *testing.T) {
	ad, _ := Get(DefaultName)
	in := testInput()
	in.Model = policy.Model{Provider: policy.ProviderPoe, Name: "GPT-5.4", APIKeyEnv: "POE_API_KEY", BaseURL: "https://poe.example/v1", ProxyAllow: []string{"poe.example:443"}}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	y := string(res.Files["agent.yaml"])
	if !strings.Contains(y, "base_url: https://poe.example/v1") || !strings.Contains(y, "api_type: openai_chatcompletions") || strings.Count(y, "base_url") != 1 {
		t.Fatalf("agent.yaml:\n%s", y)
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // the test's own golden
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update to create it)", path, err)
	}
	if string(want) != string(got) {
		t.Errorf("golden %s differs (run with -update after checking the diff):\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// docker-agent's error event carries its message in "error"; a provider out
// of credits is its own event, with the provider's words.
func TestDockerAgentErrorEventsCarryTheProvidersWords(t *testing.T) {
	ad, _ := Get(DefaultName)
	cases := []struct {
		line string
		kind EventKind
		text string
	}{
		{`{"type":"error","error":"model failed: boom","code":"model_error"}`, EventError, "model failed: boom"},
		{`{"type":"error","error":"POST \"https://openrouter.ai/api/v1/chat/completions\": 402 Payment Required {\"error\":{\"message\":\"Insufficient credits\",\"code\":402}}","code":"model_error"}`, EventOutOfCredits, "402 Payment Required"},
		{`{"type":"error","error":"POST \"https://api.poe.com/v1/chat/completions\": 402 {\"error\":{\"code\":402,\"type\":\"insufficient_credits\",\"message\":\"balance is 0\"}}"}`, EventOutOfCredits, "insufficient_credits"},
		{`{"type":"error","error":"402 Client Error: You have exceeded your monthly included credits for Inference Providers."}`, EventOutOfCredits, "monthly included credits"},
		{`{"type":"error","error":"429 Too Many Requests"}`, EventError, "429"},
		// A tool's words never end a run as out of credits.
		{`{"type":"error","error":"the tool said: 402 Payment Required","code":"tool_failed"}`, EventError, "402"},
		{`{"type":"error","message":{"content":"boom"}}`, EventError, "boom"},
	}
	for _, c := range cases {
		evs := ad.Events([]byte(c.line))
		if len(evs) != 1 || evs[0].Kind != c.kind || !strings.Contains(evs[0].Text, c.text) {
			t.Errorf("%s: events %+v, want one %s with %q", c.line, evs, c.kind, c.text)
		}
	}
}

// An interactive session whose prompt fails for a provider out of credits
// reports it as that event before the error.
func TestACPSessionReportsAProviderOutOfCredits(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		sc := bufio.NewScanner(toHarnessR)
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fromHarnessW.Write(append(b, '\n'))
		}
		for sc.Scan() {
			var m rpcMessage
			_ = json.Unmarshal(sc.Bytes(), &m)
			switch m.Method {
			case "initialize":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
			case "session/new":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "s"}})
			case "session/prompt":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": `POST "https://api.poe.com/v1/chat/completions": 402 Payment Required`}})
			}
		}
	}()
	drv := newACPSession(pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, 1, nil)
	got := make(chan Event, 8)
	go func() {
		for ev := range drv.Events() {
			got <- ev
		}
		close(got)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Prompt(ctx, "hello"); err == nil {
		t.Fatal("the prompt succeeded")
	}
	_ = drv.Close()
	for ev := range got {
		if ev.Kind == EventOutOfCredits && strings.Contains(ev.Text, "402 Payment Required") {
			return
		}
	}
	t.Fatal("no out-of-credits event")
}

// Vultr renders as a named provider at Vultr's endpoint, on Chat
// Completions, with the key as a bearer in VULTR_INFERENCE_API_KEY; a base
// URL in the policy (a test's, a mirror's) takes the endpoint's place.
func TestDockerAgentRendersVultrAsANamedProvider(t *testing.T) {
	ad, _ := Get(DefaultName)
	for _, c := range []struct{ baseURL, want string }{
		{"", "https://api.vultrinference.com/v1"},
		{"https://vultr.example/v1", "https://vultr.example/v1"},
	} {
		in := testInput()
		in.Model = policy.Model{Provider: policy.ProviderVultr, Name: "qwen3.8-flash-next", APIKeyEnv: "VULTR_INFERENCE_API_KEY", APIKeySource: modelkey.SourceKeyring, BaseURL: c.baseURL, ProxyAllow: []string{"api.vultrinference.com:443"}}
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		y := string(res.Files["agent.yaml"])
		for _, s := range []string{"provider: vultr", "base_url: " + c.want, "api_type: openai_chatcompletions", "token_key: VULTR_INFERENCE_API_KEY"} {
			if !strings.Contains(y, s) {
				t.Errorf("agent.yaml lacks %q:\n%s", s, y)
			}
		}
		if strings.Count(y, "base_url") != 1 {
			t.Errorf("agent.yaml names base_url more than once:\n%s", y)
		}
		var name, account string
		for _, e := range res.Agent.Env {
			if e.FromHostEnv != "" {
				name, account = e.Name, e.FromKeyring
			}
		}
		if name != "VULTR_INFERENCE_API_KEY" || account != modelkey.Account(policy.ProviderVultr) {
			t.Errorf("key travels as %s, else keychain %s", name, account)
		}
		if c.baseURL == "" {
			golden(t, filepath.Join("testdata", "providers", "vultr.yaml"), res.Files["agent.yaml"])
		}
	}
}

// Provider agentry renders as a named OpenAI-compatible provider at the
// registry's model endpoint from the policy, with the run's credits
// session token as its key: an env var the host fills from the session,
// never a variable or a keychain entry of the person's.
func TestDockerAgentRendersAgentryCreditsAsANamedProvider(t *testing.T) {
	ad, _ := Get(DefaultName)
	in := testInput()
	in.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: "https://registry.example" + policy.AgentryInferencePath,
		ProxyAllow: []string{"registry.example:443"}}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	y := string(res.Files["agent.yaml"])
	for _, s := range []string{"provider: agentry", "base_url: https://registry.example/api/v1/x/inference/v1", "api_type: openai_chatcompletions", "token_key: " + AgentryInferenceTokenEnv} {
		if !strings.Contains(y, s) {
			t.Errorf("agent.yaml lacks %q:\n%s", s, y)
		}
	}
	var key *spec.EnvVar
	for i, e := range res.Agent.Env {
		if e.Name == AgentryInferenceTokenEnv {
			key = &res.Agent.Env[i]
		}
		if e.FromHostEnv != "" || e.FromKeyring != "" {
			t.Errorf("the agent's env reads %s from the host (%+v): a credits run reads no key of the person's", e.Name, e)
		}
	}
	if key == nil || !key.FromInference {
		t.Fatalf("the key var is %+v, want %s from the credits session", key, AgentryInferenceTokenEnv)
	}
	golden(t, filepath.Join("testdata", "providers", "agentry.yaml"), res.Files["agent.yaml"])

	in.Model.BaseURL = ""
	if _, err := ad.Compile(in); err == nil {
		t.Fatal("provider agentry with no endpoint compiled")
	}
}
