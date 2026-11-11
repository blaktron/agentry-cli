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

func geminiServer(t *testing.T, status int, body string) (*Gemini, *http.Request, *[]byte) {
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
			w.Header().Set("Retry-After", "11")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Gemini{BaseURL: srv.URL + "/", Model: "gemini-x", Key: func() string { return "k" }, HTTP: srv.Client()}, &last, &got
}

func TestGeminiReadsAReplyAndSendsItsPartsBack(t *testing.T) {
	c, last, got := geminiServer(t, 200, chunks(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hm","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Rea"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"ding."}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fs_read","args":{"path":"a"}},"thoughtSignature":"c2ln"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"srv-2","name":"fs_list","args":{}}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"cachedContentTokenCount":900,"candidatesTokenCount":30,"thoughtsTokenCount":14}}`,
	))
	var deltas []string
	resp, err := c.Stream(context.Background(), Request{
		System:    "Be brief.",
		MaxTokens: 512,
		Tools:     []Tool{{Name: "fs_read", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "bare"}},
		Messages:  []Message{{Role: User, Parts: []Part{{Kind: Text, Text: "task"}}}},
	}, func(d Delta) { deltas = append(deltas, string(d.Kind)+":"+d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if last.URL.Path != "/v1beta/models/gemini-x:streamGenerateContent" || last.URL.Query().Get("alt") != "sse" {
		t.Errorf("request to %s?%s", last.URL.Path, last.URL.RawQuery)
	}
	if last.Header.Get("x-goog-api-key") != "k" || strings.Contains(last.URL.RawQuery, "key=") {
		t.Errorf("the key is not in its header, or is in the address: %v %s", last.Header, last.URL.RawQuery)
	}
	var first struct {
		SystemInstruction struct{ Parts []struct{ Text string } }
		Contents          []struct {
			Role  string
			Parts []map[string]any
		}
		Tools []struct {
			FunctionDeclarations []struct {
				Name                 string
				ParametersJSONSchema map[string]any `json:"parametersJsonSchema"`
			}
		}
		GenerationConfig struct{ MaxOutputTokens int }
	}
	if err := json.Unmarshal(*got, &first); err != nil {
		t.Fatal(err)
	}
	if first.SystemInstruction.Parts[0].Text != "Be brief." || first.GenerationConfig.MaxOutputTokens != 512 || len(first.Contents) != 1 || first.Contents[0].Role != "user" {
		t.Errorf("request: %s", *got)
	}
	if d := first.Tools[0].FunctionDeclarations; len(d) != 2 || d[0].Name != "fs_read" || d[1].ParametersJSONSchema["type"] != "object" {
		t.Errorf("tools: %+v", first.Tools)
	}

	if got := strings.Join(deltas, " "); got != "reasoning:hm text:Rea text:ding." {
		t.Errorf("deltas: %s", got)
	}
	if resp.Finish != FinishToolCalls || resp.Usage != (Usage{Input: 100, CacheRead: 900, Output: 44}) {
		t.Errorf("finish %q, usage %+v", resp.Finish, resp.Usage)
	}
	if len(resp.Parts) != 4 || resp.Parts[0].Kind != Reasoning || resp.Parts[1].Text != "Reading." {
		t.Fatalf("parts: %+v", resp.Parts)
	}
	a, b := resp.Parts[2], resp.Parts[3]
	if a.Name != "fs_read" || a.Arguments != `{"path":"a"}` || !strings.HasPrefix(a.CallID, "call_") || b.CallID != "srv-2" || b.Arguments != "{}" {
		t.Errorf("calls: %+v %+v", a, b)
	}

	// The reply goes back with its parts as they came: the signature with
	// its call, the server's own id with its result, and no id for the
	// call the server gave none.
	next := Request{Messages: []Message{
		{Role: User, Parts: []Part{{Kind: Text, Text: "task"}}},
		{Role: Assistant, Parts: resp.Parts},
		{Role: User, Parts: []Part{
			{Kind: Result, CallID: a.CallID, Name: "fs_read", Text: "hello", Images: []Image{{MediaType: "image/png", Data: "aGk="}}},
			{Kind: Result, CallID: b.CallID, Name: "fs_list", Failed: true},
			{Kind: Text, Text: "carry on"},
		}},
	}}
	if _, err := c.Stream(context.Background(), next, nil); err != nil {
		t.Fatal(err)
	}
	var second struct {
		Contents []struct {
			Role  string
			Parts []map[string]any
		}
	}
	if err := json.Unmarshal(*got, &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Contents) != 3 || second.Contents[1].Role != "model" {
		t.Fatalf("contents: %s", *got)
	}
	model := second.Contents[1].Parts
	if len(model) != 3 || model[0]["text"] != "Reading." || model[1]["thoughtSignature"] != "c2ln" || model[1]["functionCall"].(map[string]any)["name"] != "fs_read" {
		t.Errorf("the reply sent back (the unsigned thought left out): %v", model)
	}
	if _, has := model[1]["functionCall"].(map[string]any)["id"]; has {
		t.Errorf("a made-up call id was sent: %v", model[1])
	}
	results := second.Contents[2].Parts
	if len(results) != 4 {
		t.Fatalf("results: %v", results)
	}
	r1 := results[0]["functionResponse"].(map[string]any)
	if r1["name"] != "fs_read" || r1["id"] != nil || r1["response"].(map[string]any)["output"] != "hello" {
		t.Errorf("a result for a call with no id of the server's: %v", r1)
	}
	if results[1]["inlineData"].(map[string]any)["mimeType"] != "image/png" {
		t.Errorf("a result's image: %v", results[1])
	}
	r2 := results[2]["functionResponse"].(map[string]any)
	if r2["id"] != "srv-2" || r2["response"].(map[string]any)["error"] == "" {
		t.Errorf("a failed result for a call with the server's id: %v", r2)
	}
	if results[3]["text"] != "carry on" {
		t.Errorf("the person's text: %v", results[3])
	}
}

func TestGeminiEndingsAndFailures(t *testing.T) {
	c, _, _ := geminiServer(t, 200, chunks(
		`{"candidates":[{"content":{"parts":[{"text":"The report begins"}]}}]}`,
		`{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":9}}`,
	))
	if resp, err := c.Stream(context.Background(), Request{}, nil); err != nil || resp.Finish != FinishLength || resp.Parts[0].Text != "The report begins" {
		t.Errorf("a cut answer: %+v, %v", resp, err)
	}
	c, _, _ = geminiServer(t, 200, chunks(`{"candidates":[{"content":{"parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`))
	if resp, err := c.Stream(context.Background(), Request{}, nil); err != nil || resp.Finish != FinishOther || len(resp.Parts) != 0 {
		t.Errorf("a call the server could not form: %+v, %v", resp, err)
	}

	cases := []struct {
		name   string
		status int
		body   string
		class  Class
		words  string
	}{
		{"too long", 400, `{"error":{"code":400,"message":"The input token count (1250001) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, Overflow, "exceeds the maximum number of tokens"},
		{"a bad key", 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, Auth, "API key not valid"},
		{"quota, in a list", 429, `[{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}]`, RateLimit, "exhausted"},
		{"overloaded", 503, `{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}`, Capacity, "overloaded"},
		{"malformed", 400, `{"error":{"code":400,"message":"Invalid JSON payload received.","status":"INVALID_ARGUMENT"}}`, BadRequest, "Invalid JSON"},
		{"a server error", 500, `{"error":{"code":500,"message":"An internal error has occurred.","status":"INTERNAL"}}`, Server, "internal error"},
		{"an error in the stream", 200, chunks(`{"candidates":[{"content":{"parts":[{"text":"par"}]}}]}`, `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`), Capacity, "overloaded"},
		{"a stream that just ends", 200, chunks(`{"candidates":[{"content":{"parts":[{"text":"par"}]}}]}`), Transport, "ended before the reply did"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := geminiServer(t, tc.status, tc.body)
			_, err := c.Stream(context.Background(), Request{}, nil)
			if !IsError(err, tc.class) || !strings.Contains(err.Error(), tc.words) {
				t.Fatalf("got %v, want class %s saying %q", err, tc.class, tc.words)
			}
			if e := err.(*Error); tc.status != 200 && (e.Status != tc.status || e.RetryAfter.Seconds() != 11) {
				t.Errorf("status %d, retry after %v", e.Status, e.RetryAfter)
			}
		})
	}
}

func TestMadeUpCallIDsDoNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newCallID()
		if seen[id] {
			t.Fatalf("id %s repeated", id)
		}
		seen[id] = true
	}
}
