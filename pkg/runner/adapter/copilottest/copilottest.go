// Package copilottest is a fake Copilot CLI server mode for tests
// (agentry-cli#741): the JSON-RPC 2.0 server of `copilot --headless
// --stdio`, framed by Content-Length, in the shapes Copilot CLI 1.0.95
// answers with (recorded against the real binary with no account) and
// copilot-sdk go/v1.0.19 names. It asks for the session's token at
// session.create, as the real one does, then plays a scripted turn per
// session.send.
package copilottest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Server is one fake Copilot CLI process.
type Server struct {
	// CreateError fails session.create with this message, after the token
	// callback (GitHub refusing the token, an organization's policy).
	CreateError string
	// Turn plays one session.send on c; nil answers "Hello." and goes idle.
	Turn func(c *Conn, prompt string)

	mu sync.Mutex
	// Create is session.create's params, as the driver sent them.
	Create map[string]any
	// Options is session.options.update's params.
	Options map[string]any
	// Tokens are the callback's answers (the token's result object, or
	// the error), in order.
	Tokens []map[string]any
	// Decisions are the permission decisions the driver applied, by
	// request id.
	Decisions map[string]map[string]any
	// Methods are the methods the driver called, in order.
	Methods []string
}

// Conn is the server's side of one session, for a Turn to play on.
type Conn struct {
	s         *Server
	w         io.Writer
	wmu       sync.Mutex
	sessionID string
	regID     string
	nextID    int
	waitMu    sync.Mutex
	waits     map[string]chan map[string]any
}

// Snapshot is what the server saw, copied.
func (s *Server) Snapshot() (create, options map[string]any, tokens []map[string]any, decisions map[string]map[string]any, methods []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]map[string]any{}
	for k, v := range s.Decisions {
		d[k] = v
	}
	return s.Create, s.Options, append([]map[string]any(nil), s.Tokens...), d, append([]string(nil), s.Methods...)
}

// Serve plays the server on r (the driver's writes) and w (what the driver
// reads) until r ends.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	c := &Conn{s: s, w: w, waits: map[string]chan map[string]any{}}
	br := bufio.NewReader(r)
	for {
		frame, err := readFrame(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result map[string]any  `json:"result"`
			Error  map[string]any  `json:"error"`
		}
		if err := json.Unmarshal(frame, &m); err != nil {
			return err
		}
		if m.Method == "" {
			// The answer to one of the server's own requests.
			c.waitMu.Lock()
			ch := c.waits[string(m.ID)]
			delete(c.waits, string(m.ID))
			c.waitMu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- map[string]any{"error": m.Error}
				} else {
					ch <- m.Result
				}
			}
			continue
		}
		s.mu.Lock()
		s.Methods = append(s.Methods, m.Method)
		s.mu.Unlock()
		go c.handle(m.ID, m.Method, m.Params)
	}
}

func (c *Conn) handle(id json.RawMessage, method string, params map[string]any) {
	s := c.s
	switch method {
	case "connect":
		c.reply(id, map[string]any{"ok": true, "protocolVersion": 3, "version": "1.0.95", "taskKinds": []string{"agent", "shell"}})
	case "session.create":
		s.mu.Lock()
		s.Create = params
		s.mu.Unlock()
		c.sessionID, _ = params["sessionId"].(string)
		c.regID, _ = params["gitHubTokenProviderRegistrationId"].(string)
		if c.regID == "" {
			c.fail(id, "Request session.create failed with message: Not authenticated")
			return
		}
		if tok := c.AskToken("initial"); tok["error"] != nil {
			c.fail(id, "Request session.create failed with message: GitHub token provider failed")
			return
		}
		if s.CreateError != "" {
			c.fail(id, "Request session.create failed with message: "+s.CreateError)
			return
		}
		c.reply(id, map[string]any{"sessionId": c.sessionID})
	case "session.options.update":
		s.mu.Lock()
		s.Options = params
		s.mu.Unlock()
		c.reply(id, map[string]any{})
	case "session.send":
		if params["sessionId"] != c.sessionID {
			c.fail(id, "Session not found")
			return
		}
		c.reply(id, map[string]any{"messageId": "m1"})
		prompt, _ := params["prompt"].(string)
		if s.Turn != nil {
			s.Turn(c, prompt)
			return
		}
		c.Event("assistant.message", map[string]any{"messageId": "a1", "content": "Hello."})
		c.Event("session.idle", map[string]any{})
	case "session.permissions.handlePendingPermissionRequest":
		reqID, _ := params["requestId"].(string)
		result, _ := params["result"].(map[string]any)
		s.mu.Lock()
		if s.Decisions == nil {
			s.Decisions = map[string]map[string]any{}
		}
		s.Decisions[reqID] = result
		s.mu.Unlock()
		c.reply(id, map[string]any{"success": true})
		c.waitMu.Lock()
		ch := c.waits["perm:"+reqID]
		delete(c.waits, "perm:"+reqID)
		c.waitMu.Unlock()
		if ch != nil {
			ch <- result
		}
	default:
		c.fail(id, "Unhandled method "+method)
	}
}

// Event sends one session event.
func (c *Conn) Event(typ string, data any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session.event", "params": map[string]any{
		"sessionId": c.sessionID,
		"event":     map[string]any{"id": "e-" + typ, "timestamp": "2026-10-09T21:50:47Z", "parentId": nil, "type": typ, "data": data},
	}})
}

// AskToken asks the driver for a token, as Copilot CLI does (reason
// initial or refresh), and returns its answer: the result, or {"error": …}.
func (c *Conn) AskToken(reason string) map[string]any {
	c.waitMu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan map[string]any, 1)
	c.waits[id] = ch
	c.waitMu.Unlock()
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextIDValue(id), "method": "gitHubToken.getToken", "params": map[string]any{
		"registrationId": c.regID, "host": "https://github.com", "reason": reason, "sessionId": c.sessionID}})
	got := <-ch
	c.s.mu.Lock()
	c.s.Tokens = append(c.s.Tokens, got)
	c.s.mu.Unlock()
	return got
}

// Ask sends a request the driver does not serve, and returns its answer.
func (c *Conn) Ask(method string, params map[string]any) map[string]any {
	c.waitMu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan map[string]any, 1)
	c.waits[id] = ch
	c.waitMu.Unlock()
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextIDValue(id), "method": method, "params": params})
	return <-ch
}

func (c *Conn) nextIDValue(id string) int { n, _ := strconv.Atoi(id); return n }

// AskPermission sends a permission.requested event for request and waits
// for the driver's decision.
func (c *Conn) AskPermission(requestID string, request map[string]any) map[string]any {
	ch := make(chan map[string]any, 1)
	c.waitMu.Lock()
	c.waits["perm:"+requestID] = ch
	c.waitMu.Unlock()
	c.Event("permission.requested", map[string]any{"requestId": requestID, "permissionRequest": request})
	return <-ch
}

// ToolCall plays a gateway tool call: its start, the approval asked, and
// its completion when approved. It returns the decision's kind.
func (c *Conn) ToolCall(callID, tool string) string {
	c.Event("tool.execution_start", map[string]any{"toolCallId": callID, "toolName": "gateway-" + tool,
		"mcpServerName": "gateway", "mcpToolName": tool, "arguments": map[string]any{}})
	d := c.AskPermission("p-"+callID, map[string]any{"kind": "mcp", "serverName": "gateway", "toolName": tool,
		"toolTitle": tool, "readOnly": false, "toolCallId": callID})
	kind, _ := d["kind"].(string)
	c.Event("tool.execution_complete", map[string]any{"toolCallId": callID, "success": kind == "approve-once"})
	return kind
}

func (c *Conn) reply(id json.RawMessage, result any) {
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *Conn) fail(id json.RawMessage, msg string) {
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": msg}})
}

func (c *Conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(b))
	_, _ = c.w.Write(b)
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && n < 0 {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if n < 0 {
				continue
			}
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			if n, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, err
			}
		}
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
