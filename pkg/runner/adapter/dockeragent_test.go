// docker-agent adapter tests: the compiled agent.yaml must carry the
// gateway as its ONLY toolset (no built-ins, ever), the token must ride as
// ${env.…} (never a value), and the event mapper must be tolerant of shapes
// it does not know.
package adapter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func testInput() Input {
	return Input{
		Instruction:  "Do the thing.\n",
		Summary:      "a test agent",
		Model:        policy.Default().Model,
		HarnessImage: policy.DefaultHarnessImage,
		GatewayURL:   spec.GatewayURL(),
	}
}

func TestDockerAgentCompile(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ad.Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	y := string(res.Files["agent.yaml"])

	for _, want := range []string{
		"type: mcp",
		"url: http://gateway:8811/mcp",
		"transport_type: streamable",
		"Bearer ${env.MCP_GATEWAY_TOKEN}",
		"allow_private_ips: true",
		"version: \"" + HarnessConfigVersion + "\"", // schema of the pinned harness
		"safety: autonomous",                        // serve acp has no flag; the config carries the mode
		"Do the thing.",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("agent.yaml missing %q:\n%s", want, y)
		}
	}
	// No built-in toolsets may sneak in: the gateway is the whole tool
	// surface.
	for _, banned := range []string{"type: shell", "type: filesystem", "type: fetch", "type: script"} {
		if strings.Contains(y, banned) {
			t.Errorf("agent.yaml enables a built-in toolset %q:\n%s", banned, y)
		}
	}

	agent := res.Agent
	if !agent.StdinTask {
		t.Error("task must arrive on stdin")
	}
	joined := strings.Join(agent.Argv, " ")
	if !strings.Contains(joined, "--exec") || !strings.Contains(joined, "--json") {
		t.Errorf("argv lacks --exec/--json: %v", agent.Argv)
	}
	var hasToken, hasKey, hasProxy bool
	for _, e := range agent.Env {
		switch {
		case e.Name == spec.GatewayTokenEnv && e.FromRunToken:
			hasToken = true
		case e.Name == "ANTHROPIC_API_KEY" && e.FromHostEnv == "ANTHROPIC_API_KEY":
			hasKey = true
		case e.Name == "HTTPS_PROXY" && e.Value == spec.ProxyURL():
			hasProxy = true
		}
	}
	if !hasToken || !hasKey || !hasProxy {
		t.Errorf("agent env incomplete (token=%v key=%v proxy=%v): %+v", hasToken, hasKey, hasProxy, agent.Env)
	}
	if len(agent.Mounts) != 1 || agent.Mounts[0].Target != spec.PlanMount || !agent.Mounts[0].ReadOnly {
		t.Errorf("agent must mount only /plan read-only, got %+v", agent.Mounts)
	}
	if res.ResultKind != "text" {
		t.Errorf("result kind = %q, want text", res.ResultKind)
	}
}

func TestDockerAgentCompileWithSchema(t *testing.T) {
	ad, _ := Get(DefaultName)
	in := testInput()
	in.SchemaJSON = []byte(`{"type":"object"}`)
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultKind != "json" {
		t.Errorf("result kind = %q, want json", res.ResultKind)
	}
	if !strings.Contains(string(res.Files["agent.yaml"]), `{"type":"object"}`) {
		t.Error("schema not embedded in instruction")
	}
}

// A tool call names its tool and id, and the response that answers it is
// its result: the result's output, failed when the tool said so.
func TestDockerAgentToolCallsAndResults(t *testing.T) {
	ad, _ := Get(DefaultName)
	call := ad.Events([]byte(`{"type":"tool_call","tool_call":{"id":"call_1","function":{"name":"fs_read","arguments":"{}"}}}`))
	if len(call) != 1 || call[0].Tool != "fs_read" || call[0].Text != "fs_read" || call[0].CallID != "call_1" {
		t.Errorf("call = %+v", call)
	}
	ok := ad.Events([]byte(`{"type":"tool_call_response","tool_call_id":"call_1","tool_definition":{"name":"fs_read"},"response":"the model's copy","result":{"output":"hello"}}`))
	if len(ok) != 1 || ok[0].Kind != EventToolResult || ok[0].Tool != "fs_read" || ok[0].CallID != "call_1" || ok[0].Failed || ok[0].Text != "hello" {
		t.Errorf("result = %+v", ok)
	}
	failed := ad.Events([]byte(`{"type":"tool_call_response","tool_call_id":"call_2","tool_definition":{"name":"fs_read"},"response":"no such file","result":{"output":"","isError":true}}`))
	if len(failed) != 1 || !failed[0].Failed || failed[0].Text != "no such file" {
		t.Errorf("failed result = %+v", failed)
	}
}

func TestDockerAgentEvents(t *testing.T) {
	ad, _ := Get(DefaultName)
	tests := []struct {
		name string
		line string
		want []EventKind
	}{
		{"tool call", `{"type":"tool_call","tool":"fs_read"}`, []EventKind{EventToolCall}},
		{"tool use variant", `{"type":"tool_use_started"}`, []EventKind{EventToolCall}},
		// The 1.128.0 harness streams a call's arguments as
		// partial_tool_call chunks and answers it with tool_call_response;
		// neither is a turn (55 chunks of one write_report call would eat
		// the budget): the response is the call's result.
		{"partial chunk", `{"type":"partial_tool_call","tool_call":{"id":"call_1","function":{"name":"write_report","arguments":"| 101 |"}}}`, []EventKind{EventOther}},
		{"tool response", `{"type":"tool_call_response","tool_call_id":"call_1","response":"ok"}`, []EventKind{EventToolResult}},
		{"tool confirmation", `{"type":"tool_call_confirmation","tool_call":{"id":"call_1"}}`, []EventKind{EventOther}},
		{"complete call", `{"type":"tool_call","tool_call":{"id":"call_1","function":{"name":"list_inputs","arguments":"{}"}}}`, []EventKind{EventToolCall}},
		// The model's reasoning stream is not assistant output.
		{"reasoning", `{"type":"agent_choice_reasoning","content":"Let me"}`, []EventKind{EventReasoning}},
		{"choice text", `{"type":"agent_choice","message":{"content":"hello"}}`, []EventKind{EventAssistantText}},
		{"content parts", `{"type":"agent_choice","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, []EventKind{EventAssistantText}},
		{"top level text", `{"type":"delta","content":"chunk"}`, []EventKind{EventAssistantText}},
		{"usage nested", `{"type":"stream_stopped","usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`, []EventKind{EventUsage}},
		{"error", `{"type":"error","message":{"content":"boom"}}`, []EventKind{EventError}},
		{"unknown", `{"type":"heartbeat"}`, []EventKind{EventOther}},
		{"not json", `plain text line`, []EventKind{EventAssistantText}},
		{"empty", ``, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs := ad.Events([]byte(tc.line))
			if len(evs) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(evs), len(tc.want), evs)
			}
			for i, want := range tc.want {
				if evs[i].Kind != want {
					t.Errorf("event %d kind = %s, want %s", i, evs[i].Kind, want)
				}
			}
		})
	}

	// Text extraction from parts joins in order.
	evs := ad.Events([]byte(`{"type":"agent_choice","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`))
	if evs[0].Text != "ab" {
		t.Errorf("joined text = %q, want ab", evs[0].Text)
	}
	// Usage numbers survive.
	evs = ad.Events([]byte(`{"type":"stream_stopped","usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`))
	if evs[0].InputTokens != 10 || evs[0].OutputTokens != 5 || evs[0].CostUSD != 0.01 {
		t.Errorf("usage = %+v", evs[0])
	}
}

func TestRegistry(t *testing.T) {
	if _, err := Get("docker-agent"); err != nil {
		t.Fatalf("default adapter missing: %v", err)
	}
	if _, err := Get("goose"); err == nil || !strings.Contains(err.Error(), "planned") {
		t.Errorf("planned adapter must say it is planned, got %v", err)
	}
	if _, err := Get("nope"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown adapter must say unknown, got %v", err)
	}
}

// TestDockerAgentCompileInteractive: the interactive plan differs from the
// autonomous one only in argv, stdin and the Session block.
func TestDockerAgentCompileInteractive(t *testing.T) {
	ad, _ := Get(DefaultName)
	auto, err := ad.Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Mode = manifest.ModeInteractive
	inter, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(inter.Files["agent.yaml"]) != string(auto.Files["agent.yaml"]) {
		t.Error("agent.yaml differs between modes")
	}
	if inter.Session == nil || inter.Session.Transport != "acp-stdio" || inter.Session.Safety != "autonomous" || inter.Session.SessionDB != spec.SessionMount+"/session.db" {
		t.Errorf("session = %+v", inter.Session)
	}
	if len(inter.Session.Mounts) != 1 || inter.Session.Mounts[0].Source != spec.PHSession || inter.Session.Mounts[0].Target != spec.SessionMount || inter.Session.Mounts[0].ReadOnly {
		t.Errorf("session mounts = %+v", inter.Session.Mounts)
	}
	if inter.Agent.StdinTask {
		t.Error("interactive plan must not read a task from stdin")
	}
	if got := strings.Join(inter.Agent.Argv, " "); got != "serve acp /plan/agent.yaml --session-db /session/session.db" {
		t.Errorf("argv = %q", got)
	}
	// Everything else identical.
	a, b := auto.Agent, inter.Agent
	a.Argv, b.Argv, a.StdinTask, b.StdinTask = nil, nil, false, false
	if !reflect.DeepEqual(a, b) {
		t.Errorf("container specs differ beyond argv/stdin:\n%+v\n%+v", a, b)
	}
	if auto.Session != nil {
		t.Error("autonomous plan carries a Session block")
	}
	in.Mode = "daemon"
	if _, err := ad.Compile(in); err == nil {
		t.Error("unknown mode accepted")
	}
}

// Streamed assistant text is flagged as a delta so the engine concatenates
// it; a whole-message shape is not.
func TestDockerAgentTextDeltas(t *testing.T) {
	ad, _ := Get(DefaultName)
	if evs := ad.Events([]byte(`{"type":"agent_choice","content":"{"}`)); len(evs) != 1 || evs[0].Kind != EventAssistantText || !evs[0].Delta || evs[0].Text != "{" {
		t.Fatalf("agent_choice delta: %+v", evs)
	}
	if evs := ad.Events([]byte(`{"type":"assistant_message","message":{"content":"whole"}}`)); len(evs) != 1 || evs[0].Kind != EventAssistantText || evs[0].Delta {
		t.Fatalf("whole message: %+v", evs)
	}
}

// The policy's "custom" provider is an OpenAI-compatible endpoint of the
// operator's own; docker-agent has no provider of that name (1.128.0
// refuses `unknown provider type "custom"`) and reaches such an endpoint
// through its openai client with a base URL (measured on the pinned image).
func TestDockerAgentCompileMapsCustomToTheOpenAIClient(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Model = policy.Model{
		Provider:   "custom",
		Name:       "qwen3.8-flash",
		APIKeyEnv:  "CUSTOM_API_KEY",
		BaseURL:    "https://llm.example.net/compatible-mode/v1",
		ProxyAllow: []string{"llm.example.net:443"},
	}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	y := string(res.Files["agent.yaml"])
	for _, want := range []string{"provider: openai", "model: qwen3.8-flash", "base_url: https://llm.example.net/compatible-mode/v1"} {
		if !strings.Contains(y, want) {
			t.Errorf("agent.yaml missing %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "provider: custom") {
		t.Errorf("the harness does not know a provider called custom:\n%s", y)
	}
	if res.Model != "custom/qwen3.8-flash" {
		t.Errorf("the receipt keeps the policy's name: %q", res.Model)
	}
}

func TestDockerAgentCompileRefusesCustomWithoutABaseURL(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Model = policy.Model{Provider: "custom", Name: "m", APIKeyEnv: "CUSTOM_API_KEY"}
	if _, err := ad.Compile(in); err == nil || !strings.Contains(err.Error(), "model.baseUrl") {
		t.Fatalf("custom without a base URL must refuse naming the field: %v", err)
	}
}

// docker-agent's clients read the credential from their own variable
// (ANTHROPIC_API_KEY, GEMINI_API_KEY, OPENAI_API_KEY — measured on
// 1.128.0, which refuses to start the anthropic and google clients
// without theirs and ignores a config-level token_key for them), so the
// container carries the key under that name whatever the policy calls
// the host variable it comes from; the harness block's apiKeyEnv
// override counts, as it does for the receipt (policy.KeyEnv).
func TestDockerAgentCompileNamesTheKeyForTheHarnessClient(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	keyVar := func(res Result) (name, from string) {
		for _, e := range res.Agent.Env {
			if e.FromHostEnv != "" {
				return e.Name, e.FromHostEnv
			}
		}
		return "", ""
	}
	cases := []struct {
		provider, policyVar, baseURL, wantName string
	}{
		{"anthropic", "MY_ANTHROPIC_KEY", "", "ANTHROPIC_API_KEY"},
		{"google", "GOOGLE_API_KEY", "", "GEMINI_API_KEY"},
		{"openai", "OPENAI_API_KEY", "", "OPENAI_API_KEY"},
		{"custom", "CUSTOM_API_KEY", "https://llm.example.net/v1", "OPENAI_API_KEY"},
		{"mistral", "MISTRAL_API_KEY", "", "MISTRAL_API_KEY"}, // a provider the harness knows and this table does not
	}
	for _, c := range cases {
		in := testInput()
		in.Model = policy.Model{Provider: c.provider, Name: "m", APIKeyEnv: c.policyVar, BaseURL: c.baseURL, ProxyAllow: []string{"x:443"}}
		res, err := ad.Compile(in)
		if err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
		if name, from := keyVar(res); name != c.wantName || from != c.policyVar {
			t.Errorf("%s: key travels as %s from $%s; want %s from $%s", c.provider, name, from, c.wantName, c.policyVar)
		}
		if strings.Contains(string(res.Files["agent.yaml"]), "token_key") {
			t.Errorf("%s: token_key is not a mechanism every client has:\n%s", c.provider, res.Files["agent.yaml"])
		}
	}
	// The harness block's override names the host variable, as the
	// receipt's model.apiKey line does.
	in := testInput()
	in.Harness = policy.HarnessPolicy{APIKeyEnv: "TEAM_ANTHROPIC_KEY"}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if name, from := keyVar(res); name != "ANTHROPIC_API_KEY" || from != "TEAM_ANTHROPIC_KEY" {
		t.Errorf("harness override: key travels as %s from $%s", name, from)
	}
}

// An Anthropic subscription token is admitted from Claude Code alone — by
// Anthropic's terms and by the API — so the container harness refuses it at
// compile time, naming the harness that can use it and the key this one
// needs. The refusal is a plan-time fact, so `agentry run --plan` shows it
// before any container starts.
func TestDockerAgentCompileRefusesASubscriptionToken(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Model.APIKeyKind = policy.KeyKindSubscription
	_, err = ad.Compile(in)
	if err == nil {
		t.Fatal("a subscription token must be refused by the container harness")
	}
	if got, want := err.Error(), "apiKeyKind subscription: "+policy.SubscriptionTokenSentence; !strings.Contains(got, want) || strings.Contains(got, "key is an Anthropic") {
		t.Errorf("refusal %q, want it to carry %q", got, want)
	}
	for _, want := range []string{"subscription token", "Claude Code", "--adapter claude", "API key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	// An API key's plan holds the value to that kind at run time.
	res, err := ad.Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Agent.Env {
		if e.FromHostEnv != "" && (e.ExpectedKind == nil || e.ExpectedKind.Kind != "api" || e.ExpectedKind.Provider != "anthropic") {
			t.Errorf("the key's env does not record the api kind: %+v", e)
		}
	}
	// Other providers have one kind of key: nothing to expect.
	in = testInput()
	in.Model = policy.Model{Provider: "openai", Name: "m", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"x:443"}}
	res, err = ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Agent.Env {
		if e.FromHostEnv != "" && e.ExpectedKind != nil {
			t.Errorf("openai's key carries an expectation: %+v", e)
		}
	}
}
