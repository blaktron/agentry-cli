package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Chat reaches a model over OpenAI-style Chat Completions, as OpenAI-
// compatible servers speak it: a model server on the person's own machine,
// or a hosted router. The servers agree on the request and differ in the
// details of the reply, so the reader accepts every shape it knows.
type Chat struct {
	// BaseURL is the server's address up to and without /chat/completions.
	BaseURL string
	// Model is the model's id.
	Model string
	// Key returns the bearer. An empty key sends no Authorization header,
	// for a server that takes none.
	Key func() string
	// HTTP sends the requests. It must not set a response timeout.
	HTTP *http.Client

	// Sampling is sent with every request; a nil field is left out.
	Temperature, TopP, MinP, PresencePenalty, RepetitionPenalty *float64
	TopK                                                        *int64
	// ExtraBody's members are added to every request's body as they are.
	ExtraBody json.RawMessage
	// ReplayReasoning sends earlier reasoning back with each assistant
	// message, as reasoning_content.
	ReplayReasoning bool
}

type (
	cFunction struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	cToolCall struct {
		ID       string    `json:"id"`
		Type     string    `json:"type"`
		Function cFunction `json:"function"`
	}
	cImageURL struct {
		URL string `json:"url"`
	}
	cPart struct {
		Type     string     `json:"type"`
		Text     string     `json:"text,omitempty"`
		ImageURL *cImageURL `json:"image_url,omitempty"`
	}
	cMessage struct {
		Role string `json:"role"`
		// Content is a string, a list of parts, or null.
		Content          any         `json:"content"`
		ReasoningContent string      `json:"reasoning_content,omitempty"`
		ToolCalls        []cToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string      `json:"tool_call_id,omitempty"`
	}
	cToolDef struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
)

// body builds the request. It is a map so that ExtraBody's members and the
// sampling fields sit at the top level beside the API's own.
func (c *Chat) body(req Request) ([]byte, error) {
	out := map[string]any{
		"model":          c.Model,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if len(bytes.TrimSpace(c.ExtraBody)) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(c.ExtraBody, &extra); err != nil {
			return nil, fmt.Errorf("the extra body is not a JSON object: %w", err)
		}
		for k, v := range extra {
			out[k] = v
		}
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	for name, v := range map[string]*float64{
		"temperature": c.Temperature, "top_p": c.TopP, "min_p": c.MinP,
		"presence_penalty": c.PresencePenalty, "repetition_penalty": c.RepetitionPenalty,
	} {
		if v != nil {
			out[name] = *v
		}
	}
	if c.TopK != nil {
		out["top_k"] = *c.TopK
	}

	var msgs []cMessage
	if req.System != "" {
		msgs = append(msgs, cMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, c.messages(m)...)
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]cToolDef, 0, len(req.Tools))
		for _, t := range req.Tools {
			d := cToolDef{Type: "function"}
			d.Function.Name, d.Function.Description, d.Function.Parameters = t.Name, t.Description, t.Schema
			if len(bytes.TrimSpace(t.Schema)) == 0 {
				d.Function.Parameters = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, d)
		}
		out["tools"] = tools
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// messages is one message of the conversation as the API carries it: an
// assistant message with its calls, a tool message a result, and the
// person's text after the results it came with.
func (c *Chat) messages(m Message) []cMessage {
	if m.Role == Assistant {
		out := cMessage{Role: "assistant"}
		var text, reasoning strings.Builder
		for _, p := range m.Parts {
			switch p.Kind {
			case Text:
				text.WriteString(p.Text)
			case Reasoning:
				reasoning.WriteString(p.Text)
			case Call:
				args := p.Arguments
				if !isObject(json.RawMessage(args)) {
					// Unfinished arguments would be refused as a
					// malformed request; the call's result says why
					// it was not run.
					args = "{}"
				}
				out.ToolCalls = append(out.ToolCalls, cToolCall{ID: p.CallID, Type: "function", Function: cFunction{Name: p.Name, Arguments: args}})
			}
		}
		if text.Len() > 0 || len(out.ToolCalls) == 0 {
			out.Content = text.String()
		}
		if c.ReplayReasoning {
			out.ReasoningContent = reasoning.String()
		}
		return []cMessage{out}
	}

	var out []cMessage
	var images []cPart
	var text strings.Builder
	for _, p := range m.Parts {
		switch p.Kind {
		case Result:
			content := p.Text
			if p.Failed && content == "" {
				content = "The tool reported an error."
			}
			out = append(out, cMessage{Role: "tool", ToolCallID: p.CallID, Content: content})
			for _, img := range p.Images {
				images = append(images, cPart{Type: "image_url", ImageURL: &cImageURL{URL: "data:" + img.MediaType + ";base64," + img.Data}})
			}
		case Text:
			text.WriteString(p.Text)
		}
	}
	// A tool message carries text alone, so a result's images follow in a
	// message of their own.
	switch {
	case len(images) > 0:
		parts := []cPart{{Type: "text", Text: "The images the tools above returned:"}}
		parts = append(parts, images...)
		if text.Len() > 0 {
			parts = append(parts, cPart{Type: "text", Text: text.String()})
		}
		out = append(out, cMessage{Role: "user", Content: parts})
	case text.Len() > 0:
		out = append(out, cMessage{Role: "user", Content: text.String()})
	}
	return out
}

// Stream sends req and reads the reply's chunks.
func (c *Chat) Stream(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	body, err := c.body(req)
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if c.Key != nil {
		if key := c.Key(); key != "" {
			hreq.Header.Set("Authorization", "Bearer "+key)
		}
	}
	res, err := c.HTTP.Do(hreq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, chatHTTPError(res)
	}
	return c.read(ctx, res.Body, onDelta)
}

// chatErrorText is the words of an error body. Servers wrap them as
// {"error":{"message":…}}, {"error":"…"} or {"message":…}.
func chatErrorText(raw []byte) (message, code string) {
	var wire struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return "", ""
	}
	var obj struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	var s string
	switch {
	case json.Unmarshal(wire.Error, &obj) == nil && obj.Message != "":
		code = strings.Trim(string(obj.Code), `"`)
		if code == "" || code == "null" {
			code = obj.Type
		}
		return obj.Message, code
	case json.Unmarshal(wire.Error, &s) == nil && s != "":
		return s, ""
	}
	return wire.Message, ""
}

func chatHTTPError(res *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	msg, code := chatErrorText(raw)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = res.Status
	}
	e := &Error{Status: res.StatusCode, Message: msg, Class: chatClass(res.StatusCode, code, msg)}
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// overflowWords is how the servers say a conversation is too long for the
// model: OpenAI and vLLM ("maximum context length"), llama.cpp ("exceeds
// the available context size"), LM Studio ("greater than the context
// length"), Ollama ("prompt too long"), and routers that pass an Anthropic
// model's own words on ("prompt is too long").
var overflowWords = regexp.MustCompile(`(?i)maximum context length|exceeds? the available context size|context size has been exceeded|greater than the context length|prompt (?:is )?too long|context[_ ]length[_ ]exceeded|exceed_context_size`)

// chatClass sorts a failure by the server's code, then its words, then its
// status. The refusal of a conversation that is too long comes as an
// ordinary 400 on most servers and is told apart by its code or words.
func chatClass(status int, code, message string) Class {
	lower := strings.ToLower(message)
	switch {
	case overflowWords.MatchString(code) || overflowWords.MatchString(message) || status == http.StatusRequestEntityTooLarge:
		return Overflow
	case status == http.StatusPaymentRequired || code == "insufficient_quota" || strings.Contains(lower, "insufficient credits") || strings.Contains(lower, "out of credits"):
		return Credits
	case status == http.StatusTooManyRequests || code == "rate_limit_exceeded":
		return RateLimit
	case status == http.StatusServiceUnavailable || status == 529 || strings.Contains(lower, "overloaded"):
		return Capacity
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Auth
	case status >= 400 && status < 500:
		return BadRequest
	}
	return Server
}

// A chunk of the reply, as far as the client reads it. Reasoning arrives
// under one of three names, depending on the server.
type cChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ReasoningText    string `json:"reasoning_text"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		Details    struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func (c *Chat) read(ctx context.Context, r io.Reader, onDelta func(Delta)) (*Response, error) {
	type call struct {
		id, name string
		args     strings.Builder
	}
	var (
		text, reasoning strings.Builder
		calls           []*call
		byIndex         = map[int]*call{}
		finish          string
		event           string
		resp            = &Response{}
	)
	emit := func(kind Kind, s string) {
		if s == "" {
			return
		}
		if kind == Reasoning {
			reasoning.WriteString(s)
		} else {
			text.WriteString(s)
		}
		if onDelta != nil {
			onDelta(Delta{Kind: kind, Text: s})
		}
	}
	// Some servers put the model's reasoning in the answer itself, between
	// think tags at its start; the splitter takes it back out.
	split := thinkSplitter{emit: emit}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[len("event:"):]))
			continue
		}
		if len(line) == 0 {
			event = ""
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if string(data) == "[DONE]" {
			break
		}
		var ch cChunk
		if json.Unmarshal(data, &ch) != nil {
			continue
		}
		// A server that has already answered 200 reports a failure as an
		// error in the stream.
		if event == "error" || len(ch.Error) > 0 && string(ch.Error) != "null" {
			msg, code := chatErrorText(data)
			if msg == "" {
				msg = strings.TrimSpace(string(data))
			}
			return nil, &Error{Class: chatClass(0, code, msg), Message: msg}
		}
		if ch.Usage != nil {
			resp.Usage = Usage{
				Input:     ch.Usage.Prompt - ch.Usage.Details.Cached,
				CacheRead: ch.Usage.Details.Cached,
				Output:    ch.Usage.Completion,
			}
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			emit(Reasoning, d.ReasoningContent+d.Reasoning+d.ReasoningText)
			split.write(d.Content)
			for _, tc := range d.ToolCalls {
				// A server that numbers its calls says which one a
				// piece belongs to; one that does not sends each call
				// whole, in order.
				idx := len(calls)
				if tc.Index != nil {
					idx = *tc.Index
				} else if tc.ID == "" && tc.Function.Name == "" && len(calls) > 0 {
					idx = len(calls) - 1
				}
				cur := byIndex[idx]
				if cur == nil {
					cur = &call{}
					byIndex[idx] = cur
					calls = append(calls, cur)
				}
				if tc.ID != "" {
					cur.id = tc.ID
				}
				if tc.Function.Name != "" && cur.name == "" {
					cur.name = tc.Function.Name
				}
				cur.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
	}
	split.flush()
	if err := sc.Err(); err != nil {
		return nil, transportError(ctx, err)
	}
	if finish == "" {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Class: Transport, Message: "the reply's stream ended before the reply did"}
	}

	if reasoning.Len() > 0 {
		resp.Parts = append(resp.Parts, Part{Kind: Reasoning, Text: reasoning.String()})
	}
	if text.Len() > 0 {
		resp.Parts = append(resp.Parts, Part{Kind: Text, Text: text.String()})
	}
	for i, cl := range calls {
		id := cl.id
		if id == "" {
			// A server that gives a call no id still needs its result
			// matched to it.
			id = fmt.Sprintf("call_%d", i+1)
		}
		args := cl.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		resp.Parts = append(resp.Parts, Part{Kind: Call, CallID: id, Name: cl.name, Arguments: args})
	}
	switch finish {
	case "length":
		resp.Finish = FinishLength
	case "tool_calls", "function_call":
		resp.Finish = FinishToolCalls
	case "stop":
		// Some servers end a reply that asks for calls with "stop".
		resp.Finish = FinishStop
		if len(calls) > 0 {
			resp.Finish = FinishToolCalls
		}
	default:
		resp.Finish = FinishOther
	}
	return resp, nil
}

// thinkSplitter passes an answer's text on, taking out reasoning a server
// left in it between <think> and </think> at its very start.
type thinkSplitter struct {
	emit func(Kind, string)
	// state: 0 undecided (at the start), 1 inside the tags, 2 past them.
	state int
	held  string
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

func (t *thinkSplitter) write(s string) {
	if s == "" {
		return
	}
	switch t.state {
	case 0:
		t.held += s
		trimmed := strings.TrimLeft(t.held, " \t\r\n")
		switch {
		case strings.HasPrefix(trimmed, thinkOpen):
			t.state, t.held = 1, ""
			t.write(trimmed[len(thinkOpen):])
		case strings.HasPrefix(thinkOpen, trimmed):
			// Could still become the tag; wait for more.
		default:
			held := t.held
			t.state, t.held = 2, ""
			t.emit(Text, held)
		}
	case 1:
		t.held += s
		if i := strings.Index(t.held, thinkClose); i >= 0 {
			inside, after := t.held[:i], t.held[i+len(thinkClose):]
			t.state, t.held = 2, ""
			t.emit(Reasoning, inside)
			t.emit(Text, strings.TrimLeft(after, "\r\n"))
			return
		}
		// Pass on all but a tail that could be the start of the closing
		// tag.
		keep := 0
		for n := len(thinkClose) - 1; n > 0; n-- {
			if strings.HasSuffix(t.held, thinkClose[:n]) {
				keep = n
				break
			}
		}
		t.emit(Reasoning, t.held[:len(t.held)-keep])
		t.held = t.held[len(t.held)-keep:]
	default:
		t.emit(Text, s)
	}
}

// flush passes on what was held when the reply ended.
func (t *thinkSplitter) flush() {
	if t.held == "" {
		return
	}
	held := t.held
	t.held = ""
	if t.state == 1 {
		t.emit(Reasoning, held)
		return
	}
	t.emit(Text, held)
}
