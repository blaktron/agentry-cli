package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatServer answers every request with status and body and keeps the last
// request.
func chatServer(t *testing.T, status int, contentType, body string) (*Chat, *http.Request, *[]byte) {
	t.Helper()
	var got []byte
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		last = *r
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Chat{BaseURL: srv.URL + "/v1/", Model: "local-x", Key: func() string { return "k" }, HTTP: srv.Client()}, &last, &got
}

func chunks(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	return b.String()
}

func f(v float64) *float64 { return &v }

func TestChatBuildsTheRequest(t *testing.T) {
	c, last, got := chatServer(t, 200, "text/event-stream", chunks(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`))
	topK := int64(20)
	c.Temperature, c.TopP, c.TopK, c.RepetitionPenalty = f(0.6), f(0.95), &topK, f(1.05)
	c.ExtraBody = json.RawMessage(`{"chat_template_kwargs":{"enable_thinking":true}}`)
	c.ReplayReasoning = true

	_, err := c.Stream(context.Background(), Request{
		System:    "Be brief.",
		MaxTokens: 512,
		Tools:     []Tool{{Name: "fs_read", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "bare"}},
		Messages: []Message{
			{Role: User, Parts: []Part{{Kind: Text, Text: "task"}}},
			{Role: Assistant, Parts: []Part{
				{Kind: Reasoning, Text: "think"},
				{Kind: Text, Text: "Reading."},
				{Kind: Call, CallID: "c1", Name: "fs_read", Arguments: `{"path":"a"}`},
				{Kind: Call, CallID: "c2", Name: "fs_read", Arguments: `{"path":"unfin`},
			}},
			{Role: User, Parts: []Part{
				{Kind: Result, CallID: "c1", Text: "hello"},
				{Kind: Result, CallID: "c2", Failed: true},
				{Kind: Text, Text: "carry on"},
			}},
			{Role: Assistant, Parts: []Part{{Kind: Call, CallID: "c3", Name: "screenshot", Arguments: `{}`}}},
			{Role: User, Parts: []Part{{Kind: Result, CallID: "c3", Text: "a picture", Images: []Image{{MediaType: "image/png", Data: "aGk="}}}}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/v1/chat/completions" || last.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("request to %s with Authorization %q", last.URL.Path, last.Header.Get("Authorization"))
	}
	var body struct {
		Model             string
		Stream            bool
		StreamOptions     map[string]bool `json:"stream_options"`
		MaxTokens         int             `json:"max_tokens"`
		Temperature       float64
		TopP              float64         `json:"top_p"`
		TopK              int             `json:"top_k"`
		RepetitionPenalty float64         `json:"repetition_penalty"`
		MinP              *float64        `json:"min_p"`
		Kwargs            map[string]bool `json:"chat_template_kwargs"`
		Tools             []struct {
			Type     string
			Function struct {
				Name       string
				Parameters map[string]any
			}
		}
		Messages []struct {
			Role             string
			Content          any
			ReasoningContent string `json:"reasoning_content"`
			ToolCallID       string `json:"tool_call_id"`
			ToolCalls        []struct {
				ID       string
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		}
	}
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "local-x" || !body.Stream || !body.StreamOptions["include_usage"] || body.MaxTokens != 512 {
		t.Errorf("request: %s", *got)
	}
	if body.Temperature != 0.6 || body.TopP != 0.95 || body.TopK != 20 || body.RepetitionPenalty != 1.05 || body.MinP != nil || !body.Kwargs["enable_thinking"] {
		t.Errorf("sampling and extra body: %s", *got)
	}
	if len(body.Tools) != 2 || body.Tools[0].Function.Name != "fs_read" || body.Tools[1].Function.Parameters["type"] != "object" {
		t.Errorf("tools: %+v", body.Tools)
	}
	var roles []string
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	if got, want := strings.Join(roles, " "), "system user assistant tool tool user assistant tool user"; got != want {
		t.Fatalf("messages: %s\nwant:     %s", got, want)
	}
	a := body.Messages[2]
	if a.Content != "Reading." || a.ReasoningContent != "think" || len(a.ToolCalls) != 2 || a.ToolCalls[0].Function.Arguments != `{"path":"a"}` || a.ToolCalls[1].Function.Arguments != "{}" {
		t.Errorf("assistant message: %+v", a)
	}
	if m := body.Messages[3]; m.ToolCallID != "c1" || m.Content != "hello" {
		t.Errorf("a result: %+v", m)
	}
	if m := body.Messages[4]; m.ToolCallID != "c2" || m.Content == "" {
		t.Errorf("a failed result with no text: %+v", m)
	}
	if m := body.Messages[5]; m.Content != "carry on" {
		t.Errorf("the person's text after the results: %+v", m)
	}
	if m := body.Messages[6]; m.Content != nil || len(m.ToolCalls) != 1 {
		t.Errorf("an assistant message of calls alone: %+v", m)
	}
	parts, _ := body.Messages[8].Content.([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("a result's image: %+v", body.Messages[8].Content)
	}

	// No key, no header; and reasoning is not sent back unless asked.
	c.Key, c.ReplayReasoning = func() string { return "" }, false
	if _, err := c.Stream(context.Background(), Request{Messages: []Message{{Role: Assistant, Parts: []Part{{Kind: Reasoning, Text: "think"}, {Kind: Text, Text: "x"}}}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := last.Header["Authorization"]; has {
		t.Error("an Authorization header was sent with no key")
	}
	if strings.Contains(string(*got), "reasoning_content") || strings.Contains(string(*got), "max_tokens") {
		t.Errorf("reasoning or a limit was sent unasked: %s", *got)
	}
}

func TestChatReadsAReply(t *testing.T) {
	c, _, _ := chatServer(t, 200, "text/event-stream", chunks(
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"a"}}]}`,
		`{"choices":[{"delta":{"reasoning":"b"}}]}`,
		`{"choices":[{"delta":{"reasoning_text":"c"}}]}`,
		`{"choices":[{"delta":{"content":"Rea"}}]}`,
		`{"choices":[{"delta":{"content":"ding."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"fs_read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"fs_list","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":44,"prompt_tokens_details":{"cached_tokens":900}}}`,
		`[DONE]`,
	))
	var deltas []string
	resp, err := c.Stream(context.Background(), Request{}, func(d Delta) { deltas = append(deltas, string(d.Kind)+":"+d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, " "); got != "reasoning:a reasoning:b reasoning:c text:Rea text:ding." {
		t.Errorf("deltas: %s", got)
	}
	if resp.Finish != FinishToolCalls || resp.Usage != (Usage{Input: 100, CacheRead: 900, Output: 44}) {
		t.Errorf("finish %q, usage %+v", resp.Finish, resp.Usage)
	}
	if len(resp.Parts) != 4 || resp.Parts[0].Text != "abc" || resp.Parts[1].Text != "Reading." {
		t.Fatalf("parts: %+v", resp.Parts)
	}
	if p := resp.Parts[2]; p.CallID != "call_a" || p.Name != "fs_read" || p.Arguments != `{"path":"a"}` {
		t.Errorf("first call: %+v", p)
	}
	if p := resp.Parts[3]; p.CallID != "call_b" || p.Name != "fs_list" || p.Arguments != "{}" {
		t.Errorf("second call: %+v", p)
	}
}

func TestChatReadsWhatServersDoDifferently(t *testing.T) {
	// Calls with no index and no id, each sent whole, ended with "stop".
	c, _, _ := chatServer(t, 200, "text/event-stream", chunks(
		`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"a","arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"b","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
	))
	resp, err := c.Stream(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != FinishToolCalls || len(resp.Parts) != 2 || resp.Parts[0].CallID == "" || resp.Parts[0].CallID == resp.Parts[1].CallID || resp.Parts[1].Arguments != "{}" {
		t.Errorf("calls with no index or id: finish %q, %+v", resp.Finish, resp.Parts)
	}
	if resp.Usage != (Usage{Input: 7, Output: 3}) {
		t.Errorf("usage in the last choice's chunk: %+v", resp.Usage)
	}

	// Reasoning left in the answer between think tags, cut across chunks.
	c, _, _ = chatServer(t, 200, "text/event-stream", chunks(
		`{"choices":[{"delta":{"content":"\n<thi"}}]}`,
		`{"choices":[{"delta":{"content":"nk>plan the"}}]}`,
		`{"choices":[{"delta":{"content":" steps</th"}}]}`,
		`{"choices":[{"delta":{"content":"ink>\n\nThe answer"}}]}`,
		`{"choices":[{"delta":{"content":" is <think> literally."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	var text, reasoning strings.Builder
	resp, err = c.Stream(context.Background(), Request{}, func(d Delta) {
		if d.Kind == Reasoning {
			reasoning.WriteString(d.Text)
		} else {
			text.WriteString(d.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if reasoning.String() != "plan the steps" || text.String() != "The answer is <think> literally." {
		t.Errorf("reasoning %q, text %q", reasoning.String(), text.String())
	}
	if len(resp.Parts) != 2 || resp.Parts[0].Kind != Reasoning || resp.Parts[1].Text != "The answer is <think> literally." {
		t.Errorf("parts: %+v", resp.Parts)
	}

	// An answer that only looks like it might open a tag.
	c, _, _ = chatServer(t, 200, "text/event-stream", chunks(
		`{"choices":[{"delta":{"content":"<th"}}]}`,
		`{"choices":[{"delta":{"content":"ree> items"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`[DONE]`,
	))
	resp, err = c.Stream(context.Background(), Request{}, nil)
	if err != nil || resp.Finish != FinishLength || len(resp.Parts) != 1 || resp.Parts[0].Text != "<three> items" {
		t.Errorf("text that starts like a tag: %+v, %v", resp, err)
	}

	// Reasoning that never closes is still reasoning, not the answer.
	c, _, _ = chatServer(t, 200, "text/event-stream", chunks(
		`{"choices":[{"delta":{"content":"<think>going round"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
	))
	resp, err = c.Stream(context.Background(), Request{}, nil)
	if err != nil || len(resp.Parts) != 1 || resp.Parts[0].Kind != Reasoning || resp.Parts[0].Text != "going round" {
		t.Errorf("unclosed reasoning: %+v, %v", resp, err)
	}
}

func TestChatFailures(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		class       Class
		words       string
	}{
		{"OpenAI overflow", 400, "application/json", `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, Overflow, "maximum context length is 128000"},
		{"vLLM overflow", 400, "application/json", `{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40961 tokens (36865 in the messages, 4096 in the completion).","type":"BadRequestError","param":null,"code":400}}`, Overflow, "32768 tokens"},
		{"llama.cpp overflow", 400, "application/json", `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":12009,"n_ctx":4096}}`, Overflow, "available context size"},
		{"Ollama overflow", 400, "application/json", `{"error":{"message":"prompt too long; exceeded max context length by 1025 tokens","type":"invalid_request_error","param":null,"code":null}}`, Overflow, "prompt too long"},
		{"LM Studio overflow, an error event on a 200", 200, "text/event-stream", "event: error\ndata: {\"error\":{\"message\":\"The number of tokens to keep from the initial prompt is greater than the context length (n_keep: 5746 >= n_ctx: 4096).\"}}\n\n", Overflow, "greater than the context length"},
		{"an error chunk in the stream", 200, "text/event-stream", chunks(`{"choices":[{"delta":{"content":"par"}}]}`, `{"error":{"message":"Provider returned error","code":502}}`), Server, "Provider returned error"},
		{"an error that is a string", 404, "application/json", `{"error":"model 'nope' not found"}`, BadRequest, "model 'nope' not found"},
		{"a rate limit", 429, "application/json", `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`, RateLimit, "Rate limit reached"},
		{"no quota", 429, "application/json", `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, Credits, "current quota"},
		{"payment required", 402, "application/json", `{"error":{"message":"Insufficient credits","code":402}}`, Credits, "Insufficient credits"},
		{"a bad key", 401, "application/json", `{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`, Auth, "Incorrect API key"},
		{"unavailable", 503, "application/json", `{"error":{"message":"The server is overloaded","type":"server_error","code":null}}`, Capacity, "overloaded"},
		{"a plain body", 500, "text/plain", "upstream connect error", Server, "upstream connect error"},
		{"a stream that just ends", 200, "text/event-stream", chunks(`{"choices":[{"delta":{"content":"par"}}]}`), Transport, "ended before the reply did"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := chatServer(t, tc.status, tc.contentType, tc.body)
			_, err := c.Stream(context.Background(), Request{}, nil)
			if !IsError(err, tc.class) {
				t.Fatalf("got %v, want class %s", err, tc.class)
			}
			e := err.(*Error)
			if !strings.Contains(e.Message, tc.words) {
				t.Errorf("message %q lacks %q", e.Message, tc.words)
			}
			if tc.status != 200 && (e.Status != tc.status || e.RetryAfter.Seconds() != 9) {
				t.Errorf("status %d, retry after %v", e.Status, e.RetryAfter)
			}
		})
	}
}
