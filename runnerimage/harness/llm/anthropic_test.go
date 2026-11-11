package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// server answers every request with body as an event stream and keeps the
// last request's body.
func server(t *testing.T, status int, body string) (*Anthropic, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Anthropic{BaseURL: srv.URL + "/", Model: "claude-x", Key: func() string { return "k" }, HTTP: srv.Client()}, &got
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var typ struct{ Type string }
		_ = json.Unmarshal([]byte(e), &typ)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ.Type, e)
	}
	return b.String()
}

func TestAnthropicMarksWhatAServerCanReuse(t *testing.T) {
	a, got := server(t, http.StatusOK, sse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`))
	signed, _ := json.Marshal(reasoningMeta{Signature: "sig"})
	hidden, _ := json.Marshal(reasoningMeta{Redacted: "opaque"})
	_, err := a.Stream(context.Background(), Request{
		System:    "Be brief.",
		MaxTokens: 100,
		Tools:     []Tool{{Name: "a", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "b"}},
		Messages: []Message{
			{Role: User, Parts: []Part{{Kind: Text, Text: "task"}}},
			{Role: Assistant, Parts: []Part{
				{Kind: Reasoning, Text: "thinking", Meta: signed},
				{Kind: Reasoning, Meta: hidden},
				{Kind: Reasoning, Text: "another server's reasoning"},
				{Kind: Text, Text: ""},
				{Kind: Call, CallID: "c1", Name: "a", Arguments: `{"path":"x"}`},
				{Kind: Call, CallID: "c2", Name: "b", Arguments: `{"path":"unfinis`},
			}},
			{Role: User, Parts: []Part{
				{Kind: Result, CallID: "c1", Text: "hello"},
				{Kind: Result, CallID: "c2", Failed: true},
				{Kind: Result, CallID: "c3", Images: []Image{{MediaType: "image/png", Data: "aGk="}}},
			}},
			{Role: Assistant, Parts: []Part{{Kind: Text, Text: "done"}, {Kind: Reasoning, Text: "after", Meta: signed}}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model     string
		MaxTokens int `json:"max_tokens"`
		Stream    bool
		System    []map[string]any
		Tools     []map[string]any
		Messages  []struct {
			Role    string
			Content []map[string]any
		}
	}
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-x" || body.MaxTokens != 100 || !body.Stream {
		t.Errorf("request: %+v", body)
	}
	marked := func(m map[string]any) bool { return m["cache_control"] != nil }
	if !marked(body.System[0]) || marked(body.Tools[0]) || !marked(body.Tools[1]) {
		t.Errorf("markers on the system prompt and the last tool: %s", *got)
	}
	if body.Tools[1]["input_schema"] == nil {
		t.Error("a tool with no schema was sent with none")
	}
	if len(body.Messages) != 4 {
		t.Fatalf("%d messages, want 4", len(body.Messages))
	}
	reply := body.Messages[1].Content
	if len(reply) != 4 || reply[0]["type"] != "thinking" || reply[0]["signature"] != "sig" || reply[1]["type"] != "redacted_thinking" || reply[1]["data"] != "opaque" {
		t.Fatalf("the reply sent back (unsigned reasoning and empty text left out): %v", reply)
	}
	if in := reply[3]["input"].(map[string]any); len(in) != 0 {
		t.Errorf("a call with unfinished arguments was sent with them: %v", in)
	}
	results := body.Messages[2].Content
	if results[1]["is_error"] != true || results[1]["content"] != nil {
		t.Errorf("a failed result with no text: %v", results[1])
	}
	if img := results[2]["content"].([]any)[0].(map[string]any); img["type"] != "image" {
		t.Errorf("an image result: %v", results[2])
	}
	// The last block of each of the last two messages that can carry a
	// marker, and no other message.
	last := body.Messages[3].Content
	if marked(body.Messages[0].Content[0]) || marked(reply[3]) || !marked(results[2]) || !marked(last[0]) || marked(last[1]) {
		t.Errorf("markers in the messages: %s", *got)
	}
	if n := strings.Count(string(*got), `"cache_control"`); n != 4 {
		t.Errorf("%d markers, want 4", n)
	}
}

func TestAnthropicReadsAReply(t *testing.T) {
	a, _ := server(t, http.StatusOK, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":1}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"hm"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Rea"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ding."}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs_list","input":{}}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":44}}`,
		`{"type":"message_stop"}`,
	))
	var deltas []string
	resp, err := a.Stream(context.Background(), Request{MaxTokens: 1}, func(d Delta) { deltas = append(deltas, string(d.Kind)+":"+d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, " "); got != "reasoning:hm text:Rea text:ding." {
		t.Errorf("deltas: %s", got)
	}
	if resp.Finish != FinishToolCalls || resp.Usage != (Usage{Input: 10, CacheWrite: 20, CacheRead: 30, Output: 44}) {
		t.Errorf("finish %q, usage %+v", resp.Finish, resp.Usage)
	}
	if len(resp.Parts) != 4 {
		t.Fatalf("parts: %+v", resp.Parts)
	}
	if p := resp.Parts[0]; p.Kind != Reasoning || string(p.Meta) != `{"redacted":"opaque"}` {
		t.Errorf("redacted reasoning: %+v", p)
	}
	if p := resp.Parts[1]; p.Kind != Reasoning || p.Text != "hm" || string(p.Meta) != `{"signature":"sig"}` {
		t.Errorf("reasoning: %+v", p)
	}
	if p := resp.Parts[3]; p.Kind != Call || p.CallID != "toolu_1" || p.Name != "fs_list" || p.Arguments != "{}" {
		t.Errorf("a call with no arguments: %+v", p)
	}
}

func TestAnthropicFailures(t *testing.T) {
	a, _ := server(t, http.StatusOK, sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	))
	if _, err := a.Stream(context.Background(), Request{}, nil); !IsError(err, Capacity) {
		t.Errorf("an error event in the stream: %v", err)
	}

	a, _ = server(t, http.StatusOK, sse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	if _, err := a.Stream(context.Background(), Request{}, nil); !IsError(err, Transport) {
		t.Errorf("a stream that ends before the reply does: %v", err)
	}

	a, _ = server(t, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	_, err := a.Stream(context.Background(), Request{}, nil)
	var e *Error
	if !IsError(err, RateLimit) {
		t.Fatalf("a 429: %v", err)
	}
	e = err.(*Error)
	if e.Status != 429 || e.Message != "slow down" || e.RetryAfter.Seconds() != 7 {
		t.Errorf("error: %+v", e)
	}

	a, _ = server(t, http.StatusBadGateway, "<html>bad gateway</html>")
	if _, err := a.Stream(context.Background(), Request{}, nil); !IsError(err, Server) || !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("a body that is not the API's: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Stream(ctx, Request{}, nil); err != context.Canceled {
		t.Errorf("a cancelled request: %v", err)
	}
}
