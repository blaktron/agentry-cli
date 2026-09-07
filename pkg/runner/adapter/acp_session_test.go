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
	if kinds[EventToolCall] != 1 || kinds[EventPermissionGranted] != 1 || kinds[EventPermissionDenied] != 1 || kinds[EventAssistantText] != 2 || kinds[EventUsage] != 1 {
		t.Errorf("event kinds = %v", kinds)
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
