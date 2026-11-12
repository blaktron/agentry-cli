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

	"github.com/google/uuid"

	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// agentryHarnessTransport names the session drive of the runner's own
// harness: its serve mode, on the container's stdio.
const agentryHarnessTransport = "agentry-harness-stdio"

// Stop reasons of a turn the harness ended without a whole answer, in the
// Agent Client Protocol's words, which the other container harness uses.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopMaxSteps  = "max_turn_requests"
)

// harnessSession is the agentry-harness SessionDriver. It writes the
// harness's commands (package proto) to its stdin and reads its events from
// its stdout, the same events a one-shot run prints.
//
// The harness asks the runner two things, and the driver answers both. A
// permission event is permitted for a tool the run's gateway serves
// (SessionOptions.GatewayTools) and denied for any other; the denial is the
// call's result, so the turn goes on. A turn_ending event is accepted: a
// session's turn ends on the model's answer, and the person says what comes
// next.
type harnessSession struct {
	rw      io.ReadWriteCloser
	adapter *agentryHarnessAdapter
	allowed map[string]bool
	opts    SessionOptions

	writeMu sync.Mutex // one command a line

	mu sync.Mutex
	// opening receives the answer to the open command in flight: a ready
	// event, or the error event that refused it.
	opening chan proto.Event
	// turn is the turn in flight, from its prompt to its turn_end.
	turn *harnessTurn

	conversation string
	continued    bool
	notContinued string

	events    chan Event
	eventGate eventGate
	readDone  chan struct{}
	closeOnce sync.Once
}

// harnessTurn is what one Prompt gathers.
type harnessTurn struct {
	// done is closed when the turn's turn_end has been read, after every
	// event before it has been sent on.
	done chan struct{}
	end  proto.Event
	// failure is the turn's last error as the engine is shown it, and
	// refused the harness's words when it did not take the prompt.
	failure string
	refused string
	// in and out sum the turn's requests; reported says there was one.
	in, out  int64
	reported bool
}

// Session implements Adapter.
func (a *agentryHarnessAdapter) Session(_ context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New(AgentryHarnessAdapterName + " session: nil transport")
	}
	s := &harnessSession{
		rw:       rw,
		adapter:  a,
		allowed:  make(map[string]bool, len(opts.GatewayTools)),
		opts:     opts,
		events:   make(chan Event, 256),
		readDone: make(chan struct{}),
	}
	for _, t := range opts.GatewayTools {
		s.allowed[t] = true
	}
	go s.readLoop()
	return s, nil
}

// Start implements SessionDriver: it opens the conversation the session is
// in. A conversation it was asked to continue is loaded; one the harness
// cannot load gives way to a new one, and Continued says why.
func (s *harnessSession) Start(ctx context.Context) error {
	if id := s.opts.ContinueID; id != "" {
		ready, err := s.open(ctx, id)
		switch {
		case err == nil:
			s.mu.Lock()
			s.conversation, s.continued = id, ready.Continued
			if !ready.Continued {
				s.notContinued = "the session store holds no conversation of that id"
			}
			s.mu.Unlock()
			return nil
		case ctx.Err() != nil || s.ended():
			return err
		}
		s.mu.Lock()
		s.notContinued = "the harness could not load the stored conversation: " + err.Error()
		s.mu.Unlock()
	}
	id := s.opts.ConversationID
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := s.open(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	s.conversation = id
	s.mu.Unlock()
	return nil
}

// open sends an open command and waits for the harness's answer.
func (s *harnessSession) open(ctx context.Context, id string) (proto.Event, error) {
	answer := make(chan proto.Event, 1)
	s.mu.Lock()
	s.opening = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.opening = nil
		s.mu.Unlock()
	}()
	if err := s.send(proto.Command{Type: proto.CmdOpen, Conversation: id}); err != nil {
		return proto.Event{}, fmt.Errorf("open: %w", err)
	}
	answered := func(ev proto.Event) (proto.Event, error) {
		if ev.Type == proto.TypeError {
			return ev, errors.New(agentryHarnessErrorText(ev))
		}
		return ev, nil
	}
	select {
	case ev := <-answer:
		return answered(ev)
	case <-ctx.Done():
		return proto.Event{}, fmt.Errorf("open: %w", ctx.Err())
	case <-s.readDone:
		// The reader hands an answer over before it can see the end of
		// the stream, so one is waiting if one came.
		select {
		case ev := <-answer:
			return answered(ev)
		default:
			return proto.Event{}, errors.New("open: harness ended the session before answering")
		}
	}
}

// ended reports whether the harness's output has ended.
func (s *harnessSession) ended() bool {
	select {
	case <-s.readDone:
		return true
	default:
		return false
	}
}

// ConversationID implements Continuing.
func (s *harnessSession) ConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversation
}

// Continued implements Continuing.
func (s *harnessSession) Continued() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.continued, s.notContinued
}

// Prompt implements SessionDriver.
func (s *harnessSession) Prompt(ctx context.Context, text string) (*Turn, error) {
	if s.ConversationID() == "" {
		return nil, errors.New("session not started")
	}
	// A turn whose Prompt gave up waiting is still the harness's until its
	// turn_end: the next one starts after it.
	for {
		s.mu.Lock()
		earlier := s.turn
		s.mu.Unlock()
		if earlier == nil {
			break
		}
		select {
		case <-earlier.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("prompt: %w", ctx.Err())
		case <-s.readDone:
			return nil, errors.New("prompt: harness ended the session")
		}
	}
	t := &harnessTurn{done: make(chan struct{})}
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
	if err := s.send(proto.Command{Type: proto.CmdPrompt, Text: text}); err != nil {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
		return nil, fmt.Errorf("prompt: %w", err)
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		// The harness is told to stop the turn nobody waits for any more.
		_ = s.send(proto.Command{Type: proto.CmdInterrupt})
		return nil, fmt.Errorf("prompt: %w", ctx.Err())
	case <-s.readDone:
		select {
		case <-t.done:
		default:
			s.mu.Lock()
			failure := t.failure
			s.mu.Unlock()
			if failure != "" {
				return nil, errors.New(failure)
			}
			return nil, errors.New("prompt: harness ended the session before the turn ended")
		}
	}
	// The reader is done with the turn: nothing else writes to it.
	if t.refused != "" {
		return nil, errors.New("prompt: " + t.refused)
	}
	out := &Turn{Text: t.end.Text}
	if t.reported {
		out.Usage = &TurnUsage{InputTokens: int(t.in), OutputTokens: int(t.out), TotalTokens: int(t.in + t.out)}
	}
	switch t.end.Reason {
	case proto.EndError:
		if t.failure == "" {
			return nil, errors.New("the harness ended the turn on a failure it did not name")
		}
		return nil, errors.New(t.failure)
	case proto.EndInterrupted:
		out.StopReason = StopInterrupted
	case proto.EndAnswerCut:
		out.StopReason = stopMaxTokens
	case proto.EndMaxSteps:
		out.StopReason = stopMaxSteps
	default:
		out.StopReason = stopEndTurn
	}
	return out, nil
}

// Interrupt implements Interrupter: the harness stops the turn in flight,
// gives every call left open a result, and ends the turn as interrupted.
func (s *harnessSession) Interrupt(context.Context) error {
	s.mu.Lock()
	inTurn := s.turn != nil
	s.mu.Unlock()
	if !inTurn {
		return nil
	}
	return s.send(proto.Command{Type: proto.CmdInterrupt})
}

// Steer implements Steerer.
func (s *harnessSession) Steer(_ context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return s.send(proto.Command{Type: proto.CmdSteer, Text: text})
}

// Events implements SessionDriver.
func (s *harnessSession) Events() <-chan Event { return s.events }

// Close implements SessionDriver: closing the transport ends the harness's
// stdin, on which it stops a turn in flight and exits; the reader drains its
// last lines and the events channel closes.
func (s *harnessSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.rw.Close()
		<-s.readDone
		s.eventGate.close(s.events)
	})
	return err
}

func (s *harnessSession) send(c proto.Command) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.rw.Write(agentryHarnessCommand(c))
	return err
}

func (s *harnessSession) emit(ev Event) { s.eventGate.send(s.events, ev) }

// readLoop reads the harness's events: it answers what the harness asks,
// hands the session's own answers to whoever waits for them, and sends
// everything on as the engine's events.
func (s *harnessSession) readLoop() {
	defer close(s.readDone)
	sc := bufio.NewScanner(s.rw)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev proto.Event
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
			// Not the protocol: kept as an observation, never an error.
			s.observe(string(line), line)
			continue
		}
		s.mu.Lock()
		opening, turn := s.opening, s.turn
		s.mu.Unlock()
		switch ev.Type {
		case proto.TypeReady:
			s.observe(ev.Type, line)
			answerOpen(opening, ev)
		case proto.TypeError:
			if opening != nil && turn == nil {
				// The refusal of an open is Start's to answer, with a new
				// conversation where it can: it is not the session's error.
				s.observe(ev.Type, line)
				answerOpen(opening, ev)
				continue
			}
			if turn != nil {
				s.mu.Lock()
				turn.failure = agentryHarnessErrorText(ev)
				s.mu.Unlock()
			}
			s.forward(line)
		case proto.TypePermission:
			// The gateway is the only source of tools and the policy
			// bounds what it serves, so one of its tools is permitted.
			// Any other is denied: the harness answers that call with the
			// denial and the model reads it.
			kind, answer := EventPermissionDenied, proto.CmdDeny
			if s.allowed[ev.Tool] {
				kind, answer = EventPermissionGranted, proto.CmdPermit
			}
			_ = s.send(proto.Command{Type: answer, CallID: ev.CallID})
			s.emit(Event{Kind: kind, Text: ev.Tool, Raw: rawCopy(line)})
		case proto.TypeTurnEnding:
			_ = s.send(proto.Command{Type: proto.CmdAccept})
			s.observe(ev.Type, line)
		case proto.TypeUsage:
			if turn != nil && ev.Usage != nil && !ev.Usage.Compacted {
				s.mu.Lock()
				turn.in += ev.Usage.Input + ev.Usage.CacheRead + ev.Usage.CacheWrite
				turn.out += ev.Usage.Output
				turn.reported = true
				s.mu.Unlock()
			}
			s.forward(line)
		case proto.TypeWarning:
			if ev.Code == proto.WarnCommandRefused && ev.Reason == proto.CmdPrompt && turn != nil {
				// The harness did not take the prompt, so no turn_end
				// will come for it.
				s.observe(ev.Code, line)
				s.finish(turn, func() { turn.refused = "the harness did not take the prompt: " + ev.Text })
				continue
			}
			s.forward(line)
		case proto.TypeTurnEnd:
			if ev.Reason == proto.EndAnswered || ev.Reason == proto.EndInterrupted || ev.Reason == proto.EndError {
				// The turn's own outcome says these; the event is kept
				// for the record.
				s.observe(ev.Type, line)
			} else {
				s.forward(line)
			}
			if turn != nil {
				s.finish(turn, func() { turn.end = ev })
			}
		default:
			s.forward(line)
		}
	}
}

// observe keeps a line the engine has nothing to act on, under a word that
// says what it was.
func (s *harnessSession) observe(what string, line []byte) {
	s.emit(Event{Kind: EventOther, Text: what, Raw: rawCopy(line)})
}

// answerOpen hands the open in flight its answer. Only the first is taken:
// a harness that answered twice must not hold the reader.
func answerOpen(opening chan proto.Event, ev proto.Event) {
	if opening == nil {
		return
	}
	select {
	case opening <- ev:
	default:
	}
}

// forward sends a line on as the events the adapter reads it as.
func (s *harnessSession) forward(line []byte) {
	for _, ev := range s.adapter.Events(line) {
		s.emit(ev)
	}
}

// finish closes the turn in flight, after set has recorded how it ended.
func (s *harnessSession) finish(t *harnessTurn, set func()) {
	s.mu.Lock()
	set()
	if s.turn == t {
		s.turn = nil
	}
	s.mu.Unlock()
	close(t.done)
}
