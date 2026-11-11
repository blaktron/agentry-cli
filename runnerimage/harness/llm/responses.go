package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// OpenAIBaseURL is the OpenAI API's own address.
const OpenAIBaseURL = "https://api.openai.com/v1"

// Responses reaches a model over the OpenAI Responses API.
//
// The client keeps no state on the server: every request sets store to
// false, carries the whole conversation, and asks for each reasoning item's
// encrypted content, which it sends back unchanged so the model has its
// earlier reasoning without the server having kept it.
type Responses struct {
	// BaseURL is the server's address up to and without /responses.
	BaseURL string
	// Model is the model's id.
	Model string
	// Key returns the bearer.
	Key func() string
	// HTTP sends the requests. It must not set a response timeout.
	HTTP *http.Client
}

type (
	rContent struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL string `json:"image_url,omitempty"`
	}
	rMessage struct {
		Type    string     `json:"type"`
		Role    string     `json:"role"`
		Content []rContent `json:"content"`
	}
	rCall struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	rOutput struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	rTool struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	}
	rRequest struct {
		Model           string   `json:"model"`
		Instructions    string   `json:"instructions,omitempty"`
		Input           []any    `json:"input"`
		Tools           []rTool  `json:"tools,omitempty"`
		Stream          bool     `json:"stream"`
		Store           bool     `json:"store"`
		Include         []string `json:"include"`
		MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	}
)

func (c *Responses) body(req Request) ([]byte, error) {
	out := rRequest{
		Model: c.Model, Instructions: req.System, Stream: true, Store: false,
		Include: []string{"reasoning.encrypted_content"}, MaxOutputTokens: req.MaxTokens,
		Input: []any{},
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(bytes.TrimSpace(schema)) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		// Not strict: a strict tool needs a schema with no optional
		// property, which the tools' own schemas do not promise.
		out.Tools = append(out.Tools, rTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schema})
	}
	for _, m := range req.Messages {
		out.Input = append(out.Input, responsesItems(m)...)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// responsesItems is one message as the API's input items: the API takes a
// message's text, each reasoning item, each call and each call's output as
// an item of its own, in order.
func responsesItems(m Message) []any {
	var out []any
	if m.Role == Assistant {
		for _, p := range m.Parts {
			switch p.Kind {
			case Reasoning:
				// The server's own item, sent back as it came. Reasoning
				// from another server has no item and is left out.
				if isObject(p.Meta) {
					out = append(out, p.Meta)
				}
			case Text:
				if p.Text != "" {
					out = append(out, rMessage{Type: "message", Role: "assistant", Content: []rContent{{Type: "output_text", Text: p.Text}}})
				}
			case Call:
				args := p.Arguments
				if !isObject(json.RawMessage(args)) {
					args = "{}"
				}
				out = append(out, rCall{Type: "function_call", CallID: p.CallID, Name: p.Name, Arguments: args})
			}
		}
		return out
	}
	var content []rContent
	for _, p := range m.Parts {
		switch p.Kind {
		case Result:
			text := p.Text
			if p.Failed && text == "" {
				text = "The tool reported an error."
			}
			out = append(out, rOutput{Type: "function_call_output", CallID: p.CallID, Output: text})
			for _, img := range p.Images {
				content = append(content, rContent{Type: "input_image", ImageURL: "data:" + img.MediaType + ";base64," + img.Data})
			}
		case Text:
			if p.Text != "" {
				content = append(content, rContent{Type: "input_text", Text: p.Text})
			}
		}
	}
	if len(content) > 0 {
		out = append(out, rMessage{Type: "message", Role: "user", Content: content})
	}
	return out
}

// Stream sends req and reads the reply's events.
func (c *Responses) Stream(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	body, err := c.body(req)
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = OpenAIBaseURL
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("Authorization", "Bearer "+c.Key())
	res, err := c.HTTP.Do(hreq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// The API's errors have Chat Completions' shape and codes.
		return nil, chatHTTPError(res)
	}
	return c.read(ctx, res.Body, onDelta)
}

// The stream's events, as far as the client reads them.
type rEvent struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta"`
	Item  json.RawMessage `json:"item"`
	// Response is set on the events that end a reply.
	Response struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			Input   int64 `json:"input_tokens"`
			Details struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
	// Code and Message are set on an error event.
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Responses) read(ctx context.Context, r io.Reader, onDelta func(Delta)) (*Response, error) {
	resp := &Response{}
	ended := false
	calls := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() && !ended {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var ev rEvent
		if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if onDelta != nil && ev.Delta != "" {
				onDelta(Delta{Kind: Text, Text: ev.Delta})
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if onDelta != nil && ev.Delta != "" {
				onDelta(Delta{Kind: Reasoning, Text: ev.Delta})
			}
		case "response.output_item.done":
			// An item is whole when it is done. One the reply stopped
			// inside is never done, so a call cut short never appears.
			var item struct {
				Type    string `json:"type"`
				CallID  string `json:"call_id"`
				Name    string `json:"name"`
				Args    string `json:"arguments"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			}
			if json.Unmarshal(ev.Item, &item) != nil {
				continue
			}
			switch item.Type {
			case "message":
				var text strings.Builder
				for _, part := range item.Content {
					if part.Type == "output_text" {
						text.WriteString(part.Text)
					}
				}
				if text.Len() > 0 {
					resp.Parts = append(resp.Parts, Part{Kind: Text, Text: text.String()})
				}
			case "reasoning":
				var text strings.Builder
				for _, part := range item.Summary {
					text.WriteString(part.Text)
				}
				resp.Parts = append(resp.Parts, Part{Kind: Reasoning, Text: text.String(), Meta: append(json.RawMessage(nil), ev.Item...)})
			case "function_call":
				args := item.Args
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				resp.Parts = append(resp.Parts, Part{Kind: Call, CallID: item.CallID, Name: item.Name, Arguments: args})
				calls++
			}
		case "response.completed", "response.incomplete":
			ended = true
			u := ev.Response.Usage
			resp.Usage = Usage{Input: u.Input - u.Details.Cached, CacheRead: u.Details.Cached, Output: u.Output}
			switch {
			case ev.Type == "response.incomplete" && ev.Response.IncompleteDetails.Reason == "max_output_tokens":
				resp.Finish = FinishLength
			case ev.Type == "response.incomplete":
				resp.Finish = FinishOther
			case calls > 0:
				resp.Finish = FinishToolCalls
			default:
				resp.Finish = FinishStop
			}
		case "response.failed":
			e := ev.Response.Error
			return nil, responsesFailure(e.Code, e.Message)
		case "error":
			return nil, responsesFailure(ev.Code, ev.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, transportError(ctx, err)
	}
	if !ended {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Class: Transport, Message: "the reply's stream ended before the reply did"}
	}
	return resp, nil
}

// responsesFailure is a failure the server reported in the stream.
func responsesFailure(code, message string) *Error {
	if message == "" {
		message = "the model's server failed the reply"
		if code != "" {
			message += " (" + code + ")"
		}
	}
	return &Error{Class: chatClass(0, code, message), Message: message}
}
