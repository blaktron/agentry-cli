package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GeminiBaseURL is the Gemini API's own address.
const GeminiBaseURL = "https://generativelanguage.googleapis.com"

// Gemini reaches a model over the Gemini API's generateContent.
//
// A part of the model's reply may carry a thought signature, which the API
// expects back with that part, so each part the model sent is kept as it
// came and sent back unchanged.
type Gemini struct {
	// BaseURL is the server's address with no trailing slash and no
	// version.
	BaseURL string
	// Model is the model's id.
	Model string
	// Key returns the API key.
	Key func() string
	// HTTP sends the requests. It must not set a response timeout.
	HTTP *http.Client
}

type (
	gInline struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	gCall struct {
		ID   string          `json:"id,omitempty"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	gResponse struct {
		ID       string            `json:"id,omitempty"`
		Name     string            `json:"name"`
		Response map[string]string `json:"response"`
	}
	gPart struct {
		Text             string     `json:"text,omitempty"`
		Thought          bool       `json:"thought,omitempty"`
		ThoughtSignature string     `json:"thoughtSignature,omitempty"`
		FunctionCall     *gCall     `json:"functionCall,omitempty"`
		FunctionResponse *gResponse `json:"functionResponse,omitempty"`
		InlineData       *gInline   `json:"inlineData,omitempty"`
	}
	gContent struct {
		Role  string `json:"role,omitempty"`
		Parts []any  `json:"parts"`
	}
	gDecl struct {
		Name                 string          `json:"name"`
		Description          string          `json:"description,omitempty"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
	}
	gTools struct {
		FunctionDeclarations []gDecl `json:"functionDeclarations"`
	}
	gConfig struct {
		MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	}
	gRequest struct {
		SystemInstruction *gContent  `json:"systemInstruction,omitempty"`
		Contents          []gContent `json:"contents"`
		Tools             []gTools   `json:"tools,omitempty"`
		GenerationConfig  *gConfig   `json:"generationConfig,omitempty"`
	}
)

// geminiCallMeta is what a call part keeps for the server: the part as it
// came, and whether the server gave the call an id of its own. A call the
// server gave none is matched to its result by name, and the id the client
// made for it is never sent.
type geminiCallMeta struct {
	Part  json.RawMessage `json:"part"`
	OwnID bool            `json:"ownId"`
}

func (g *Gemini) body(req Request) ([]byte, error) {
	out := gRequest{Contents: []gContent{}}
	if req.System != "" {
		out.SystemInstruction = &gContent{Parts: []any{gPart{Text: req.System}}}
	}
	if req.MaxTokens > 0 {
		out.GenerationConfig = &gConfig{MaxOutputTokens: req.MaxTokens}
	}
	if len(req.Tools) > 0 {
		var decls []gDecl
		for _, t := range req.Tools {
			schema := t.Schema
			if len(bytes.TrimSpace(schema)) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			decls = append(decls, gDecl{Name: t.Name, Description: t.Description, ParametersJSONSchema: schema})
		}
		out.Tools = []gTools{{FunctionDeclarations: decls}}
	}
	// ownID says, for each call the conversation holds, whether its id is
	// the server's, so its result is sent with the id or without.
	ownID := map[string]bool{}
	for _, m := range req.Messages {
		c := gContent{Role: "user"}
		if m.Role == Assistant {
			c.Role = "model"
		}
		for _, p := range m.Parts {
			c.Parts = append(c.Parts, geminiParts(p, ownID)...)
		}
		if len(c.Parts) > 0 {
			out.Contents = append(out.Contents, c)
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

func geminiParts(p Part, ownID map[string]bool) []any {
	switch p.Kind {
	case Text:
		if isObject(p.Meta) {
			return []any{p.Meta}
		}
		if p.Text == "" {
			return nil
		}
		return []any{gPart{Text: p.Text}}
	case Reasoning:
		// A thought goes back only when the server signed it; its
		// summary alone is the server's to drop.
		if isObject(p.Meta) {
			return []any{p.Meta}
		}
		return nil
	case Call:
		var meta geminiCallMeta
		if json.Unmarshal(p.Meta, &meta) == nil && isObject(meta.Part) {
			ownID[p.CallID] = meta.OwnID
			return []any{meta.Part}
		}
		// A call another server made: its id means nothing here.
		args := json.RawMessage(p.Arguments)
		if !isObject(args) {
			args = json.RawMessage(`{}`)
		}
		return []any{gPart{FunctionCall: &gCall{Name: p.Name, Args: args}}}
	case Result:
		r := &gResponse{Name: p.Name, Response: map[string]string{"output": p.Text}}
		if p.Failed {
			text := p.Text
			if text == "" {
				text = "The tool reported an error."
			}
			r.Response = map[string]string{"error": text}
		}
		if ownID[p.CallID] {
			r.ID = p.CallID
		}
		out := []any{gPart{FunctionResponse: r}}
		for _, img := range p.Images {
			out = append(out, gPart{InlineData: &gInline{MimeType: img.MediaType, Data: img.Data}})
		}
		return out
	}
	return nil
}

// Stream sends req and reads the reply's chunks.
func (g *Gemini) Stream(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	body, err := g.body(req)
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = GeminiBaseURL
	}
	u := base + "/v1beta/models/" + url.PathEscape(g.Model) + ":streamGenerateContent?alt=sse"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: BadRequest, Message: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	// The key rides in a header, never in the address, which a proxy logs.
	hreq.Header.Set("x-goog-api-key", g.Key())
	res, err := g.HTTP.Do(hreq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		e := geminiFailure(res.StatusCode, raw)
		if s := res.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 {
				e.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, e
	}
	return g.read(ctx, res.Body, onDelta)
}

// geminiFailure is a failure as the API reports it: an error object, which
// the streaming method may wrap in a list.
func geminiFailure(status int, raw []byte) *Error {
	type wire struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	var one wire
	var many []wire
	if json.Unmarshal(raw, &one) != nil || one.Error.Message == "" {
		if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
			one = many[0]
		}
	}
	msg := one.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if status == 0 {
		status = one.Error.Code
	}
	return &Error{Status: status, Message: msg, Class: geminiClass(status, one.Error.Status, msg)}
}

// geminiClass sorts a failure by its words, then the API's status name,
// then the HTTP status. A conversation that is too long and a key the API
// does not accept both come as a 400 INVALID_ARGUMENT, told apart by words.
func geminiClass(status int, name, message string) Class {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "exceeds the maximum number of tokens") || strings.Contains(lower, "input token count"):
		return Overflow
	case strings.Contains(lower, "api key") || name == "UNAUTHENTICATED" || name == "PERMISSION_DENIED" || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Auth
	case strings.Contains(lower, "billing") || strings.Contains(lower, "prepayment credits"):
		return Credits
	case name == "RESOURCE_EXHAUSTED" || status == http.StatusTooManyRequests:
		return RateLimit
	case name == "UNAVAILABLE" || status == http.StatusServiceUnavailable || strings.Contains(lower, "overloaded"):
		return Capacity
	case status >= 400 && status < 500:
		return BadRequest
	}
	return Server
}

// A chunk of the reply: a whole response object holding the parts that
// arrived since the last one.
type gChunk struct {
	Candidates []struct {
		Content struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		Prompt     int64 `json:"promptTokenCount"`
		Cached     int64 `json:"cachedContentTokenCount"`
		Candidates int64 `json:"candidatesTokenCount"`
		Thoughts   int64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Error json.RawMessage `json:"error"`
}

func (g *Gemini) read(ctx context.Context, r io.Reader, onDelta func(Delta)) (*Response, error) {
	var (
		resp   = &Response{}
		finish string
		calls  int
	)
	// Text and thoughts stream as many parts; each run of one kind becomes
	// one part of the reply. A signed part is kept as it came.
	add := func(kind Kind, text, signature string, raw json.RawMessage) {
		if n := len(resp.Parts); n > 0 && resp.Parts[n-1].Kind == kind && resp.Parts[n-1].Meta == nil && signature == "" {
			resp.Parts[n-1].Text += text
			return
		}
		p := Part{Kind: kind, Text: text}
		if signature != "" {
			p.Meta = append(json.RawMessage(nil), raw...)
		}
		resp.Parts = append(resp.Parts, p)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		var ch gChunk
		if json.Unmarshal(data, &ch) != nil {
			continue
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			return nil, geminiFailure(0, data)
		}
		if u := ch.UsageMetadata; u != nil {
			resp.Usage = Usage{Input: u.Prompt - u.Cached, CacheRead: u.Cached, Output: u.Candidates + u.Thoughts}
		}
		for _, cand := range ch.Candidates {
			for _, raw := range cand.Content.Parts {
				var p gPart
				if json.Unmarshal(raw, &p) != nil {
					continue
				}
				switch {
				case p.FunctionCall != nil:
					id, own := p.FunctionCall.ID, p.FunctionCall.ID != ""
					if !own {
						id = newCallID()
					}
					args := string(p.FunctionCall.Args)
					if strings.TrimSpace(args) == "" || args == "null" {
						args = "{}"
					}
					meta, _ := json.Marshal(geminiCallMeta{Part: raw, OwnID: own})
					resp.Parts = append(resp.Parts, Part{Kind: Call, CallID: id, Name: p.FunctionCall.Name, Arguments: args, Meta: meta})
					calls++
				case p.Thought:
					add(Reasoning, p.Text, p.ThoughtSignature, raw)
					if onDelta != nil && p.Text != "" {
						onDelta(Delta{Kind: Reasoning, Text: p.Text})
					}
				case p.Text != "" || p.ThoughtSignature != "":
					add(Text, p.Text, p.ThoughtSignature, raw)
					if onDelta != nil && p.Text != "" {
						onDelta(Delta{Kind: Text, Text: p.Text})
					}
				}
			}
			if cand.FinishReason != "" {
				finish = cand.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, transportError(ctx, err)
	}
	switch {
	case finish == "":
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Class: Transport, Message: "the reply's stream ended before the reply did"}
	case finish == "MAX_TOKENS":
		resp.Finish = FinishLength
	case finish == "STOP" && calls > 0:
		resp.Finish = FinishToolCalls
	case finish == "STOP":
		resp.Finish = FinishStop
	default:
		resp.Finish = FinishOther
	}
	return resp, nil
}

// newCallID is an id for a call the server gave none. Results are matched
// to calls by id across the whole conversation, so it must not repeat.
func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}
