// Package acpagent is the agent side of the Agent Client Protocol: it lets an
// editor or an orchestrating agent drive one QuickAgent over a pair of
// pipes, as the runner itself drives a harness (the other direction, in
// pkg/runner/adapter).
//
// It speaks JSON-RPC 2.0 over newline-delimited frames: initialize,
// session/new, session/prompt, session/cancel and session/close, and one
// request of its own, session/request_permission, to ask the person for the
// run's consent. It offers its client nothing it did not have, and takes
// nothing from it that a run would not take from a person at a terminal:
//
//   - One session per connection, and one run behind it. The first prompt
//     starts the run; nothing is verified, installed or started before it, and
//     the run's consent is asked of the client first. A client that cannot
//     show the request, answers it with an error, cancels it, answers with an
//     option the agent did not offer, or does not answer within
//     ConsentTimeout has refused: consent is never assumed.
//   - The client's own tools are not used. The agent never calls fs/* or
//     terminal/*, answers a client's call of any method it does not serve
//     with method-not-found, and refuses a session/new that names MCP
//     servers: a QuickAgent's tools are the ones its release declares.
//   - Prompts are text (a resource link is read as a note of what the client
//     meant). One prompt is in flight at a time.
//   - The connection is bounded: a frame over MaxFrame is refused, and a
//     client that stops reading costs the run nothing (see outbox).
package acpagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the protocol version the agent speaks.
const ProtocolVersion = 1

// ConsentTimeout is how long the agent waits for the client's answer to the
// consent request before it counts as refused. A variable so a test can
// shorten it.
var ConsentTimeout = 5 * time.Minute

// lingerAfterEnd is how long the connection is kept once the session is over
// and its last answer is written, for a client that still sends session/close.
var lingerAfterEnd = 30 * time.Second

// Info names the agent to the client.
type Info struct {
	Name    string
	Version string
}

// Backend runs the QuickAgent behind the connection's one session.
type Backend interface {
	// Run is called once, with the text of the session's first prompt. It
	// prepares the run, asks the person's consent through h, runs, and
	// reports what the client should see through h. It returns when the
	// session is over, after its teardown, and ctx ends when the client
	// cancels the session or goes away.
	Run(ctx context.Context, h *Host, first string) error
}

type state int

const (
	stNew      state = iota // initialized, no session yet
	stIdle                  // session/new answered, no prompt yet
	stStarting              // the first prompt is in flight
	stLive                  // an interactive session between or within turns
	stOver                  // the session has ended
)

type server struct {
	info    Info
	backend Backend
	out     *outbox
	ctx     context.Context
	cancel  context.CancelFunc

	mu          sync.Mutex
	initialized bool
	sid         string
	st          state
	turnID      json.RawMessage // the prompt in flight; nil when none
	closeID     json.RawMessage // a session/close awaiting its answer
	first       string
	prompts     chan string // later prompts of a live session
	closeReq    chan struct{}
	runCancel   context.CancelCauseFunc
	cancelled   bool
	perms       map[int]chan permReply
	nextID      int
	backendDone chan struct{}
	overAt      chan struct{} // closed when the session is over and answered
	overOnce    sync.Once
}

type permReply struct {
	outcome string
	option  string
	err     error
}

// Serve speaks the agent side over in and out until the client closes its
// end, or ends the session and the session's last answer is written. It
// returns after the run's teardown, which a client going away does not skip.
func Serve(ctx context.Context, in io.Reader, out io.Writer, info Info, b Backend) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{
		info: info, backend: b, out: newOutbox(out), ctx: ctx, cancel: cancel,
		prompts: make(chan string, 1), closeReq: make(chan struct{}, 1),
		perms: map[int]chan permReply{}, backendDone: make(chan struct{}), overAt: make(chan struct{}),
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		s.readLoop(in)
	}()
	select {
	case <-readDone:
	case <-ctx.Done(): // the process was told to stop
	case <-s.overAt:
		select {
		case <-readDone:
		case <-time.After(lingerAfterEnd):
		case <-ctx.Done():
		}
	}
	// The client is gone, or the session is over: stop the run if one is
	// still going, and wait for its teardown.
	s.mu.Lock()
	started := s.st >= stStarting
	if s.runCancel != nil {
		s.runCancel(errClientGone)
	}
	s.mu.Unlock()
	if started {
		<-s.backendDone
	}
	s.out.close(flushGrace)
	return nil
}

var errClientGone = errors.New("the client closed the connection")
var errCancelled = errors.New("the client cancelled the session")

func (s *server) readLoop(in io.Reader) {
	fr := newFrameReader(in, MaxFrame)
	for {
		line, err := fr.next()
		if errors.Is(err, errFrameTooLong) {
			s.errorResponse(nil, codeParse, fmt.Sprintf("a frame is at most %d bytes", MaxFrame))
			continue
		}
		if err != nil {
			return
		}
		s.dispatch(line)
	}
}

func (s *server) dispatch(line []byte) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		s.errorResponse(nil, codeParse, "the frame is not JSON")
		return
	}
	if f.JSONRPC != "2.0" {
		s.errorResponse(f.ID, codeInvalidRequest, `the frame is not JSON-RPC 2.0 ("jsonrpc": "2.0")`)
		return
	}
	if f.ID != nil && len(*f.ID) > maxIDLen {
		s.errorResponse(nil, codeInvalidRequest, "the request id is too long")
		return
	}
	switch {
	case f.Method == "" && f.ID != nil:
		s.handleReply(f)
	case f.Method == "":
		s.errorResponse(f.ID, codeInvalidRequest, "the frame names no method")
	case f.ID == nil:
		s.handleNotification(f)
	default:
		s.handleRequest(f)
	}
}

func (s *server) handleNotification(f frame) {
	if f.Method == "session/cancel" {
		s.cancelSession(f.Params)
	}
	// Any other notification, a client's $/ extension included, is ignored: a
	// notification has no answer to refuse with.
}

func (s *server) handleRequest(f frame) {
	id := *f.ID
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if f.Method != "initialize" && !initialized {
		s.errorResponse(&id, codeInvalidRequest, "send initialize first")
		return
	}
	switch f.Method {
	case "initialize":
		s.initialize(id, f.Params)
	case "session/new":
		s.newSession(id, f.Params)
	case "session/prompt":
		s.prompt(id, f.Params)
	case "session/close":
		s.closeSession(id, f.Params)
	default:
		// Everything else a client may ask of an agent — loading or listing
		// sessions, a mode, authentication — and anything it should be asking
		// the other way (fs/*, terminal/*) is not this agent's to serve.
		s.errorResponse(&id, codeMethodNotFound, "this agent does not serve "+safeName(f.Method))
	}
}

func safeName(m string) string {
	if len(m) > 64 {
		m = m[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, m)
}

func (s *server) initialize(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.ProtocolVersion < 1 {
		s.errorResponse(&id, codeInvalidParams, "initialize needs the protocolVersion the client speaks")
		return
	}
	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidRequest, "already initialized")
		return
	}
	s.initialized = true
	s.mu.Unlock()
	// The client's capabilities (a file system, a terminal) are read for
	// nothing: the agent offers its model no tool of the client's.
	s.result(&id, map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession": false,
			"promptCapabilities": map[string]any{
				"image": false, "audio": false, "embeddedContext": false,
			},
			"mcpCapabilities":     map[string]any{"http": false, "sse": false},
			"sessionCapabilities": map[string]any{"close": map[string]any{}},
		},
		"agentInfo":   map[string]any{"name": s.info.Name, "version": s.info.Version},
		"authMethods": []any{},
	})
}

func (s *server) newSession(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		CWD                   string            `json:"cwd"`
		MCPServers            []json.RawMessage `json:"mcpServers"`
		AdditionalDirectories []json.RawMessage `json:"additionalDirectories"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.errorResponse(&id, codeInvalidParams, "session/new takes cwd and mcpServers")
		return
	}
	if len(p.MCPServers) > 0 {
		s.errorResponse(&id, codeInvalidParams, "this agent's tools are the ones its release declares: a client's MCP servers are not taken")
		return
	}
	if len(p.AdditionalDirectories) > 0 {
		s.errorResponse(&id, codeInvalidParams, "this agent works in no directory of the client's")
		return
	}
	s.mu.Lock()
	if s.sid != "" {
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidRequest, "this connection has its session: one agentry acp process serves one run")
		return
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		s.mu.Unlock()
		s.errorResponse(&id, codeInternal, "no session id")
		return
	}
	s.sid = "agentry-" + hex.EncodeToString(b[:])
	s.st = stIdle
	sid := s.sid
	s.mu.Unlock()
	s.result(&id, map[string]any{"sessionId": sid})
}

// promptText reads a prompt's content blocks into one text. A text block is
// the text; a resource link is a note of what the client linked, which the
// agent cannot open; every other block is refused.
func promptText(blocks []json.RawMessage) (string, error) {
	var parts []string
	for _, raw := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URI  string `json:"uri"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", errors.New("a prompt block is not an object")
		}
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			parts = append(parts, fmt.Sprintf("(The client linked %q at %s; this agent cannot open it.)", oneLine(b.Name), oneLine(b.URI)))
		default:
			return "", fmt.Errorf("this agent takes text prompts: a %q block is not accepted", safeName(b.Type))
		}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the prompt is empty: it names the task")
	}
	return text, nil
}

func oneLine(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func (s *server) prompt(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.errorResponse(&id, codeInvalidParams, "session/prompt takes a sessionId and a prompt")
		return
	}
	text, perr := promptText(p.Prompt)
	s.mu.Lock()
	switch {
	case s.sid == "" || p.SessionID != s.sid:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidParams, "no such session")
		return
	case perr != nil:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidParams, perr.Error())
		return
	case s.st == stOver:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidRequest, "the session has ended")
		return
	case s.turnID != nil || s.closeID != nil:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidRequest, "a prompt is in flight: send the next when it is answered")
		return
	}
	s.turnID = id
	switch s.st {
	case stIdle:
		s.st = stStarting
		s.first = text
		runCtx, cancel := context.WithCancelCause(s.ctx)
		s.runCancel = cancel
		s.mu.Unlock()
		go s.runBackend(runCtx, text)
	default: // stLive: Converse is waiting for it
		s.mu.Unlock()
		s.prompts <- text
	}
}

func (s *server) runBackend(ctx context.Context, first string) {
	defer close(s.backendDone)
	err := s.backend.Run(ctx, &Host{s: s}, first)
	s.settle(err)
}

// settle answers whatever the run left unanswered and ends the session: a
// prompt that never got its answer (a refusal, a failure, a cancel) and a
// session/close.
func (s *server) settle(err error) {
	s.mu.Lock()
	turn, closeID, cancelled := s.turnID, s.closeID, s.cancelled
	s.turnID, s.closeID = nil, nil
	s.st = stOver
	s.mu.Unlock()
	if turn != nil {
		switch {
		case cancelled:
			s.result(&turn, map[string]any{"stopReason": "cancelled"})
		case errors.Is(err, ErrConsentRefused) || errors.Is(err, ErrConsentUnavailable):
			s.message(err.Error(), false)
			s.result(&turn, map[string]any{"stopReason": "refusal"})
		case err != nil:
			s.errorResponse(&turn, codeInternal, err.Error())
		default:
			s.result(&turn, map[string]any{"stopReason": "end_turn"})
		}
	}
	if closeID != nil {
		s.result(&closeID, map[string]any{})
	}
	s.over()
}

// over marks the session over and answered, once.
func (s *server) over() { s.overOnce.Do(func() { close(s.overAt) }) }

func (s *server) closeSession(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		s.errorResponse(&id, codeInvalidParams, "session/close takes a sessionId")
		return
	}
	s.mu.Lock()
	switch {
	case s.sid == "" || p.SessionID != s.sid:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidParams, "no such session")
		return
	case s.st == stOver:
		s.mu.Unlock()
		s.result(&id, map[string]any{})
		return
	case s.closeID != nil:
		s.mu.Unlock()
		s.errorResponse(&id, codeInvalidRequest, "the session is already closing")
		return
	case s.st == stIdle:
		// No run was started: nothing to tear down.
		s.st = stOver
		s.mu.Unlock()
		s.result(&id, map[string]any{})
		s.over()
		return
	}
	s.closeID = id
	if s.turnID != nil && s.runCancel != nil {
		// A turn still going is stopped, not waited on: closing a session
		// ends the run, and a run cannot be stopped one turn at a time.
		s.cancelled = true
		s.runCancel(errCancelled)
	}
	s.mu.Unlock()
	select {
	case s.closeReq <- struct{}{}:
	default:
	}
}

func (s *server) cancelSession(raw json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sid == "" || p.SessionID != s.sid || s.st < stStarting || s.st == stOver || s.runCancel == nil {
		return
	}
	// The run cannot be stopped one turn at a time: stopping it is ending it,
	// and a prompt after a cancel finds the session over.
	s.cancelled = true
	s.runCancel(errCancelled)
}

func (s *server) handleReply(f frame) {
	var n int
	if err := json.Unmarshal(*f.ID, &n); err != nil {
		return // not an id the agent sent
	}
	s.mu.Lock()
	ch := s.perms[n]
	delete(s.perms, n)
	s.mu.Unlock()
	if ch == nil {
		return
	}
	if f.Error != nil {
		ch <- permReply{err: fmt.Errorf("the client answered with an error (%d)", f.Error.Code)}
		return
	}
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		ch <- permReply{err: errors.New("the client's answer was not an outcome")}
		return
	}
	ch <- permReply{outcome: res.Outcome.Outcome, option: res.Outcome.OptionID}
}

// --- outbound ---

func (s *server) marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func (s *server) result(id *json.RawMessage, result any) {
	if id == nil {
		return
	}
	if b := s.marshal(map[string]any{"jsonrpc": "2.0", "id": *id, "result": result}); b != nil {
		s.out.send(b, false)
	}
}

func (s *server) errorResponse(id *json.RawMessage, code int, msg string) {
	raw := json.RawMessage("null")
	if id != nil {
		raw = *id
	}
	if b := s.marshal(map[string]any{"jsonrpc": "2.0", "id": raw, "error": map[string]any{"code": code, "message": msg}}); b != nil {
		s.out.send(b, false)
	}
}

func (s *server) sessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

// update sends one session/update. Progress is droppable; what the client
// must see to read the run's answer is not.
func (s *server) update(u map[string]any, droppable bool) {
	if b := s.marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": s.sessionID(), "update": u}}); b != nil {
		s.out.send(b, droppable)
	}
}

func textContent(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

// message sends text as the agent's message.
func (s *server) message(text string, droppable bool) {
	s.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": textContent(text)}, droppable)
}
