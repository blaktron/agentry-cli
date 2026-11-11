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

func responsesServer(t *testing.T, status int, body string) (*Responses, *http.Request, *[]byte) {
	t.Helper()
	var got []byte
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		last = *r
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Responses{BaseURL: srv.URL + "/v1/", Model: "gpt-x", Key: func() string { return "k" }, HTTP: srv.Client()}, &last, &got
}

const responsesDone = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`

func TestResponsesBuildsTheRequest(t *testing.T) {
	c, last, got := responsesServer(t, 200, sse(responsesDone))
	reasoningItem := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"opaque"}`)
	_, err := c.Stream(context.Background(), Request{
		System:    "Be brief.",
		MaxTokens: 512,
		Tools:     []Tool{{Name: "fs_read", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "bare"}},
		Messages: []Message{
			{Role: User, Parts: []Part{{Kind: Text, Text: "task"}}},
			{Role: Assistant, Parts: []Part{
				{Kind: Reasoning, Text: "think", Meta: reasoningItem},
				{Kind: Reasoning, Text: "another server's reasoning"},
				{Kind: Text, Text: "Reading."},
				{Kind: Call, CallID: "c1", Name: "fs_read", Arguments: `{"path":"a"}`},
				{Kind: Call, CallID: "c2", Name: "fs_read", Arguments: `{"path":"unfin`},
			}},
			{Role: User, Parts: []Part{
				{Kind: Result, CallID: "c1", Text: "hello", Images: []Image{{MediaType: "image/png", Data: "aGk="}}},
				{Kind: Result, CallID: "c2", Failed: true},
				{Kind: Text, Text: "carry on"},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/v1/responses" || last.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("request to %s with Authorization %q", last.URL.Path, last.Header.Get("Authorization"))
	}
	var body struct {
		Model           string
		Instructions    string
		Stream          bool
		Store           *bool
		Include         []string
		MaxOutputTokens int `json:"max_output_tokens"`
		Tools           []struct {
			Type, Name string
			Parameters map[string]any
			Strict     bool
		}
		Input []map[string]any
	}
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "gpt-x" || body.Instructions != "Be brief." || !body.Stream || body.MaxOutputTokens != 512 {
		t.Errorf("request: %s", *got)
	}
	if body.Store == nil || *body.Store || len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("the request keeps state on the server or does not ask for its reasoning back: store %v, include %v", body.Store, body.Include)
	}
	if len(body.Tools) != 2 || body.Tools[0].Type != "function" || body.Tools[0].Name != "fs_read" || body.Tools[0].Strict || body.Tools[1].Parameters["type"] != "object" {
		t.Errorf("tools: %+v", body.Tools)
	}
	var kinds []string
	for _, item := range body.Input {
		kinds = append(kinds, item["type"].(string))
	}
	if got, want := strings.Join(kinds, " "), "message reasoning message function_call function_call function_call_output function_call_output message"; got != want {
		t.Fatalf("input items: %s\nwant:        %s", got, want)
	}
	if r := body.Input[1]; r["encrypted_content"] != "opaque" || r["id"] != "rs_1" {
		t.Errorf("the reasoning item did not go back as it came: %v", r)
	}
	if m := body.Input[2]; m["role"] != "assistant" || m["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Errorf("assistant text: %v", m)
	}
	if c := body.Input[3]; c["call_id"] != "c1" || c["arguments"] != `{"path":"a"}` {
		t.Errorf("a call: %v", c)
	}
	if c := body.Input[4]; c["arguments"] != "{}" {
		t.Errorf("a call with unfinished arguments was sent with them: %v", c)
	}
	if o := body.Input[5]; o["call_id"] != "c1" || o["output"] != "hello" {
		t.Errorf("a result: %v", o)
	}
	if o := body.Input[6]; o["output"] == "" {
		t.Errorf("a failed result with no text: %v", o)
	}
	last2 := body.Input[7]["content"].([]any)
	if len(last2) != 2 || last2[0].(map[string]any)["type"] != "input_image" || last2[1].(map[string]any)["text"] != "carry on" {
		t.Errorf("the image and the person's text: %v", last2)
	}
}

func TestResponsesReadsAReply(t *testing.T) {
	c, _, _ := responsesServer(t, 200, sse(
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"hm"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hm"}],"encrypted_content":"opaque"}}`,
		`{"type":"response.output_text.delta","delta":"Rea"}`,
		`{"type":"response.output_text.delta","delta":"ding."}`,
		`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reading."}]}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"pa"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"fs_read","arguments":"{\"path\":\"a\"}"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"fs_list","arguments":""}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900},"output_tokens":44,"output_tokens_details":{"reasoning_tokens":12}}}}`,
	))
	var deltas []string
	resp, err := c.Stream(context.Background(), Request{}, func(d Delta) { deltas = append(deltas, string(d.Kind)+":"+d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, " "); got != "reasoning:hm text:Rea text:ding." {
		t.Errorf("deltas: %s", got)
	}
	if resp.Finish != FinishToolCalls || resp.Usage != (Usage{Input: 100, CacheRead: 900, Output: 44}) {
		t.Errorf("finish %q, usage %+v", resp.Finish, resp.Usage)
	}
	if len(resp.Parts) != 4 {
		t.Fatalf("parts: %+v", resp.Parts)
	}
	if p := resp.Parts[0]; p.Kind != Reasoning || p.Text != "hm" || !strings.Contains(string(p.Meta), `"encrypted_content":"opaque"`) {
		t.Errorf("reasoning: %+v", p)
	}
	if p := resp.Parts[1]; p.Kind != Text || p.Text != "Reading." {
		t.Errorf("text: %+v", p)
	}
	if p := resp.Parts[2]; p.CallID != "call_a" || p.Name != "fs_read" || p.Arguments != `{"path":"a"}` {
		t.Errorf("first call: %+v", p)
	}
	if p := resp.Parts[3]; p.CallID != "call_b" || p.Arguments != "{}" {
		t.Errorf("a call with no arguments: %+v", p)
	}
}

func TestResponsesEndingsAndFailures(t *testing.T) {
	// A reply that stops at the output limit inside a call: the call was
	// never done, so it is not in the reply at all.
	c, _, _ := responsesServer(t, 200, sse(
		`{"type":"response.output_text.delta","delta":"Writing"}`,
		`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Writing"}]}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_a","name":"fs_write","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"path\":\"rep"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":9}}}`,
	))
	resp, err := c.Stream(context.Background(), Request{}, nil)
	if err != nil || resp.Finish != FinishLength || len(resp.Parts) != 1 || resp.Parts[0].Text != "Writing" || resp.Usage.Output != 9 {
		t.Errorf("a reply cut inside a call: %+v, %v", resp, err)
	}

	c, _, _ = responsesServer(t, 200, sse(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`))
	if resp, err := c.Stream(context.Background(), Request{}, nil); err != nil || resp.Finish != FinishOther {
		t.Errorf("a reply the server ended for its own reason: %+v, %v", resp, err)
	}

	c, _, _ = responsesServer(t, 200, sse(`{"type":"response.output_text.delta","delta":"Done."}`, `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Done."}]}}`, responsesDone))
	if resp, err := c.Stream(context.Background(), Request{}, nil); err != nil || resp.Finish != FinishStop {
		t.Errorf("an answer: %+v, %v", resp, err)
	}

	cases := []struct {
		name   string
		status int
		body   string
		class  Class
		words  string
	}{
		{"failed in the stream", 200, sse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"The model failed to generate a response."}}}`), Server, "failed to generate"},
		{"failed with a code alone", 200, sse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded"}}}`), RateLimit, "rate_limit_exceeded"},
		{"an error event", 200, sse(`{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`), Overflow, "exceeds the context window"},
		{"too long, refused outright", 400, `{"error":{"message":"This model's maximum context length is 400000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`, Overflow, "maximum context length"},
		{"no quota", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, Credits, "current quota"},
		{"a bad key", 401, `{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`, Auth, "Incorrect API key"},
		{"a stream that just ends", 200, sse(`{"type":"response.output_text.delta","delta":"par"}`), Transport, "ended before the reply did"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := responsesServer(t, tc.status, tc.body)
			_, err := c.Stream(context.Background(), Request{}, nil)
			if !IsError(err, tc.class) || !strings.Contains(err.Error(), tc.words) {
				t.Errorf("got %v, want class %s saying %q", err, tc.class, tc.words)
			}
		})
	}
}
