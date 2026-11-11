package scriptedmodel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	anthropicPath = "/v1/messages"
	chatPath      = "/v1/chat/completions"
)

// event is one server-sent event as a client reads it.
type event struct {
	name string
	data string
}

func post(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// events reads a stream to its end and returns its events and the error that
// ended it, nil for a clean end.
func events(r io.Reader) ([]event, error) {
	var out []event
	var cur event
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.data != "" || cur.name != "" {
				out = append(out, cur)
			}
			cur = event{}
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out, sc.Err()
}

// reply is what a stream adds up to.
type reply struct {
	text      string
	reasoning string
	signature string
	calls     []Call
	finish    string
	usage     Usage
	sawUsage  bool
}

func readAnthropic(t *testing.T, evs []event) reply {
	t.Helper()
	var r reply
	blocks := map[int]string{}
	args := map[int]*Call{}
	var order []int
	for _, e := range evs {
		var d struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type, ID, Name string
			} `json:"content_block"`
			Delta struct {
				Type        string
				Text        string
				Thinking    string
				Signature   string
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			}
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(e.data), &d); err != nil {
			t.Fatalf("event %q is not JSON: %v", e.data, err)
		}
		if d.Type != e.name {
			t.Errorf("event named %q carries type %q", e.name, d.Type)
		}
		switch d.Type {
		case "message_start":
			var m struct {
				Message struct {
					Usage struct {
						Input int `json:"input_tokens"`
						Write int `json:"cache_creation_input_tokens"`
						Read  int `json:"cache_read_input_tokens"`
					}
				}
			}
			_ = json.Unmarshal([]byte(e.data), &m)
			r.usage.Input, r.usage.CacheWrite, r.usage.CacheRead = m.Message.Usage.Input, m.Message.Usage.Write, m.Message.Usage.Read
			r.sawUsage = true
		case "content_block_start":
			blocks[d.Index] = d.ContentBlock.Type
			if d.ContentBlock.Type == "tool_use" {
				args[d.Index] = &Call{ID: d.ContentBlock.ID, Name: d.ContentBlock.Name}
				order = append(order, d.Index)
			}
		case "content_block_delta":
			switch d.Delta.Type {
			case "text_delta":
				r.text += d.Delta.Text
			case "thinking_delta":
				r.reasoning += d.Delta.Thinking
			case "signature_delta":
				r.signature += d.Delta.Signature
			case "input_json_delta":
				args[d.Index].Arguments += d.Delta.PartialJSON
			}
		case "message_delta":
			r.finish = d.Delta.StopReason
			r.usage.Output = d.Usage.OutputTokens
		}
	}
	for _, i := range order {
		r.calls = append(r.calls, *args[i])
	}
	return r
}

func readChat(t *testing.T, evs []event) (reply, bool) {
	t.Helper()
	var r reply
	done := false
	calls := map[int]*Call{}
	var order []int
	for _, e := range evs {
		if e.data == "[DONE]" {
			done = true
			continue
		}
		var d struct {
			Choices []struct {
				Delta struct {
					Content          string
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				FinishReason *string `json:"finish_reason"`
			}
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				Out struct {
					Reasoning int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			}
		}
		if err := json.Unmarshal([]byte(e.data), &d); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", e.data, err)
		}
		if d.Usage != nil {
			r.sawUsage = true
			r.usage = Usage{Input: d.Usage.Prompt - d.Usage.Details.Cached, CacheRead: d.Usage.Details.Cached, Output: d.Usage.Completion, Reasoning: d.Usage.Out.Reasoning}
		}
		for _, c := range d.Choices {
			r.text += c.Delta.Content
			r.reasoning += c.Delta.ReasoningContent
			for _, tc := range c.Delta.ToolCalls {
				if calls[tc.Index] == nil {
					calls[tc.Index] = &Call{}
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					calls[tc.Index].ID = tc.ID
				}
				calls[tc.Index].Name += tc.Function.Name
				calls[tc.Index].Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil {
				r.finish = *c.FinishReason
			}
		}
	}
	for _, i := range order {
		r.calls = append(r.calls, *calls[i])
	}
	return r, done
}

func stream(t *testing.T, s *Server, path, body string) (*http.Response, []event) {
	t.Helper()
	resp := post(t, context.Background(), s.URL()+path, body)
	evs, err := events(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return resp, evs
}

func TestAnthropicStreamsAReply(t *testing.T) {
	want := Usage{Input: 120, CacheRead: 900, CacheWrite: 40, Output: 33}
	s := New(t, ToolCalls(
		Call{Name: "fs_read", Arguments: `{"path":"a.txt"}`},
		Call{ID: "toolu_mine", Name: "fs_list", Arguments: `{}`},
	).WithText("Reading the file.").WithReasoning("The file comes first.").WithUsage(want))

	resp, evs := stream(t, s, anthropicPath, `{"model":"m","stream":true,"messages":[]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	got := readAnthropic(t, evs)
	if got.text != "Reading the file." || got.reasoning != "The file comes first." {
		t.Errorf("text %q, reasoning %q", got.text, got.reasoning)
	}
	if got.signature == "" {
		t.Error("the thinking block has no signature")
	}
	if got.finish != "tool_use" {
		t.Errorf("stop reason %q, want tool_use", got.finish)
	}
	if len(got.calls) != 2 || got.calls[0].Name != "fs_read" || got.calls[0].Arguments != `{"path":"a.txt"}` || got.calls[0].ID == "" {
		t.Errorf("calls %+v", got.calls)
	}
	if got.calls[1].ID != "toolu_mine" {
		t.Errorf("a call's own id was not kept: %q", got.calls[1].ID)
	}
	if got.usage != want {
		t.Errorf("usage %+v, want %+v", got.usage, want)
	}
	if last := evs[len(evs)-1].name; last != "message_stop" {
		t.Errorf("the stream ends with %q", last)
	}
}

func TestChatStreamsAReply(t *testing.T) {
	want := Usage{Input: 120, CacheRead: 900, Output: 33, Reasoning: 12}
	s := New(t,
		ToolCalls(Call{Name: "fs_read", Arguments: `{"path":"a.txt"}`}, Call{Name: "fs_list", Arguments: `{}`}).
			WithText("Reading.").WithReasoning("First the file.").WithUsage(want),
		Text("Done.").WithUsage(want),
	)

	_, evs := stream(t, s, chatPath, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	got, done := readChat(t, evs)
	if !done {
		t.Error("the stream has no [DONE]")
	}
	if got.text != "Reading." || got.reasoning != "First the file." || got.finish != "tool_calls" {
		t.Errorf("text %q, reasoning %q, finish %q", got.text, got.reasoning, got.finish)
	}
	if len(got.calls) != 2 || got.calls[0].Arguments != `{"path":"a.txt"}` || got.calls[1].Name != "fs_list" || got.calls[0].ID == got.calls[1].ID {
		t.Errorf("calls %+v", got.calls)
	}
	if got.usage != want {
		t.Errorf("usage %+v, want %+v", got.usage, want)
	}

	// A stream that was not asked for usage carries none.
	_, evs = stream(t, s, chatPath, `{"model":"m","stream":true,"messages":[]}`)
	got, _ = readChat(t, evs)
	if got.text != "Done." || got.finish != "stop" {
		t.Errorf("text %q, finish %q", got.text, got.finish)
	}
	if got.sawUsage {
		t.Error("usage was sent without stream_options.include_usage")
	}
}

func TestWholeResponsesWhenNoStreamIsAsked(t *testing.T) {
	s := New(t,
		ToolCalls(Call{Name: "fs_read", Arguments: `{"path":"a.txt"}`}).WithText("Reading.").WithUsage(Usage{Input: 5, Output: 7}),
		Text("Summary.").WithUsage(Usage{Input: 5, CacheRead: 2, Output: 7}),
	)

	var a struct {
		Content []struct {
			Type, Text, Name string
			Input            map[string]string
		}
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Output int `json:"output_tokens"`
		}
	}
	resp := post(t, context.Background(), s.URL()+anthropicPath, `{"model":"m","messages":[]}`)
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if len(a.Content) != 2 || a.Content[0].Text != "Reading." || a.Content[1].Name != "fs_read" || a.Content[1].Input["path"] != "a.txt" || a.StopReason != "tool_use" || a.Usage.Output != 7 {
		t.Errorf("anthropic message %+v", a)
	}

	var c struct {
		Choices []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			Prompt int `json:"prompt_tokens"`
		}
	}
	resp = post(t, context.Background(), s.URL()+chatPath, `{"model":"m","messages":[]}`)
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.Choices) != 1 || c.Choices[0].Message.Content != "Summary." || c.Choices[0].FinishReason != "stop" || c.Usage.Prompt != 7 {
		t.Errorf("chat completion %+v", c)
	}
}

func TestAnAnswerCutAtTheOutputLimit(t *testing.T) {
	s := New(t,
		Cut("The report begins"), CutToolCall("write_report", `{"title":"Fin`),
		Cut("The report begins"), CutToolCall("write_report", `{"title":"Fin`),
	)
	body := `{"model":"m","stream":true,"messages":[]}`

	_, evs := stream(t, s, anthropicPath, body)
	if got := readAnthropic(t, evs); got.text != "The report begins" || got.finish != "max_tokens" {
		t.Errorf("anthropic cut text: %+v", got)
	}
	_, evs = stream(t, s, anthropicPath, body)
	got := readAnthropic(t, evs)
	if got.finish != "max_tokens" || len(got.calls) != 1 || got.calls[0].Arguments != `{"title":"Fin` {
		t.Errorf("anthropic cut call: %+v", got)
	}
	if json.Valid([]byte(got.calls[0].Arguments)) {
		t.Error("a cut call's arguments parse as JSON")
	}

	_, evs = stream(t, s, chatPath, body)
	if got, _ := readChat(t, evs); got.text != "The report begins" || got.finish != "length" {
		t.Errorf("chat cut text: %+v", got)
	}
	_, evs = stream(t, s, chatPath, body)
	if got, _ := readChat(t, evs); got.finish != "length" || len(got.calls) != 1 || got.calls[0].Arguments != `{"title":"Fin` {
		t.Errorf("chat cut call: %+v", got)
	}
}

func TestARepeatedCall(t *testing.T) {
	call := ToolCalls(Call{Name: "check_status", Arguments: `{"id":7}`})
	s := New(t, Repeat(5, call)...)
	s.Script(Text("Still pending."))
	if s.Remaining() != 6 {
		t.Fatalf("remaining %d, want 6", s.Remaining())
	}
	for i := 0; i < 5; i++ {
		_, evs := stream(t, s, chatPath, `{"model":"m","stream":true,"messages":[]}`)
		got, _ := readChat(t, evs)
		if len(got.calls) != 1 || got.calls[0].Name != "check_status" || got.calls[0].Arguments != `{"id":7}` {
			t.Fatalf("request %d: %+v", i+1, got)
		}
	}
	_, evs := stream(t, s, chatPath, `{"model":"m","stream":true,"messages":[]}`)
	if got, _ := readChat(t, evs); got.text != "Still pending." {
		t.Errorf("after the repeats: %+v", got)
	}
	if s.Remaining() != 0 {
		t.Errorf("remaining %d, want 0", s.Remaining())
	}
}

func TestUnavailableThenSuccess(t *testing.T) {
	s := New(t, Repeat(2, Status(http.StatusServiceUnavailable, "The server is overloaded."))...)
	s.Script(Status(529, "Overloaded"), Text("Here."), Text("Here."))

	for i := 0; i < 2; i++ {
		resp := post(t, context.Background(), s.URL()+chatPath, `{"model":"m","stream":true,"messages":[]}`)
		var e struct {
			Error struct{ Message, Type string }
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if resp.StatusCode != 503 || e.Error.Message != "The server is overloaded." || e.Error.Type != "server_error" {
			t.Fatalf("request %d: status %d, error %+v", i+1, resp.StatusCode, e)
		}
	}
	resp := post(t, context.Background(), s.URL()+anthropicPath, `{"model":"m","stream":true,"messages":[]}`)
	var e struct {
		Type  string
		Error struct{ Message, Type string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if resp.StatusCode != 529 || e.Type != "error" || e.Error.Type != "overloaded_error" {
		t.Errorf("anthropic overload: status %d, %+v", resp.StatusCode, e)
	}
	_, evs := stream(t, s, anthropicPath, `{"model":"m","stream":true,"messages":[]}`)
	if got := readAnthropic(t, evs); got.text != "Here." || got.finish != "end_turn" {
		t.Errorf("after the failures: %+v", got)
	}
	_, evs = stream(t, s, chatPath, `{"model":"m","stream":true,"messages":[]}`)
	if got, _ := readChat(t, evs); got.text != "Here." {
		t.Errorf("after the failures: %+v", got)
	}
}

func TestRateLimitedSaysWhenToRetry(t *testing.T) {
	s := New(t, RateLimited(20*time.Second), RateLimited(20*time.Second), Raw(418, "text/plain", "short and stout"))

	resp := post(t, context.Background(), s.URL()+chatPath, `{"model":"m","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "20" || !strings.Contains(string(body), `"code":"rate_limit_exceeded"`) {
		t.Errorf("chat: status %d, Retry-After %q, body %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	resp = post(t, context.Background(), s.URL()+anthropicPath, `{"model":"m","messages":[]}`)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "20" || !strings.Contains(string(body), `"type":"rate_limit_error"`) {
		t.Errorf("anthropic: status %d, Retry-After %q, body %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	resp = post(t, context.Background(), s.URL()+chatPath, `{"model":"m","messages":[]}`)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 418 || resp.Header.Get("Content-Type") != "text/plain" || string(body) != "short and stout" {
		t.Errorf("raw: status %d, %q, %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func TestOverflowAsEachServerWordsIt(t *testing.T) {
	cases := []struct {
		name    string
		dialect Dialect
		path    string
		status  int
		words   []string
	}{
		{"OpenAI", OpenAI, chatPath, 400, []string{"maximum context length is 4096 tokens", `"code":"context_length_exceeded"`}},
		{"vLLM", VLLM, chatPath, 400, []string{"maximum context length is 4096 tokens", "you requested 5121 tokens"}},
		{"llama.cpp", LlamaCpp, chatPath, 400, []string{"the request exceeds the available context size", `"type":"exceed_context_size_error"`, `"n_ctx":4096`}},
		{"LM Studio", LMStudio, chatPath, 200, []string{"greater than the context length", "n_ctx: 4096"}},
		{"Ollama", Ollama, chatPath, 400, []string{"prompt too long; exceeded max context length by 1025 tokens"}},
		{"Anthropic", AnthropicAPI, anthropicPath, 400, []string{"prompt is too long: 5121 tokens > 4096 maximum", `"type":"invalid_request_error"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(t, Overflow(c.dialect, 4096))
			resp := post(t, context.Background(), s.URL()+c.path, `{"model":"m","stream":true,"messages":[]}`)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != c.status {
				t.Errorf("status %d, want %d", resp.StatusCode, c.status)
			}
			for _, w := range c.words {
				if !strings.Contains(string(body), w) {
					t.Errorf("body lacks %q:\n%s", w, body)
				}
			}
			if c.dialect == LMStudio {
				evs, _ := events(strings.NewReader(string(body)))
				if resp.Header.Get("Content-Type") != "text/event-stream" || len(evs) != 1 || evs[0].name != "error" {
					t.Errorf("LM Studio's refusal is not one error event on a stream: %+v", evs)
				}
			} else if !json.Valid(body) {
				t.Errorf("body is not JSON: %s", body)
			}
		})
	}
}

func TestAStreamThatStalls(t *testing.T) {
	for _, path := range []string{anthropicPath, chatPath} {
		t.Run(path, func(t *testing.T) {
			s := New(t, Stall("Working on it"))
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			resp := post(t, ctx, s.URL()+path, `{"model":"m","stream":true,"messages":[]}`)
			evs, err := events(resp.Body)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the stream ended with %v, want the client's deadline", err)
			}
			var text string
			if path == anthropicPath {
				text = readAnthropic(t, evs).text
			} else {
				got, done := readChat(t, evs)
				text = got.text
				if done {
					t.Error("a stalled stream sent [DONE]")
				}
			}
			if text == "" || !strings.HasPrefix("Working on it", text) || text == "Working on it" {
				t.Errorf("before the stall the client read %q, want a part of the text", text)
			}
		})
	}
}

func TestAStalledStreamEndsWhenTheServerCloses(t *testing.T) {
	s := New(t, Stall("Working on it"))
	resp := post(t, context.Background(), s.URL()+chatPath, `{"model":"m","stream":true,"messages":[]}`)
	finished := make(chan struct{})
	go func() {
		_, _ = events(resp.Body)
		close(finished)
	}()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the client is still reading after the server closed")
	}
}

func TestAConnectionThatDrops(t *testing.T) {
	for _, path := range []string{anthropicPath, chatPath} {
		t.Run(path, func(t *testing.T) {
			s := New(t, Drop("Working on it"))
			resp := post(t, context.Background(), s.URL()+path, `{"model":"m","stream":true,"messages":[]}`)
			evs, err := events(resp.Body)
			if err == nil {
				t.Fatal("the stream ended cleanly, want a broken connection")
			}
			if len(evs) == 0 {
				t.Error("nothing arrived before the drop")
			}
			for _, e := range evs {
				if e.name == "message_stop" || e.data == "[DONE]" {
					t.Errorf("a dropped stream reached its end: %+v", e)
				}
			}
		})
	}
}

func TestRequestsAreRecordedAsSent(t *testing.T) {
	s := New(t, Text("ok"), Text("ok"))

	post(t, context.Background(), s.URL()+anthropicPath, `{
		"model":"claude-x","stream":true,"max_tokens":512,
		"system":[{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"fs_read","input_schema":{"type":"object"}},{"name":"fs_list","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"Read a.txt"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"The file first.","signature":"sig"},
				{"type":"text","text":"Reading."},
				{"type":"tool_use","id":"toolu_1","name":"fs_read","input":{"path":"a.txt"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"no such file"}]},
				{"type":"text","text":"Note from the runtime."}]}
		]}`)
	post(t, context.Background(), s.URL()+chatPath, `{
		"model":"local-x","stream":true,"max_completion_tokens":256,"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"fs_read","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"system","content":"Be brief."},
			{"role":"user","content":[{"type":"text","text":"Read a.txt"}]},
			{"role":"assistant","content":"Reading.","reasoning_content":"The file first.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"fs_read","arguments":"{\"path\":\"a.txt\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"no such file"}
		]}`)

	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests recorded, want 2", len(reqs))
	}

	a := reqs[0]
	if a.API != Anthropic || a.Method != "POST" || a.Path != anthropicPath || a.Model != "claude-x" || !a.Stream || a.MaxTokens != 512 {
		t.Errorf("anthropic request: %+v", a)
	}
	if a.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("headers were not kept: %v", a.Header)
	}
	if a.System != "Be brief." || strings.Join(a.Tools, ",") != "fs_read,fs_list" {
		t.Errorf("system %q, tools %v", a.System, a.Tools)
	}
	if len(a.Messages) != 3 {
		t.Fatalf("%d messages, want 3", len(a.Messages))
	}
	if m := a.Messages[1]; m.Role != "assistant" || m.Text != "Reading." || m.Reasoning != "The file first." || len(m.Calls) != 1 || m.Calls[0].ID != "toolu_1" || m.Calls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("assistant message: %+v", m)
	}
	if m := a.Messages[2]; m.Role != "user" || m.Text != "Note from the runtime." || len(m.Results) != 1 || m.Results[0] != (Result{CallID: "toolu_1", Text: "no such file", IsError: true}) {
		t.Errorf("result message: %+v", m)
	}
	if !json.Valid(a.Body) || !strings.Contains(string(a.Messages[0].Raw), `"Read a.txt"`) {
		t.Error("the body and the raw messages were not kept as sent")
	}

	c := reqs[1]
	if c.API != ChatCompletions || c.Model != "local-x" || c.MaxTokens != 256 || !c.IncludeUsage {
		t.Errorf("chat request: %+v", c)
	}
	if c.System != "Be brief." || strings.Join(c.Tools, ",") != "fs_read" || len(c.Messages) != 4 {
		t.Errorf("system %q, tools %v, %d messages", c.System, c.Tools, len(c.Messages))
	}
	if m := c.Messages[1]; m.Role != "user" || m.Text != "Read a.txt" {
		t.Errorf("user message: %+v", m)
	}
	if m := c.Messages[2]; m.Reasoning != "The file first." || len(m.Calls) != 1 || m.Calls[0].Name != "fs_read" {
		t.Errorf("assistant message: %+v", m)
	}
	if m := c.Messages[3]; m.Role != "tool" || len(m.Results) != 1 || m.Results[0] != (Result{CallID: "call_1", Text: "no such file"}) {
		t.Errorf("tool message: %+v", m)
	}
}

func TestPrefixOf(t *testing.T) {
	s := New(t, Repeat(6, Text("ok"))...)
	send := func(body string) Request {
		post(t, context.Background(), s.URL()+anthropicPath, body)
		reqs := s.Requests()
		return reqs[len(reqs)-1]
	}
	const tools = `"tools":[{"name":"fs_read"}],`
	first := send(`{"system":"Be brief.",` + tools + `"messages":[
		{"role":"user","content":[{"type":"text","text":"Read a.txt","cache_control":{"type":"ephemeral"}}]}]}`)

	// The marker moved to the new last message and the keys are in another
	// order: still a continuation.
	next := send(`{` + tools + `"system":"Be brief.","messages":[
		{"content":[{"text":"Read a.txt","type":"text"}],"role":"user"},
		{"role":"assistant","content":"Reading."},
		{"role":"user","content":[{"type":"text","text":"And b.txt","cache_control":{"type":"ephemeral"}}]}]}`)
	if err := PrefixOf(first, next); err != nil {
		t.Errorf("a continuation was refused: %v", err)
	}
	if err := PrefixOf(next, first); err == nil {
		t.Error("a shorter request was taken for a continuation")
	}

	rewritten := send(`{"system":"Be brief.",` + tools + `"messages":[
		{"role":"user","content":[{"type":"text","text":"Read a.txt, please"}]},
		{"role":"assistant","content":"Reading."}]}`)
	if err := PrefixOf(first, rewritten); err == nil || !strings.Contains(err.Error(), "message 0") {
		t.Errorf("a rewritten first message: %v", err)
	}

	otherSystem := send(`{"system":"Be thorough.",` + tools + `"messages":[
		{"role":"user","content":[{"type":"text","text":"Read a.txt"}]}]}`)
	if err := PrefixOf(first, otherSystem); err == nil || !strings.Contains(err.Error(), "system") {
		t.Errorf("a changed system prompt: %v", err)
	}

	otherTools := send(`{"system":"Be brief.","tools":[{"name":"fs_read"},{"name":"fs_list"}],"messages":[
		{"role":"user","content":[{"type":"text","text":"Read a.txt"}]}]}`)
	if err := PrefixOf(first, otherTools); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Errorf("a changed tool list: %v", err)
	}
}

// recorder is a testing.TB that keeps what a server reports at cleanup.
type recorder struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (r *recorder) Helper()          {}
func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, format)
}

func TestARequestPastTheScriptFailsTheTest(t *testing.T) {
	rec := &recorder{TB: t}
	s := New(rec, Text("only one"))

	post(t, context.Background(), s.URL()+chatPath, `{"model":"m","messages":[]}`)
	resp := post(t, context.Background(), s.URL()+chatPath, `{"model":"m","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || !strings.Contains(string(body), "no step left") {
		t.Errorf("past the script: status %d, %s", resp.StatusCode, body)
	}
	resp = post(t, context.Background(), s.URL()+"/v1/models", `{}`)
	if resp.StatusCode != 404 {
		t.Errorf("an unserved path: status %d", resp.StatusCode)
	}
	if reqs := s.Requests(); len(reqs) != 3 || reqs[2].API != Unknown {
		t.Errorf("recorded %d requests", len(reqs))
	}

	for _, f := range rec.cleanups {
		f()
	}
	if len(rec.errors) != 1 {
		t.Errorf("the test was failed %d times, want once for the request past the script", len(rec.errors))
	}
}

const responsesPath = "/v1/responses"

func TestResponsesStreamsAReply(t *testing.T) {
	s := New(t,
		ToolCalls(Call{Name: "fs_read", Arguments: `{"path":"a.txt"}`}).WithText("Reading.").WithReasoning("First the file.").
			WithUsage(Usage{Input: 120, CacheRead: 900, Output: 33, Reasoning: 12}),
		CutToolCall("write_report", `{"title":"Fin`),
		Text("Done.").WithUsage(Usage{Input: 5, Output: 7}),
	)
	read := func(evs []event) (types []string, last map[string]any, items []map[string]any) {
		for _, e := range evs {
			var d map[string]any
			if err := json.Unmarshal([]byte(e.data), &d); err != nil {
				t.Fatalf("event %q is not JSON: %v", e.data, err)
			}
			if d["type"] != e.name {
				t.Errorf("event named %q carries type %v", e.name, d["type"])
			}
			types = append(types, e.name)
			if e.name == "response.output_item.done" {
				items = append(items, d["item"].(map[string]any))
			}
			last = d
		}
		return types, last, items
	}

	_, evs := stream(t, s, responsesPath, `{"model":"m","stream":true,"input":[]}`)
	types, last, items := read(evs)
	if types[len(types)-1] != "response.completed" || len(items) != 3 {
		t.Fatalf("events %v, %d finished items", types, len(items))
	}
	if items[0]["type"] != "reasoning" || items[0]["encrypted_content"] == "" || items[1]["type"] != "message" {
		t.Errorf("reasoning and text items: %v", items[:2])
	}
	if c := items[2]; c["type"] != "function_call" || c["name"] != "fs_read" || c["arguments"] != `{"path":"a.txt"}` || c["call_id"] == "" {
		t.Errorf("call item: %v", c)
	}
	usage := last["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1020) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(900) || usage["output_tokens"] != float64(33) {
		t.Errorf("usage: %v", usage)
	}

	// A call the reply stopped inside is started and never done.
	_, evs = stream(t, s, responsesPath, `{"model":"m","stream":true,"input":[]}`)
	types, last, items = read(evs)
	if types[len(types)-1] != "response.incomplete" || len(items) != 0 {
		t.Errorf("a cut call: events %v, %d finished items", types, len(items))
	}
	if r := last["response"].(map[string]any); r["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("why the reply is incomplete: %v", r)
	}

	// A whole response when no stream is asked for.
	resp := post(t, context.Background(), s.URL()+responsesPath, `{"model":"m","input":[]}`)
	var whole struct {
		Status string
		Output []struct {
			Type    string
			Content []struct{ Text string }
		}
		Usage struct {
			Output int `json:"output_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&whole); err != nil {
		t.Fatal(err)
	}
	if whole.Status != "completed" || len(whole.Output) != 1 || whole.Output[0].Content[0].Text != "Done." || whole.Usage.Output != 7 {
		t.Errorf("whole response: %+v", whole)
	}
}

func TestResponsesRequestsAreRecorded(t *testing.T) {
	s := New(t, Text("ok"))
	post(t, context.Background(), s.URL()+responsesPath, `{
		"model":"gpt-x","stream":true,"store":false,"include":["reasoning.encrypted_content"],"max_output_tokens":256,
		"instructions":"Be brief.",
		"tools":[{"type":"function","name":"fs_read","parameters":{"type":"object"}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Read a.txt"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"The file first."}],"encrypted_content":"opaque"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reading."}]},
			{"type":"function_call","call_id":"call_1","name":"fs_read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"no such file"}
		]}`)
	r := s.Requests()[0]
	if r.API != Responses || r.Model != "gpt-x" || !r.Stream || r.MaxTokens != 256 || r.System != "Be brief." || strings.Join(r.Tools, ",") != "fs_read" {
		t.Errorf("request: %+v", r)
	}
	if r.Store == nil || *r.Store || len(r.Include) != 1 {
		t.Errorf("store %v, include %v", r.Store, r.Include)
	}
	if len(r.Messages) != 5 {
		t.Fatalf("%d input items, want 5", len(r.Messages))
	}
	if m := r.Messages[0]; m.Role != "user" || m.Text != "Read a.txt" {
		t.Errorf("user message: %+v", m)
	}
	if m := r.Messages[1]; m.Reasoning != "The file first." {
		t.Errorf("reasoning item: %+v", m)
	}
	if m := r.Messages[3]; len(m.Calls) != 1 || m.Calls[0].ID != "call_1" || m.Calls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("call item: %+v", m)
	}
	if m := r.Messages[4]; m.Role != "tool" || len(m.Results) != 1 || m.Results[0] != (Result{CallID: "call_1", Text: "no such file"}) {
		t.Errorf("output item: %+v", m)
	}
}
