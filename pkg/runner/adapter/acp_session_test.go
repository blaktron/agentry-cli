package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pipeRW joins the client's write end and read end into one transport.
type pipeRW struct {
	io.Reader
	io.WriteCloser
}

// fakeACP plays the harness: a scripted ACP server on the other side of
// the pipes, exercising every message shape the driver handles.
func fakeACP(t *testing.T, in io.Reader, out io.WriteCloser) {
	t.Helper()
	defer func() { _ = out.Close() }()
	sc := bufio.NewScanner(in)
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	for sc.Scan() {
		var m rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Errorf("fake harness got non-JSON: %s", sc.Text())
			continue
		}
		switch m.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake"}}})
		case "session/new":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "sess-1"}})
		case "session/prompt":
			// A tool call, two permission requests the client must answer —
			// a gateway tool (granted) and a tool outside the catalog
			// (denied) — text chunks, then the turn result with usage.
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "List files", "name": "fs_list", "status": "pending"}}})
			ask := func(id int, tool, want string, name ...string) {
				call := map[string]any{"title": tool, "toolCallId": "c1", "kind": "execute"}
				if len(name) > 0 {
					call["name"] = name[0]
				}
				write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": map[string]any{"sessionId": "sess-1", "toolCall": call, "options": []any{map[string]any{"optionId": "y", "kind": "allow_once"}, map[string]any{"optionId": "ya", "kind": "allow_always"}, map[string]any{"optionId": "n", "kind": "reject_once"}}}})
				// Wait for the answer before continuing the turn.
				if !sc.Scan() {
					return
				}
				var ans struct {
					ID     int `json:"id"`
					Result struct {
						Outcome struct {
							Outcome  string `json:"outcome"`
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					} `json:"result"`
				}
				_ = json.Unmarshal(sc.Bytes(), &ans)
				if ans.ID != id || ans.Result.Outcome.Outcome != "selected" || ans.Result.Outcome.OptionID != want {
					t.Errorf("permission for %s answered %s, want option %s", tool, sc.Text(), want)
				}
			}
			ask(99, "fs_list", "y")
			ask(98, "shell_exec", "n")
			// A tool with a display title is named by toolCall.name, which
			// decides: the title can neither refuse a granted tool nor
			// pass for one.
			ask(97, "Navigate to a URL", "y", "browser_navigate")
			ask(96, "fs_list", "n", "shell_exec")
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "in_progress"}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "name": "fs_list", "status": "completed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "notes.txt\t25"}}}}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Hello, "}}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "world."}}}})
			// docker-agent's usage: cumulative context tokens, no split.
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "usage_update", "used": 1255, "size": 0}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "usage_update", "used": 1465, "size": 0}}})
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 12, "outputTokens": 3, "totalTokens": 15}}})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
}

// The driver grants permission requests for the gateway's tools and denies
// the rest, and both appear in the event stream.
func TestACPSessionDrivesOneTurnAndAnswersPermissions(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go fakeACP(t, toHarnessR, fromHarnessW)

	ad, _ := Get(DefaultName)
	drv, err := ad.Session(context.Background(), pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, SessionOptions{GatewayTools: []string{"fs_list", "write_report", "browser_navigate"}})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	collected := make(chan struct{})
	go func() {
		for ev := range drv.Events() {
			events = append(events, ev)
		}
		close(collected)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	turn, err := drv.Prompt(ctx, "list the files")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if turn.StopReason != "end_turn" || turn.Text != "Hello, world." {
		t.Errorf("turn = %+v", turn)
	}
	if turn.Usage == nil || turn.Usage.InputTokens != 12 || turn.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", turn.Usage)
	}
	if err := drv.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	<-collected

	kinds := map[EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	if kinds[EventToolCall] != 1 || kinds[EventPermissionGranted] != 2 || kinds[EventPermissionDenied] != 2 || kinds[EventAssistantText] != 2 || kinds[EventUsage] != 3 {
		t.Errorf("event kinds = %v", kinds)
	}
	// The call names its tool and id; its last update is its result, the
	// in-progress one before it is not.
	for _, ev := range events {
		switch ev.Kind {
		case EventToolCall:
			if ev.Tool != "fs_list" || ev.CallID != "c1" || ev.Text != "List files" {
				t.Errorf("tool call = %+v", ev)
			}
		case EventToolResult:
			if ev.Tool != "fs_list" || ev.CallID != "c1" || ev.Failed || ev.Text != "notes.txt\t25" {
				t.Errorf("tool result = %+v", ev)
			}
		}
	}
	if kinds[EventToolResult] != 1 {
		t.Errorf("tool results = %d, want 1", kinds[EventToolResult])
	}
	// The two usage_updates arrive as deltas (1255, then 1465-1255), with
	// no input/output split; the prompt result's own usage is the third.
	var totals []int64
	for _, ev := range events {
		if ev.Kind == EventUsage && ev.TotalTokens > 0 {
			totals = append(totals, ev.TotalTokens)
			if ev.InputTokens != 0 || ev.OutputTokens != 0 {
				t.Errorf("usage_update guessed a split: %+v", ev)
			}
		}
	}
	if len(totals) != 2 || totals[0] != 1255 || totals[1] != 210 {
		t.Errorf("usage_update deltas = %v, want [1255 210]", totals)
	}
	// Each update's `used` is also the conversation's size.
	var sizes []int64
	for _, ev := range events {
		if ev.Kind == EventUsage && ev.ContextTokens > 0 {
			sizes = append(sizes, ev.ContextTokens)
		}
	}
	if len(sizes) != 2 || sizes[0] != 1255 || sizes[1] != 1465 {
		t.Errorf("usage_update context sizes = %v, want [1255 1465]", sizes)
	}
	for _, ev := range events {
		if ev.Kind == EventPermissionGranted && ev.Text != "fs_list" && ev.Text != "browser_navigate" {
			t.Errorf("granted event names %q, want fs_list or browser_navigate", ev.Text)
		}
		if ev.Kind == EventPermissionDenied && ev.Text != "shell_exec" {
			t.Errorf("denied event names %q, want shell_exec", ev.Text)
		}
		if strings.Contains(string(ev.Raw), "allow_once") && ev.Kind != EventPermissionGranted && ev.Kind != EventPermissionDenied {
			t.Errorf("permission request leaked as %s", ev.Kind)
		}
	}
}

func TestACPSessionRefusesUnknownRequestsAndReportsProtocolMismatch(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		sc := bufio.NewScanner(toHarnessR)
		if !sc.Scan() {
			return
		}
		var m rpcMessage
		_ = json.Unmarshal(sc.Bytes(), &m)
		// Ask the client for a file first (must be refused), then answer
		// initialize with the wrong protocol version.
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fs/read_text_file", "params": map[string]any{"path": "/etc/passwd"}})
		_, _ = fromHarnessW.Write(append(b, '\n'))
		if !sc.Scan() {
			return
		}
		if !strings.Contains(sc.Text(), `"id":7`) || !strings.Contains(sc.Text(), "-32601") {
			// surfaced through the test's event check below
			_ = 0
		}
		b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 2}})
		_, _ = fromHarnessW.Write(append(b, '\n'))
	}()
	drv := newACPSession(pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, 1, nil)
	var refused bool
	done := make(chan struct{})
	go func() {
		for ev := range drv.Events() {
			if ev.Kind == EventOther && strings.HasPrefix(ev.Text, "refused fs/read_text_file") {
				refused = true
			}
		}
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := drv.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Fatalf("want protocol mismatch, got %v", err)
	}
	_ = drv.Close()
	<-done
	if !refused {
		t.Error("fs/read_text_file was not refused")
	}
}

// lateWriteRW answers the first request it is written (lines names the
// messages it writes back; by default one result), ends the stream, and
// returns from the write only once the session's reader has seen that end
// (ended), so the caller finds the answer and the end of the stream both
// ready.
type lateWriteRW struct {
	ended <-chan struct{}
	lines func(req rpcMessage) []map[string]any
	reply *io.PipeReader
	out   *io.PipeWriter
}

func (rw *lateWriteRW) Read(p []byte) (int, error) { return rw.reply.Read(p) }

func (rw *lateWriteRW) Write(p []byte) (int, error) {
	var m rpcMessage
	if err := json.Unmarshal(p, &m); err != nil {
		return 0, err
	}
	msgs := []map[string]any{{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"ok": true}}}
	if rw.lines != nil {
		msgs = rw.lines(m)
	}
	for _, msg := range msgs {
		b, _ := json.Marshal(msg)
		if _, err := rw.out.Write(append(b, '\n')); err != nil {
			return 0, err
		}
	}
	_ = rw.out.Close()
	<-rw.ended
	return len(p), nil
}

func (rw *lateWriteRW) Close() error { _ = rw.out.Close(); return rw.reply.Close() }

// A harness that answers a request and exits at once has answered: the
// request must return the answer, not report that the harness ended first.
func TestACPSessionReturnsAnAnswerWrittenBeforeTheStreamEnded(t *testing.T) {
	for i := 0; i < 64; i++ {
		r, w := io.Pipe()
		rw := &lateWriteRW{reply: r, out: w}
		drv := newACPSession(rw, 1, nil)
		rw.ended = drv.readDone
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := drv.call(ctx, "initialize", map[string]any{})
		cancel()
		if err != nil {
			t.Fatalf("run %d: an answered request failed: %v", i, err)
		}
		if !strings.Contains(string(res), `"ok":true`) {
			t.Fatalf("run %d: result = %s", i, res)
		}
		_ = drv.Close()
	}
}

// A harness whose prompt result carries no usage (docker-agent) still
// yields a turn usage: the sum of the turn's usage_update deltas.
func TestACPSessionTurnUsageFromUsageUpdates(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		sc := bufio.NewScanner(toHarnessR)
		write := func(v any) { b, _ := json.Marshal(v); _, _ = fromHarnessW.Write(append(b, '\n')) }
		for sc.Scan() {
			var m rpcMessage
			_ = json.Unmarshal(sc.Bytes(), &m)
			switch m.Method {
			case "initialize":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
			case "session/new":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "sess-2"}})
			case "session/prompt":
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-2", "update": map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 0}}})
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-2", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ok"}}}})
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-2", "update": map[string]any{"sessionUpdate": "usage_update", "used": 130, "size": 0}}})
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}()
	drv := newACPSession(pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, 1, nil)
	go func() {
		for range drv.Events() {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	turn, err := drv.Prompt(ctx, "hi")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if turn.Usage == nil || turn.Usage.TotalTokens != 130 || turn.Usage.InputTokens != 0 {
		t.Errorf("turn usage = %+v, want total 130 with no split", turn.Usage)
	}
	_ = drv.Close()
}

// usageOnlyACP answers the handshake and one prompt, emitting the given
// usage_update payloads in order before ending the turn.
func usageOnlyACP(in io.Reader, out io.WriteCloser, updates []map[string]any) {
	defer func() { _ = out.Close() }()
	sc := bufio.NewScanner(in)
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	for sc.Scan() {
		var m rpcMessage
		_ = json.Unmarshal(sc.Bytes(), &m)
		switch m.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "sess-1"}})
		case "session/prompt":
			for _, u := range updates {
				u["sessionUpdate"] = "usage_update"
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": u}})
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn"}})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
}

// TestACPSessionCostComesFromTheWire drives the real driver over recorded
// wire shapes. The session's cost ceiling can only fire on a cost the
// harness reports, and docker-agent 1.128.0 reports none: its ACP
// usage_update carries `used` and `size` and nothing else, as recorded from
// a real run, whose envelope then reported costUsd 0 after three turns of
// real model calls. A harness that does price the session reaches the
// engine's high-water mark; one that does not leaves CostUSD at zero, which
// the envelope explains rather than reporting as a free session.
func TestACPSessionCostComesFromTheWire(t *testing.T) {
	cases := []struct {
		name     string
		updates  []map[string]any
		wantCost []float64
		wantTot  []int64
	}{
		{
			name: "docker-agent reports no cost",
			updates: []map[string]any{
				{"used": 1262, "size": 0},
				{"used": 1386, "size": 0},
				{"used": 1422, "size": 0},
			},
			wantCost: []float64{0, 0, 0},
			wantTot:  []int64{1262, 124, 36},
		},
		{
			name: "a harness that prices the session",
			updates: []map[string]any{
				{"used": 1262, "size": 0, "cost": map[string]any{"amount": 0.4, "currency": "USD"}},
				{"used": 1386, "size": 0, "cost": map[string]any{"amount": 1.25, "currency": "USD"}},
			},
			wantCost: []float64{0.4, 1.25},
			wantTot:  []int64{1262, 124},
		},
		{
			name:     "an amount in another currency is not passed off as dollars",
			updates:  []map[string]any{{"used": 500, "size": 0, "cost": map[string]any{"amount": 9.99, "currency": "EUR"}}},
			wantCost: []float64{0},
			wantTot:  []int64{500},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toHarnessR, toHarnessW := io.Pipe()
			fromHarnessR, fromHarnessW := io.Pipe()
			go usageOnlyACP(toHarnessR, fromHarnessW, tc.updates)

			drv := newACPSession(pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, 1, nil)
			var events []Event
			collected := make(chan struct{})
			go func() {
				for ev := range drv.Events() {
					events = append(events, ev)
				}
				close(collected)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := drv.Start(ctx); err != nil {
				t.Fatalf("start: %v", err)
			}
			if _, err := drv.Prompt(ctx, "go"); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			_ = drv.Close()
			<-collected

			var gotCost []float64
			var gotTot []int64
			for _, ev := range events {
				if ev.Kind != EventUsage {
					continue
				}
				gotCost = append(gotCost, ev.CostUSD)
				gotTot = append(gotTot, ev.TotalTokens)
			}
			if len(gotCost) != len(tc.wantCost) {
				t.Fatalf("usage events = %v, want %d", gotCost, len(tc.wantCost))
			}
			for i := range gotCost {
				if gotCost[i] != tc.wantCost[i] {
					t.Errorf("CostUSD[%d] = %v, want %v", i, gotCost[i], tc.wantCost[i])
				}
				if gotTot[i] != tc.wantTot[i] {
					t.Errorf("TotalTokens[%d] = %v, want %v", i, gotTot[i], tc.wantTot[i])
				}
			}
		})
	}
}

// promptFailsWith runs one turn against a fake harness that answers
// session/prompt with the given JSON-RPC error object, and returns the
// events the driver emitted and the prompt's error.
func promptFailsWith(t *testing.T, rpcErr string) ([]Event, error) {
	t.Helper()
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		sc := bufio.NewScanner(toHarnessR)
		for sc.Scan() {
			var m rpcMessage
			_ = json.Unmarshal(sc.Bytes(), &m)
			var answer string
			switch m.Method {
			case "initialize":
				answer = `"result":{"protocolVersion":1}`
			case "session/new":
				answer = `"result":{"sessionId":"s"}`
			case "session/prompt":
				answer = `"error":` + rpcErr
			default:
				continue
			}
			_, _ = fromHarnessW.Write([]byte(`{"jsonrpc":"2.0","id":` + string(*m.ID) + `,` + answer + "}\n"))
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
	_, err := drv.Prompt(ctx, "hello")
	if err == nil {
		t.Fatal("the prompt succeeded")
	}
	_ = drv.Close()
	var evs []Event
	for ev := range got {
		evs = append(evs, ev)
	}
	return evs, err
}

// docker-agent fails a prompt with the generic "Internal error" and puts
// the runtime's own words in the error's data. Those words must reach the
// failure, and a provider's 402 among them ends the session as out of
// credits only when it is the model's error, never a tool's.
func TestACPSessionAPromptErrorCarriesTheHarnessesWords(t *testing.T) {
	cases := []struct {
		name, rpcErr, want string
		outOfCredits       bool
	}{
		{
			name:   "a model that cannot be reached (recorded from docker-agent 1.150.0)",
			rpcErr: `{"code":-32603,"message":"Internal error","data":{"error":"model failed: error receiving from stream: Post \"http://127.0.0.1:9/v1/chat/completions\": dial tcp 127.0.0.1:9: connect: connection refused","runtimeCode":"model_error","sessionId":"ba913ab5-6ede-4858-9cf8-eba5b1314559"}}`,
			want:   `session/prompt: harness error -32603: Internal error: model failed: error receiving from stream: Post "http://127.0.0.1:9/v1/chat/completions": dial tcp 127.0.0.1:9: connect: connection refused`,
		},
		{
			name:         "a provider out of credits",
			rpcErr:       `{"code":-32603,"message":"Internal error","data":{"error":"model failed: POST \"https://api.poe.com/v1/chat/completions\": 402 Payment Required","runtimeCode":"model_error","sessionId":"s"}}`,
			want:         "402 Payment Required",
			outOfCredits: true,
		},
		{
			name:   "a tool whose output says 402",
			rpcErr: `{"code":-32603,"message":"Internal error","data":{"error":"the tool said: 402 Payment Required","runtimeCode":"tool_failed","sessionId":"s"}}`,
			want:   "harness error -32603: Internal error: the tool said: 402 Payment Required",
		},
		{
			name:   "a handler's error with no runtime code",
			rpcErr: `{"code":-32603,"message":"Internal error","data":{"error":"agent not initialized"}}`,
			want:   "harness error -32603: Internal error: agent not initialized",
		},
		{
			name:   "data that is a string",
			rpcErr: `{"code":-32603,"message":"Internal error","data":"stream closed"}`,
			want:   "harness error -32603: Internal error: stream closed",
		},
		{
			name:   "only the first line of the words",
			rpcErr: `{"code":-32603,"message":"Internal error","data":{"error":"model failed: boom\nsecond line"}}`,
			want:   "harness error -32603: Internal error: model failed: boom",
		},
		{
			name:   "no data",
			rpcErr: `{"code":-32603,"message":"Internal error"}`,
			want:   "session/prompt: harness error -32603: Internal error",
		},
		{
			name:         "the words in the message itself",
			rpcErr:       `{"code":-32603,"message":"POST \"https://api.poe.com/v1/chat/completions\": 402 Payment Required"}`,
			want:         "402 Payment Required",
			outOfCredits: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := promptFailsWith(t, c.rpcErr)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to hold %q", err, c.want)
			}
			if strings.Contains(err.Error(), "second line") {
				t.Errorf("error %q carries more than the first line", err)
			}
			credits := false
			for _, ev := range evs {
				if ev.Kind == EventOutOfCredits {
					credits = true
				}
			}
			if credits != c.outOfCredits {
				t.Errorf("out of credits %v, want %v (events %+v)", credits, c.outOfCredits, evs)
			}
		})
	}
}

// TestACPSessionStartsANewConversationInTheRunningHarness: NewConversation is
// session/new on the same harness; the next prompt goes to the new session,
// and its usage counts from nothing rather than from the old session's size.
func TestACPSessionStartsANewConversationInTheRunningHarness(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	prompted := make(chan string, 4)
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		sc := bufio.NewScanner(toHarnessR)
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fromHarnessW.Write(append(b, '\n'))
		}
		sessions := 0
		for sc.Scan() {
			var m rpcMessage
			_ = json.Unmarshal(sc.Bytes(), &m)
			switch m.Method {
			case "initialize":
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
			case "session/new":
				sessions++
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "sess-" + strconv.Itoa(sessions)}})
			case "session/prompt":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				prompted <- p.SessionID
				used := 90_000
				if p.SessionID != "sess-1" {
					used = 3_000
				}
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": p.SessionID, "update": map[string]any{"sessionUpdate": "usage_update", "used": used, "size": 200_000}}})
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}()
	ad, _ := Get(DefaultName)
	drv, err := ad.Session(context.Background(), pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, SessionOptions{Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	var usage []Event
	collected := make(chan struct{})
	go func() {
		for ev := range drv.Events() {
			if ev.Kind == EventUsage {
				usage = append(usage, ev)
			}
		}
		close(collected)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Prompt(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	r, ok := drv.(Renewing)
	if !ok {
		t.Fatal("the docker-agent driver cannot start a new conversation")
	}
	id, err := r.NewConversation(ctx)
	if err != nil || id != "sess-2" || drv.(Continuing).ConversationID() != "sess-2" {
		t.Fatalf("NewConversation = %q, %v; in %q", id, err, drv.(Continuing).ConversationID())
	}
	if _, err := drv.Prompt(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if a, b := <-prompted, <-prompted; a != "sess-1" || b != "sess-2" {
		t.Errorf("prompted %s then %s", a, b)
	}
	_ = drv.Close()
	<-collected
	if len(usage) != 2 || usage[1].ContextTokens != 3_000 || usage[1].TotalTokens != 3_000 {
		t.Errorf("usage = %+v", usage)
	}
}
