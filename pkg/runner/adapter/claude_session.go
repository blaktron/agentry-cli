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
	readDone  chan struct{}
	closeOnce sync.Once
}

// claudeTurnResult is what the result message of one turn carried.
type claudeTurnResult struct {
	stopReason string
	text       string
	usage      *TurnUsage
}

// Start implements SessionDriver. Claude Code's stream-json input has no
// handshake and prints nothing before the first user turn, so opening the
// session is the first Prompt; a harness that failed to start surfaces as
// that Prompt's error or as the process exiting.
func (s *claudeSession) Start(context.Context) error { return nil }

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
		return &Turn{StopReason: r.stopReason, Text: r.text, Usage: r.usage}, nil
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
		close(s.events)
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
		isResult := json.Unmarshal(line, &msg) == nil && msg.Type == "result"
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
		res := claudeTurnResult{stopReason: msg.StopReason, text: msg.Result}
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

func (s *claudeSession) emit(ev Event) { s.events <- ev }
