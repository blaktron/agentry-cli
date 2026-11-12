package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/internal/scriptedmodel"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/harness"
)

// A session on the runner's own harness, with nothing played: the plan is
// compiled for the adapter, the engine's session drives the adapter's
// driver, and the agent container is the harness's real session mode
// reading the plan's rendered configuration. Only what lies outside the
// runner is scripted: the model's server and the tool gateway.

// scriptedGateway is an MCP server over HTTP that lists tools and answers
// every call through call.
type scriptedGateway struct {
	srv   *httptest.Server
	tools []string
	call  func(ctx context.Context, name string) string

	mu     sync.Mutex
	called []string
}

func newScriptedGateway(t *testing.T, tools ...string) *scriptedGateway {
	g := &scriptedGateway{tools: tools, call: func(context.Context, string) string { return "ok" }}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "gateway", "version": "test"}}
		case "tools/list":
			var listed []map[string]any
			for _, name := range g.tools {
				listed = append(listed, map[string]any{"name": name, "description": "The " + name + " tool.", "inputSchema": map[string]any{"type": "object"}})
			}
			result = map[string]any{"tools": listed}
		case "tools/call":
			g.mu.Lock()
			g.called = append(g.called, msg.Params.Name)
			g.mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": g.call(r.Context(), msg.Params.Name)}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	t.Cleanup(func() {
		g.srv.CloseClientConnections()
		g.srv.Close()
	})
	return g
}

func (g *scriptedGateway) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.called...)
}

// agentryHarnessContainer plays the agent container of a plan compiled for
// the runner's own harness: the harness itself, on the configuration the
// run rendered, with the addresses a container would resolve on the run
// network replaced by the test's servers and the record kept in store.
func agentryHarnessContainer(t *testing.T, model *scriptedmodel.Server, g *scriptedGateway, store string) func(context.Context, dockerx.RunSpec, io.Reader, io.Writer, io.Writer) (int, error) {
	return func(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(r.Argv) < 4 || r.Argv[0] != "harness" || r.Argv[2] != "--config" {
			return 2, fmt.Errorf("not a harness command line: %v", r.Argv)
		}
		var plan, record bool
		config := ""
		for _, m := range r.Mounts {
			switch m.Target {
			case spec.PlanMount:
				plan = m.ReadOnly
				config = filepath.Join(m.Source, strings.TrimPrefix(r.Argv[3], spec.PlanMount+"/"))
			case spec.SessionMount:
				record = !m.ReadOnly
			}
		}
		if !plan || !record {
			return 2, fmt.Errorf("the harness's container mounts %+v, want its plan read-only and its record", r.Mounts)
		}
		raw, err := os.ReadFile(config) //nolint:gosec // the test's own run directory
		if err != nil {
			return 2, err
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return 2, err
		}
		cfg["model"].(map[string]any)["baseUrl"] = model.URL()
		cfg["gateway"].(map[string]any)["url"] = g.srv.URL + "/mcp"
		cfg["sessionDir"] = store
		edited, _ := json.Marshal(cfg)
		path := filepath.Join(t.TempDir(), "harness.json")
		if err := os.WriteFile(path, edited, 0o600); err != nil {
			return 2, err
		}
		env := func(name string) string { return r.Env[name] }
		args := append([]string{"--config", path}, r.Argv[4:]...)
		if r.Argv[1] == "serve" {
			return harness.Serve(ctx, args, stdin, stdout, stderr, env), nil
		}
		return harness.Run(ctx, args, stdin, stdout, stderr, env), nil
	}
}

// startAgentryHarnessSession starts a real session of the two-mode fixture
// on the runner's own harness. served are the tools the gateway lists
// beyond the plan's own grant.
func startAgentryHarnessSession(t *testing.T, model *scriptedmodel.Server, g *scriptedGateway, served ...string) (session.Session, Params, string) {
	t.Helper()
	p, f := sessionFixture(t)
	happyFake(f)
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.AgentryHarnessAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(p.RunRoot, "plan", ad.Name())
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local", AdapterName: ad.Name(),
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p.Plan, p.PlanDir, p.Adapter = plan, planDir, ad
	store := t.TempDir()
	f.Foreground["-probe"] = &dockerx.FakeForeground{Handler: probeListing(served...)}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: agentryHarnessContainer(t, model, g, store)}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return s, p, store
}

func TestASessionOnTheRunnersOwnHarness(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(
			scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`},
			scriptedmodel.Call{ID: "c2", Name: "shell_exec", Arguments: `{"command":"id"}`},
		).WithUsage(scriptedmodel.Usage{Input: 100, Output: 10}),
		scriptedmodel.Text("The note says hello. The token is sekret-value-1234.").WithUsage(scriptedmodel.Usage{Input: 30, CacheRead: 110, Output: 12}),
		scriptedmodel.Text("Two words.").WithUsage(scriptedmodel.Usage{Input: 8, CacheRead: 152, Output: 3}),
	)
	// The gateway lists a tool the run was never granted: the model may ask
	// for it, and the runner does not permit it.
	g := newScriptedGateway(t, "fs_read", "shell_exec")
	g.call = func(context.Context, string) string { return "hello from the workspace" }
	s, p, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	get, done := collect(s)
	ctx := context.Background()

	if err := s.Send(ctx, "what does the note say?"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if err := s.Send(ctx, "how many words is the greeting?"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	env, err := s.End(ctx, true)
	<-done
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if env.Status != "ok" || env.Harness.Adapter != adapter.AgentryHarnessAdapterName || env.Harness.Mode != string(manifest.ModeInteractive) {
		t.Errorf("envelope: status %q, harness %+v", env.Status, env.Harness)
	}
	// Every request's tokens, input cached or not; two tool calls asked for.
	if u := env.Usage; u.InputTokens != 100+140+160 || u.OutputTokens != 25 || u.Turns != 2 || u.UserTurns != 2 {
		t.Errorf("usage: %+v", u)
	}
	if string(env.Result) != `"Two words."` {
		t.Errorf("result: %s", env.Result)
	}

	var granted, denied, turns []string
	for _, ev := range get() {
		switch ev.Kind {
		case session.KindPermissionGranted:
			granted = append(granted, ev.Text)
		case session.KindPermissionDenied:
			denied = append(denied, ev.Text)
		case session.KindAssistantTurn:
			turns = append(turns, ev.Text)
		case session.KindError:
			t.Errorf("an error event: %+v", ev)
		}
		if strings.Contains(ev.Text, "sekret-value-1234") || strings.Contains(string(ev.Raw), "sekret-value-1234") {
			t.Errorf("an event carries the secret: %+v", ev)
		}
	}
	if strings.Join(granted, ",") != "fs_read" || strings.Join(denied, ",") != "shell_exec" {
		t.Errorf("granted %v, denied %v", granted, denied)
	}
	if len(turns) != 2 || !strings.HasPrefix(turns[0], "The note says hello.") || turns[1] != "Two words." {
		t.Errorf("assistant turns: %q", turns)
	}
	// The denied call never reached the gateway, its denial was the
	// model's to read, and the turn went on to its answer.
	if got := g.calls(); len(got) != 1 || got[0] != "fs_read" {
		t.Errorf("the gateway was sent: %v", got)
	}
	reqs := model.Requests()
	results := reqs[1].Messages[len(reqs[1].Messages)-1].Results
	if len(results) != 2 || results[0].IsError || !results[1].IsError || !strings.Contains(results[1].Text, "not permitted") {
		t.Errorf("the results the model read: %+v", results)
	}
	// One conversation: the second turn's request carries the first.
	if n := len(reqs[2].Messages); n != 5 {
		t.Errorf("the second turn's request holds %d messages, want 5", n)
	}
	// The receipts are the session's own.
	for _, name := range []string{"events.ndjson", "transcript.md"} {
		if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", name)); err != nil {
			t.Errorf("receipt %s: %v", name, err)
		}
	}
}

func TestASessionsEnvelopeCountsWhatItsHarnessDidToCarryTheConversation(t *testing.T) {
	read := scriptedmodel.ToolCalls(scriptedmodel.Call{Name: "fs_read", Arguments: `{"path":"notes.txt"}`})
	steps := scriptedmodel.Repeat(5, read)
	steps = append(steps,
		// The fifth identical call is answered with a message, and the
		// answer after it is cut off at the output limit.
		scriptedmodel.Cut("on and on and"),
		scriptedmodel.Text("The note says hello."),
		// The second turn's request is refused as too long, and the
		// conversation is summarised.
		scriptedmodel.Overflow(scriptedmodel.AnthropicAPI, 4096),
		scriptedmodel.Text("SUMMARY: the note was read five times and says hello."),
		scriptedmodel.Text("Still hello."),
	)
	model := scriptedmodel.New(t, steps...)
	g := newScriptedGateway(t, "fs_read")
	s, p, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	get, done := collect(s)
	ctx := context.Background()
	if err := s.Send(ctx, "what does the note say?"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if err := s.Send(ctx, "and now?"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	env, err := s.End(ctx, true)
	<-done
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	u := env.Usage
	if u.LoopNudges != 1 || u.LengthRecoveries != (receipt.LengthRecoveries{Answer: 1}) || u.ResultClears != 0 || u.Compactions != (receipt.Compactions{Overflow: 1}) {
		t.Errorf("usage: nudges %d, recoveries %+v, clears %d, compactions %+v", u.LoopNudges, u.LengthRecoveries, u.ResultClears, u.Compactions)
	}
	// The envelope on disk says the same, and each count is an event of
	// the session's too.
	raw, err := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	compact := func(member string) string {
		var b bytes.Buffer
		_ = json.Compact(&b, written.Usage[member])
		return b.String()
	}
	if compact("loopNudges") != "1" || compact("lengthRecoveries") != `{"answer":1}` || compact("compactions") != `{"overflow":1}` {
		t.Errorf("result.json usage: %s", raw)
	}
	if _, ok := written.Usage["resultClears"]; ok {
		t.Errorf("a count of nothing is written: %s", raw)
	}
	said := map[string]int{}
	for _, ev := range get() {
		var line struct {
			Code string `json:"code"`
		}
		if ev.Kind == session.KindOther && json.Unmarshal(ev.Raw, &line) == nil && line.Code != "" {
			said[line.Code]++
		}
	}
	if said["loop_nudge"] != 1 || said["length_recovery"] != 1 || said["compaction"] != 1 {
		t.Errorf("the session's events: %v", said)
	}
}

func TestAMessageSentDuringATurnJoinsItAfterTheCallsResult(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`}),
		scriptedmodel.Text("The note says hello, and today is not in it."),
		scriptedmodel.Text("Nothing more."),
	)
	g := newScriptedGateway(t, "fs_read")
	inCall, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.call = func(context.Context, string) string {
		once.Do(func() { close(inCall) })
		<-release
		return "hello from the workspace"
	}
	s, p, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	get, done := collect(s)
	ctx := context.Background()
	st, ok := s.(session.Steerer)
	if !ok {
		t.Fatal("the engine's session takes no message during a turn")
	}

	// Between turns there is no turn to join.
	if err := st.Steer(ctx, "too early"); !errors.Is(err, session.ErrNoTurn) {
		t.Fatalf("a message with no turn in flight: %v", err)
	}

	sent := make(chan error, 1)
	go func() { sent <- s.Send(ctx, "what does the note say?") }()
	<-inCall
	// While the tool runs: the person's message, and a file they add.
	if err := st.Steer(ctx, "also tell me whether it gives today's date"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	added := filepath.Join(t.TempDir(), "more.txt")
	if err := os.WriteFile(added, []byte("one more line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInput(ctx, added); err != nil {
		t.Fatalf("add during the turn: %v", err)
	}
	if err := st.Steer(ctx, "   "); err == nil {
		t.Error("a blank message was taken")
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatalf("the turn: %v", err)
	}

	// The next request carries both after the call's result, in the order
	// they were sent, the person's words unchanged.
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	msgs := reqs[1].Messages
	last := msgs[len(msgs)-1]
	if len(last.Results) != 1 || last.Results[0].Text != "hello from the workspace" {
		t.Fatalf("the request after the call ends with %+v", last)
	}
	said, notice := strings.Index(last.Text, "also tell me whether it gives today's date"), strings.Index(last.Text, "A new input file is available at /inputs/more.txt")
	if said < 0 || notice < said {
		t.Errorf("the message and the notice in the request after the call: %q", last.Text)
	}

	// The cap on the person's messages counts one sent this way: the
	// fixture allows three, and a turn and a message are two.
	second := make(chan error, 1)
	go func() { second <- s.Send(ctx, "anything else?") }()
	if err := <-second; err != nil {
		t.Fatalf("the second turn: %v", err)
	}
	env, err := s.End(ctx, false)
	<-done
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if env.Usage.UserTurns != 3 {
		t.Errorf("user messages counted: %d, want a turn, a message during it and a turn", env.Usage.UserTurns)
	}

	// The session's record: each message as it was sent, then where the
	// harness put them, before the turn's answer.
	var order []string
	for _, ev := range get() {
		switch ev.Kind {
		case session.KindUserTurn:
			order = append(order, "turn")
		case session.KindToolCall:
			order = append(order, "call")
		case session.KindSteer:
			if ev.Synthetic {
				order = append(order, "notice")
			} else {
				order = append(order, "steer:"+ev.Text)
			}
		case session.KindSteered:
			order = append(order, "steered")
		case session.KindAssistantTurn:
			order = append(order, "answer")
		}
	}
	want := "turn call steer:also tell me whether it gives today's date notice steered answer turn answer"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("the session's events:\n%s\nwant\n%s", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "transcript.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"## User, during the turn\n\nalso tell me whether it gives today's date", "## Runner, during the turn\n\nA new input file is available", "- what was sent during the turn joined the conversation here"} {
		if !strings.Contains(string(raw), line) {
			t.Errorf("the transcript lacks %q:\n%s", line, raw)
		}
	}
	events, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"kind":"steer"`) || !strings.Contains(string(events), `"kind":"steered"`) {
		t.Error("events.ndjson records no steer or no steered event")
	}
}

func TestTheCapOnAPersonsMessagesHoldsForOnesSentDuringATurn(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`}),
		scriptedmodel.Text("Done."),
	)
	g := newScriptedGateway(t, "fs_read")
	inCall, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.call = func(context.Context, string) string {
		once.Do(func() { close(inCall) })
		<-release
		return "ok"
	}
	s, _, _ := startAgentryHarnessSession(t, model, g, "fs_read") // three messages at most
	get, done := collect(s)
	ctx := context.Background()
	st := s.(session.Steerer)
	sent := make(chan error, 1)
	go func() { sent <- s.Send(ctx, "read the note") }()
	<-inCall
	for _, text := range []string{"second", "third"} {
		if err := st.Steer(ctx, text); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
	}
	err := st.Steer(ctx, "fourth")
	if err == nil || !strings.Contains(err.Error(), "limit of 3 messages") || errors.Is(err, session.ErrCannotSteer) {
		t.Errorf("a message over the cap: %v", err)
	}
	// The refusal ends nothing: the turn in flight is answered.
	close(release)
	if err := <-sent; err != nil {
		t.Fatalf("the turn: %v", err)
	}
	if ended, _ := s.Ended(); ended {
		t.Error("the refused message ended the session")
	}
	env, _ := s.End(ctx, false)
	<-done
	if env.Usage.UserTurns != 3 {
		t.Errorf("user messages counted: %d", env.Usage.UserTurns)
	}
	steers := 0
	for _, ev := range get() {
		if ev.Kind == session.KindSteer {
			steers++
		}
	}
	if steers != 2 {
		t.Errorf("%d steer events, want the two that were taken", steers)
	}
}

func TestAnInterruptStopsATurnOnTheRunnersOwnHarness(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"big.txt"}`}),
		scriptedmodel.Text("I had been stopped while reading; nothing was repeated."),
	)
	g := newScriptedGateway(t, "fs_read")
	reading := make(chan struct{})
	var once sync.Once
	g.call = func(ctx context.Context, _ string) string {
		once.Do(func() { close(reading) })
		<-ctx.Done()
		return ""
	}
	s, _, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	get, done := collect(s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	it, ok := s.(session.Interrupter)
	if !ok {
		t.Fatal("the session cannot stop a turn")
	}
	sent := make(chan error, 1)
	go func() { sent <- s.Send(ctx, "read big.txt") }()
	<-reading
	if err := it.Interrupt(ctx); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := <-sent; !errors.Is(err, session.ErrInterrupted) {
		t.Fatalf("the stopped turn: %v", err)
	}
	// The session is there for the next turn, on a whole conversation.
	if err := s.Send(ctx, "what happened?"); err != nil {
		t.Fatalf("the turn after the stop: %v", err)
	}
	if _, err := s.End(ctx, false); err != nil {
		t.Fatalf("end: %v", err)
	}
	<-done
	stopped := 0
	for _, ev := range get() {
		if ev.Kind == session.KindAssistantTurn && ev.StopReason == session.StopInterrupted {
			stopped++
		}
		if ev.Kind == session.KindError {
			t.Errorf("an error event: %+v", ev)
		}
	}
	if stopped != 1 {
		t.Errorf("%d stopped turns in the events, want 1", stopped)
	}
	req := model.Requests()[1]
	last := req.Messages[len(req.Messages)-1]
	if len(last.Results) != 1 || !last.Results[0].IsError || !strings.HasSuffix(last.Text, "what happened?") {
		t.Errorf("the request after the stop ends with %+v", last)
	}
	if got := g.calls(); len(got) != 1 {
		t.Errorf("the stopped call was sent %d times, want once", len(got))
	}
}

func TestASessionOnTheRunnersOwnHarnessEndsOutOfCredits(t *testing.T) {
	model := scriptedmodel.New(t, scriptedmodel.Status(402, "Insufficient credits to complete this request."))
	g := newScriptedGateway(t, "fs_read")
	s, _, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	_, done := collect(s)
	ctx := context.Background()
	_ = s.Send(ctx, "hello")
	deadline := time.After(10 * time.Second)
	for {
		if ended, reason := s.Ended(); ended {
			if reason != session.EndOutOfCredits {
				t.Fatalf("ended for %s", reason)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("running out of credits never ended the session")
		case <-time.After(20 * time.Millisecond):
		}
	}
	env, err := s.End(ctx, true)
	<-done
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || env.Reason == nil || env.Reason.Code != receipt.ReasonOutOfCredits ||
		!strings.Contains(err.Error(), "Insufficient credits") {
		t.Fatalf("end = %v (%+v)", err, env.Reason)
	}
}

// The session protocol is the session's other client: the same harness,
// driven by JSON lines.
func TestTheSessionProtocolDrivesTheRunnersOwnHarness(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`}),
		scriptedmodel.Text("The note says hello."),
	)
	g := newScriptedGateway(t, "fs_read")
	s, _, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	d := serveProtocol(t, context.Background(), s, protocol.Options{})
	d.send(`{"send":{"text":"what does the note say?"}}`)
	turn := d.until(isKind(session.KindAssistantTurn))
	if last := turn[len(turn)-1]; last.Text != "The note says hello." {
		t.Errorf("the turn: %+v", last)
	}
	d.send(`{"end":{"finish":true}}`)
	d.wait()
	if d.err != nil || d.env == nil || d.env.Status != "ok" || string(d.env.Result) != `"The note says hello."` {
		t.Fatalf("env %+v, err %v", d.env, d.err)
	}
	calls := 0
	for _, ev := range d.seen {
		if ev.Kind == session.KindToolCall {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("%d tool calls in the protocol's events, want 1: %v", calls, eventKinds(d.seen))
	}
}

func TestTheSessionProtocolSteersATurnOnTheRunnersOwnHarness(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"notes.txt"}`}),
		scriptedmodel.Text("The note says hello, on no date."),
	)
	g := newScriptedGateway(t, "fs_read")
	release := make(chan struct{})
	g.call = func(context.Context, string) string {
		<-release
		return "hello from the workspace"
	}
	s, _, _ := startAgentryHarnessSession(t, model, g, "fs_read")
	d := serveProtocol(t, context.Background(), s, protocol.Options{})

	// Before any turn there is none to join.
	d.send(`{"steer":{"text":"too early"}}`)
	refused := d.until(func(ev protocol.Event) bool { return ev.Refused != "" })
	if last := refused[len(refused)-1]; last.Refused != protocol.RefusedNoTurn {
		t.Fatalf("a steer with no turn in flight: %+v", last)
	}

	d.send(`{"send":{"text":"what does the note say?"}}`)
	d.until(isKind(session.KindToolCall))
	d.send(`{"steer":{"text":"and is there a date in it?"}}`)
	said := d.until(isKind(session.KindSteer))
	if last := said[len(said)-1]; last.Text != "and is there a date in it?" || last.Synthetic {
		t.Fatalf("the steer event: %+v", last)
	}
	close(release)
	rest := d.until(isKind(session.KindAssistantTurn))
	joined := false
	for _, ev := range rest {
		joined = joined || ev.Kind == session.KindSteered
	}
	if !joined {
		t.Errorf("no steered event before the turn's answer: %v", eventKinds(rest))
	}
	if req := model.Requests()[1]; !strings.Contains(string(req.Body), "and is there a date in it?") {
		t.Error("the request after the call does not carry the message")
	}
	d.send(`{"end":{"finish":false}}`)
	d.wait()
	if d.err != nil || d.env == nil || d.env.Usage.UserTurns != 2 {
		t.Fatalf("env %+v, err %v", d.env, d.err)
	}
}
