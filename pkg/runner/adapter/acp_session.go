package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// acpSession is the docker-agent SessionDriver: a minimal Agent Client
// Protocol client (JSON-RPC 2.0, newline-delimited, over the harness
// container's attached stdio) shaped by the agentry-cli#33 spike. It
// speaks exactly what the runner needs — initialize, session/new,
// session/prompt — and answers every agent→client request itself: a
// permission request is denied (the human channel carries information,
// never approvals; plan §6.10 line 4) and anything else is refused as
// unsupported, so the harness can never reach the host through the
// client side of the protocol.
type acpSession struct {
	rw              io.ReadWriteCloser
	protocolVersion int

	writeMu sync.Mutex // one JSON line at a time

	mu      sync.Mutex // pending + turn state
	nextID  int
	pending map[int]chan rpcMessage
	turn    *turnState

	sessionID string
	events    chan Event
	readDone  chan struct{}
	closeOnce sync.Once
}

// turnState accumulates one Prompt's answer.
type turnState struct {
	text strings.Builder
}

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// newACPSession starts the reader; the protocol begins with Start.
func newACPSession(rw io.ReadWriteCloser, protocolVersion int) *acpSession {
	s := &acpSession{
		rw:              rw,
		protocolVersion: protocolVersion,
		pending:         map[int]chan rpcMessage{},
		events:          make(chan Event, 256),
		readDone:        make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// Session implements Adapter.
func (d *dockerAgent) Session(_ context.Context, rw io.ReadWriteCloser) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New("docker-agent session: nil transport")
	}
	return newACPSession(rw, acpProtocolVersion), nil
}

// Start implements SessionDriver.
func (s *acpSession) Start(ctx context.Context) error {
	res, err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": s.protocolVersion,
		"clientCapabilities": map[string]any{
			// The client offers the harness nothing: no filesystem, no
			// terminal. Tools are the gateway's, declared in the plan.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "agentry", "version": "runner"},
	})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		return fmt.Errorf("initialize: decode: %w", err)
	}
	if init.ProtocolVersion != s.protocolVersion {
		return fmt.Errorf("initialize: harness speaks ACP protocol version %d, runner expects %d", init.ProtocolVersion, s.protocolVersion)
	}
	res, err = s.call(ctx, "session/new", map[string]any{
		// The harness's cwd is meaningless inside the sandbox (no
		// workspace mount, no filesystem tools); the toolset is the plan's.
		"cwd":        "/tmp",
		"mcpServers": []any{},
	})
	if err != nil {
		return err
	}
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &ns); err != nil || ns.SessionID == "" {
		return fmt.Errorf("session/new: no session id in %s", string(res))
	}
	s.sessionID = ns.SessionID
	return nil
}

// Prompt implements SessionDriver.
func (s *acpSession) Prompt(ctx context.Context, text string) (*Turn, error) {
	if s.sessionID == "" {
		return nil, errors.New("session not started")
	}
	s.mu.Lock()
	s.turn = &turnState{}
	s.mu.Unlock()
	res, err := s.call(ctx, "session/prompt", map[string]any{
		"sessionId": s.sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	})
	s.mu.Lock()
	turn := s.turn
	s.turn = nil
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var pr struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
			TotalTokens  int `json:"totalTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res, &pr); err != nil {
		return nil, fmt.Errorf("session/prompt: decode: %w", err)
	}
	out := &Turn{StopReason: pr.StopReason, Text: turn.text.String()}
	if pr.Usage != nil {
		out.Usage = &TurnUsage{InputTokens: pr.Usage.InputTokens, OutputTokens: pr.Usage.OutputTokens, TotalTokens: pr.Usage.TotalTokens}
		s.emit(Event{Kind: EventUsage, InputTokens: int64(pr.Usage.InputTokens), OutputTokens: int64(pr.Usage.OutputTokens), Raw: rawCopy(res)})
	}
	return out, nil
}

// Events implements SessionDriver.
func (s *acpSession) Events() <-chan Event { return s.events }

// Close implements SessionDriver: closing the transport is stdin EOF for
// the harness, which exits; the reader drains its last lines and the
// events channel closes. The engine bounds the wait by removing the
// container, which ends the reader too.
func (s *acpSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.rw.Close()
		<-s.readDone
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
		close(s.events)
	})
	return err
}

func (s *acpSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.rw.Write(append(b, '\n'))
	return err
}

func (s *acpSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan rpcMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: harness ended the session before answering", method)
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: harness error %d: %s", method, m.Error.Code, firstLineOf(m.Error.Message))
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-s.readDone:
		return nil, fmt.Errorf("%s: harness ended the session before answering", method)
	}
}

// readLoop dispatches responses to callers and handles everything the
// harness sends on its own.
func (s *acpSession) readLoop() {
	defer close(s.readDone)
	sc := bufio.NewScanner(s.rw)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			// Not protocol: keep it as an observation, never an error.
			s.emit(Event{Kind: EventOther, Text: string(line), Raw: rawCopy(line)})
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			s.handleRequest(m, line)
		case m.Method != "":
			s.handleNotification(m, line)
		case m.ID != nil:
			var id int
			if err := json.Unmarshal(*m.ID, &id); err != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

func (s *acpSession) handleNotification(m rpcMessage, line []byte) {
	if m.Method != "session/update" {
		s.emit(Event{Kind: EventOther, Text: m.Method, Raw: rawCopy(line)})
		return
	}
	var p struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"update"`
	}
	_ = json.Unmarshal(m.Params, &p)
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		s.mu.Lock()
		if s.turn != nil {
			s.turn.text.WriteString(u.Content.Text)
		}
		s.mu.Unlock()
		s.emit(Event{Kind: EventAssistantText, Text: u.Content.Text, Raw: rawCopy(line)})
	case "tool_call":
		// One tool call per tool_call update; updates of its status are
		// observations, so --max-turns counts calls, not progress.
		s.emit(Event{Kind: EventToolCall, Text: u.Title, Raw: rawCopy(line)})
	default:
		s.emit(Event{Kind: EventOther, Text: u.SessionUpdate, Raw: rawCopy(line)})
	}
}

func (s *acpSession) handleRequest(m rpcMessage, line []byte) {
	switch m.Method {
	case "session/request_permission":
		var p struct {
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(m.Params, &p)
		// Deny: pick a reject option when offered, cancel otherwise.
		reply := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		for _, o := range p.Options {
			if o.Kind == "reject_once" || o.Kind == "reject_always" {
				reply = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
				break
			}
		}
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": reply})
		s.emit(Event{Kind: EventPermissionDenied, Text: p.ToolCall.Title, Raw: rawCopy(line)})
	default:
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{
			"code": -32601, "message": "not supported by the agentry runner: " + m.Method}})
		s.emit(Event{Kind: EventOther, Text: "refused " + m.Method, Raw: rawCopy(line)})
	}
}

func (s *acpSession) emit(ev Event) {
	s.events <- ev
}

func firstLineOf(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i]
	}
	return msg
}
