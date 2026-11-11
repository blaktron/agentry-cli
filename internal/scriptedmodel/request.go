package scriptedmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Request is one request the server received.
type Request struct {
	API    API
	Method string
	Path   string
	Header http.Header
	// Body is the request body as sent.
	Body []byte

	Model     string
	Stream    bool
	MaxTokens int
	// IncludeUsage is Chat Completions' stream_options.include_usage.
	IncludeUsage bool
	// Store is the Responses API's store, false when the request set it
	// so; Include its include list.
	Store   *bool
	Include []string
	// System is the system prompt's text: the Anthropic system field, or
	// the text of Chat Completions' system and developer messages.
	System string
	// Messages is the conversation, in order. A Chat Completions system
	// message is in it too.
	Messages []Message
	// Tools is the names of the tools offered, in the order sent.
	Tools []string

	system json.RawMessage
	tools  json.RawMessage
}

// Message is one message of a request's conversation.
type Message struct {
	// Role is system, developer, user, assistant or tool.
	Role string
	// Text is the message's text blocks, joined.
	Text string
	// Reasoning is the reasoning an assistant message carries back.
	Reasoning string
	// Calls is the tool calls an assistant message made.
	Calls []Call
	// Results is the tool results the message carries: any number in an
	// Anthropic user message, one in a Chat Completions tool message.
	Results []Result
	// Raw is the message as sent.
	Raw json.RawMessage
}

// Result is one tool result.
type Result struct {
	CallID  string
	Text    string
	IsError bool
}

func parseRequest(r *http.Request, body []byte) Request {
	req := Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		req.API = ChatCompletions
	case strings.HasSuffix(r.URL.Path, "/messages"):
		req.API = Anthropic
	case strings.HasSuffix(r.URL.Path, "/responses"):
		req.API = Responses
		return parseResponsesRequest(req, body)
	case strings.HasSuffix(r.URL.Path, ":generateContent"), strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
		req.API = Gemini
		return parseGeminiRequest(req, body)
	default:
		return req
	}

	var wire struct {
		Model               string            `json:"model"`
		Stream              bool              `json:"stream"`
		MaxTokens           int               `json:"max_tokens"`
		MaxCompletionTokens int               `json:"max_completion_tokens"`
		System              json.RawMessage   `json:"system"`
		Messages            []json.RawMessage `json:"messages"`
		Tools               json.RawMessage   `json:"tools"`
		StreamOptions       struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return req
	}
	req.Model, req.Stream, req.IncludeUsage = wire.Model, wire.Stream, wire.StreamOptions.IncludeUsage
	req.MaxTokens = wire.MaxTokens
	if req.MaxTokens == 0 {
		req.MaxTokens = wire.MaxCompletionTokens
	}
	req.system, req.tools = wire.System, wire.Tools
	req.Tools = toolNames(wire.Tools)

	var system []string
	if req.API == Anthropic {
		if t := blocksText(wire.System); t != "" {
			system = append(system, t)
		}
	}
	for _, raw := range wire.Messages {
		m := parseMessage(raw)
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, m.Text)
		}
		req.Messages = append(req.Messages, m)
	}
	req.System = strings.Join(system, "\n")
	return req
}

// toolNames reads tool names from either API's tools list: Anthropic names
// a tool at the top of its entry, Chat Completions under "function".
func toolNames(raw json.RawMessage) []string {
	var tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	var names []string
	for _, t := range tools {
		if t.Function.Name != "" {
			names = append(names, t.Function.Name)
		} else if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// blocksText is the text of a content value: a string, or the text blocks of
// a list of blocks, joined.
func blocksText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []wireBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "input_text" || b.Type == "output_text" || b.Type == "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

func parseMessage(raw json.RawMessage) Message {
	var wire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCallID       string          `json:"tool_call_id"`
		ToolCalls        []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	m := Message{Raw: raw}
	if json.Unmarshal(raw, &wire) != nil {
		return m
	}
	m.Role, m.Reasoning = wire.Role, wire.ReasoningContent
	m.Text = blocksText(wire.Content)
	for _, c := range wire.ToolCalls {
		m.Calls = append(m.Calls, Call{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	if wire.Role == "tool" {
		m.Results = append(m.Results, Result{CallID: wire.ToolCallID, Text: m.Text})
		m.Text = ""
		return m
	}

	var blocks []wireBlock
	if json.Unmarshal(wire.Content, &blocks) != nil {
		return m
	}
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			m.Reasoning += b.Thinking
		case "tool_use":
			m.Calls = append(m.Calls, Call{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
		case "tool_result":
			m.Results = append(m.Results, Result{CallID: b.ToolUseID, Text: blocksText(b.Content), IsError: b.IsError})
		}
	}
	return m
}

// PrefixOf reports whether next continues prev: the same system prompt, the
// same tools, and prev's messages unchanged at the start of next's. That is
// what lets a server reuse the prefix it has cached. Cache markers
// (cache_control) are left out of the comparison, because a client moves
// them to the end of each request. A nil error means next continues prev;
// otherwise the error names the first difference.
func PrefixOf(prev, next Request) error {
	if !sameJSON(prev.system, next.system) {
		return fmt.Errorf("the system prompt differs")
	}
	if !sameJSON(prev.tools, next.tools) {
		return fmt.Errorf("the tools differ")
	}
	if len(next.Messages) < len(prev.Messages) {
		return fmt.Errorf("the later request has %d messages, the earlier one %d", len(next.Messages), len(prev.Messages))
	}
	for i, m := range prev.Messages {
		if !sameJSON(m.Raw, next.Messages[i].Raw) {
			return fmt.Errorf("message %d differs:\n  earlier: %s\n  later:   %s", i, canonical(m.Raw), canonical(next.Messages[i].Raw))
		}
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	return bytes.Equal(canonical(a), canonical(b))
}

// canonical is raw with its object keys sorted and every cache_control
// member removed. Raw that is empty or not JSON is returned as it is.
func canonical(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(dropCacheControl(v))
	if err != nil {
		return raw
	}
	return out
}

func dropCacheControl(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, e := range t {
			t[k] = dropCacheControl(e)
		}
	case []any:
		for i, e := range t {
			t[i] = dropCacheControl(e)
		}
	}
	return v
}

// parseResponsesRequest reads a Responses API request. Its input is a list
// of items, not of messages: a message, a reasoning item, a function call
// and a function call's output are each one item, and each becomes one
// Message here so that a test reads them in the order sent.
func parseResponsesRequest(req Request, body []byte) Request {
	var wire struct {
		Model           string            `json:"model"`
		Stream          bool              `json:"stream"`
		MaxOutputTokens int               `json:"max_output_tokens"`
		Instructions    json.RawMessage   `json:"instructions"`
		Input           []json.RawMessage `json:"input"`
		Tools           json.RawMessage   `json:"tools"`
		Store           *bool             `json:"store"`
		Include         []string          `json:"include"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return req
	}
	req.Model, req.Stream, req.MaxTokens = wire.Model, wire.Stream, wire.MaxOutputTokens
	req.Store, req.Include = wire.Store, wire.Include
	req.system, req.tools = wire.Instructions, wire.Tools
	req.Tools = toolNames(wire.Tools)
	_ = json.Unmarshal(wire.Instructions, &req.System)
	for _, raw := range wire.Input {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
			Summary   []struct {
				Text string `json:"text"`
			} `json:"summary"`
		}
		m := Message{Raw: raw}
		if json.Unmarshal(raw, &item) != nil {
			req.Messages = append(req.Messages, m)
			continue
		}
		switch item.Type {
		case "function_call":
			m.Role = "assistant"
			m.Calls = []Call{{ID: item.CallID, Name: item.Name, Arguments: item.Arguments}}
		case "function_call_output":
			m.Role = "tool"
			m.Results = []Result{{CallID: item.CallID, Text: blocksText(item.Output)}}
		case "reasoning":
			m.Role = "assistant"
			for _, part := range item.Summary {
				m.Reasoning += part.Text
			}
		default:
			m.Role, m.Text = item.Role, blocksText(item.Content)
		}
		req.Messages = append(req.Messages, m)
	}
	return req
}

// parseGeminiRequest reads a Gemini generateContent request. The model is
// named in the path (…/models/<model>:streamGenerateContent), and a request
// to the streaming method is a stream.
func parseGeminiRequest(req Request, body []byte) Request {
	if i := strings.LastIndex(req.Path, "/models/"); i >= 0 {
		req.Model, _, _ = strings.Cut(req.Path[i+len("/models/"):], ":")
	}
	req.Stream = strings.HasSuffix(req.Path, ":streamGenerateContent")
	var wire struct {
		SystemInstruction json.RawMessage   `json:"systemInstruction"`
		Contents          []json.RawMessage `json:"contents"`
		Tools             json.RawMessage   `json:"tools"`
		GenerationConfig  struct {
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"generationConfig"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return req
	}
	req.MaxTokens = wire.GenerationConfig.MaxOutputTokens
	req.system, req.tools = wire.SystemInstruction, wire.Tools
	var decls []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	}
	if json.Unmarshal(wire.Tools, &decls) == nil {
		for _, d := range decls {
			for _, f := range d.FunctionDeclarations {
				req.Tools = append(req.Tools, f.Name)
			}
		}
	}
	type part struct {
		Text         string `json:"text"`
		Thought      bool   `json:"thought"`
		FunctionCall *struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCall"`
		FunctionResponse *struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Response struct {
				Output *string `json:"output"`
				Error  *string `json:"error"`
			} `json:"response"`
		} `json:"functionResponse"`
	}
	var sys struct {
		Parts []part `json:"parts"`
	}
	if json.Unmarshal(wire.SystemInstruction, &sys) == nil {
		for _, p := range sys.Parts {
			req.System += p.Text
		}
	}
	for _, raw := range wire.Contents {
		var content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		}
		m := Message{Raw: raw}
		if json.Unmarshal(raw, &content) != nil {
			req.Messages = append(req.Messages, m)
			continue
		}
		m.Role = content.Role
		if m.Role == "model" {
			m.Role = "assistant"
		}
		for _, p := range content.Parts {
			switch {
			case p.FunctionCall != nil:
				m.Calls = append(m.Calls, Call{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args)})
			case p.FunctionResponse != nil:
				r := Result{CallID: p.FunctionResponse.ID}
				if r.CallID == "" {
					r.CallID = p.FunctionResponse.Name
				}
				switch {
				case p.FunctionResponse.Response.Error != nil:
					r.Text, r.IsError = *p.FunctionResponse.Response.Error, true
				case p.FunctionResponse.Response.Output != nil:
					r.Text = *p.FunctionResponse.Response.Output
				}
				m.Results = append(m.Results, r)
			case p.Thought:
				m.Reasoning += p.Text
			default:
				m.Text += p.Text
			}
		}
		req.Messages = append(req.Messages, m)
	}
	return req
}
