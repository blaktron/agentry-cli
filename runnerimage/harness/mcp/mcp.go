// Package mcp is the harness's client of its one tool source: an MCP server
// reached over streamable HTTP with a bearer token. It lists the server's
// tools and calls them, and does nothing else the protocol offers.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// protocolVersion is the MCP revision the client asks for. The server may
// answer with another it speaks, which the client then uses.
const protocolVersion = "2025-06-18"

// Client is a connection to one MCP server.
type Client struct {
	// URL is the server's streamable HTTP endpoint.
	URL string
	// Token returns the bearer token. It is called for every request.
	Token func() string
	// HTTP sends the requests.
	HTTP *http.Client
	// Name and Version identify the client to the server.
	Name, Version string

	mu       sync.Mutex
	nextID   int64
	session  string
	protocol string
}

// Tool is a tool the server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Content is one item of a tool's result.
type Content struct {
	// Type is text, image, resource, or a type the client passes over.
	Type string `json:"type"`
	Text string `json:"text"`
	// Data and MimeType hold an image, base64-encoded.
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	// Resource is an embedded resource. Its text is content the server
	// marked as its own to pass on whole.
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
	} `json:"resource"`
}

// Result is a tool call's result.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Error is a failure to reach the server, or the server's refusal.
type Error struct {
	// Status is the HTTP status, zero when there was none. Code is the
	// JSON-RPC error code, zero when the failure was not one.
	Status  int
	Code    int
	Message string
}

func (e *Error) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("tool gateway refused the request (%d): %s", e.Code, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("tool gateway answered HTTP %d: %s", e.Status, e.Message)
	}
	return "tool gateway: " + e.Message
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Initialize opens the session. It must be called once before the others.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": c.Name, "version": c.Version},
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	c.mu.Lock()
	c.protocol = res.ProtocolVersion
	if c.protocol == "" {
		c.protocol = protocolVersion
	}
	c.mu.Unlock()
	return c.notify(ctx, "notifications/initialized")
}

// ListTools returns every tool the server offers, following its pages.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 1000; page++ {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, &Error{Message: "the tool list did not end"}
}

// CallTool runs one tool. arguments is the JSON object of its arguments. A
// tool that reports an error is a Result with IsError set; an *Error means
// the call did not reach the tool or the server refused it.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*Result, error) {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	params := map[string]any{"name": name, "arguments": arguments}
	var res Result
	if err := c.call(ctx, "tools/call", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	res, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := readAnswer(res, id)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if answer.Error != nil {
		return &Error{Code: answer.Error.Code, Message: answer.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer.Result, out); err != nil {
		return &Error{Message: "the answer to " + method + " is not what the protocol describes: " + err.Error()}
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string) error {
	res, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	return res.Body.Close()
}

func (c *Client) post(ctx context.Context, msg rpcRequest) (*http.Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != nil {
		if t := c.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	c.mu.Lock()
	session, protocol := c.session, c.protocol
	c.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Message: err.Error()}
	}
	if s := res.Header.Get("Mcp-Session-Id"); s != "" {
		c.mu.Lock()
		c.session = s
		c.mu.Unlock()
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<10))
		_ = res.Body.Close()
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = res.Status
		}
		return nil, &Error{Status: res.StatusCode, Message: msg}
	}
	return res, nil
}

// readAnswer reads the response to request id from a JSON body, or from an
// event stream in which it is one of the messages.
func readAnswer(res *http.Response, id int64) (*rpcResponse, error) {
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		var answer rpcResponse
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(&answer); err != nil {
			return nil, &Error{Message: "the answer is not JSON: " + err.Error()}
		}
		return &answer, nil
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			// An event's data lines are joined by newlines.
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		case len(line) == 0 && data.Len() > 0:
			var answer rpcResponse
			if json.Unmarshal(data.Bytes(), &answer) == nil && answer.ID != nil && *answer.ID == id {
				return &answer, nil
			}
			data.Reset()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &Error{Message: err.Error()}
	}
	return nil, &Error{Message: "the stream ended with no answer"}
}
