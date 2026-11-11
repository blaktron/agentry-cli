package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AnthropicBaseURL is the Anthropic API's own address.
const AnthropicBaseURL = "https://api.anthropic.com"

// anthropicVersion is the API version the client is written against.
const anthropicVersion = "2023-06-01"

// Anthropic reaches a model over the Anthropic Messages API.
type Anthropic struct {
	// BaseURL is the server's address with no trailing slash.
	BaseURL string
	// Model is the model's id.
	Model string
	// Key returns the API key. It is called for every request, so the key
	// is held nowhere else.
	Key func() string
	// HTTP sends the requests. It must not set a response timeout: a
	// reply streams for as long as the model writes.
	HTTP *http.Client
}

// The request body. A cache marker goes on the last tool, on the system
// prompt and on the last block of each of the last two messages, which is
// the most the API allows and lets a server reuse everything before the
// newest message.
type (
	aCache struct {
		Type string `json:"type"`
	}
	aSource struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	aBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`

		Thinking  string `json:"thinking,omitempty"`
		Signature string `json:"signature,omitempty"`
		Data      string `json:"data,omitempty"`

		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`

		ToolUseID string   `json:"tool_use_id,omitempty"`
		Content   []aBlock `json:"content,omitempty"`
		IsError   bool     `json:"is_error,omitempty"`

		Source *aSource `json:"source,omitempty"`

		CacheControl *aCache `json:"cache_control,omitempty"`
	}
	aMessage struct {
		Role    string   `json:"role"`
		Content []aBlock `json:"content"`
	}
	aTool struct {
		Name         string          `json:"name"`
		Description  string          `json:"description,omitempty"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl *aCache         `json:"cache_control,omitempty"`
	}
	aRequest struct {
		Model     string     `json:"model"`
		MaxTokens int        `json:"max_tokens"`
		Stream    bool       `json:"stream"`
		System    []aBlock   `json:"system,omitempty"`
		Tools     []aTool    `json:"tools,omitempty"`
		Messages  []aMessage `json:"messages"`
	}
)

// reasoningMeta is what a reasoning part keeps for the server: a thinking
// block's signature, or a redacted block's data.
type reasoningMeta struct {
	Signature string `json:"signature,omitempty"`
	Redacted  string `json:"redacted,omitempty"`
}

var ephemeral = &aCache{Type: "ephemeral"}

func (a *Anthropic) body(req Request) ([]byte, error) {
	out := aRequest{Model: a.Model, MaxTokens: req.MaxTokens, Stream: true}
	if req.System != "" {
		out.System = []aBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(bytes.TrimSpace(schema)) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, aTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if n := len(out.Tools); n > 0 {
		out.Tools[n-1].CacheControl = ephemeral
	}
	for _, m := range req.Messages {
		msg := aMessage{Role: string(m.Role)}
		for _, p := range m.Parts {
			b, ok := anthropicBlock(p)
			if ok {
				msg.Content = append(msg.Content, b)
			}
		}
		if len(msg.Content) == 0 {
			continue
		}
		out.Messages = append(out.Messages, msg)
	}
	// A thinking block takes no marker, so the marker goes on the last
	// block of a message that can carry one.
	marked := 0
	for i := len(out.Messages) - 1; i >= 0 && marked < 2; i-- {
		c := out.Messages[i].Content
		for j := len(c) - 1; j >= 0; j-- {
			if c[j].Type != "thinking" && c[j].Type != "redacted_thinking" {
				c[j].CacheControl = ephemeral
				marked++
				break
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func anthropicBlock(p Part) (aBlock, bool) {
	switch p.Kind {
	case Text:
		if p.Text == "" {
			return aBlock{}, false
		}
		return aBlock{Type: "text", Text: p.Text}, true
	case Reasoning:
		var meta reasoningMeta
		_ = json.Unmarshal(p.Meta, &meta)
		switch {
		case meta.Redacted != "":
			return aBlock{Type: "redacted_thinking", Data: meta.Redacted}, true
		case meta.Signature != "":
			return aBlock{Type: "thinking", Thinking: p.Text, Signature: meta.Signature}, true
		}
		// Reasoning with no signature is not this server's, and it
		// refuses a thinking block it did not sign.
		return aBlock{}, false
	case Call:
		// The API takes a call's input only as an object. A call whose
		// arguments were cut is sent back with none; its result says why
		// it was not run.
		args := json.RawMessage(p.Arguments)
		if !isObject(args) {
			args = json.RawMessage(`{}`)
		}
		return aBlock{Type: "tool_use", ID: p.CallID, Name: p.Name, Input: args}, true
	case Result:
		b := aBlock{Type: "tool_result", ToolUseID: p.CallID, IsError: p.Failed}
		// The API refuses an empty text block; a result with nothing in
		// it is sent with no content.
		if p.Text != "" {
			b.Content = append(b.Content, aBlock{Type: "text", Text: p.Text})
		}
		for _, img := range p.Images {
			b.Content = append(b.Content, aBlock{Type: "image", Source: &aSource{Type: "base64", MediaType: img.MediaType, Data: img.Data}})
		}
		return b, true
	}
	return aBlock{}, false
}

// Stream sends req and reads the reply's event stream.
func (a *Anthropic) Stream(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	body, err := a.body(req)
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	base := strings.TrimRight(a.BaseURL, "/")
	if base == "" {
		base = AnthropicBaseURL
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("anthropic-version", anthropicVersion)
	hreq.Header.Set("x-api-key", a.Key())

	res, err := a.HTTP.Do(hreq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, anthropicHTTPError(res)
	}
	return a.read(ctx, res.Body, onDelta)
}

// transportError is a failure with no answer from the server. A cancelled
// request is returned as the context's own error.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &Error{Class: Transport, Message: err.Error()}
}

func anthropicHTTPError(res *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var wire struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &wire)
	msg := wire.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = res.Status
	}
	e := &Error{Status: res.StatusCode, Message: msg, Class: anthropicClass(res.StatusCode, wire.Error.Type, msg)}
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// anthropicClass sorts a failure by its status, then by the error type the
// API names, then by its words: the refusal of a conversation that is too
// long is an ordinary 400 told apart only by its message.
func anthropicClass(status int, kind, message string) Class {
	lower := strings.ToLower(message)
	switch {
	case kind == "overloaded_error" || status == 529 || status == http.StatusServiceUnavailable:
		return Capacity
	case kind == "rate_limit_error" || status == http.StatusTooManyRequests:
		return RateLimit
	case status == http.StatusPaymentRequired || strings.Contains(lower, "credit balance is too low"):
		return Credits
	case kind == "authentication_error" || kind == "permission_error" || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Auth
	case strings.Contains(lower, "prompt is too long") || kind == "request_too_large" || status == http.StatusRequestEntityTooLarge:
		return Overflow
	case status >= 400 && status < 500:
		return BadRequest
	}
	return Server
}

// The stream's events, as far as the client reads them.
type aEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage aUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
		Data string `json:"data"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage aUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type aUsage struct {
	Input      int64 `json:"input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

// block is one content block being put together from its deltas.
type block struct {
	kind      string
	text      strings.Builder
	signature strings.Builder
	redacted  string
	id, name  string
	args      strings.Builder
}

func (a *Anthropic) read(ctx context.Context, r io.Reader, onDelta func(Delta)) (*Response, error) {
	var (
		blocks  []*block
		byIndex = map[int]*block{}
		resp    = &Response{Finish: FinishOther}
		stopped bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		var ev aEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			resp.Usage.Input, resp.Usage.CacheRead, resp.Usage.CacheWrite, resp.Usage.Output = u.Input, u.CacheRead, u.CacheWrite, u.Output
		case "content_block_start":
			b := &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, redacted: ev.ContentBlock.Data}
			b.text.WriteString(ev.ContentBlock.Text)
			byIndex[ev.Index] = b
			blocks = append(blocks, b)
		case "content_block_delta":
			b := byIndex[ev.Index]
			if b == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				if onDelta != nil && ev.Delta.Text != "" {
					onDelta(Delta{Kind: Text, Text: ev.Delta.Text})
				}
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				if onDelta != nil && ev.Delta.Thinking != "" {
					onDelta(Delta{Kind: Reasoning, Text: ev.Delta.Thinking})
				}
			case "signature_delta":
				b.signature.WriteString(ev.Delta.Signature)
			case "input_json_delta":
				b.args.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Usage.Output != 0 {
				resp.Usage.Output = ev.Usage.Output
			}
			switch ev.Delta.StopReason {
			case "end_turn", "stop_sequence":
				resp.Finish = FinishStop
			case "tool_use":
				resp.Finish = FinishToolCalls
			case "max_tokens":
				resp.Finish = FinishLength
			case "":
			default:
				resp.Finish = FinishOther
			}
		case "message_stop":
			stopped = true
		case "error":
			return nil, &Error{Class: anthropicClass(0, ev.Error.Type, ev.Error.Message), Message: ev.Error.Message}
		}
		if stopped {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, transportError(ctx, err)
	}
	if !stopped {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Class: Transport, Message: "the reply's stream ended before the reply did"}
	}
	for _, b := range blocks {
		switch b.kind {
		case "text":
			if b.text.Len() > 0 {
				resp.Parts = append(resp.Parts, Part{Kind: Text, Text: b.text.String()})
			}
		case "thinking":
			meta, _ := json.Marshal(reasoningMeta{Signature: b.signature.String()})
			resp.Parts = append(resp.Parts, Part{Kind: Reasoning, Text: b.text.String(), Meta: meta})
		case "redacted_thinking":
			meta, _ := json.Marshal(reasoningMeta{Redacted: b.redacted})
			resp.Parts = append(resp.Parts, Part{Kind: Reasoning, Meta: meta})
		case "tool_use":
			args := b.args.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			resp.Parts = append(resp.Parts, Part{Kind: Call, CallID: b.id, Name: b.name, Arguments: args})
		}
	}
	return resp, nil
}

// isObject reports whether raw is a JSON object.
func isObject(raw json.RawMessage) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil && v != nil
}

// IsError reports whether err is a model failure of class c.
func IsError(err error, c Class) bool {
	var e *Error
	return errors.As(err, &e) && e.Class == c
}
