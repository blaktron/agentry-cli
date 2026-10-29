package adapter

// The Copilot CLI driver: the runner's own JSON-RPC 2.0 client for Copilot
// CLI's server mode over the agent container's attached stdio, framed as
// LSP is (a Content-Length header, a blank line, the body). The messages
// are copilot-sdk's (MIT, github/copilot-sdk go/v1.0.19, protocol version
// 3: go/client.go, go/types.go, go/rpc/zrpc.go and
// go/rpc/zsession_events.go); the SDK's own client starts the CLI as its
// child, and the runner's container is the engine's to start, so the few
// messages the run needs are spoken here. Measured against Copilot CLI
// 1.0.95 with no account: connect answers protocol 3; session.create calls
// gitHubToken.getToken before it answers, then asks api.github.com for the
// account's Copilot entitlement, and a token GitHub refuses fails
// session.create with "GitHub token validation failed: … 401 Unauthorized".
//
// The order of a session: connect; session.create with the plan's
// session.json, a fresh session id, the token callback's registration, the
// instruction as the whole system message, and the gateway's bearer in its
// MCP server's headers; session.options.update switching off what the SDK's
// empty mode does (custom instructions, plugins, built-in skills). A turn is
// session.send, then session.event notifications until session.idle; a
// session.error ends it as a failure.
//
// What Copilot CLI asks of the driver: gitHubToken.getToken, answered from
// the run's model sign-in (SessionOptions.SignInToken), renewed first when
// it asks for a refresh; permission.requested events, approved once for a
// gateway tool of the run's grant and rejected otherwise. Anything else it
// asks is refused.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// copilotGitHubHost is the host a token is asked for: github.com's, the
// only one a model sign-in is for.
const copilotGitHubHost = "https://github.com"

// copilotSession is the SessionDriver for Copilot CLI.
type copilotSession struct {
	rw   io.ReadWriteCloser
	opts SessionOptions

	writeMu sync.Mutex // one frame at a time

	mu      sync.Mutex // pending + turn state
	nextID  int
	pending map[int]chan rpcMessage
	turn    *copilotTurn

	// sessionID is the session's id, set again by Start when it resumes a
	// stored session or falls back to a new one: read it with id().
	idMu           sync.Mutex
	sessionID      string
	registrationID string
	// continued says Start resumed SessionOptions.ContinueID's session, and
	// notContinued why not when Copilot CLI could not.
	continued    bool
	notContinued string
	permit       func(server, tool string) bool

	events    chan Event
	eventGate eventGate // orders sends against close(events)
	readDone  chan struct{}
	closeOnce sync.Once
	// workers are the goroutines that answer off the read loop (a token,
	// an approval); Close waits for them before it closes events.
	workers sync.WaitGroup
	// planLimited and refused say the plan's limit, or an organization's
	// policy, once; breached is why the session was ended for a tool the
	// run does not grant (breach), "" until then.
	planLimited, refused bool
	breached             string
	// ctx is the session's own, for the requests the driver makes while
	// reading (an approval's answer, a token renewal); cancelled by Close.
	ctx    context.Context
	cancel context.CancelFunc
}

// copilotTurn accumulates one Prompt's answer.
type copilotTurn struct {
	text     string
	inTok    int64
	outTok   int64
	done     chan copilotTurnEnd
	finished bool
}

type copilotTurnEnd struct {
	err string
}

func newCopilotSession(rw io.ReadWriteCloser, opts SessionOptions) (*copilotSession, error) {
	if opts.HarnessFile == nil {
		return nil, errors.New("copilot session: the run gave the driver no plan files")
	}
	return newCopilotDriver(rw, opts)
}

// newCopilotDriver is the driver on rw, reading: a run's session
// (newCopilotSession) or a model list (ListCopilotModels).
func newCopilotDriver(rw io.ReadWriteCloser, opts SessionOptions) (*copilotSession, error) {
	if rw == nil {
		return nil, errors.New("copilot session: nil transport")
	}
	allowed := make(map[string]bool, len(opts.GatewayTools))
	for _, t := range opts.GatewayTools {
		allowed[t] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &copilotSession{
		rw:             rw,
		opts:           opts,
		pending:        map[int]chan rpcMessage{},
		events:         make(chan Event, 256),
		readDone:       make(chan struct{}),
		sessionID:      uuid.NewString(),
		registrationID: uuid.NewString(),
		ctx:            ctx,
		cancel:         cancel,
		permit: func(server, tool string) bool {
			// A tool of the gateway the run's grant holds; the gateway serves
			// no other, and no other server is in the session.
			return server == copilotGatewayServer && allowed[tool]
		},
	}
	go s.readLoop()
	return s, nil
}

// Start implements SessionDriver: connect, then the session.
func (s *copilotSession) Start(ctx context.Context) error {
	if err := s.connect(ctx); err != nil {
		return err
	}
	if err := s.open(ctx); err != nil {
		return err
	}
	if _, err := s.call(ctx, "session.options.update", map[string]any{
		"sessionId": s.id(), "skipCustomInstructions": true, "customAgentsLocalOnly": true,
		"coauthorEnabled": false, "manageScheduleEnabled": false,
		"installedPlugins": []any{}, "includedBuiltinSkills": []any{},
	}); err != nil {
		return err
	}
	return nil
}

// open is the session: a stored one resumed with the session's
// configuration as it is now, so a changed instruction or gateway bearer
// reaches it as it would a new one; else, or when Copilot CLI cannot resume
// it, a new one.
func (s *copilotSession) open(ctx context.Context) error {
	if id := s.opts.ContinueID; id != "" {
		s.setID(id)
		params, err := s.createParams()
		if err != nil {
			return err
		}
		_, err = s.call(ctx, "session.resume", params)
		if err == nil {
			s.continued = true
			return nil
		}
		if ctx.Err() != nil || policyWords(err.Error()) {
			return s.startError(err)
		}
		s.notContinued = "Copilot CLI could not resume the stored session: " + firstLineOf(err.Error())
		s.setID(uuid.NewString())
	}
	params, err := s.createParams()
	if err != nil {
		return err
	}
	if _, err := s.call(ctx, "session.create", params); err != nil {
		return s.startError(err)
	}
	return nil
}

// ConversationID implements Continuing: the session's id, which Copilot CLI
// keeps under COPILOT_HOME.
func (s *copilotSession) ConversationID() string { return s.id() }

// id is the session's id as it stands.
func (s *copilotSession) id() string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.sessionID
}

// setID changes the session's id, while Start opens it.
func (s *copilotSession) setID(id string) {
	s.idMu.Lock()
	s.sessionID = id
	s.idMu.Unlock()
}

// Continued implements Continuing.
func (s *copilotSession) Continued() (bool, string) { return s.continued, s.notContinued }

// connect is the handshake, held to the protocol this driver speaks.
func (s *copilotSession) connect(ctx context.Context) error {
	res, err := s.call(ctx, "connect", map[string]any{
		"clientInfo": map[string]any{"editorName": agentryClientName, "editorVersion": "runner"},
	})
	if err != nil {
		return err
	}
	var cr struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Version         string `json:"version"`
	}
	if err := json.Unmarshal(res, &cr); err != nil || cr.ProtocolVersion != copilotProtocolVersion {
		return fmt.Errorf("copilot session: Copilot CLI %s speaks protocol %d, and this runner speaks %d", cr.Version, cr.ProtocolVersion, copilotProtocolVersion)
	}
	return nil
}

// createParams is session.create's: the plan's session.json as it is, with
// the session's ids, the instruction as the whole system message, and the
// gateway's bearer added. The bearer travels the session alone.
func (s *copilotSession) createParams() (map[string]any, error) {
	raw, err := s.opts.HarnessFile(copilotSessionFile)
	if err != nil {
		return nil, fmt.Errorf("copilot session: the plan's %s: %w", copilotSessionFile, err)
	}
	instruction, err := s.opts.HarnessFile(copilotInstructionFile)
	if err != nil {
		return nil, fmt.Errorf("copilot session: the plan's %s: %w", copilotInstructionFile, err)
	}
	var cfg copilotSessionConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("copilot session: the plan's %s: %w", copilotSessionFile, err)
	}
	gw, ok := cfg.MCPServers[copilotGatewayServer]
	if !ok || len(cfg.MCPServers) != 1 || gw.URL != spec.GatewayURL() {
		return nil, fmt.Errorf("copilot session: the plan's %s must name the run's gateway, %s, as its one MCP server", copilotSessionFile, spec.GatewayURL())
	}
	// What keeps the session to the gateway's tools and keeps its telemetry
	// off is held here as well as in the plan, so a plan file can only add
	// to it, never take it away (I1).
	cfg.AvailableTools = []string{copilotAvailableTools}
	cfg.RequestPermission, cfg.EnableSessionTelemetry = true, false
	cfg.SkipCustomInstructions, cfg.EnableSkills, cfg.ToolSearch.Enabled, cfg.Memory.Enabled = true, false, false, false
	if s.opts.GatewayToken != "" {
		gw.Headers = map[string]string{"Authorization": "Bearer " + s.opts.GatewayToken}
	}
	cfg.MCPServers = map[string]copilotMCPServer{copilotGatewayServer: gw}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var params map[string]any
	if err := json.Unmarshal(b, &params); err != nil {
		return nil, err
	}
	params["sessionId"] = s.id()
	params["systemMessage"] = map[string]any{"mode": "replace", "content": string(instruction)}
	if s.opts.SignInToken != nil {
		params["gitHubTokenProviderRegistrationId"] = s.registrationID
	}
	return params, nil
}

// copilotPolicyRefusal are the words Copilot CLI and GitHub use when an
// organization's or an enterprise's policy turns the CLI off for the
// account ("Access denied by policy settings", GitHub's Copilot CLI
// troubleshooting page) or bars the account (copilot_internal/userblocked).
var copilotPolicyRefusal = []string{"denied by policy", "policy settings", "userblocked", "user is blocked", "disabled by your organization", "disabled by your enterprise"}

// startError is session.create's failure as the run reports it: an
// organization's policy that refused the account is that outcome, an
// *OrgPolicyRefusal the engine ends the run with; GitHub refusing the token
// says to sign in again.
func (s *copilotSession) startError(err error) error {
	msg := err.Error()
	low := strings.ToLower(msg)
	if policyWords(msg) {
		return &OrgPolicyRefusal{Policy: receipt.OrgPolicyClient, Text: firstLineOf(msg)}
	}
	if strings.Contains(low, "401") || strings.Contains(low, "bad credentials") {
		return fmt.Errorf("%w (GitHub refused the sign-in's token: sign in again with agentry model-signins add copilot)", err)
	}
	return err
}

// refuse says an organization's policy refused the run, once.
func (s *copilotSession) refuse(ev Event) {
	s.mu.Lock()
	said := s.refused
	s.refused = true
	s.mu.Unlock()
	if !said {
		s.emit(ev)
	}
}

// Prompt implements SessionDriver: one session.send, answered when the
// session goes idle.
func (s *copilotSession) Prompt(ctx context.Context, text string) (*Turn, error) {
	t := &copilotTurn{done: make(chan copilotTurnEnd, 1)}
	s.mu.Lock()
	if s.breached != "" {
		why := s.breached
		s.mu.Unlock()
		return nil, errors.New("copilot session: " + why)
	}
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("copilot session: a turn is already in flight")
	}
	s.turn = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()
	if _, err := s.call(ctx, "session.send", map[string]any{"sessionId": s.id(), "prompt": text}); err != nil {
		return nil, err
	}
	select {
	case end := <-t.done:
		s.mu.Lock()
		out := &Turn{StopReason: "end_turn", Text: t.text}
		if t.inTok > 0 || t.outTok > 0 {
			out.Usage = &TurnUsage{InputTokens: int(t.inTok), OutputTokens: int(t.outTok), TotalTokens: int(t.inTok + t.outTok)}
		}
		s.mu.Unlock()
		// A turn that ended on an error is not an answer, whatever came
		// before it (I9).
		if end.err != "" {
			out.StopReason = "error"
			return out, fmt.Errorf("copilot session: %s", end.err)
		}
		return out, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("session.send: %w", ctx.Err())
	case <-s.readDone:
		return nil, errors.New("session.send: harness ended the session before answering")
	}
}

// CompactConversation implements Compacting: session.history.compact, the
// SDK's compaction of the session's history in place, between turns. It
// answers when the compaction is done, and says whether it succeeded;
// session.compaction_complete carries the size after to the events.
func (s *copilotSession) CompactConversation(ctx context.Context) error {
	if s.id() == "" {
		return errors.New("copilot session: not started")
	}
	res, err := s.call(ctx, "session.history.compact", map[string]any{"sessionId": s.id(), "trigger": "manual"})
	if err != nil {
		return err
	}
	var r struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(res, &r); err != nil || !r.Success {
		return errors.New("copilot session: Copilot CLI did not complete the compaction")
	}
	return nil
}

// Interrupt implements Interrupter: session.abort, after which the session
// goes idle and the turn ends with what it had said.
func (s *copilotSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	inTurn := s.turn != nil
	s.mu.Unlock()
	if !inTurn || s.id() == "" {
		return nil
	}
	_, err := s.call(ctx, "session.abort", map[string]any{"sessionId": s.id()})
	return err
}

// Events implements SessionDriver.
func (s *copilotSession) Events() <-chan Event { return s.events }

// Close implements SessionDriver: closing stdin ends Copilot CLI.
func (s *copilotSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		err = s.rw.Close()
		<-s.readDone
		s.workers.Wait()
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

// send writes one frame.
func (s *copilotSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := fmt.Fprintf(s.rw, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = s.rw.Write(b)
	return err
}

func (s *copilotSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
			return nil, &harnessRPCError{method: method, rpc: m.Error}
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-s.readDone:
		return nil, fmt.Errorf("%s: harness ended the session before answering", method)
	}
}

// maxFrame bounds one frame from the harness.
const maxFrame = 16 << 20

// readFrame reads one Content-Length frame; io.EOF at a clean end.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	first := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && first && line == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		first = false
		line = strings.TrimSpace(line)
		if line == "" {
			if length < 0 {
				// A blank line before any header: skip it.
				first = true
				continue
			}
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("not a frame header: %q", firstLineOf(line))
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n <= 0 || n > maxFrame {
				return nil, fmt.Errorf("a frame of %q bytes", value)
			}
			length = n
		}
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *copilotSession) readLoop() {
	defer close(s.readDone)
	r := bufio.NewReaderSize(s.rw, 64<<10)
	for {
		frame, err := readFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				s.emit(Event{Kind: EventOther, Text: "copilot session: " + err.Error()})
			}
			return
		}
		var m rpcMessage
		if err := json.Unmarshal(frame, &m); err != nil {
			s.emit(Event{Kind: EventOther, Text: string(frame), Raw: rawCopy(frame)})
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			s.handleRequest(m, frame)
		case m.Method != "":
			s.handleNotification(m, frame)
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

// handleRequest answers what Copilot CLI asks of the driver.
func (s *copilotSession) handleRequest(m rpcMessage, frame []byte) {
	switch m.Method {
	case "gitHubToken.getToken":
		// Answered off the read loop: a renewal asks the vendor.
		s.workers.Go(func() { s.answerToken(m, frame) })
	default:
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{
			"code": -32601, "message": "not supported by the agentry runner: " + m.Method}})
		s.emit(Event{Kind: EventOther, Text: "refused " + m.Method, Raw: rawCopy(frame)})
	}
}

// answerToken hands Copilot CLI the run's token for github.com, renewed
// first when it asks for a refresh. The answer goes to the harness alone:
// the request is kept in the receipts, the token never.
func (s *copilotSession) answerToken(m rpcMessage, frame []byte) {
	var p struct {
		Host           string `json:"host"`
		Reason         string `json:"reason"`
		RegistrationID string `json:"registrationId"`
	}
	_ = json.Unmarshal(m.Params, &p)
	fail := func(why string) {
		_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": why}})
		s.emit(Event{Kind: EventError, Text: "copilot asked for a token and was refused: " + why, Raw: rawCopy(frame)})
	}
	switch {
	case s.opts.SignInToken == nil:
		fail("this run hands the harness no sign-in")
		return
	case p.RegistrationID != s.registrationID:
		fail("not this session's token callback")
		return
	case strings.TrimRight(p.Host, "/") != copilotGitHubHost:
		fail("a model sign-in is for github.com, and the token was asked for " + p.Host)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	t, err := s.opts.SignInToken(ctx, p.Reason == "refresh")
	if err != nil {
		fail(err.Error())
		return
	}
	life := int64(math.Floor(time.Until(t.ExpiresAt).Seconds()))
	if life <= 0 {
		fail("the sign-in's token has expired")
		return
	}
	_ = s.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
		"kind": "token", "accessToken": t.AccessToken, "expiresIn": life, "tokenType": "bearer"}})
	s.emit(Event{Kind: EventOther, Text: "gitHubToken.getToken (" + p.Reason + ")", Raw: rawCopy(frame)})
}

// copilotEvent is the part of a session event this driver reads (copilot-sdk
// go/rpc/zsession_events.go).
type copilotEvent struct {
	Type string `json:"type"`
	Data struct {
		// assistant.message
		Content string `json:"content"`
		// assistant.usage
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
		// session.usage_info: the conversation's size and the model's
		// window. session.compaction_complete: the window it compacted
		// for, too.
		CurrentTokens int64 `json:"currentTokens"`
		TokenLimit    int64 `json:"tokenLimit"`
		// session.compaction_complete: the conversation's size after,
		// read when Success says it compacted.
		PostCompactionTokens int64 `json:"postCompactionTokens"`
		// tool.execution_start
		ToolName      string `json:"toolName"`
		MCPServerName string `json:"mcpServerName"`
		MCPToolName   string `json:"mcpToolName"`
		// tool.execution_start and tool.execution_complete
		ToolCallID string `json:"toolCallId"`
		// tool.execution_complete and session.compaction_complete: Success
		// says whether it worked.
		// tool.execution_complete: Result's content is what the tool
		// returned, Error's message why it failed. Both are read only on
		// that event, so they stay raw here: another event's field of the
		// same name must not fail the whole event's decoding. Success is
		// absent from an event that does not say.
		Success *bool           `json:"success"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
		// session.error
		ErrorType  string `json:"errorType"`
		ErrorCode  string `json:"errorCode"`
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
		// permission.requested
		RequestID         string `json:"requestId"`
		ResolvedByHook    bool   `json:"resolvedByHook"`
		PermissionRequest struct {
			Kind                    string `json:"kind"`
			ServerName              string `json:"serverName"`
			ToolName                string `json:"toolName"`
			ManagedApprovalRequired bool   `json:"managedApprovalRequired"`
		} `json:"permissionRequest"`
	} `json:"data"`
}

func (s *copilotSession) handleNotification(m rpcMessage, frame []byte) {
	raw := rawCopy(frame)
	if m.Method != "session.event" {
		s.emit(Event{Kind: EventOther, Text: m.Method, Raw: raw})
		return
	}
	var n struct {
		SessionID string       `json:"sessionId"`
		Event     copilotEvent `json:"event"`
	}
	if err := json.Unmarshal(m.Params, &n); err != nil || n.SessionID != s.id() {
		s.emit(Event{Kind: EventOther, Text: m.Method, Raw: raw})
		return
	}
	ev := n.Event
	switch ev.Type {
	case "tool.execution_start":
		name := ev.Data.MCPToolName
		if name == "" {
			name = ev.Data.ToolName
		}
		s.emit(Event{Kind: EventToolCall, Text: name, Tool: name, CallID: ev.Data.ToolCallID, Raw: raw})
		if ev.Data.MCPServerName != copilotGatewayServer {
			// A tool of Copilot CLI's own, or of another server, started:
			// the allowlist that should have unloaded it did not. The run
			// is ended rather than trusted further (I1).
			s.breach("Copilot CLI started a tool that is not the gateway's (" + ev.Data.ToolName + "), which the run's allowlist should have unloaded; the session is ended")
		}
	case "tool.execution_complete":
		s.emit(copilotToolResult(ev, raw))
	case "assistant.message":
		if ev.Data.Content == "" {
			s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
			return
		}
		s.mu.Lock()
		if s.turn != nil {
			s.turn.text = ev.Data.Content
		}
		s.mu.Unlock()
		s.emit(Event{Kind: EventAssistantText, Text: ev.Data.Content, Raw: raw})
	case "assistant.usage":
		s.mu.Lock()
		if s.turn != nil {
			s.turn.inTok += ev.Data.InputTokens
			s.turn.outTok += ev.Data.OutputTokens
		}
		s.mu.Unlock()
		s.emit(Event{Kind: EventUsage, InputTokens: ev.Data.InputTokens, OutputTokens: ev.Data.OutputTokens, Raw: raw})
	case "session.usage_info":
		if ev.Data.CurrentTokens <= 0 {
			s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
			return
		}
		s.emit(Event{Kind: EventUsage, ContextTokens: ev.Data.CurrentTokens, ContextLimit: ev.Data.TokenLimit, Raw: raw})
	case "session.compaction_complete":
		// Copilot CLI compacted the conversation itself, and may send no
		// usage_info after it before the turn ends: its size after is the
		// conversation's size now. A failed compaction changed nothing.
		if ev.Data.Success == nil || !*ev.Data.Success {
			s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
			return
		}
		s.emit(Event{Kind: EventUsage, ContextTokens: ev.Data.PostCompactionTokens, ContextLimit: ev.Data.TokenLimit, ContextCompacted: true, Raw: raw})
	case "permission.requested":
		s.decide(ev, raw)
	case "session.error":
		s.sessionError(ev, raw)
	case "session.idle":
		s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
		s.endTurn("")
	case "assistant.message_delta", "assistant.reasoning_delta", "assistant.streaming_delta":
		// Streaming chunks: the receipts have the whole messages.
	default:
		s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
	}
}

// copilotToolResult maps a tool.execution_complete: the call it answers,
// whether it failed (it said so, or it carried an error), and the result's
// content or, failing that, the error's message. The event does not repeat
// the tool's name; its call's event carries it.
func copilotToolResult(ev copilotEvent, raw json.RawMessage) Event {
	var res struct {
		Content string `json:"content"`
	}
	var cerr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(ev.Data.Result, &res)
	_ = json.Unmarshal(ev.Data.Error, &cerr)
	text := res.Content
	if text == "" {
		text = cerr.Message
	}
	failed := (ev.Data.Success != nil && !*ev.Data.Success) || cerr.Message != ""
	return Event{Kind: EventToolResult, CallID: ev.Data.ToolCallID, Failed: failed, Text: text, Raw: raw}
}

// decide answers a permission request: approved once for a gateway tool of
// the run's grant, rejected for anything else. A request that an
// organization's managed settings say a person must approve is rejected,
// and ends the run with that policy named: the run's driver approves its
// tools itself, which that policy forbids.
func (s *copilotSession) decide(ev copilotEvent, raw json.RawMessage) {
	d := ev.Data
	if d.ResolvedByHook || d.RequestID == "" {
		s.emit(Event{Kind: EventOther, Text: ev.Type, Raw: raw})
		return
	}
	pr := d.PermissionRequest
	tool := pr.ToolName
	if pr.ServerName == copilotGatewayServer {
		// Copilot CLI names an MCP tool with its server's prefix
		// ("gateway-fs_read"); the grant holds the gateway's own names.
		tool = strings.TrimPrefix(tool, copilotGatewayServer+"-")
	}
	if tool == "" {
		tool = pr.Kind
	}
	result := map[string]any{"kind": "reject"}
	granted := false
	switch {
	case pr.Kind == "mcp" && pr.ManagedApprovalRequired:
		s.refuse(Event{Kind: EventOrgPolicy, Policy: receipt.OrgPolicyManagedApproval,
			Text: "the organization's managed settings require a person to approve " + tool, Raw: raw})
	case pr.Kind == "mcp" && s.permit(pr.ServerName, tool):
		result, granted = map[string]any{"kind": "approve-once"}, true
	}
	s.workers.Go(func() {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		_, _ = s.call(ctx, "session.permissions.handlePendingPermissionRequest", map[string]any{
			"sessionId": s.id(), "requestId": d.RequestID, "result": result})
	})
	if granted {
		s.emit(Event{Kind: EventPermissionGranted, Text: tool, Raw: raw})
	} else {
		s.emit(Event{Kind: EventPermissionDenied, Text: tool, Raw: raw})
	}
}

// sessionError is a session.error: on a run paid by a sign-in, a quota or a
// rate limit is the plan's limit (I9), said once, and an authorization
// refused by policy the organization's outcome; anything else an error.
// Each ends the turn as a failure.
func (s *copilotSession) sessionError(ev copilotEvent, raw json.RawMessage) {
	d := ev.Data
	msg := d.Message
	if d.ErrorCode != "" {
		msg += " (" + d.ErrorType + ": " + d.ErrorCode + ")"
	}
	switch {
	case s.opts.ModelSignIn && (d.ErrorType == "quota" || d.ErrorType == "rate_limit"):
		s.mu.Lock()
		said := s.planLimited
		s.planLimited = true
		s.mu.Unlock()
		if !said {
			s.emit(Event{Kind: EventPlanLimit, Text: msg, Raw: raw})
		}
	case d.ErrorType == "authorization" && policyWords(d.Message):
		s.refuse(Event{Kind: EventOrgPolicy, Policy: receipt.OrgPolicyClient, Text: msg, Raw: raw})
	default:
		s.emit(Event{Kind: EventError, Text: msg, Raw: raw})
	}
	if msg == "" {
		msg = "copilot reported an error of type " + d.ErrorType
	}
	s.endTurn(msg)
}

func policyWords(msg string) bool {
	low := strings.ToLower(msg)
	for _, w := range copilotPolicyRefusal {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// breach ends the session for a tool Copilot CLI started outside the
// gateway: the turn in flight fails, the session is aborted, and no turn
// is taken after it.
func (s *copilotSession) breach(why string) {
	s.mu.Lock()
	first := s.breached == ""
	if first {
		s.breached = why
	}
	s.mu.Unlock()
	if !first {
		return
	}
	s.emit(Event{Kind: EventError, Text: why})
	// Sent before the turn ends, and not waited on: its answer, which no
	// call is pending for, is dropped.
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	_ = s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session.abort", "params": map[string]any{"sessionId": s.id()}})
	s.endTurn(why)
}

// endTurn ends the turn in flight, once: why is "" for an answer.
func (s *copilotSession) endTurn(why string) {
	s.mu.Lock()
	t := s.turn
	if t == nil || t.finished {
		s.mu.Unlock()
		return
	}
	t.finished = true
	s.mu.Unlock()
	t.done <- copilotTurnEnd{err: why}
}

func (s *copilotSession) emit(ev Event) { s.eventGate.send(s.events, ev) }
