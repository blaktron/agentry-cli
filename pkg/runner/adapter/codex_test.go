// codex adapter tests: the compiled config must give the Codex CLI the
// gateway and nothing else, carry every secret by name, and the event
// mappers must read the shapes codex-cli 0.153.4 actually emits.
package adapter

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func codexInput() Input {
	in := Input{
		Instruction: "Do the thing.\n",
		Summary:     "a test agent",
		Model:       policy.Default().Model,
		GatewayURL:  spec.GatewayURL(),
	}
	in.Model.Provider, in.Model.Name, in.Model.APIKeyEnv = "openai", "some-model", "OPENAI_API_KEY"
	in.Model.BaseURL = "https://compatible.example/v1"
	return in
}

func codexCompile(t *testing.T, in Input) Result {
	t.Helper()
	ad, err := Get(CodexAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCodexCompileGatewayOnly(t *testing.T) {
	res := codexCompile(t, codexInput())
	cfg := string(res.Files[codexConfigFile])
	for _, want := range []string{
		// The gateway is the only MCP server, its token by variable name.
		"[mcp_servers." + codexGatewayServer + "]",
		"url = \"" + spec.PHHostGatewayURL + "\"",
		"bearer_token_env_var = \"" + spec.GatewayTokenEnv + "\"",
		// Every built-in tool off, and web search with it.
		"shell_tool = false", "view_image = false", "apps = false", "browser_use = false",
		"computer_use = false", "image_generation = false", "multi_agent = false",
		"hooks = false", "memories = false", "web_search = \"disabled\"",
		// Nothing prompts, nothing phones home.
		"approval_policy = \"never\"", "sandbox_mode = \"read-only\"", "default_tools_approval_mode = \"approve\"",
		"[analytics]\nenabled = false", "[otel]\nexporter = \"none\"",
		// The model endpoint is the policy's, over the only wire API this
		// release speaks; the key by variable name.
		"base_url = \"https://compatible.example/v1\"", "env_key = \"OPENAI_API_KEY\"",
		"wire_api = \"responses\"", "model = \"some-model\"",
		// The instruction replaces codex's built-in prompt, by a path
		// rendered per run.
		"model_instructions_file = \"" + spec.PHCfg + "/harness/" + codexInstructionFile + "\"",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.toml lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Count(cfg, "[mcp_servers.") != 1 {
		t.Errorf("the gateway must be the only MCP server:\n%s", cfg)
	}
	if got := string(res.Files[codexInstructionFile]); got != "Do the thing.\n" {
		t.Errorf("instruction file = %q", got)
	}
	if res.Agent.Image != "" || res.Host == nil || res.Host.Command != codexExecutable {
		t.Fatalf("codex runs the person's own installation: agent=%+v host=%+v", res.Agent, res.Host)
	}
	argv := strings.Join(res.Host.Argv, " ")
	for _, want := range []string{"exec", "--json", "--ephemeral", "--strict-config", "--ignore-rules", "--skip-git-repo-check"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %v", want, res.Host.Argv)
		}
	}
	if res.Host.Argv[len(res.Host.Argv)-1] != "-" || !res.Host.StdinTask {
		t.Error("an autonomous run takes its task on stdin")
	}
	if res.Session != nil {
		t.Errorf("autonomous plan carries a session block: %+v", res.Session)
	}
}

// TestCodexCompileRendersModelCatalog: codex's bundled catalog switches on
// apply_patch, deferred tool_search, code mode and experimental tools for
// real OpenAI models (agentry-cli#737). The adapter renders its own
// one-model catalog instead, and keeps the gateway's tools out of the
// deferred surface, so the model's tools are the gateway's and nothing else
// whatever the model id.
func TestCodexCompileRendersModelCatalog(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-6-astra", "some-model"} {
		in := codexInput()
		in.Model.Name = model
		res := codexCompile(t, in)
		cfg := string(res.Files[codexConfigFile])
		for _, want := range []string{
			"model_catalog_json = \"" + spec.PHCfg + "/harness/" + codexCatalogFile + "\"",
			"omit_tools_from = [\"deferred\"]",
			"code_mode = false", "code_mode_only = false", "goals = false",
			"[tools.experimental_request_user_input]\nenabled = false",
		} {
			if !strings.Contains(cfg, want) {
				t.Errorf("%s: config.toml lacks %q:\n%s", model, want, cfg)
			}
		}
		// The deferral switch belongs to the gateway's server block: the
		// last table header before it is the gateway's.
		if j := strings.Index(cfg, "omit_tools_from"); j < 0 || tomlTableAt(cfg, j) != "[mcp_servers."+codexGatewayServer+"]" {
			t.Errorf("%s: omit_tools_from is not in the gateway's block:\n%s", model, cfg)
		}

		raw := res.Files[codexCatalogFile]
		var catalog struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil {
			t.Fatalf("%s: catalog is not JSON: %v\n%s", model, err, raw)
		}
		if len(catalog.Models) != 1 {
			t.Fatalf("%s: catalog has %d models, want exactly the run's one", model, len(catalog.Models))
		}
		m := catalog.Models[0]
		for key, want := range map[string]any{
			"slug":                              model,
			"apply_patch_tool_type":             nil,
			"supports_search_tool":              false,
			"tool_mode":                         "direct",
			"shell_type":                        "disabled",
			"include_skills_usage_instructions": false,
			"include_plugin_usage_instructions": false,
			"include_apps_usage_instructions":   false,
			"supported_in_api":                  true,
		} {
			got, ok := m[key]
			if !ok || got != want {
				t.Errorf("%s: catalog %s = %v (present %v), want %v", model, key, got, ok, want)
			}
		}
		if tools, _ := m["experimental_supported_tools"].([]any); tools == nil || len(tools) != 0 {
			t.Errorf("%s: experimental_supported_tools = %v, want an empty list", model, m["experimental_supported_tools"])
		}
		// Every field codex-cli 0.153.4 requires to parse a ModelInfo (no
		// serde default) is present, so the catalog is never refused.
		for _, key := range codexCatalogRequiredFields {
			if _, ok := m[key]; !ok {
				t.Errorf("%s: catalog lacks required field %q", model, key)
			}
		}
	}
}

// tomlTableAt returns the header of the TOML table that holds offset i.
func tomlTableAt(cfg string, i int) string {
	header := ""
	for _, line := range strings.Split(cfg[:i], "\n") {
		if strings.HasPrefix(line, "[") {
			header = strings.TrimSpace(line)
		}
	}
	return header
}

// codexCatalogRequiredFields are ModelInfo's fields without a serde default
// in codex-cli 0.153.4 (protocol/src/openai_models.rs), plus tool_mode,
// which defaults to unset but must be set here, and base_instructions.
var codexCatalogRequiredFields = []string{
	"slug", "display_name", "description", "supported_reasoning_levels", "shell_type",
	"visibility", "supported_in_api", "priority", "availability_nux", "upgrade",
	"support_verbosity", "default_verbosity", "apply_patch_tool_type", "truncation_policy",
	"experimental_supported_tools", "tool_mode",
	// Not a ModelInfo field but required beside it unless the entry carries
	// model_messages.instructions_template (deserialize_model_infos_with_legacy_base).
	"base_instructions",
}

func TestCodexCompileEnvironment(t *testing.T) {
	in := codexInput()
	res := codexCompile(t, in)
	env := map[string]spec.EnvVar{}
	for _, v := range res.Host.Env {
		env[v.Name] = v
	}
	if v := env[spec.GatewayTokenEnv]; !v.FromRunToken || v.Value != "" {
		t.Errorf("%s = %+v, want the run token by reference", spec.GatewayTokenEnv, v)
	}
	// The key under the very name the config's env_key reads.
	if v := env["OPENAI_API_KEY"]; v.FromHostEnv != "OPENAI_API_KEY" || v.Value != "" {
		t.Errorf("model key = %+v, want it resolved from OPENAI_API_KEY at run time", v)
	}
	if !strings.Contains(env["CODEX_HOME"].Value, codexHomeDir) {
		t.Errorf("CODEX_HOME = %q, want a run-scoped directory", env["CODEX_HOME"].Value)
	}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY"} {
		if env[name].Value != spec.PHHostProxyURL {
			t.Errorf("%s = %q, want the run's proxy", name, env[name].Value)
		}
	}
	if !strings.Contains(env["NO_PROXY"].Value, "127.0.0.1") {
		t.Errorf("NO_PROXY = %q, want it to spare the loopback", env["NO_PROXY"].Value)
	}
	for _, name := range res.Host.InheritEnv {
		if strings.Contains(name, "KEY") || strings.Contains(name, "TOKEN") {
			t.Errorf("InheritEnv passes a credential through: %q", name)
		}
	}
}

func TestCodexCompilePolicyFallbacks(t *testing.T) {
	// No endpoint in the policy: OpenAI's own.
	in := codexInput()
	in.Model.BaseURL = ""
	if cfg := string(codexCompile(t, in).Files[codexConfigFile]); !strings.Contains(cfg, "base_url = \""+codexDefaultBaseURL+"\"") {
		t.Errorf("no baseUrl must fall back to OpenAI:\n%s", cfg)
	}
	// The harness block overrides the model block.
	in = codexInput()
	in.Harness = policy.HarnessPolicy{BaseURL: "https://other.example/v1", Model: "other-model", APIKeyEnv: "OTHER_KEY"}
	res := codexCompile(t, in)
	cfg := string(res.Files[codexConfigFile])
	for _, want := range []string{"base_url = \"https://other.example/v1\"", "model = \"other-model\"", "env_key = \"OTHER_KEY\""} {
		if !strings.Contains(cfg, want) {
			t.Errorf("harness override missing %q:\n%s", want, cfg)
		}
	}
	found := false
	for _, v := range res.Host.Env {
		if v.Name == "OTHER_KEY" && v.FromHostEnv == "OTHER_KEY" {
			found = true
		}
	}
	if !found {
		t.Errorf("the overridden key variable is not passed by name: %+v", res.Host.Env)
	}
	if res.Model != "openai/other-model" {
		t.Errorf("receipt model = %q", res.Model)
	}
}

func TestCodexCompileInteractive(t *testing.T) {
	in := codexInput()
	in.Mode = manifest.ModeInteractive
	res := codexCompile(t, in)
	if res.Session == nil || res.Session.Transport != codexTransport {
		t.Fatalf("interactive plan needs the app-server session block: %+v", res.Session)
	}
	if res.Host.Argv[0] != "app-server" || res.Host.StdinTask {
		t.Errorf("interactive argv = %v stdinTask=%v", res.Host.Argv, res.Host.StdinTask)
	}
	autonomous := codexCompile(t, codexInput())
	if string(autonomous.Files[codexConfigFile]) != string(res.Files[codexConfigFile]) {
		t.Error("the two modes must compile the same config")
	}
}

func TestCodexCompileRefusesIncompletePolicy(t *testing.T) {
	ad, _ := Get(CodexAdapterName)
	for name, mutate := range map[string]func(*Input){
		"no instruction": func(in *Input) { in.Instruction = "" },
		"no model":       func(in *Input) { in.Model.Name = "" },
		"no key":         func(in *Input) { in.Model.APIKeyEnv = "" },
		"unknown mode":   func(in *Input) { in.Mode = manifest.Mode("sideways") },
		// A model id is substituted into plan files the engine renders.
		"placeholder model": func(in *Input) { in.Model.Name = "gpt-5.5" + spec.PHCfg },
		"control model":     func(in *Input) { in.Model.Name = "gpt-5.5\r" },
	} {
		in := codexInput()
		mutate(&in)
		if _, err := ad.Compile(in); err == nil {
			t.Errorf("%s: Compile succeeded, want a refusal", name)
		}
	}
}

func TestTomlString(t *testing.T) {
	if got := tomlString(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("tomlString = %s", got)
	}
}

func TestCodexEvents(t *testing.T) {
	ad, _ := Get(CodexAdapterName)
	kinds := func(line string) []EventKind {
		var out []EventKind
		for _, ev := range ad.Events([]byte(line)) {
			out = append(out, ev.Kind)
			if len(ev.Raw) == 0 {
				t.Errorf("event %s dropped its raw line", ev.Kind)
			}
		}
		return out
	}
	// Shapes recorded from codex-cli 0.153.4.
	if k := kinds(`{"type":"item.started","item":{"id":"item_2","type":"mcp_tool_call","server":"gateway","tool":"write_report","arguments":{},"status":"in_progress"}}`); len(k) != 1 || k[0] != EventToolCall {
		t.Errorf("a starting MCP call = %v, want one tool_call", k)
	}
	if k := kinds(`{"type":"item.completed","item":{"id":"item_2","type":"mcp_tool_call","server":"gateway","tool":"write_report","status":"completed"}}`); len(k) != 1 || k[0] != EventOther {
		t.Errorf("a completed MCP call = %v, want an observation, not a second count", k)
	}
	evs := ad.Events([]byte(`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"hello from the workspace"}}`))
	if len(evs) != 1 || evs[0].Kind != EventAssistantText || evs[0].Text != "hello from the workspace" || evs[0].Delta {
		t.Errorf("agent message = %+v", evs)
	}
	if k := kinds(`{"type":"item.completed","item":{"id":"item_1","type":"reasoning","text":"thinking"}}`); len(k) != 1 || k[0] != EventOther {
		t.Errorf("reasoning = %v, want kept out of the result", k)
	}
	evs = ad.Events([]byte(`{"type":"turn.completed","usage":{"input_tokens":8257,"cached_input_tokens":0,"output_tokens":16,"reasoning_output_tokens":13}}`))
	if len(evs) != 1 || evs[0].Kind != EventUsage || evs[0].InputTokens != 8257 || evs[0].OutputTokens != 16 {
		t.Errorf("usage = %+v", evs)
	}
	evs = ad.Events([]byte(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata not found"}}`))
	if len(evs) != 1 || evs[0].Kind != EventError || evs[0].Text != "Model metadata not found" {
		t.Errorf("error item = %+v", evs)
	}
	evs = ad.Events([]byte(`{"type":"turn.failed","error":{"message":"boom"}}`))
	if len(evs) != 1 || evs[0].Kind != EventError || evs[0].Text != "boom" {
		t.Errorf("turn.failed = %+v", evs)
	}
	for _, line := range []string{"not json", `{"type":"thread.started","thread_id":"x"}`, `{"type":"turn.started"}`} {
		for _, ev := range ad.Events([]byte(line)) {
			if ev.Kind == EventAssistantText {
				t.Errorf("%q became assistant text", line)
			}
		}
	}
	if evs := ad.Events([]byte("  ")); len(evs) != 0 {
		t.Errorf("blank line = %+v", evs)
	}
}

// codexServer scripts the app-server side of one session over fakeStdio:
// answers requests by method and replays notifications.
func TestCodexSession(t *testing.T) {
	ad, _ := Get(CodexAdapterName)
	stdio := newFakeStdio()
	drv, err := ad.Session(context.Background(), stdio, SessionOptions{GatewayTools: []string{"write_report"}})
	if err != nil {
		t.Fatal(err)
	}
	// The scripted server: read what the runner writes, answer in kind.
	go func() {
		for line := range stdio.lines {
			var m struct {
				ID     *int           `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
				Result map[string]any `json:"result"`
			}
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.Method {
			case "initialize":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"userAgent":"codex"}}`)
			case "initialized":
			case "thread/start":
				if m.Params["ephemeral"] != true {
					t.Errorf("thread/start must be ephemeral: %v", m.Params)
				}
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"thread":{"id":"t1"},"approvalPolicy":"on-request"}}`)
			case "turn/start":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`)
				stdio.say(`{"method":"item/started","params":{"threadId":"t1","turnId":"u1","item":{"type":"mcpToolCall","id":"c1","server":"gateway","tool":"write_report","status":"inProgress"}}}`)
				// The approval the runner must answer.
				stdio.say(`{"method":"mcpServer/elicitation/request","id":0,"params":{"threadId":"t1","turnId":"u1","serverName":"gateway","mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call","tool_description":"Write the report."}}}`)
			case "":
				// A response from the runner to our request.
				if m.ID != nil && *m.ID == 0 {
					if m.Result["action"] != "accept" {
						t.Errorf("gateway approval answered %v, want accept", m.Result)
					}
					stdio.say(`{"method":"item/completed","params":{"threadId":"t1","turnId":"u1","item":{"type":"mcpToolCall","id":"c1","server":"gateway","tool":"write_report","status":"completed"}}}`)
					stdio.say(`{"method":"thread/tokenUsage/updated","params":{"threadId":"t1","turnId":"u1","tokenUsage":{"last":{"inputTokens":10,"outputTokens":5},"total":{"inputTokens":10,"outputTokens":5}}}}`)
					stdio.say(`{"method":"item/completed","params":{"threadId":"t1","turnId":"u1","item":{"type":"agentMessage","id":"m1","text":"DONE"}}}`)
					stdio.say(`{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"u1","status":"completed"}}}`)
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	turn, err := drv.Prompt(ctx, "Write the report.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if turn.Text != "DONE" || turn.StopReason != "completed" || turn.Usage == nil || turn.Usage.InputTokens != 10 || turn.Usage.OutputTokens != 5 {
		t.Errorf("turn = %+v usage=%+v", turn, turn.Usage)
	}
	drained := make(chan []Event, 1)
	go func() {
		var evs []Event
		for ev := range drv.Events() {
			evs = append(evs, ev)
		}
		drained <- evs
	}()
	if err := drv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var calls, granted, texts, usage int
	for _, ev := range <-drained {
		switch ev.Kind {
		case EventToolCall:
			calls++
		case EventPermissionGranted:
			granted++
		case EventAssistantText:
			texts++
		case EventUsage:
			usage++
		}
	}
	if calls != 1 || granted != 1 || texts != 1 || usage != 1 {
		t.Errorf("transcript: calls=%d granted=%d texts=%d usage=%d, want 1 each", calls, granted, texts, usage)
	}
}

func TestCodexSessionDeclinesForeignApprovals(t *testing.T) {
	ad, _ := Get(CodexAdapterName)
	stdio := newFakeStdio()
	drv, _ := ad.Session(context.Background(), stdio, SessionOptions{GatewayTools: []string{"write_report"}})
	answered := make(chan map[string]any, 2)
	go func() {
		for line := range stdio.lines {
			var m struct {
				ID     *int           `json:"id"`
				Method string         `json:"method"`
				Result map[string]any `json:"result"`
				Error  map[string]any `json:"error"`
			}
			if json.Unmarshal(line, &m) != nil || m.Method != "" || m.ID == nil {
				continue
			}
			if m.Result != nil {
				answered <- m.Result
			} else {
				answered <- m.Error
			}
		}
	}()
	// A server that is not the gateway, and a shell approval: both refused.
	stdio.say(`{"method":"mcpServer/elicitation/request","id":7,"params":{"serverName":"someone-else","_meta":{"codex_approval_kind":"mcp_tool_call"}}}`)
	stdio.say(`{"method":"item/commandExecution/requestApproval","id":8,"params":{"command":"rm -rf /"}}`)
	for i := 0; i < 2; i++ {
		select {
		case a := <-answered:
			if a["action"] == "accept" {
				t.Errorf("a foreign request was accepted: %v", a)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no answer to a server request")
		}
	}
	drained := make(chan []Event, 1)
	go func() {
		var evs []Event
		for ev := range drv.Events() {
			evs = append(evs, ev)
		}
		drained <- evs
	}()
	_ = drv.Close()
	var denied, refused int
	for _, ev := range <-drained {
		if ev.Kind == EventPermissionDenied {
			denied++
		}
		if ev.Kind == EventOther && strings.HasPrefix(ev.Text, "refused ") {
			refused++
		}
	}
	if denied != 1 || refused != 1 {
		t.Errorf("transcript: denied=%d refused=%d, want 1 and 1", denied, refused)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
