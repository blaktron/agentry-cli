package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// ErrConsentRefused is what Consent returns when the person declined the run.
var ErrConsentRefused = errors.New("the run was not consented to: the client declined it")

// ErrConsentUnavailable is what Consent returns when the client did not
// answer the request with a choice it was offered: it cannot show a consent
// request, or it did not answer. Nothing is run on it.
var ErrConsentUnavailable = errors.New("the run was not consented to: the client did not answer the consent request")

// Consent is the request the client shows the person before the run starts.
type Consent struct {
	// Title is one line: what is asked ("Run acme/weekly?").
	Title string
	// Text is what the person is asked to accept: the release, its
	// capabilities and where the run's output goes.
	Text string
}

// Host is what a Backend reports through.
type Host struct {
	s *server

	mu          sync.Mutex
	interactive bool
	fwd         *forwarder
	calls       map[string]string // harness call id -> ACP tool call id
	nextCall    int
}

// The two consent options. Only "allow" consents.
const (
	optionAllow  = "allow"
	optionReject = "reject"
)

// Consent asks the client to show c and returns nil only if the person
// chose to run. Everything else — a refusal, a dismissed dialog, a client
// that answers with an error or with an option it was not offered, no answer
// in ConsentTimeout, a cancel — is an error, and the run does not start.
func (h *Host) Consent(ctx context.Context, c Consent) error {
	s := h.s
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan permReply, 1)
	s.perms[id] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.perms, id)
		s.mu.Unlock()
	}
	req := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "session/request_permission",
		"params": map[string]any{
			"sessionId": s.sessionID(),
			"toolCall": map[string]any{
				"toolCallId": "consent",
				"title":      cleanLine(c.Title),
				"kind":       "execute",
				"status":     "pending",
				"content":    []any{map[string]any{"type": "content", "content": textContent(c.Text)}},
			},
			"options": []any{
				map[string]any{"optionId": optionAllow, "name": "Run it", "kind": "allow_once"},
				map[string]any{"optionId": optionReject, "name": "Do not run it", "kind": "reject_once"},
			},
		},
	}
	b := s.marshal(req)
	if b == nil {
		forget()
		return ErrConsentUnavailable
	}
	s.out.send(b, false)
	timer := time.NewTimer(ConsentTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		switch {
		case r.err != nil:
			return fmt.Errorf("%w (%v)", ErrConsentUnavailable, r.err)
		case r.outcome == "selected" && r.option == optionAllow:
			return nil
		case r.outcome == "selected" && r.option == optionReject, r.outcome == "cancelled":
			return ErrConsentRefused
		default:
			return fmt.Errorf("%w (an answer the request did not offer)", ErrConsentUnavailable)
		}
	case <-timer.C:
		forget()
		return fmt.Errorf("%w (no answer in %s)", ErrConsentUnavailable, ConsentTimeout)
	case <-ctx.Done():
		forget()
		return context.Cause(ctx)
	}
}

// Say tells the person something as the agent's message.
func (h *Host) Say(text string) { h.s.notice(text) }

// Event reports one live event of an autonomous run: a tool call and its
// result, as the client's tool-call progress. It never blocks.
func (h *Host) Event(ev adapter.Event) {
	switch ev.Kind {
	case adapter.EventToolCall:
		id := h.callID(ev.CallID)
		h.s.update(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": id, "title": toolTitle(ev.Tool, ev.Text), "kind": "other", "status": "in_progress",
		}, true)
	case adapter.EventToolResult:
		id := h.knownCall(ev.CallID)
		if id == "" {
			return
		}
		status := "completed"
		if ev.Failed {
			status = "failed"
		}
		u := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status}
		if t := strings.TrimSpace(ev.Text); t != "" {
			u["content"] = []any{map[string]any{"type": "content", "content": bodyContent(t)}}
		}
		h.s.update(u, true)
	}
}

func toolTitle(tool, text string) string {
	t := tool
	if t == "" {
		t = text
	}
	t = cleanLine(t)
	if t == "" {
		t = "tool call"
	}
	return t
}

func (h *Host) callID(harness string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls == nil {
		h.calls = map[string]string{}
	}
	h.nextCall++
	id := fmt.Sprintf("call-%d", h.nextCall)
	if harness != "" {
		h.calls[harness] = id
	}
	return id
}

func (h *Host) knownCall(harness string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[harness]
}

// Converse serves an interactive session: the first prompt is the first turn,
// each later prompt another, and the session's events are the client's live
// updates. It returns when the person closes or cancels the session, the
// client goes away, or the session ends by itself (a budget, a cap, the
// harness exiting); the caller then ends the session and reports through
// Finish.
func (h *Host) Converse(ctx context.Context, sess session.Session) error {
	s := h.s
	fwd := newForwarder(h, sess.Events())
	h.mu.Lock()
	h.interactive = true
	h.fwd = fwd
	h.mu.Unlock()
	go fwd.run()
	s.mu.Lock()
	s.st = stLive
	first := s.first
	s.mu.Unlock()
	defer h.stopTakingPrompts()
	if !h.turn(ctx, sess, fwd, first) {
		return nil
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case text := <-s.prompts:
			if !h.turn(ctx, sess, fwd, text) {
				return nil
			}
		case <-s.closeReq:
			return nil
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if ended, _ := sess.Ended(); ended {
				return nil
			}
		}
	}
}

// stopTakingPrompts closes the session to prompts once Converse is done, and
// answers one that raced it.
func (h *Host) stopTakingPrompts() {
	s := h.s
	s.mu.Lock()
	s.st = stOver
	var raced json.RawMessage
	select {
	case <-s.prompts:
		raced = s.turnID
		s.turnID = nil
	default:
	}
	s.mu.Unlock()
	if raced != nil {
		s.errorResponse(&raced, codeInvalidRequest, "the session has ended")
	}
}

// turn runs one prompt and answers it. It reports whether the session can
// take another.
func (h *Host) turn(ctx context.Context, sess session.Session, fwd *forwarder, text string) bool {
	s := h.s
	fwd.drainTurnEnds()
	err := sess.Send(ctx, text)
	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()
	if !cancelled && ctx.Err() == nil && (err == nil || !errors.Is(err, session.ErrEnded)) {
		fwd.waitTurnEnd()
	}
	if cancelled || ctx.Err() != nil {
		// The run was stopped: settle answers the prompt as cancelled.
		return false
	}
	ended, reason := sess.Ended()
	switch {
	case err == nil:
		s.answerTurn("end_turn", nil)
	case errors.Is(err, session.ErrEnded) || ended:
		switch reason {
		case session.EndToolBudget, session.EndUserTurns:
			s.answerTurn("max_turn_requests", map[string]any{"agentry": map[string]any{"endReason": string(reason)}})
		default:
			s.answerTurnError(codeInternal, fmt.Sprintf("the session ended: %s", reasonOrHarness(reason)))
		}
		return false
	default:
		s.answerTurnError(codeInternal, cleanLine(err.Error()))
	}
	return !ended
}

func reasonOrHarness(r session.EndReason) string {
	if r == "" {
		return string(session.EndHarness)
	}
	return string(r)
}

func (s *server) answerTurn(stop string, meta map[string]any) {
	s.mu.Lock()
	id := s.turnID
	s.turnID = nil
	s.mu.Unlock()
	if id == nil {
		return
	}
	res := map[string]any{"stopReason": stop}
	if meta != nil {
		res["_meta"] = meta
	}
	s.result(&id, res)
}

func (s *server) answerTurnError(code int, msg string) {
	s.mu.Lock()
	id := s.turnID
	s.turnID = nil
	s.mu.Unlock()
	if id != nil {
		s.errorResponse(&id, code, msg)
	}
}

// Finish reports the end of the run: for an autonomous run its result and its
// deliverables, answering the prompt that started it; for a session the
// deliverables its teardown listed, ahead of the answer to a session/close. env
// may be nil, and runErr is the run's own error. A run the client cancelled is
// answered as cancelled by the caller's return, not here.
func (h *Host) Finish(env *receipt.Envelope, runErr error) {
	s := h.s
	h.mu.Lock()
	interactive, fwd := h.interactive, h.fwd
	h.mu.Unlock()
	if fwd != nil {
		fwd.wait()
	}
	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()
	if cancelled {
		return
	}
	if env != nil {
		if !interactive {
			if text := resultText(env.Result); text != "" {
				s.message(capResult(text, env.OutDir), false)
			}
		}
		if env.Deliverables != nil && len(env.Deliverables.Files) > 0 && env.OutDir != "" {
			blocks, notes := deliverableBlocks(env.OutDir, env.Deliverables)
			if len(blocks) > 0 {
				s.notice(deliverablesHeading(len(blocks)))
			}
			for _, b := range blocks {
				s.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": b}, false)
			}
			for _, n := range notes {
				s.notice(n)
			}
		}
	}
	if interactive {
		return
	}
	if runErr != nil {
		s.answerTurnError(codeInternal, cleanLine(runErr.Error()))
		return
	}
	s.answerTurn("end_turn", runMeta(env))
}

func deliverablesHeading(n int) string {
	if n == 1 {
		return "Deliverable:"
	}
	return fmt.Sprintf("Deliverables (%d):", n)
}

// capResult bounds a run's result at InlineTotal bytes, at a character
// boundary: a frame past the queue's bound would give the whole connection up.
func capResult(text, outDir string) string {
	if len(text) <= InlineTotal {
		return text
	}
	cut := InlineTotal
	for cut > 0 && !isRuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n\n[The result is longer than %d bytes and is cut here; all of it is in result.json in %s.]", InlineTotal, outDir)
}

// runMeta is what the final answer says of the run beside its stop reason.
func runMeta(env *receipt.Envelope) map[string]any {
	if env == nil {
		return nil
	}
	m := map[string]any{"runId": env.RunID, "status": env.Status, "exitClass": env.ExitClass, "outDir": env.OutDir}
	if len(env.Deliveries) > 0 {
		var ds []map[string]any
		for _, d := range env.Deliveries {
			ds = append(ds, map[string]any{"destination": d.Destination, "kind": d.Kind, "status": d.Status, "error": d.Error})
		}
		m["deliveries"] = ds
	}
	return map[string]any{"agentry": m}
}

// resultText is a run's result as text: a JSON string as itself, any other
// result as its JSON.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var pretty json.RawMessage
	if b, err := json.MarshalIndent(json.RawMessage(raw), "", "  "); err == nil {
		pretty = b
		return string(pretty)
	}
	return string(raw)
}

// forwarder turns an interactive session's events into the client's updates.
type forwarder struct {
	h        *Host
	events   <-chan session.Event
	turnEnds chan struct{}
	done     chan struct{}
	open     []string
	n        int
}

func newForwarder(h *Host, ev <-chan session.Event) *forwarder {
	return &forwarder{h: h, events: ev, turnEnds: make(chan struct{}, 16), done: make(chan struct{})}
}

func (f *forwarder) run() {
	defer close(f.done)
	for ev := range f.events {
		switch ev.Kind {
		case session.KindAssistantText:
			if ev.Text != "" {
				f.h.s.message(ev.Text, true)
			}
		case session.KindToolCall:
			f.n++
			id := fmt.Sprintf("call-%d", f.n)
			f.open = append(f.open, id)
			f.h.s.update(map[string]any{
				"sessionUpdate": "tool_call", "toolCallId": id, "title": toolTitle("", ev.Text), "kind": "other", "status": "in_progress",
			}, true)
		case session.KindAssistantTurn, session.KindError:
			for _, id := range f.open {
				f.h.s.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed"}, true)
			}
			f.open = nil
			select {
			case f.turnEnds <- struct{}{}:
			default:
			}
		}
	}
}

func (f *forwarder) drainTurnEnds() {
	for {
		select {
		case <-f.turnEnds:
		default:
			return
		}
	}
}

// waitTurnEnd waits, a moment at most, until the forwarder has sent the
// updates of the turn that just returned, so the answer to its prompt follows
// them.
func (f *forwarder) waitTurnEnd() {
	select {
	case <-f.turnEnds:
	case <-f.done:
	case <-time.After(5 * time.Second):
	}
}

// wait is for the forwarder to be done once the session's events have
// closed; it gives up after a moment, so a session that never closes them
// does not hold the run's end.
func (f *forwarder) wait() {
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
	}
}
