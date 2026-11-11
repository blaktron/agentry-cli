// Package harnesstest runs the runner's own harness end to end against a
// scripted model and a scripted tool gateway, with no network and no key.
package harnesstest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/internal/scriptedmodel"
	"github.com/blaktron/agentry-cli/runnerimage/harness"
	"github.com/blaktron/agentry-cli/runnerimage/harness/convo"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

const (
	modelKey     = "model-key-value"
	gatewayToken = "gateway-token-value"
)

// gateway is a scripted MCP server over streamable HTTP.
type gateway struct {
	t   *testing.T
	srv *httptest.Server

	// tools is what tools/list answers, pageSize a page (0 is all at once).
	tools    []string
	pageSize int
	// call answers tools/call: a result object, or a JSON-RPC error.
	call func(ctx context.Context, name string, args json.RawMessage) (result string, rpcError string)
	// stream answers tools/call as an event stream.
	stream bool

	mu      sync.Mutex
	calls   []string
	headers []http.Header
}

func newGateway(t *testing.T, tools ...string) *gateway {
	g := &gateway{t: t, tools: tools}
	g.call = func(context.Context, string, json.RawMessage) (string, string) {
		return `{"content":[{"type":"text","text":"ok"}]}`, ""
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(func() {
		g.srv.CloseClientConnections()
		g.srv.Close()
	})
	return g
}

func (g *gateway) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Cursor    string          `json:"cursor"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.headers = append(g.headers, r.Header.Clone())
	g.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+gatewayToken {
		http.Error(w, "no bearer", http.StatusUnauthorized)
		return
	}
	answer := func(result, rpcError string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *msg.ID, result)
		if rpcError != "" {
			body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":%q}}`, *msg.ID, rpcError)
		}
		if g.stream && msg.Method == "tools/call" {
			// An event's data is one line.
			var one bytes.Buffer
			if err := json.Compact(&one, []byte(body)); err == nil {
				body = one.String()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}

	switch msg.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "session-1")
		answer(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"gateway","version":"test"}}`, "")
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		start := 0
		if msg.Params.Cursor != "" {
			_, _ = fmt.Sscanf(msg.Params.Cursor, "%d", &start)
		}
		end := len(g.tools)
		next := ""
		if g.pageSize > 0 && start+g.pageSize < end {
			end = start + g.pageSize
			next = fmt.Sprintf(`,"nextCursor":"%d"`, end)
		}
		var items []string
		for _, name := range g.tools[start:end] {
			items = append(items, fmt.Sprintf(`{"name":%q,"description":"The %s tool.","inputSchema":{"type":"object","properties":{"path":{"type":"string"}}}}`, name, name))
		}
		answer(fmt.Sprintf(`{"tools":[%s]%s}`, strings.Join(items, ","), next), "")
	case "tools/call":
		g.mu.Lock()
		g.calls = append(g.calls, msg.Params.Name+" "+string(msg.Params.Arguments))
		g.mu.Unlock()
		answer(g.call(r.Context(), msg.Params.Name, msg.Params.Arguments))
	default:
		answer("", "no such method")
	}
}

// run runs the harness's one-shot mode and returns its exit code, its
// events and what it wrote to stderr.
type runOpts struct {
	dir          string // the session folder; a new one when empty
	conversation string
	task         string
	maxSteps     int
	ctx          context.Context
	configEdit   func(map[string]any)
}

func run(t *testing.T, model *scriptedmodel.Server, g *gateway, o runOpts) (int, []proto.Event, string) {
	t.Helper()
	if o.dir == "" {
		o.dir = t.TempDir()
	}
	if o.conversation == "" {
		o.conversation = "run-1"
	}
	if o.task == "" {
		o.task = "Read a.txt and say what it holds."
	}
	if o.ctx == nil {
		o.ctx = context.Background()
	}
	cfg := map[string]any{
		"version": proto.Version,
		"model": map[string]any{
			"api": "anthropic", "name": "claude-test", "baseUrl": model.URL(),
			"keyEnv": "MODEL_KEY", "maxOutputTokens": 1024, "contextWindow": 200000,
		},
		"system":     "You are a careful agent.",
		"gateway":    map[string]any{"url": g.srv.URL + "/mcp", "tokenEnv": "MCP_GATEWAY_TOKEN"},
		"sessionDir": o.dir,
	}
	if o.maxSteps > 0 {
		cfg["maxSteps"] = o.maxSteps
	}
	if o.configEdit != nil {
		o.configEdit(cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "harness.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(name string) string {
		return map[string]string{"MODEL_KEY": modelKey, "MCP_GATEWAY_TOKEN": gatewayToken}[name]
	}
	var stdout, stderr bytes.Buffer
	code := harness.Run(o.ctx, []string{"--config", path, "--conversation", o.conversation}, strings.NewReader(o.task), &stdout, &stderr, env)

	var events []proto.Event
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev proto.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("stdout line is not an event: %q", sc.Text())
		}
		events = append(events, ev)
	}
	for i, ev := range events {
		if ev.V != proto.Version || ev.Seq != int64(i+1) {
			t.Fatalf("event %d has v %d seq %d", i+1, ev.V, ev.Seq)
		}
	}
	return code, events, stderr.String()
}

func types(events []proto.Event) string {
	var out []string
	for _, ev := range events {
		// Pieces of one text or reasoning stream count once.
		if n := len(out); n > 0 && out[n-1] == ev.Type && (ev.Type == proto.TypeText || ev.Type == proto.TypeReasoning) {
			continue
		}
		out = append(out, ev.Type)
	}
	return strings.Join(out, " ")
}

func last(t *testing.T, events []proto.Event, typ string) proto.Event {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return events[i]
		}
	}
	t.Fatalf("no %s event in: %s", typ, types(events))
	return proto.Event{}
}

func joined(events []proto.Event, typ string) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == typ {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func TestOneRunEndToEnd(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "toolu_1", Name: "fs_read", Arguments: `{"path":"a.txt"}`}).
			WithText("I will read it.").WithReasoning("The file comes first.").
			WithUsage(scriptedmodel.Usage{Input: 40, CacheWrite: 900, Output: 30}),
		scriptedmodel.Text("It holds one word: hello.").
			WithUsage(scriptedmodel.Usage{Input: 12, CacheRead: 940, Output: 9}),
	)
	g := newGateway(t, "fs_write", "fs_read")
	g.call = func(_ context.Context, name string, args json.RawMessage) (string, string) {
		return `{"content":[{"type":"text","text":"hello"}]}`, ""
	}
	dir := t.TempDir()

	code, events, stderr := run(t, model, g, runOpts{dir: dir})
	if code != harness.ExitAnswered {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got, want := types(events), "ready reasoning text usage tool_call tool_result text usage turn_end"; got != want {
		t.Fatalf("events: %s\nwant:   %s", got, want)
	}
	if ready := events[0]; strings.Join(ready.Tools, ",") != "fs_read,fs_write" || ready.Conversation != "run-1" {
		t.Errorf("ready: %+v", ready)
	}
	if got := joined(events, proto.TypeReasoning); got != "The file comes first." {
		t.Errorf("reasoning %q", got)
	}
	if got := joined(events, proto.TypeText); got != "I will read it.It holds one word: hello." {
		t.Errorf("text %q", got)
	}
	var call, result proto.Event
	var usages []proto.Usage
	for _, ev := range events {
		switch ev.Type {
		case proto.TypeToolCall:
			call = ev
		case proto.TypeToolResult:
			result = ev
		case proto.TypeUsage:
			usages = append(usages, *ev.Usage)
		}
	}
	if call.CallID != "toolu_1" || call.Tool != "fs_read" || string(call.Arguments) != `{"path":"a.txt"}` {
		t.Errorf("tool_call: %+v", call)
	}
	if result.CallID != "toolu_1" || result.Tool != "fs_read" || result.Text != "hello" || result.Failed {
		t.Errorf("tool_result: %+v", result)
	}
	want := []proto.Usage{
		{Input: 40, CacheWrite: 900, Output: 30, ContextTokens: 940, ContextLimit: 200000},
		{Input: 12, CacheRead: 940, Output: 9, ContextTokens: 952, ContextLimit: 200000},
	}
	if len(usages) != 2 || usages[0] != want[0] || usages[1] != want[1] {
		t.Errorf("usage %+v\nwant  %+v", usages, want)
	}
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndAnswered || end.Text != "It holds one word: hello." || end.Conversation != "run-1" {
		t.Errorf("turn_end: %+v", end)
	}

	// What the model was sent.
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model requests, want 2", len(reqs))
	}
	first, second := reqs[0], reqs[1]
	if first.Path != "/v1/messages" || first.Header.Get("x-api-key") != modelKey || first.Header.Get("anthropic-version") == "" {
		t.Errorf("request: path %q, headers %v", first.Path, first.Header)
	}
	if first.Model != "claude-test" || !first.Stream || first.MaxTokens != 1024 || first.System != "You are a careful agent." {
		t.Errorf("request: %+v", first)
	}
	if strings.Join(first.Tools, ",") != "fs_read,fs_write" {
		t.Errorf("the model was offered %v, want exactly the gateway's tools in name order", first.Tools)
	}
	if len(first.Messages) != 1 || first.Messages[0].Role != "user" || first.Messages[0].Text != "Read a.txt and say what it holds." {
		t.Errorf("first request's messages: %+v", first.Messages)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("second request has %d messages, want 3", len(second.Messages))
	}
	if m := second.Messages[1]; m.Role != "assistant" || m.Text != "I will read it." || m.Reasoning != "The file comes first." || len(m.Calls) != 1 || m.Calls[0].ID != "toolu_1" {
		t.Errorf("the reply sent back: %+v", m)
	}
	if !strings.Contains(string(second.Messages[1].Raw), `"signature":"signature-1"`) {
		t.Errorf("the reasoning went back without its signature: %s", second.Messages[1].Raw)
	}
	if m := second.Messages[2]; m.Role != "user" || len(m.Results) != 1 || m.Results[0] != (scriptedmodel.Result{CallID: "toolu_1", Text: "hello"}) {
		t.Errorf("the result sent back: %+v", m)
	}
	if err := scriptedmodel.PrefixOf(first, second); err != nil {
		t.Errorf("the second request does not continue the first: %v", err)
	}
	for _, r := range reqs {
		if n := strings.Count(string(r.Body), `"cache_control"`); n < 2 || n > 4 {
			t.Errorf("a request carries %d cache markers, want 2 to 4", n)
		}
	}

	// What the gateway was sent.
	if got := g.called(); len(got) != 1 || got[0] != `fs_read {"path":"a.txt"}` {
		t.Errorf("gateway calls: %v", got)
	}
	g.mu.Lock()
	lastHeader := g.headers[len(g.headers)-1]
	g.mu.Unlock()
	if lastHeader.Get("Mcp-Session-Id") != "session-1" || lastHeader.Get("Mcp-Protocol-Version") != "2025-06-18" {
		t.Errorf("the call did not carry the session: %v", lastHeader)
	}

	// What was recorded.
	raw, err := os.ReadFile(filepath.Join(dir, "run-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e convo.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		kinds = append(kinds, e.Kind)
	}
	if got, want := strings.Join(kinds, " "), "header user assistant usage call result assistant usage"; got != want {
		t.Errorf("log: %s\nwant: %s", got, want)
	}
	if strings.Contains(string(raw), modelKey) || strings.Contains(string(raw), gatewayToken) {
		t.Error("the log holds a credential")
	}
}

func TestACutCallIsNeverRun(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.CutToolCall("fs_write", `{"path":"report.md","content":"# Fin`),
		scriptedmodel.Text("Written in two parts instead."),
	)
	g := newGateway(t, "fs_write")
	dir := t.TempDir()

	code, events, _ := run(t, model, g, runOpts{dir: dir})
	if code != harness.ExitIncomplete {
		t.Errorf("exit %d, want %d", code, harness.ExitIncomplete)
	}
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndAnswerCut {
		t.Errorf("turn_end: %+v", end)
	}
	if strings.Contains(types(events), proto.TypeToolCall) {
		t.Errorf("a cut call was reported as run: %s", types(events))
	}
	if got := g.called(); len(got) != 0 {
		t.Fatalf("a cut call reached the gateway: %v", got)
	}

	// The conversation carries on: the model is told the call was not run.
	code, _, stderr := run(t, model, g, runOpts{dir: dir, task: "Go on."})
	if code != harness.ExitAnswered {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	reqs := model.Requests()
	resumed := reqs[len(reqs)-1]
	if len(resumed.Messages) != 3 {
		t.Fatalf("resumed request has %d messages, want 3", len(resumed.Messages))
	}
	m := resumed.Messages[2]
	if len(m.Results) != 1 || !m.Results[0].IsError || !strings.Contains(m.Results[0].Text, "output limit") || m.Text != "Go on." {
		t.Errorf("the message after the cut call: %+v", m)
	}
	if !strings.Contains(string(resumed.Messages[1].Raw), `"input":{}`) {
		t.Errorf("the cut call went back with its half arguments: %s", resumed.Messages[1].Raw)
	}
	if err := scriptedmodel.PrefixOf(reqs[0], resumed); err != nil {
		t.Errorf("the resumed request does not continue the first: %v", err)
	}
	if got := g.called(); len(got) != 0 {
		t.Errorf("the cut call was run on the second start: %v", got)
	}
}

func TestACallInterruptedMidwayIsNotRunAgain(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "toolu_1", Name: "run_command", Arguments: `{"command":"make deploy"}`}),
		scriptedmodel.Text("The deploy was interrupted; I did not repeat it."),
	)
	g := newGateway(t, "run_command")
	started := make(chan struct{})
	g.call = func(ctx context.Context, _ string, _ json.RawMessage) (string, string) {
		close(started)
		<-ctx.Done()
		return `{"content":[]}`, ""
	}
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	code, events, _ := run(t, model, g, runOpts{dir: dir, ctx: ctx})
	if code != harness.ExitFailed {
		t.Errorf("exit %d, want %d", code, harness.ExitFailed)
	}
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndError {
		t.Errorf("turn_end: %+v", end)
	}

	g.call = func(context.Context, string, json.RawMessage) (string, string) {
		return `{"content":[{"type":"text","text":"deployed"}]}`, ""
	}
	code, _, stderr := run(t, model, g, runOpts{dir: dir, task: "The run was interrupted. Carry on."})
	if code != harness.ExitAnswered {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got := g.called(); len(got) != 1 {
		t.Fatalf("the interrupted call was sent %d times, want once", len(got))
	}
	reqs := model.Requests()
	m := reqs[len(reqs)-1].Messages[2]
	if len(m.Results) != 1 || !m.Results[0].IsError || m.Results[0].Text != convo.Interrupted {
		t.Errorf("the interrupted call's result: %+v", m.Results)
	}
	if m.Text != "The run was interrupted. Carry on." {
		t.Errorf("the new prompt: %q", m.Text)
	}
}

func TestModelFailuresAreReportedWithTheirClass(t *testing.T) {
	cases := []struct {
		name   string
		step   scriptedmodel.Step
		class  string
		status int
		words  string
		retry  int
	}{
		{"overloaded", scriptedmodel.Status(529, "Overloaded"), proto.ClassCapacity, 529, "Overloaded", 0},
		{"unavailable", scriptedmodel.Status(503, "Service unavailable"), proto.ClassCapacity, 503, "Service unavailable", 0},
		{"too long", scriptedmodel.Overflow(scriptedmodel.AnthropicAPI, 200000), proto.ClassOverflow, 400, "prompt is too long: 250001 tokens > 200000 maximum", 0},
		{"rate limited", scriptedmodel.RateLimited(20 * time.Second), proto.ClassRateLimit, 429, "Rate limit reached", 20},
		{"bad key", scriptedmodel.Status(401, "invalid x-api-key"), proto.ClassAuth, 401, "invalid x-api-key", 0},
		{"no credits", scriptedmodel.Status(400, "Your credit balance is too low to access the Anthropic API."), proto.ClassCredits, 400, "credit balance is too low", 0},
		{"malformed", scriptedmodel.Status(400, "messages: field required"), proto.ClassRequest, 400, "field required", 0},
		{"server", scriptedmodel.Status(500, "Internal server error"), proto.ClassServer, 500, "Internal server error", 0},
		{"dropped", scriptedmodel.Drop("Working"), proto.ClassTransport, 0, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := scriptedmodel.New(t, c.step)
			g := newGateway(t, "fs_read")
			code, events, stderr := run(t, model, g, runOpts{})
			if code != harness.ExitFailed {
				t.Errorf("exit %d, want %d", code, harness.ExitFailed)
			}
			ev := last(t, events, proto.TypeError)
			if ev.Class != c.class || ev.Status != c.status || ev.RetryAfterSeconds != c.retry || !strings.Contains(ev.Text, c.words) {
				t.Errorf("error event %+v\nwant class %q status %d retry %d words %q", ev, c.class, c.status, c.retry, c.words)
			}
			if end := events[len(events)-1]; end.Type != proto.TypeTurnEnd || end.Reason != proto.EndError {
				t.Errorf("the last event: %+v", end)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Error("nothing on stderr for a failed run")
			}
			if strings.Contains(stderr, modelKey) {
				t.Error("stderr holds the key")
			}
		})
	}
}

func TestToolFailuresAreResultsTheModelReads(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(
			scriptedmodel.Call{ID: "a", Name: "fs_read", Arguments: `{"path":"missing.txt"}`},
			scriptedmodel.Call{ID: "b", Name: "fetch", Arguments: `{"url":"https://example.com"}`},
			scriptedmodel.Call{ID: "c", Name: "shell", Arguments: `{"command":"ls"}`},
			scriptedmodel.Call{ID: "d", Name: "fs_read", Arguments: `["a.txt"]`},
		),
		scriptedmodel.Text("Three of those failed."),
	)
	g := newGateway(t, "fs_read", "fetch")
	g.call = func(_ context.Context, name string, _ json.RawMessage) (string, string) {
		if name == "fetch" {
			return "", "the call was refused: its arguments look like a secret"
		}
		return `{"content":[{"type":"text","text":"no such file"}],"isError":true}`, ""
	}

	code, events, stderr := run(t, model, g, runOpts{})
	if code != harness.ExitAnswered {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got := g.called(); len(got) != 2 {
		t.Errorf("the gateway was sent %v, want only the two calls to tools it serves with object arguments", got)
	}
	results := map[string]proto.Event{}
	for _, ev := range events {
		if ev.Type == proto.TypeToolResult {
			results[ev.CallID] = ev
		}
	}
	for id, words := range map[string]string{"a": "no such file", "b": "look like a secret", "c": `no tool named "shell"`, "d": "not a JSON object"} {
		if ev := results[id]; !ev.Failed || !strings.Contains(ev.Text, words) {
			t.Errorf("result %s: %+v, want a failure saying %q", id, ev, words)
		}
	}
	sent := model.Requests()[1].Messages[2].Results
	if len(sent) != 4 || sent[0].CallID != "a" || sent[3].CallID != "d" {
		t.Fatalf("results sent to the model: %+v", sent)
	}
	for _, r := range sent {
		if !r.IsError {
			t.Errorf("result %s went to the model as a success", r.CallID)
		}
	}
}

func TestAGatewayThatStreamsPagesAndReturnsMoreThanText(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "toolu_1", Name: "read_input", Arguments: `{"name":"notes.txt"}`}),
		scriptedmodel.Text("Read."),
	)
	g := newGateway(t, "screenshot", "read_input", "fs_read", "fs_write", "fetch")
	g.pageSize = 2
	g.stream = true
	g.call = func(context.Context, string, json.RawMessage) (string, string) {
		return `{"content":[
			{"type":"text","text":"notes.txt, 2 lines"},
			{"type":"resource","resource":{"uri":"input://notes.txt","mimeType":"text/plain","text":"token: abc123\nline two"}},
			{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}`, ""
	}

	code, events, stderr := run(t, model, g, runOpts{})
	if code != harness.ExitAnswered {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got := strings.Join(events[0].Tools, ","); got != "fetch,fs_read,fs_write,read_input,screenshot" {
		t.Errorf("tools from a paged list: %s", got)
	}
	res := last(t, events, proto.TypeToolResult)
	if !res.Unchanged || res.Text != "" {
		t.Errorf("a result holding the person's own content was reported with its text: %+v", res)
	}
	sent := model.Requests()[1].Messages[2]
	if len(sent.Results) != 1 || sent.Results[0].Text != "notes.txt, 2 lines\ntoken: abc123\nline two" {
		t.Errorf("the model was sent %+v, want the resource's text whole", sent.Results)
	}
	if !strings.Contains(string(sent.Raw), `"type":"image"`) || !strings.Contains(string(sent.Raw), `"media_type":"image/png"`) {
		t.Errorf("the image did not reach the model as an image: %s", sent.Raw)
	}
}

func TestTheStepLimitEndsATurn(t *testing.T) {
	call := scriptedmodel.ToolCalls(scriptedmodel.Call{Name: "fs_read", Arguments: `{"path":"a.txt"}`})
	model := scriptedmodel.New(t, scriptedmodel.Repeat(3, call)...)
	g := newGateway(t, "fs_read")

	code, events, _ := run(t, model, g, runOpts{maxSteps: 3})
	if code != harness.ExitIncomplete {
		t.Errorf("exit %d, want %d", code, harness.ExitIncomplete)
	}
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndMaxSteps {
		t.Errorf("turn_end: %+v", end)
	}
	if n := len(model.Requests()); n != 3 {
		t.Errorf("%d model requests, want 3", n)
	}
}

func TestNothingIsSentOnAConfigurationThatCannotBeUsed(t *testing.T) {
	cases := map[string]func(map[string]any){
		"an unknown field":         func(c map[string]any) { c["tools"] = []string{"shell"} },
		"a second tool source":     func(c map[string]any) { c["gateway"].(map[string]any)["also"] = "http://other/mcp" },
		"another version":          func(c map[string]any) { c["version"] = proto.Version + 1 },
		"an API it does not speak": func(c map[string]any) { c["model"].(map[string]any)["api"] = "telepathy" },
		"no gateway":               func(c map[string]any) { delete(c, "gateway") },
		"no key variable":          func(c map[string]any) { c["model"].(map[string]any)["keyEnv"] = "" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			model := scriptedmodel.New(t)
			g := newGateway(t, "fs_read")
			code, events, stderr := run(t, model, g, runOpts{configEdit: edit})
			if code != harness.ExitUsage {
				t.Errorf("exit %d, want %d", code, harness.ExitUsage)
			}
			if len(events) != 0 || len(model.Requests()) != 0 || len(g.headers) != 0 {
				t.Errorf("something was sent: %d events, %d model requests, %d gateway requests", len(events), len(model.Requests()), len(g.headers))
			}
			if !strings.Contains(stderr, "harness configuration") {
				t.Errorf("stderr: %s", stderr)
			}
		})
	}

	t.Run("no task", func(t *testing.T) {
		model := scriptedmodel.New(t)
		g := newGateway(t, "fs_read")
		code, _, stderr := run(t, model, g, runOpts{task: " \n"})
		if code != harness.ExitUsage || !strings.Contains(stderr, "no task") || len(model.Requests()) != 0 {
			t.Errorf("exit %d, stderr %q, %d requests", code, stderr, len(model.Requests()))
		}
	})
}

func TestAGatewayThatRefusesTheBearerEndsTheRunBeforeTheModel(t *testing.T) {
	model := scriptedmodel.New(t)
	g := newGateway(t, "fs_read")
	code, events, _ := run(t, model, g, runOpts{configEdit: func(c map[string]any) {
		c["gateway"].(map[string]any)["tokenEnv"] = "SOME_OTHER_VARIABLE"
	}})
	if code != harness.ExitFailed {
		t.Errorf("exit %d, want %d", code, harness.ExitFailed)
	}
	if ev := last(t, events, proto.TypeError); ev.Class != proto.ClassGateway || !strings.Contains(ev.Text, "401") {
		t.Errorf("error event: %+v", ev)
	}
	if len(model.Requests()) != 0 {
		t.Error("the model was reached with no tools")
	}
}
