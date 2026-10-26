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

// codexSession is the SessionDriver for an interactive Codex run: a
// JSON-RPC 2.0 client over the app-server's stdio, newline-delimited. The
// protocol is codex's own (v2): initialize → thread/start → one turn/start
// per user turn, with item/* and turn/* notifications in between and a
// turn/completed notification closing each turn. Closing stdin ends the
// process (verified on codex-cli 0.153.4).
//
// The runner is the approval authority here. A thread does not inherit the
// config file's approval policy; it asks the client for every MCP call with
// an mcpServer/elicitation/request, and the driver accepts it for a gateway
// tool and declines anything else. Every other request the server can make
// — shell approvals, file-change approvals, dynamic tool calls, credential
// refreshes — is refused, because nothing in the plan grants what they ask
// for.
type codexSession struct {
	rw io.ReadWriteCloser

	writeMu sync.Mutex // one JSON line at a time

	mu      sync.Mutex // pending + turn state
	nextID  int
	pending map[int]chan rpcMessage
	turn    *codexTurn

	threadID  string
	events    chan Event
	eventGate eventGate // orders sends against close(events)
	readDone  chan struct{}
	closeOnce sync.Once

	// permit says whether an MCP approval for server/tool is granted.
	permit func(server, tool string) bool
	// signIn says the run is paid by a model sign-in (SessionOptions): a
	// 429 is then the plan's limit, said once (planLimited).
	signIn      bool
	planLimited bool
	// resumable keeps the thread so Resume can carry it to a new process,
	// or a persistent agent's next segment can continue it.
	resumable bool
	// continueID is the stored thread Start continues
	// (SessionOptions.ContinueID); continued says it did, and notContinued
	// why not when it could not.
	continueID   string
	continued    bool
	notContinued string
}

// codexTurn accumulates one Prompt's answer.
type codexTurn struct {
	text     string // the last agent message of the turn
	inTok    int64
	outTok   int64
	done     chan codexTurnEnd
	turnID   string
	finished bool
}

type codexTurnEnd struct {
	status string
	err    string
}

// codexElicitationAccept is the answer to an approval elicitation for a
// gateway tool: the MCP elicitation result shape, accepted with no form
// content — the request carries no fields to fill for an approval.
var codexElicitationAccept = map[string]any{"action": "accept", "content": map[string]any{}}

// codexElicitationDecline is the answer for anything else.
var codexElicitationDecline = map[string]any{"action": "decline"}

// Start implements SessionDriver: the handshake and a fresh thread,
// ephemeral unless the conversation may be resumed (SessionOptions.Resumable). The working directory is the process's, so the thread's cwd is
// left to default to it — a plan carries no host path.
func (s *codexSession) Start(ctx context.Context) error {
	if err := s.handshake(ctx); err != nil {
		return err
	}
	if s.continueID != "" {
		// A thread an earlier process stored is continued; one codex cannot
		// load leaves a new thread, and the reason, in its place.
		id, err := s.resumeThread(ctx, s.continueID)
		if err == nil {
			s.threadID, s.continued = id, true
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		s.notContinued = "codex could not resume the stored thread: " + firstLineOf(err.Error())
	}
	// A thread the restart route may resume, or a persistent agent's, is
	// kept in CODEX_HOME, which is then the session store; otherwise nothing
	// of the conversation is persisted.
	id, err := s.startThread(ctx)
	if err != nil {
		return err
	}
	s.threadID = id
	return nil
}

// startThread is thread/start: a new thread, whose id it returns.
func (s *codexSession) startThread(ctx context.Context) (string, error) {
	res, err := s.call(ctx, "thread/start", map[string]any{"ephemeral": !s.resumable})
	if err != nil {
		return "", err
	}
	var ts struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &ts); err != nil || ts.Thread.ID == "" {
		return "", fmt.Errorf("thread/start: no thread id in %s", string(res))
	}
	return ts.Thread.ID, nil
}

// NewConversation implements Renewing: thread/start on the running
// app-server, between turns. The thread it leaves stays in CODEX_HOME.
func (s *codexSession) NewConversation(ctx context.Context) (string, error) {
	if s.threadID == "" {
		return "", errors.New("codex session: not started")
	}
	id, err := s.startThread(ctx)
	if err != nil {
		return "", err
	}
	s.threadID, s.continued, s.notContinued = id, false, ""
	return id, nil
}

// handshake is initialize and initialized. The name is the request
// originator codex sends; OpenAI's plan usage asks that it match the app's
// registered name (agent_name_hint), and it is the same for a key run.
func (s *codexSession) handshake(ctx context.Context) error {
	if _, err := s.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": codexClientName, "title": codexClientName, "version": "runner"},
		"capabilities": map[string]any{},
	}); err != nil {
		return err
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	return nil
}

// Resume implements Resumer: the old process is gone (its stdout ended),
// so its unanswered requests are dropped, and the conversation goes on
// over rw — the handshake again, then thread/resume with the thread's id
// (OpenAI's route for a renewed token: restart app-server, initialize,
// thread/resume). Called between turns only.
func (s *codexSession) Resume(ctx context.Context, rw io.ReadWriteCloser) error {
	if !s.resumable || s.threadID == "" {
		return errors.New("codex session: this conversation cannot be resumed")
	}
	select {
	case <-s.readDone:
	case <-ctx.Done():
		return fmt.Errorf("codex session: the old harness did not end: %w", ctx.Err())
	}
	s.mu.Lock()
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
	s.mu.Unlock()
	s.writeMu.Lock()
	s.rw = rw
	s.writeMu.Unlock()
	s.readDone = make(chan struct{})
	go s.readLoop(rw, s.readDone)
	if err := s.handshake(ctx); err != nil {
		return err
	}
	_, err := s.resumeThread(ctx, s.threadID)
	return err
}

// resumeThread is thread/resume of the thread id, held to answering with
// that thread.
func (s *codexSession) resumeThread(ctx context.Context, id string) (string, error) {
	res, err := s.call(ctx, "thread/resume", map[string]any{"threadId": id})
	if err != nil {
		return "", err
	}
	var tr struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &tr); err != nil || tr.Thread.ID != id {
		return "", fmt.Errorf("thread/resume: resumed %q, not the conversation %q", tr.Thread.ID, id)
	}
	return id, nil
}

// ConversationID implements Continuing: the thread the session is in.
func (s *codexSession) ConversationID() string { return s.threadID }

// Continued implements Continuing.
func (s *codexSession) Continued() (bool, string) { return s.continued, s.notContinued }

// Prompt implements SessionDriver.
func (s *codexSession) Prompt(ctx context.Context, text string) (*Turn, error) {
	if s.threadID == "" {
		return nil, errors.New("codex session: not started")
	}
	t := &codexTurn{done: make(chan codexTurnEnd, 1)}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("codex session: a turn is already in flight")
	}
	s.turn = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()
	res, err := s.call(ctx, "turn/start", map[string]any{
		"threadId": s.threadID,
		"input":    []any{map[string]any{"type": "text", "text": text}},
	})
	if err != nil {
		return nil, err
	}
	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &tr)
	s.mu.Lock()
	t.turnID = tr.Turn.ID
	s.mu.Unlock()
	finish := func(end codexTurnEnd) (*Turn, error) {
		s.mu.Lock()
		out := &Turn{StopReason: end.status, Text: t.text}
		if t.inTok > 0 || t.outTok > 0 {
			out.Usage = &TurnUsage{InputTokens: int(t.inTok), OutputTokens: int(t.outTok), TotalTokens: int(t.inTok + t.outTok)}
		}
		s.mu.Unlock()
		// A turn that did not complete is not an answer, whether or not it
		// said why: a failed turn with no error object would otherwise read
		// as a success (I9).
		if end.status != "completed" {
			why := firstLineOf(end.err)
			if why == "" {
				why = "codex gave no reason"
			}
			return out, fmt.Errorf("codex session: turn %s: %s", end.status, why)
		}
		return out, nil
	}
	select {
	case end := <-t.done:
		return finish(end)
	case <-ctx.Done():
		return nil, fmt.Errorf("turn/start: %w", ctx.Err())
	case <-s.readDone:
		// A harness that finishes the turn and exits at once leaves both
		// the turn's end and the end of the stream ready, and select picks
		// between them at random. The reader hands a turn's end over before
		// it can see the end of the stream, so a turn that ended is already
		// waiting.
		select {
		case end := <-t.done:
			return finish(end)
		default:
			return nil, errors.New("turn/start: harness ended the session before answering")
		}
	}
}

// Events implements SessionDriver.
func (s *codexSession) Events() <-chan Event { return s.events }

// Close implements SessionDriver.
func (s *codexSession) Close() error {
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
		s.eventGate.close(s.events)
	})
	return err
}

func (s *codexSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.rw.Write(append(b, '\n'))
	return err
}

func (s *codexSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan rpcMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	answered := func(m rpcMessage, ok bool) (json.RawMessage, error) {
		if !ok {
			return nil, fmt.Errorf("%s: harness ended the session before answering", method)
		}
		if m.Error != nil {
			return nil, &harnessRPCError{method: method, rpc: m.Error}
		}
		return m.Result, nil
	}
	select {
	case m, ok := <-ch:
		return answered(m, ok)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-s.readDone:
		// A harness that answers and exits at once leaves both the answer
		// and the end of the stream ready, and select picks between them
		// at random. The reader hands an answer over before it can see the
		// end of the stream, so an answer is already waiting if one came.
		select {
		case m, ok := <-ch:
			return answered(m, ok)
		default:
			return nil, fmt.Errorf("%s: harness ended the session before answering", method)
		}
	}
}

func (s *codexSession) readLoop(rw io.Reader, done chan struct{}) {
	defer close(done)
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
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

// codexClientName is the app's name to codex, and to OpenAI through it.
const codexClientName = "Agentry"

// codexError is codex's error object, on an error notification and a
// failed turn (codex-cli 0.162.0, measured): its message and, under
// codexErrorInfo, a variant that may carry the HTTP status the model's
// endpoint answered (responseTooManyFailedAttempts for a 429 it gave up on,
// httpConnectionFailed for a 403).
type codexError struct {
	Message string          `json:"message"`
	Info    json.RawMessage `json:"codexErrorInfo"`
}

// codexUsageLimit is codex's own variant for a usage limit (with a
// limitId, in 0.162.0's binary): the plan's limit, as a 429 is.
const codexUsageLimit = "usageLimitExceeded"

// status is the HTTP status the error carries, 0 when it names none; codex's
// usage-limit variant, as an object or a bare name, counts as a 429.
func (e *codexError) status() int {
	if e == nil || len(e.Info) == 0 {
		return 0
	}
	var name string
	if json.Unmarshal(e.Info, &name) == nil {
		if name == codexUsageLimit {
			return 429
		}
		return 0
	}
	var variants map[string]json.RawMessage
	if json.Unmarshal(e.Info, &variants) != nil {
		return 0
	}
	if _, ok := variants[codexUsageLimit]; ok {
		return 429
	}
	for _, raw := range variants {
		var v struct {
			HTTPStatusCode int `json:"httpStatusCode"`
		}
		if json.Unmarshal(raw, &v) == nil && v.HTTPStatusCode != 0 {
			return v.HTTPStatusCode
		}
	}
	return 0
}

// codexPlanRefusal is what a 403 on a run paid by a ChatGPT plan adds to
// the vendor's words: OpenAI refuses plan usage for an account, workspace
// or region it does not allow, and says not to retry.
const codexPlanRefusal = " (ChatGPT refused plan usage for this account, its workspace or this region; if your plan does not include it in other apps, use an OpenAI API key: agentry setup)"

// signInError is the event for a model error on this session: on a run
// paid by a sign-in, a 429 is the plan's limit (I9), said once, and a 403
// carries the advice; anything else is an error as it stands.
func (s *codexSession) signInError(e *codexError, raw json.RawMessage) (Event, bool) {
	msg := ""
	if e != nil {
		msg = e.Message
	}
	if s.signIn {
		status := e.status()
		// A release whose error object carries no status still says it in
		// its message (0.162.0: "exceeded retry limit, last status: 429 …").
		if status == 0 && strings.Contains(msg, "last status: 429") {
			status = 429
		}
		switch status {
		case 429:
			s.mu.Lock()
			said := s.planLimited
			s.planLimited = true
			s.mu.Unlock()
			if said {
				return Event{}, false
			}
			return Event{Kind: EventPlanLimit, Text: msg, Raw: raw}, true
		case 403:
			return Event{Kind: EventError, Text: msg + codexPlanRefusal, Raw: raw}, true
		}
	}
	return Event{Kind: EventError, Text: msg, Raw: raw}, true
}

// codexNotification is the part of a notification's params this driver
// reads across the methods it recognizes.
type codexNotification struct {
	ThreadID string    `json:"threadId"`
	TurnID   string    `json:"turnId"`
	Item     codexItem `json:"item"`
	Turn     struct {
		ID     string      `json:"id"`
		Status string      `json:"status"`
		Error  *codexError `json:"error"`
	} `json:"turn"`
	TokenUsage struct {
		Last struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
			// TotalTokens is what codex counts as in the context window
			// after this call.
			TotalTokens int64 `json:"totalTokens"`
		} `json:"last"`
		ModelContextWindow int64 `json:"modelContextWindow"`
	} `json:"tokenUsage"`
	Message string      `json:"message"`
	Error   *codexError `json:"error"`
}

func (s *codexSession) handleNotification(m rpcMessage, line []byte) {
	var p codexNotification
	_ = json.Unmarshal(m.Params, &p)
	raw := rawCopy(line)
	switch m.Method {
	case "item/started":
		switch p.Item.Type {
		case "mcpToolCall":
			s.emit(Event{Kind: EventToolCall, Text: p.Item.Tool, Tool: p.Item.Tool, CallID: p.Item.ID, Raw: raw})
			return
		case "contextCompaction":
			// codex is compacting the thread itself: the size known before
			// is no longer true. Before the item completes it recounts the
			// thread and reports the size after as a token usage update.
			s.emit(Event{Kind: EventUsage, ContextCompacted: true, Raw: raw})
			return
		}
	case "item/completed":
		switch p.Item.Type {
		case "mcpToolCall":
			s.emit(codexToolResultEvent(p.Item, raw))
			return
		case "agentMessage":
			s.mu.Lock()
			if s.turn != nil && p.Item.Text != "" {
				s.turn.text = p.Item.Text
			}
			s.mu.Unlock()
			if p.Item.Text != "" {
				s.emit(Event{Kind: EventAssistantText, Text: p.Item.Text, Raw: raw})
				return
			}
		case "error":
			s.emit(Event{Kind: EventError, Text: p.Item.Text, Raw: raw})
			return
		}
	case "thread/tokenUsage/updated":
		// `last` is the most recent model call's usage; the turn's usage
		// is their sum. (`total` is the whole thread's.)
		s.mu.Lock()
		if s.turn != nil {
			s.turn.inTok += p.TokenUsage.Last.InputTokens
			s.turn.outTok += p.TokenUsage.Last.OutputTokens
		}
		s.mu.Unlock()
		s.emit(Event{Kind: EventUsage, InputTokens: p.TokenUsage.Last.InputTokens, OutputTokens: p.TokenUsage.Last.OutputTokens,
			ContextTokens: p.TokenUsage.Last.TotalTokens, ContextLimit: p.TokenUsage.ModelContextWindow, Raw: raw})
		return
	case "turn/completed":
		// A failed turn's error is the plan's limit when it is a 429 on a
		// sign-in, and goes out first; the marker goes out before the turn
		// ends: every event of a turn is queued before Prompt returns
		// (SessionDriver).
		if p.Turn.Status == "failed" && p.Turn.Error != nil && s.signIn && p.Turn.Error.status() == 429 {
			if ev, ok := s.signInError(p.Turn.Error, raw); ok {
				s.emit(ev)
			}
		}
		s.emit(Event{Kind: EventOther, Text: m.Method, Raw: raw})
		s.mu.Lock()
		t := s.turn
		s.mu.Unlock()
		if t != nil && !t.finished {
			t.finished = true
			end := codexTurnEnd{status: p.Turn.Status}
			if p.Turn.Error != nil {
				end.err = p.Turn.Error.Message
			}
			t.done <- end
		}
		return
	case "error":
		// The error object (0.162.0), or the bare message of an older
		// release.
		e := p.Error
		if e == nil {
			e = &codexError{Message: p.Message}
		}
		if ev, ok := s.signInError(e, raw); ok {
			s.emit(ev)
		}
		return
	case "item/agentMessage/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta",
		"item/reasoning/summaryPartAdded", "item/plan/delta", "account/rateLimits/updated":
		// Streaming chunks and rate-limit chatter: the receipts have
		// the whole lines they belong to; the transcript does not need them.
		return
	}
	s.emit(Event{Kind: EventOther, Text: m.Method, Raw: raw})
}

func (s *codexSession) handleRequest(m rpcMessage, line []byte) {
	switch m.Method {
	case "mcpServer/elicitation/request":
		// What the request carries (codex-cli 0.153.4): the server's id,
		// and under _meta the approval kind, the tool's description and
		// its arguments — not the tool's name. The transcript line uses
		// the name when a release adds one, the message otherwise.
		var p struct {
			ServerName string `json:"serverName"`
			Message    string `json:"message"`
			Meta       struct {
				Kind string `json:"codex_approval_kind"`
				Tool string `json:"tool_name"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		tool := p.Meta.Tool
		if tool == "" {
			tool = firstLineOf(p.Message)
		}
		if tool == "" {
			tool = p.ServerName
		}
		if s.permit != nil && s.permit(p.ServerName, p.Meta.Tool) {
			_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": codexElicitationAccept})
			s.emit(Event{Kind: EventPermissionGranted, Text: tool, Raw: rawCopy(line)})
			return
		}
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": codexElicitationDecline})
		s.emit(Event{Kind: EventPermissionDenied, Text: tool, Raw: rawCopy(line)})
	default:
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{
			"code": -32601, "message": "not supported by the agentry runner: " + m.Method}})
		s.emit(Event{Kind: EventOther, Text: "refused " + m.Method, Raw: rawCopy(line)})
	}
}

func (s *codexSession) emit(ev Event) { s.eventGate.send(s.events, ev) }
