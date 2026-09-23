package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "fs_list", "status": "pending"}}})
			ask := func(id int, tool, want string) {
				write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": map[string]any{"sessionId": "sess-1", "toolCall": map[string]any{"title": tool, "toolCallId": "c1", "kind": "execute"}, "options": []any{map[string]any{"optionId": "y", "kind": "allow_once"}, map[string]any{"optionId": "ya", "kind": "allow_always"}, map[string]any{"optionId": "n", "kind": "reject_once"}}}})
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
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed"}}})
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
// the rest, and both appear in the event stream (DR-15, agentry-cli#61).
func TestACPSessionDrivesOneTurnAndAnswersPermissions(t *testing.T) {
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	go fakeACP(t, toHarnessR, fromHarnessW)

	ad, _ := Get(DefaultName)
	drv, err := ad.Session(context.Background(), pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, SessionOptions{GatewayTools: []string{"fs_list", "write_report"}})
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
	if kinds[EventToolCall] != 1 || kinds[EventPermissionGranted] != 1 || kinds[EventPermissionDenied] != 1 || kinds[EventAssistantText] != 2 || kinds[EventUsage] != 3 {
		t.Errorf("event kinds = %v", kinds)
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
	for _, ev := range events {
		if ev.Kind == EventPermissionGranted && ev.Text != "fs_list" {
			t.Errorf("granted event names %q, want fs_list", ev.Text)
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
// usage_update carries `used` and `size` and nothing else, as recorded on
// the dev host on 2026-09-14 (run 01cac818, fixtures/hello-chat@1.0.0),
// whose envelope then reported costUsd 0 after three turns of real model
// calls. A harness that does price the session reaches the engine's
// high-water mark; one that does not leaves CostUSD at zero, which the
// envelope explains rather than reporting as a free session
// (agentry-cli#222).
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
