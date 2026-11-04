package persistent

// A hold lends a running agent to a client for a set-up that needs what the
// running segment has: an image server's sign-in writes the volume the
// server's container holds, so the segment must end first. The supervisor
// waits for a turn in flight, ends the segment out of the chats' sight and
// keeps the turn, so a message sent meanwhile waits; the client sets up and
// records what it did through changes on the same connection; resuming (or
// the connection closing, or the hold's bound passing, or the agent
// stopping) ends the hold, and the next segment starts from the config as
// it is then, its conversation continuing. Each hold has a number: only the
// connection that took it, or its own bound, ends it. The hold is the owner
// socket's alone: the app link has no such command.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// holdBound is the longest a client may hold the agent: a sign-in waits on
// the person (Microsoft's device code up to 15 minutes, after its image and
// volume are readied), and a message sent meanwhile waits as long.
var holdBound = 20 * time.Minute

// HoldFor says what a hold sets up, for the chats: a server's name.
type HoldFor struct {
	What string `json:"what"`
}

// ResumeFrom ends a hold; Done says the set-up finished, which is what the
// chats are told.
type ResumeFrom struct {
	Done bool `json:"done"`
}

// ErrHoldTaken is a hold asked for while another client holds the agent,
// or is waiting to.
var ErrHoldTaken = errors.New("the agent is held for a set-up already")

// hold is the supervisor's record of the hold in force.
type hold struct {
	id    int
	what  string
	end   func()
	timer *time.Timer
}

// Hold ends the running segment between turns and keeps the turn for a
// client's set-up of what (a server's name), until the hold it answers is
// resumed. A turn in flight is waited for, at most until ctx ends.
func (s *Supervisor) Hold(ctx context.Context, what string) (int, error) {
	what = strings.TrimSpace(what)
	if what == "" || len(what) > 120 || !utf8.ValidString(what) || strings.ContainsFunc(what, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return 0, errors.New("a hold names what it sets up in one short line of printable text")
	}
	s.mu.Lock()
	switch {
	case s.stopped || s.closing:
		s.mu.Unlock()
		return 0, ErrNotRunning
	case s.hold != nil || s.holdPending:
		s.mu.Unlock()
		return 0, ErrHoldTaken
	}
	s.holdPending = true
	s.mu.Unlock()
	pending := func() { s.mu.Lock(); s.holdPending = false; s.mu.Unlock() }
	if !s.waitTurn(ctx) {
		pending()
		return 0, fmt.Errorf("a turn is in flight: %w", ctx.Err())
	}
	s.seg.Lock()
	s.mu.Lock()
	if s.stopped || s.closing {
		s.mu.Unlock()
		s.seg.Unlock()
		s.turn.Unlock()
		pending()
		return 0, ErrNotRunning
	}
	s.mu.Unlock()
	if _, err := s.endSegment(context.WithoutCancel(ctx), true); err != nil && !s.segmentEnded() {
		s.seg.Unlock()
		s.turn.Unlock()
		pending()
		return 0, fmt.Errorf("the running segment did not end: %w", err)
	}
	s.seg.Unlock()
	end := s.beginWait("Setting up "+what+": your message waits until it is done.", holdBound)
	s.notice("Setting up " + what + ": the agent pauses between turns, and its conversation continues once it is done.")
	s.mu.Lock()
	s.holds++
	h := &hold{id: s.holds, what: what, end: end}
	s.hold, s.holdPending = h, false
	// A client that never resumes does not keep the agent: past the bound
	// the hold ends by itself.
	h.timer = time.AfterFunc(holdBound, func() {
		s.logf("the set-up of %s held the agent past %s: it goes on without waiting longer", what, holdBound)
		_ = s.Resume(context.Background(), h.id, false)
	})
	s.mu.Unlock()
	s.logf("held for setting up %s", what)
	return h.id, nil
}

// Resume ends hold id: the next segment starts from the config as it is,
// continuing the conversation, and the turn is let go. done says the
// set-up finished. A next segment that cannot start is handled as a
// change's is (startNext). A hold that is no longer in force (its bound
// ended it, the agent stopped) is refused.
func (s *Supervisor) Resume(ctx context.Context, id int, done bool) error {
	h, err := s.takeHold(id)
	if err != nil {
		return err
	}
	defer s.turn.Unlock()
	defer h.end()
	s.seg.Lock()
	defer s.seg.Unlock()
	s.mu.Lock()
	stopped := s.stopped || s.closing
	s.mu.Unlock()
	// Stopped meanwhile: there is no next segment to start.
	if stopped {
		return nil
	}
	if err := s.startNext(ctx); err != nil {
		return err
	}
	if done {
		s.notice(h.what + " is set up: the agent goes on with it.")
	} else {
		s.notice("Setting up " + h.what + " did not finish: the agent goes on as it was.")
	}
	s.logf("resumed after setting up %s (finished: %t)", h.what, done)
	return nil
}

// takeHold ends hold id's record and stops its bound, or refuses an id that
// is not the hold in force.
func (s *Supervisor) takeHold(id int) (*hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hold
	if h == nil || h.id != id {
		return nil, errors.New("the agent is not held by this set-up: it ended")
	}
	s.hold = nil
	h.timer.Stop()
	return h, nil
}

// releaseHold ends the hold in force without starting a segment, as a stop
// does, so nothing waits on its turn.
func (s *Supervisor) releaseHold() {
	s.mu.Lock()
	h := s.hold
	s.mu.Unlock()
	if h == nil {
		return
	}
	if h, err := s.takeHold(h.id); err == nil {
		h.end()
		s.turn.Unlock()
	}
}

// holding says id is the hold in force.
func (s *Supervisor) holding(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hold != nil && s.hold.id == id
}

// heldFor is what the hold in force sets up, or "".
func (s *Supervisor) heldFor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold == nil {
		return ""
	}
	return s.hold.what
}

// segmentEnded says the running segment has ended.
func (s *Supervisor) segmentEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.segEnded
}

// AgentHold is a client's hold on a running agent, on its own connection
// to the owner socket.
type AgentHold struct {
	conn net.Conn
	sc   *bufio.Scanner
	name string
}

// HoldAgent holds the running agent name for a set-up of what, waiting for a
// turn in flight to end. It answers ErrNotRunning when no supervisor
// answers. The caller ends it with Resume, or by closing it.
func HoldAgent(ctx context.Context, home, name, what string) (*AgentHold, error) {
	c, err := dial(ctx, home, name)
	if err != nil {
		return nil, err
	}
	h := &AgentHold{conn: c, sc: bufio.NewScanner(c), name: name}
	h.sc.Buffer(make([]byte, 64*1024), 16<<20)
	if err := h.call(ctx, Command{Hold: &HoldFor{What: what}}, func(r Reply) bool { return r.Held != nil }); err != nil {
		_ = c.Close()
		return nil, err
	}
	return h, nil
}

// Change sends a change to the held agent, applied as any client's is; a
// sign-in record is taken on a holding connection alone.
func (h *AgentHold) Change(ctx context.Context, ch Change) (Roster, error) {
	var r Roster
	err := h.call(ctx, Command{Change: &ch}, func(rep Reply) bool {
		if rep.Roster != nil {
			r = rep.Roster
			return true
		}
		return false
	})
	return r, err
}

// Resume ends the hold and waits for the next segment to start; done says
// the set-up finished.
func (h *AgentHold) Resume(ctx context.Context, done bool) error {
	defer func() { _ = h.conn.Close() }()
	return h.call(ctx, Command{Resume: &ResumeFrom{Done: done}}, func(r Reply) bool { return r.Resumed != nil })
}

// Close lets the hold go without waiting: the supervisor resumes on its
// own, as for a set-up that did not finish.
func (h *AgentHold) Close() error { return h.conn.Close() }

// call sends cmd and reads replies until want takes one.
func (h *AgentHold) call(ctx context.Context, cmd Command, want func(Reply) bool) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = h.conn.SetDeadline(dl)
		defer func() { _ = h.conn.SetDeadline(time.Time{}) }()
	}
	b, _ := json.Marshal(cmd)
	if _, err := h.conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", h.name, err)
	}
	for h.sc.Scan() {
		var r Reply
		if json.Unmarshal(h.sc.Bytes(), &r) != nil {
			continue
		}
		if r.Event != nil && r.Event.Refused == RefusedConsent {
			return &ConsentError{Agent: h.name, Entries: r.Consent}
		}
		if r.Event != nil && r.Event.Refused != "" {
			return &RefusedError{Class: string(r.Event.Refused), Text: r.Event.Text}
		}
		if want(r) {
			return nil
		}
	}
	if err := h.sc.Err(); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", h.name, err)
	}
	return fmt.Errorf("the %s agent's supervisor closed the socket without answering: %w", h.name, ErrNoAnswer)
}

// RefusedError is a command the supervisor refused, by its class.
type RefusedError struct {
	Class, Text string
}

func (e *RefusedError) Error() string { return e.Text }

// ErrOlderSupervisor is a running agent whose supervisor is an older
// Agentry's, which refuses a command this client sends as malformed: the
// client hands the agent over (HandOver) and asks again.
var ErrOlderSupervisor = errors.New("its supervisor is an older Agentry's, which does not take this")

// OlderSupervisor says err is a running supervisor's refusal of a command it
// does not know.
func OlderSupervisor(err error) bool {
	var refused *RefusedError
	return errors.Is(err, ErrOlderSupervisor) || errors.As(err, &refused) && refused.Class == string(protocol.RefusedMalformed) && strings.HasPrefix(refused.Text, unknownCommand)
}

// unknownCommand begins the refusal of a line that names no command the
// supervisor knows.
const unknownCommand = "a command is one JSON object naming exactly one of"

// handOverWait bounds how long a hand-over waits for a turn in flight.
var handOverWait = 10 * time.Minute

// HandOver moves the running agent a onto a supervisor of this binary, for
// a change an older one cannot take: it waits until no turn is in flight,
// stops the agent and waits for its lock, then start starts it again, its
// conversation continuing. The person does nothing.
func HandOver(ctx context.Context, home string, a Agent, client string, start func(context.Context) error) error {
	if a.DesktopOnly {
		return fmt.Errorf("the %s agent runs in Agentry Desktop alone: update Agentry Desktop to change it", a.Name)
	}
	deadline := time.Now().Add(handOverWait)
	for {
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := QueryStatus(qctx, home, a.Name)
		cancel()
		if errors.Is(err, ErrNotRunning) || errors.Is(err, ErrNoAnswer) {
			break
		}
		if err == nil && st.Client != "" && st.Client != client {
			return fmt.Errorf("the %s agent runs in %s, whose supervisor is older than this client's: update %s to change it, or stop the agent there", a.Name, Holder{Client: st.Client}.Where(), Holder{Client: st.Client}.Where())
		}
		if err == nil && !st.Busy {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the %s agent's turn did not end within %s, so it was not handed over", a.Name, handOverWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if _, err := StopForRemoval(ctx, home, a, client); err != nil {
		return err
	}
	return start(ctx)
}
