// claude adapter tests: the compiled run must give Claude Code the
// gateway and nothing else (no built-in tools, no ambient configuration),
// carry every secret by name, and read the stream-json event shapes the
// engine depends on. The flags and shapes asserted here were established
// by running Claude Code 2.1.263 (claudeProbeVersion).
package adapter

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func claudeInput() Input {
	return Input{
		Instruction: "Do the thing.\n",
		Summary:     "a test agent",
		Model:       policy.Default().Model,
		GatewayURL:  spec.GatewayURL(),
	}
}

func claudeCompile(t *testing.T, in Input) Result {
	t.Helper()
	ad, err := Get(ClaudeAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestClaudeCompileGatewayOnly(t *testing.T) {
	res := claudeCompile(t, claudeInput())

	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(res.Files[claudeMCPFile], &cfg); err != nil {
		t.Fatalf("%s is not JSON: %v", claudeMCPFile, err)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("the gateway must be the only MCP server: %+v", cfg.MCPServers)
	}
	gw, ok := cfg.MCPServers[claudeGatewayServer]
	if !ok {
		t.Fatalf("no %q server in %s", claudeGatewayServer, claudeMCPFile)
	}
	if gw.Type != "http" || gw.URL != spec.PHHostGatewayURL {
		t.Errorf("gateway server = %+v, want an http server at %s", gw, spec.PHHostGatewayURL)
	}
	// The token rides as a variable NAME, expanded by Claude Code from the
	// process environment: a plan file must never hold the value.
	if want := "Bearer ${" + spec.GatewayTokenEnv + "}"; gw.Headers["Authorization"] != want {
		t.Errorf("Authorization header = %q, want %q", gw.Headers["Authorization"], want)
	}
	if got := string(res.Files[claudeInstructionFile]); got != "Do the thing.\n" {
		t.Errorf("instruction file = %q", got)
	}

	if res.Agent.Image != "" || res.Host == nil {
		t.Fatalf("claude runs the person's own installation, not a container: agent=%+v host=%+v", res.Agent, res.Host)
	}
	argv := strings.Join(res.Host.Argv, " ")
	for _, want := range []string{
		"-p", "--bare",
		"--output-format stream-json",
		"--system-prompt-file " + claudeInstructionFile,
		"--mcp-config " + claudeMCPFile,
		// Only the servers in that file, only the gateway's tools, and
		// nobody to widen it at run time.
		"--strict-mcp-config",
		"--allowedTools mcp__" + claudeGatewayServer,
		"--permission-mode dontAsk",
		"--permission-prompts none",
		// No user, project or local settings, and no session left behind.
		"--setting-sources",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--model " + policy.Default().Model.Name,
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %v", want, res.Host.Argv)
		}
	}
	// --tools with an empty value: every built-in tool (Bash, the file
	// tools, WebFetch) is off, whatever the person's settings say.
	tools := false
	for i, a := range res.Host.Argv {
		if a == "--tools" && i+1 < len(res.Host.Argv) && res.Host.Argv[i+1] == "" {
			tools = true
		}
	}
	if !tools {
		t.Errorf("argv must pass --tools with an empty value to disable the built-in tools: %v", res.Host.Argv)
	}
	if !res.Host.StdinTask {
		t.Error("an autonomous run takes its task on stdin")
	}
	if res.Session != nil {
		t.Errorf("autonomous plan carries a session block: %+v", res.Session)
	}
	if res.Host.Command != claudeExecutable {
		t.Errorf("command = %q, want the bare executable name %q", res.Host.Command, claudeExecutable)
	}
}

func TestClaudeCompileEnvironment(t *testing.T) {
	in := claudeInput()
	res := claudeCompile(t, in)
	env := map[string]spec.EnvVar{}
	for _, v := range res.Host.Env {
		env[v.Name] = v
	}
	if v := env[spec.GatewayTokenEnv]; !v.FromRunToken || v.Value != "" {
		t.Errorf("%s = %+v, want the per-run token by reference", spec.GatewayTokenEnv, v)
	}
	if v := env["ANTHROPIC_AUTH_TOKEN"]; v.FromHostEnv != in.Model.APIKeyEnv || v.Value != "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %+v, want it resolved from %s at run time", v, in.Model.APIKeyEnv)
	}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY"} {
		if env[name].Value != spec.PHHostProxyURL {
			t.Errorf("%s = %q, want the run's proxy %s", name, env[name].Value, spec.PHHostProxyURL)
		}
	}
	// The gateway and the proxy are both on the loopback: gateway traffic
	// must not be sent to the proxy.
	if !strings.Contains(env["NO_PROXY"].Value, "127.0.0.1") {
		t.Errorf("NO_PROXY = %q, want it to spare the loopback", env["NO_PROXY"].Value)
	}
	// Nothing of this run may land in the person's own configuration, and
	// nothing of theirs may leak into the run.
	if !strings.Contains(env["CLAUDE_CONFIG_DIR"].Value, claudeConfigDirName) {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want a run-scoped directory", env["CLAUDE_CONFIG_DIR"].Value)
	}
	for _, off := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_TELEMETRY",
		"DISABLE_ERROR_REPORTING", "DISABLE_AUTOUPDATER", "DISABLE_BUG_COMMAND",
	} {
		if env[off].Value != "1" {
			t.Errorf("%s = %q, want telemetry and background traffic off", off, env[off].Value)
		}
	}
	// Without an endpoint in the policy the harness keeps its own default.
	if _, set := env["ANTHROPIC_BASE_URL"]; set {
		t.Errorf("ANTHROPIC_BASE_URL set with no harness.claude.baseUrl: %+v", env["ANTHROPIC_BASE_URL"])
	}
	// The person's own credentials are deliberately not inherited: a run
	// spends the operator's key or none.
	for _, name := range res.Host.InheritEnv {
		if strings.Contains(name, "ANTHROPIC") || strings.Contains(name, "KEY") || strings.Contains(name, "TOKEN") {
			t.Errorf("InheritEnv passes a credential through: %q", name)
		}
	}
}

func TestClaudeCompileHarnessPolicy(t *testing.T) {
	in := claudeInput()
	in.Harness = policy.HarnessPolicy{
		BaseURL:   "https://compatible.example/anthropic",
		Model:     "some-other-model",
		APIKeyEnv: "COMPATIBLE_KEY",
	}
	res := claudeCompile(t, in)
	env := map[string]spec.EnvVar{}
	for _, v := range res.Host.Env {
		env[v.Name] = v
	}
	if env["ANTHROPIC_BASE_URL"].Value != in.Harness.BaseURL {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the policy's endpoint", env["ANTHROPIC_BASE_URL"].Value)
	}
	if env["ANTHROPIC_AUTH_TOKEN"].FromHostEnv != "COMPATIBLE_KEY" {
		t.Errorf("key env = %+v, want it resolved from COMPATIBLE_KEY", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if !strings.Contains(strings.Join(res.Host.Argv, " "), "--model some-other-model") {
		t.Errorf("argv does not ask for the policy's model: %v", res.Host.Argv)
	}
	if res.Model != "anthropic/some-other-model" {
		t.Errorf("receipt model = %q", res.Model)
	}
}

func TestClaudeCompileInteractive(t *testing.T) {
	in := claudeInput()
	in.Mode = manifest.ModeInteractive
	res := claudeCompile(t, in)
	if res.Session == nil || res.Session.Transport != claudeTransport {
		t.Fatalf("interactive plan needs the stream-json session block: %+v", res.Session)
	}
	if !strings.Contains(strings.Join(res.Host.Argv, " "), "--input-format stream-json") {
		t.Errorf("interactive argv must read turns as stream-json: %v", res.Host.Argv)
	}
	if res.Host.StdinTask {
		t.Error("an interactive session takes its turns from the driver, not a piped task")
	}
	// The two plans differ in the input side only.
	autonomous := claudeCompile(t, claudeInput())
	if string(autonomous.Files[claudeMCPFile]) != string(res.Files[claudeMCPFile]) {
		t.Error("the two modes must compile the same gateway configuration")
	}
}

func TestClaudeCompileSchemaRidesThePrompt(t *testing.T) {
	in := claudeInput()
	in.SchemaJSON = []byte(`{"type":"object"}`)
	res := claudeCompile(t, in)
	if res.ResultKind != "json" {
		t.Errorf("resultKind = %q, want json", res.ResultKind)
	}
	if !strings.Contains(string(res.Files[claudeInstructionFile]), `{"type":"object"}`) {
		t.Error("the schema must ride in the instruction")
	}
}

func TestClaudeCompileRefusesIncompletePolicy(t *testing.T) {
	ad, err := Get(ClaudeAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Input){
		"no instruction": func(in *Input) { in.Instruction = "" },
		"no model":       func(in *Input) { in.Model.Name = "" },
		"no key":         func(in *Input) { in.Model.APIKeyEnv = "" },
		"unknown mode":   func(in *Input) { in.Mode = manifest.Mode("sideways") },
	} {
		in := claudeInput()
		mutate(&in)
		if _, err := ad.Compile(in); err == nil {
			t.Errorf("%s: Compile succeeded, want a refusal", name)
		}
	}
}

func TestClaudeEvents(t *testing.T) {
	ad, err := Get(ClaudeAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("assistant text is one event per message", func(t *testing.T) {
		// A final answer split over several blocks must arrive whole: the
		// engine keeps the last assistant text as the run's result.
		evs := ad.Events([]byte(`{"type":"assistant","message":{"content":[
			{"type":"thinking","thinking":"reasoning that must not leak into the result"},
			{"type":"text","text":"part one "},{"type":"text","text":"part two"}]}}`))
		if len(evs) != 1 || evs[0].Kind != EventAssistantText || evs[0].Text != "part one part two" {
			t.Fatalf("events = %+v", evs)
		}
		if evs[0].Delta {
			t.Error("a whole message must not be reported as a delta")
		}
	})
	t.Run("tool calls are counted once", func(t *testing.T) {
		evs := ad.Events([]byte(`{"type":"assistant","message":{"content":[
			{"type":"tool_use","id":"t1","name":"mcp__gateway__fs_read","input":{}},
			{"type":"tool_use","id":"t2","name":"mcp__gateway__write_report","input":{}}]}}`))
		calls := 0
		for _, ev := range evs {
			if ev.Kind == EventToolCall {
				calls++
			}
		}
		if calls != 2 {
			t.Fatalf("counted %d tool calls in %+v", calls, evs)
		}
	})
	t.Run("tool results are observations", func(t *testing.T) {
		evs := ad.Events([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"notes.txt"}]}]}}`))
		for _, ev := range evs {
			if ev.Kind == EventAssistantText || ev.Kind == EventToolCall {
				t.Errorf("a tool result became %s: %+v", ev.Kind, ev)
			}
		}
	})
	t.Run("the result message carries the answer and the accounting", func(t *testing.T) {
		evs := ad.Events([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"the note said hello",
			"stop_reason":"end_turn","total_cost_usd":0.25,"usage":{"input_tokens":11,"output_tokens":22}}`))
		var text, usage bool
		for _, ev := range evs {
			switch ev.Kind {
			case EventAssistantText:
				text = ev.Text == "the note said hello"
			case EventUsage:
				usage = ev.InputTokens == 11 && ev.OutputTokens == 22 && ev.CostUSD == 0.25
			case EventError:
				t.Errorf("a successful result reported an error: %+v", ev)
			}
		}
		if !text || !usage {
			t.Fatalf("text=%v usage=%v in %+v", text, usage, evs)
		}
	})
	t.Run("a failed result is an error", func(t *testing.T) {
		evs := ad.Events([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the model refused","usage":{}}`))
		found := false
		for _, ev := range evs {
			if ev.Kind == EventError && ev.Text == "the model refused" {
				found = true
			}
		}
		if !found {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("the token counter is not transcript material", func(t *testing.T) {
		if evs := ad.Events([]byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":4}`)); len(evs) != 0 {
			t.Fatalf("events = %+v, want none", evs)
		}
	})
	t.Run("a refused tool is recorded", func(t *testing.T) {
		evs := ad.Events([]byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash"}`))
		if len(evs) != 1 || evs[0].Kind != EventPermissionDenied || evs[0].Text != "Bash" {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("nothing unknown becomes the answer", func(t *testing.T) {
		// stdout is strictly NDJSON here, so a line that is not JSON is
		// noise and must never be mistaken for the run's result.
		for _, line := range []string{"not json at all", `{"type":"stream_event","event":{}}`, `{"type":"system","subtype":"init"}`} {
			for _, ev := range ad.Events([]byte(line)) {
				if ev.Kind == EventAssistantText {
					t.Errorf("%q became assistant text: %+v", line, ev)
				}
				if len(ev.Raw) == 0 {
					t.Errorf("%q dropped its raw line", line)
				}
			}
		}
	})
	t.Run("blank lines are nothing", func(t *testing.T) {
		if evs := ad.Events([]byte("   ")); len(evs) != 0 {
			t.Fatalf("events = %+v", evs)
		}
	})
}

// fakeStdio is a harness's stdio: what the runner writes is captured, what
// the harness "says" is replayed.
type fakeStdio struct {
	out    *io.PipeReader
	outW   *io.PipeWriter
	mu     sync.Mutex
	writes strings.Builder
	// lines receives every line the runner writes, for a scripted harness
	// that answers requests (the codex session test).
	lines chan []byte
	once  sync.Once
	// closed is set under mu before lines is closed, so a write racing
	// the harness's exit is recorded but never sent on a closed channel.
	closed bool
}

func newFakeStdio() *fakeStdio {
	r, w := io.Pipe()
	return &fakeStdio{out: r, outW: w, lines: make(chan []byte, 256)}
}
func (f *fakeStdio) Read(p []byte) (int, error) { return f.out.Read(p) }
func (f *fakeStdio) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes.Write(p)
	if f.closed {
		return 0, io.ErrClosedPipe // what a real pipe says after the harness exited
	}
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		select {
		case f.lines <- []byte(line):
		default:
		}
	}
	return len(p), nil
}
func (f *fakeStdio) Close() error {
	f.once.Do(func() {
		_ = f.outW.Close()
		f.mu.Lock()
		f.closed = true
		close(f.lines)
		f.mu.Unlock()
	})
	return nil
}
func (f *fakeStdio) say(line string) { _, _ = f.outW.Write([]byte(line + "\n")) }

// awaitLine waits, up to d, for the runner to write a line containing want,
// and reports whether it did. A scripted harness that answers a turn calls
// it first: a real harness replies only after it has read the turn, and a
// fake that replies earlier can have its answer dropped before the driver
// has registered the turn it belongs to.
func (f *fakeStdio) awaitLine(want string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case line, ok := <-f.lines:
			if !ok {
				return false
			}
			if strings.Contains(string(line), want) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
func (f *fakeStdio) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes.String()
}

func TestClaudeSessionTurn(t *testing.T) {
	ad, err := Get(ClaudeAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	stdio := newFakeStdio()
	drv, err := ad.Session(context.Background(), stdio, SessionOptions{GatewayTools: []string{"mcp__gateway__fs_read"}})
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code's stream-json input has no handshake: nothing is sent or
	// expected until the first user turn.
	if err := drv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if stdio.written() != "" {
		t.Errorf("Start wrote to the harness: %q", stdio.written())
	}

	// The harness answers only once the user turn has reached it, as Claude
	// Code does: the driver drops a result that arrives while no turn is in
	// flight, so a fake that answered at once could race Prompt's
	// registration and leave it waiting for its deadline.
	go func() {
		if !stdio.awaitLine(`"type":"user"`, 5*time.Second) {
			return // Prompt times out and the test says why
		}
		stdio.say(`{"type":"system","subtype":"init","tools":["mcp__gateway__fs_read"]}`)
		stdio.say(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__gateway__fs_read"}]}}`)
		stdio.say(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[]}]}}`)
		stdio.say(`{"type":"assistant","message":{"content":[{"type":"text","text":"the note said hello"}]}}`)
		stdio.say(`{"type":"result","subtype":"success","result":"the note said hello","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := drv.Prompt(ctx, "what did the note say?")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if turn.Text != "the note said hello" || turn.StopReason != "end_turn" {
		t.Errorf("turn = %+v", turn)
	}
	if turn.Usage == nil || turn.Usage.InputTokens != 10 || turn.Usage.OutputTokens != 5 {
		t.Errorf("turn usage = %+v", turn.Usage)
	}
	// The user turn goes out as one stream-json line the harness understands.
	var sent struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdio.written())), &sent); err != nil {
		t.Fatalf("the turn written to the harness is not JSON: %q", stdio.written())
	}
	if sent.Type != "user" || sent.Message.Role != "user" ||
		len(sent.Message.Content) != 1 || sent.Message.Content[0].Text != "what did the note say?" {
		t.Errorf("turn written = %q", stdio.written())
	}

	// The transcript sees the tool call and the answer exactly once: the
	// result message repeats the text, and the driver drops that copy.
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
	var texts, calls int
	for _, ev := range <-drained {
		switch ev.Kind {
		case EventAssistantText:
			texts++
		case EventToolCall:
			calls++
		}
	}
	if texts != 1 || calls != 1 {
		t.Errorf("transcript had %d assistant texts and %d tool calls, want 1 and 1", texts, calls)
	}
}

func TestClaudeSessionEndsWithoutAnAnswer(t *testing.T) {
	ad, _ := Get(ClaudeAdapterName)
	stdio := newFakeStdio()
	drv, err := ad.Session(context.Background(), stdio, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = stdio.Close() }() // the harness exits mid-turn
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := drv.Prompt(ctx, "are you there?"); err == nil {
		t.Fatal("Prompt returned no error after the harness exited")
	}
}

// A subscription OAuth token (policy apiKeyKind subscription) is what
// Claude Code reads from CLAUDE_CODE_OAUTH_TOKEN — and bare mode does not
// read that variable, by Anthropic's own documentation — so the plan drops
// --bare for that kind and keeps every other isolation flag; the plan
// records the kind it was rendered for, so the engine refuses an API key
// under it. An API key keeps --bare.
func TestClaudeCompileSubscriptionTokenDropsBareAndUsesTheOAuthVariable(t *testing.T) {
	in := claudeInput()
	in.Model.APIKeyKind = policy.KeyKindSubscription
	res := claudeCompile(t, in)
	argv := strings.Join(res.Host.Argv, " ")
	if strings.Contains(argv, "--bare") {
		t.Errorf("--bare kept for a subscription token, which bare mode cannot read: %s", argv)
	}
	for _, keep := range []string{"--strict-mcp-config", "--setting-sources ", "--tools ", "--permission-mode dontAsk", "--no-session-persistence"} {
		if !strings.Contains(argv, keep) {
			t.Errorf("isolation flag %q lost: %s", keep, argv)
		}
	}
	env := map[string]spec.EnvVar{}
	for _, v := range res.Host.Env {
		env[v.Name] = v
	}
	if _, set := env["ANTHROPIC_AUTH_TOKEN"]; set {
		t.Errorf("ANTHROPIC_AUTH_TOKEN set for a subscription token: %+v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	v := env["CLAUDE_CODE_OAUTH_TOKEN"]
	if v.FromHostEnv != in.Model.APIKeyEnv || v.Value != "" || v.ExpectedKind == nil || v.ExpectedKind.Provider != "anthropic" || v.ExpectedKind.Kind != "subscription" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %+v, want it resolved from %s and held to the subscription kind", v, in.Model.APIKeyEnv)
	}
	if !strings.Contains(env["CLAUDE_CONFIG_DIR"].Value, claudeConfigDirName) {
		t.Errorf("the run-scoped config dir is what keeps the person's own login out without --bare: %+v", env["CLAUDE_CONFIG_DIR"])
	}
	// Without --bare the harness reads memory files up the tree: the
	// plan names them for the engine to refuse the run over.
	if got := strings.Join(res.Host.RefuseAncestorFiles, ","); got != "CLAUDE.md,CLAUDE.local.md,.claude/rules" {
		t.Errorf("RefuseAncestorFiles = %q", got)
	}

	// The harness block's kind wins, as its variable does.
	in = claudeInput()
	in.Harness = policy.HarnessPolicy{APIKeyEnv: "TEAM_OAUTH", APIKeyKind: policy.KeyKindSubscription}
	res = claudeCompile(t, in)
	if strings.Contains(strings.Join(res.Host.Argv, " "), "--bare") {
		t.Errorf("harness block's subscription kind ignored")
	}

	// An API key: --bare, ANTHROPIC_AUTH_TOKEN, held to the api kind.
	res = claudeCompile(t, claudeInput())
	if !strings.Contains(strings.Join(res.Host.Argv, " "), "--bare") {
		t.Errorf("--bare must stay for an API key")
	}
	env = map[string]spec.EnvVar{}
	for _, v := range res.Host.Env {
		env[v.Name] = v
	}
	if v := env["ANTHROPIC_AUTH_TOKEN"]; v.ExpectedKind == nil || v.ExpectedKind.Kind != "api" {
		t.Errorf("an API key's plan records the api kind: %+v", v)
	}
	if _, set := env["CLAUDE_CODE_OAUTH_TOKEN"]; set {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN set for an API key")
	}
	if res.Host.RefuseAncestorFiles != nil {
		t.Errorf("an API-key run has --bare and names nothing to refuse over: %v", res.Host.RefuseAncestorFiles)
	}
}
