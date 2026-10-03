package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
)

// The host adapters (claude, codex) run the person's own harness as a
// process, so a session test that drives the real engine needs a real
// process on the other end of the pipes. This test binary is that process:
// the policy's harness path points at it, and when it is started with a
// harness's argv it plays that harness over its recorded wire shape (the
// adapters' session tests, pkg/runner/adapter; the conformance recordings)
// instead of running the tests.
func TestMain(m *testing.M) {
	if play := fakeHostHarness(os.Args[1:]); play != nil {
		os.Exit(play(os.Stdin, os.Stdout))
	}
	// No test touches DNS: the model hosts resolve nowhere unless a test
	// says otherwise (modelhosts_test.go).
	lookupModelHost = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no DNS in tests") }
	os.Exit(m.Run())
}

// fakeHostHarness is the harness this argv asks for, nil for a test run.
// A Go test binary is never started with --version or a harness's argv.
func fakeHostHarness(args []string) func(io.Reader, io.Writer) int {
	switch {
	case slices.Equal(args, []string{"--version"}):
		return func(_ io.Reader, w io.Writer) int { _, _ = fmt.Fprintln(w, "fake-harness 0.0.0"); return 0 }
	case len(args) > 0 && args[0] == "app-server":
		return playCodex
	case slices.Contains(args, "--input-format") && slices.Contains(args, "stream-json"):
		return playClaude
	}
	return nil
}

// fakeReply is what every fake harness answers a turn with: the turn's text
// back, and the fixture's secret and model key, which every receipt and
// every protocol event must redact.
func fakeReply(text string) string {
	return "You said: " + text + ". The token is sekret-value-1234 and the key is sk-test-key-9876."
}

func sayLine(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = w.Write(append(b, '\n'))
}

// playClaude is Claude Code's bidirectional stream-json: no handshake, and
// per user line an init (the first time), a gateway tool call and its
// result, the answer, and the result message that closes the turn.
func playClaude(in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	inited := false
	for sc.Scan() {
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Type != "user" {
			continue
		}
		text := ""
		if len(m.Message.Content) > 0 {
			text = m.Message.Content[0].Text
		}
		if !inited {
			sayLine(out, map[string]any{"type": "system", "subtype": "init", "tools": []string{"mcp__gateway__read_input"}})
			inited = true
		}
		sayLine(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__gateway__read_input"}}}})
		sayLine(out, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{}}}}})
		reply := fakeReply(text)
		sayLine(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": reply}}}})
		sayLine(out, map[string]any{"type": "result", "subtype": "success", "result": reply, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "total_cost_usd": 0.01})
	}
	return 0
}

// playCodex is codex's app-server JSON-RPC: initialize, an ephemeral
// thread, and per turn a gateway tool call whose approval the runner must
// accept, the usage, the answer and turn/completed.
func playCodex(in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	nextID := 1000
	pending := map[int]string{} // our request id → the turn's text
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "codex"}})
		case "initialized":
		case "thread/start":
			sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "t1"}, "approvalPolicy": "on-request"}})
		case "turn/start":
			text := ""
			if len(m.Params.Input) > 0 {
				text = m.Params.Input[0].Text
			}
			sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"turn": map[string]any{"id": "u1", "status": "inProgress"}}})
			sayLine(out, map[string]any{"method": "item/started", "params": map[string]any{"threadId": "t1", "turnId": "u1", "item": map[string]any{"type": "mcpToolCall", "id": "c1", "server": "gateway", "tool": "read_input", "status": "inProgress"}}})
			nextID++
			pending[nextID] = text
			sayLine(out, map[string]any{"method": "mcpServer/elicitation/request", "id": nextID, "params": map[string]any{"threadId": "t1", "turnId": "u1", "serverName": "gateway", "mode": "form", "_meta": map[string]any{"codex_approval_kind": "mcp_tool_call"}}})
		case "":
			var id int
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			text, ok := pending[id]
			if !ok {
				continue
			}
			delete(pending, id)
			sayLine(out, map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "t1", "turnId": "u1", "item": map[string]any{"type": "mcpToolCall", "id": "c1", "server": "gateway", "tool": "read_input", "status": "completed"}}})
			sayLine(out, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "t1", "turnId": "u1", "tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 10, "outputTokens": 5}, "total": map[string]any{"inputTokens": 10, "outputTokens": 5}}}})
			sayLine(out, map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "t1", "turnId": "u1", "item": map[string]any{"type": "agentMessage", "id": "m1", "text": fakeReply(strings.TrimSpace(text))}}})
			sayLine(out, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "u1", "status": "completed"}}})
		}
	}
	return 0
}
