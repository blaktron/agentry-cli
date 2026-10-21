package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// claudeSession is the SessionDriver for an interactive Claude Code run.
// Claude Code's `--input-format stream-json` is not the Agent Client
// Protocol: there is no JSON-RPC envelope, no handshake and no per-call
// permission request. A user turn is one newline-delimited JSON message
// written to stdin; the harness answers with the same event stream an
// autonomous run prints and closes the turn with a `result` message;
// closing stdin ends the process. That is the whole protocol, which is why
// this driver is smaller than acpSession rather than a variant of it.
//
// Verified against Claude Code 2.1.263: nothing is printed until the
// first user turn, context carries across turns, and stdin EOF exits 0.
type claudeSession struct {
	rw io.ReadWriteCloser
	ad *claudeAdapter

	writeMu sync.Mutex // one JSON line at a time

	mu   sync.Mutex
	turn chan claudeTurnResult // non-nil while a Prompt is in flight

	events    chan Event
	eventGate eventGate // orders sends against close(events)
	readDone  chan struct{}
	closeOnce sync.Once

	// continueID is the stored conversation a persistent agent's segment
	// continues (--resume), conversationID the new one it opens
	// (--session-id); continued says Start continued it. starting is
	// non-nil until Start has its handshake's answer.
	continueID, conversationID string
	continued                  bool
	starting                   chan claudeStart
}

// claudeStart is how Claude Code answered the driver's initialize request:
// failed is set when it ended the session instead, with its reasons.
type claudeStart struct {
	failed bool
	reason string
}

// claudeInitializeID is the request id of the driver's initialize request.
const claudeInitializeID = "agentry-initialize"

// claudeTurnResult is what the result message of one turn carried.
type claudeTurnResult struct {
	stopReason string
	text       string
	usage      *TurnUsage
	// failed is an error result's reason: the turn is not an answer (I9).
	failed string
}

// Start implements SessionDriver. Claude Code's stream-json input prints
// nothing before the first user turn, so a run's session opens with that
// turn; a harness that failed to start surfaces as its error or as the
// process exiting. A persistent agent's segment, which names its
// conversation on the command line, sends Claude Code's initialize control
// request and waits for the answer: a conversation --resume cannot load
// ends the process with its reasons before any answer, which is reported as
// a *NotContinuedError for the engine to start a new one.
func (s *claudeSession) Start(ctx context.Context) error {
	if s.starting == nil {
		return nil
	}
	defer func() {
		s.mu.Lock()
		s.starting = nil
		s.mu.Unlock()
	}()
	s.mu.Lock()
	ch := s.starting
	s.mu.Unlock()
	// A harness that already ended refuses the write; what it said before
	// it ended is the answer.
	sendErr := s.send(map[string]any{
		"type": "control_request", "request_id": claudeInitializeID,
		"request": map[string]any{"subtype": "initialize"},
	})
	select {
	case r := <-ch:
		return s.startAnswer(r)
	case <-s.readDone:
		select {
		case r := <-ch:
			return s.startAnswer(r)
		default:
		}
		if sendErr != nil {
			return fmt.Errorf("claude session: send initialize: %w", sendErr)
		}
		return errors.New("claude session: the harness ended before it answered the initialize request")
	case <-ctx.Done():
		return fmt.Errorf("claude session: initialize: %w", ctx.Err())
	}
}

// startAnswer is Start's outcome for Claude Code's answer to initialize.
func (s *claudeSession) startAnswer(r claudeStart) error {
	switch {
	case !r.failed:
		s.continued = s.continueID != ""
		return nil
	case s.continueID != "" && strings.Contains(r.reason, s.continueID):
		// Claude Code names the conversation it could not load; any other
		// failure at the start is the start's, not the conversation's.
		return &NotContinuedError{Reason: "Claude Code could not resume the stored conversation: " + r.reason}
	}
	return fmt.Errorf("claude session: Claude Code did not start: %s", r.reason)
}

// ConversationID implements Continuing: the conversation the segment
// continued, or the one it opened.
func (s *claudeSession) ConversationID() string {
	if s.continued {
		return s.continueID
	}
	return s.conversationID
}

// Continued implements Continuing. A conversation Claude Code could not
// load fails Start instead.
func (s *claudeSession) Continued() (bool, string) { return s.continued, "" }

// started hands Start the answer to its initialize request; false when no
// Start is waiting for one.
func (s *claudeSession) started(r claudeStart) bool {
	s.mu.Lock()
	ch := s.starting
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- r:
	default:
	}
	return true
}

// Prompt implements SessionDriver: one user turn in, everything the
// harness says until its result message out.
func (s *claudeSession) Prompt(ctx context.Context, text string) (*Turn, error) {
	ch := make(chan claudeTurnResult, 1)
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("claude session: a turn is already in flight")
	}
	s.turn = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()

	if err := s.send(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}); err != nil {
		return nil, fmt.Errorf("claude session: send turn: %w", err)
	}
	select {
	case r := <-ch:
		out := &Turn{StopReason: r.stopReason, Text: r.text, Usage: r.usage}
		// A turn that ended on an error result is not an answer (I9).
		if r.failed != "" {
			return out, fmt.Errorf("claude session: turn failed: %s", firstLineOf(r.failed))
		}
		return out, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("claude session: %w", ctx.Err())
	case <-s.readDone:
		return nil, errors.New("claude session: the harness ended the session before answering")
	}
}

// Events implements SessionDriver.
func (s *claudeSession) Events() <-chan Event { return s.events }

// Close implements SessionDriver: closing the transport is stdin EOF for
// Claude Code, which exits; the reader drains its last lines and the
// event channel closes. The engine's teardown bounds the wait.
func (s *claudeSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.rw.Close()
		<-s.readDone
		s.eventGate.close(s.events)
	})
	return err
}

func (s *claudeSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.rw.Write(append(b, '\n'))
	return err
}

// readLoop normalizes every harness line through the adapter's own
// Events, so an interactive transcript and an autonomous receipt describe
// the run the same way, and completes the in-flight turn on the result
// message.
func (s *claudeSession) readLoop() {
	defer close(s.readDone)
	sc := bufio.NewScanner(s.rw)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var text strings.Builder
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg claudeMessage
		parsed := json.Unmarshal(line, &msg) == nil
		if parsed && msg.Type == "control_response" {
			// The answer to the driver's own request, not the harness's
			// output.
			if msg.Response.RequestID == claudeInitializeID {
				s.started(claudeStart{failed: msg.Response.Subtype != "success", reason: msg.Response.Error})
			}
			continue
		}
		isResult := parsed && msg.Type == "result"
		reason := ""
		if isResult && msg.IsError {
			// A result before the handshake's answer is Claude Code ending
			// the session at its start.
			reason = strings.Join(msg.Errors, "; ")
			if reason == "" {
				reason = msg.Result
			}
			if reason == "" {
				reason = msg.Subtype
			}
			s.started(claudeStart{failed: true, reason: reason})
		}
		for _, ev := range s.ad.Events(line) {
			if isResult && ev.Kind == EventAssistantText {
				// The assistant message already carried this text into the
				// transcript; the result message's copy would double it.
				continue
			}
			if ev.Kind == EventAssistantText {
				text.WriteString(ev.Text)
			}
			s.emit(ev)
		}
		if !isResult {
			continue
		}
		res := claudeTurnResult{stopReason: msg.StopReason, text: msg.Result, failed: reason}
		if res.text == "" {
			res.text = text.String()
		}
		if msg.Usage.InputTokens > 0 || msg.Usage.OutputTokens > 0 {
			res.usage = &TurnUsage{
				InputTokens:  int(msg.Usage.InputTokens),
				OutputTokens: int(msg.Usage.OutputTokens),
				TotalTokens:  int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
			}
		}
		text.Reset()
		s.mu.Lock()
		ch := s.turn
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- res:
			default:
			}
		}
	}
}

func (s *claudeSession) emit(ev Event) { s.eventGate.send(s.events, ev) }
