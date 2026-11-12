package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// Session is a session.Session whose session runs on a runhost: its
// commands go over the job's input and its events come back as frames.
type Session struct {
	events chan session.Event
	cmds   chan []byte

	mu     sync.Mutex
	ended  bool
	reason session.EndReason
	// pending is the outcome of the Send ("turn") and of the AddInput
	// ("input") in flight: a file may be added while a turn runs, as a
	// local session takes one.
	pending map[string]chan error
	closed  bool

	// done closes when the job's call ends: its result, or the connection
	// lost.
	done chan struct{}
	res  runhost.JobResult
	err  error
	next int

	finish  func(ctx context.Context, res runhost.JobResult, err error, next int) (*receipt.Envelope, error)
	endOnce sync.Once
	env     *receipt.Envelope
	endErr  error
}

// NewSession is a session that runs on a runhost: drive Start with its
// Frame and Input, hand its outcome to Over, and End calls finish to
// collect it.
func NewSession(finish func(ctx context.Context, res runhost.JobResult, err error, next int) (*receipt.Envelope, error)) *Session {
	return &Session{finish: finish, events: make(chan session.Event, 256), cmds: make(chan []byte, 16), done: make(chan struct{}), pending: map[string]chan error{}}
}

// Input writes the session's commands to the job's input until End.
func (s *Session) Input(w io.Writer) error {
	for line := range s.cmds {
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// Frame takes one event frame from the runhost.
func (s *Session) Frame(f runhost.Frame) {
	var ev protocol.Event
	if json.Unmarshal(f.Result, &ev) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Refused != "" {
		// A command the runhost's session protocol refused: the call in
		// flight that it answers learns why.
		switch {
		case ev.Refused == protocol.RefusedInput:
			s.settle("input", refusalError(ev))
		case ev.Refused == protocol.RefusedNoTurn, ev.Refused == protocol.RefusedCannotSteer, ev.Refused == protocol.RefusedSteer:
			s.settle("steer", refusalError(ev))
		case ev.Refused == protocol.RefusedUnknown && s.pending["steer"] != nil:
			// Every runhost knows a send, so the command this one does
			// not know is the steer: its CLI is older than the command.
			s.settle("steer", fmt.Errorf("%w: the runhost's CLI is older than the command", session.ErrCannotSteer))
		default:
			s.settle("turn", refusalError(ev))
		}
		return
	}
	switch ev.Kind {
	case session.KindSteer:
		if !ev.Synthetic {
			s.settle("steer", nil)
		}
	case session.KindAssistantTurn:
		if ev.StopReason == session.StopInterrupted {
			s.settle("turn", session.ErrInterrupted)
		} else {
			s.settle("turn", nil)
		}
	case session.KindInputAdded:
		s.settle("input", nil)
	case session.KindSessionEnd:
		s.ended, s.reason = true, ev.Reason
		s.settle("turn", session.ErrEnded)
		s.settle("input", session.ErrEnded)
		s.settle("steer", session.ErrEnded)
	}
	if !s.closed {
		select {
		case s.events <- ev.Event:
		default:
			// A chat that stopped reading loses events, never the session.
		}
	}
}

func refusalError(ev protocol.Event) error {
	switch ev.Refused {
	case protocol.RefusedBusy:
		return session.ErrBusy
	case protocol.RefusedNoTurn:
		return session.ErrNoTurn
	case protocol.RefusedCannotSteer:
		return fmt.Errorf("%w: %s", session.ErrCannotSteer, ev.Text)
	default:
		return fmt.Errorf("the runhost's session refused it (%s): %s", ev.Refused, ev.Text)
	}
}

// settle hands the pending call of kind what its outcome; s.mu is held.
func (s *Session) settle(what string, err error) {
	if ch := s.pending[what]; ch != nil {
		ch <- err
		delete(s.pending, what)
	}
}

// Over marks the call ended: the job's result, or the connection lost.
func (s *Session) Over(res runhost.JobResult, err error, next int) {
	s.mu.Lock()
	s.res, s.err, s.next = res, err, next
	if !s.ended {
		s.ended = true
		if s.reason == "" {
			s.reason = session.EndCancelled
		}
	}
	s.settle("turn", session.ErrEnded)
	s.settle("input", session.ErrEnded)
	s.settle("steer", session.ErrEnded)
	s.closed = true
	close(s.events)
	s.mu.Unlock()
	close(s.done)
}

func (s *Session) Events() <-chan session.Event { return s.events }

func (s *Session) begin(what string, line []byte) (chan error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, session.ErrEnded
	}
	if s.pending[what] != nil {
		return nil, session.ErrBusy
	}
	ch := make(chan error, 1)
	s.pending[what] = ch
	if line != nil {
		select {
		case s.cmds <- line:
		default:
			delete(s.pending, what)
			return nil, session.ErrBusy
		}
	}
	return ch, nil
}

func (s *Session) await(ctx context.Context, what string, ch chan error) error {
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		s.mu.Lock()
		if s.pending[what] == ch {
			delete(s.pending, what)
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

func command(c protocol.Command) []byte {
	raw, _ := json.Marshal(c)
	line, _ := json.Marshal(runhost.SessionInput{Command: (*json.RawMessage)(&raw)})
	return line
}

func (s *Session) Send(ctx context.Context, text string) error {
	ch, err := s.begin("turn", command(protocol.Command{Send: &protocol.Send{Text: text}}))
	if err != nil {
		return err
	}
	return s.await(ctx, "turn", ch)
}

// Steer implements session.Steerer: the message goes to the session on the
// runhost, and Steer returns once that session has taken or refused it.
func (s *Session) Steer(ctx context.Context, text string) error {
	ch, err := s.begin("steer", command(protocol.Command{Steer: &protocol.Steer{Text: text}}))
	if err != nil {
		return err
	}
	return s.await(ctx, "steer", ch)
}

// AddInput sends a regular file of this device's to the session's inputs
// folder on the runhost, then has the session add it.
func (s *Session) AddInput(ctx context.Context, hostPath string) error {
	fi, err := os.Lstat(hostPath)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", hostPath)
	}
	if fi.Size() > runhost.MaxSessionFile {
		return fmt.Errorf("%s is over the %d bytes a session on a runhost takes", hostPath, runhost.MaxSessionFile)
	}
	name := filepath.Base(hostPath)
	ch, err := s.begin("input", nil)
	if err != nil {
		return err
	}
	f, err := os.Open(hostPath) //nolint:gosec // a file the person added
	if err != nil {
		s.mu.Lock()
		delete(s.pending, "input")
		s.mu.Unlock()
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1<<20)
	for first := true; ; first = false {
		n, rerr := io.ReadFull(f, buf)
		more := rerr == nil
		if n > 0 || first {
			line, _ := json.Marshal(runhost.SessionInput{File: &runhost.SessionFile{Name: name, Data: buf[:n], More: more}})
			select {
			case s.cmds <- line:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !more {
			break
		}
	}
	select {
	case s.cmds <- command(protocol.Command{Add: &protocol.Add{Path: name}}):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.await(ctx, "input", ch)
}

// End ends the session (with finish, asking for its final result), waits
// for the job to end on the runhost, and collects its output and envelope.
func (s *Session) End(ctx context.Context, finish bool) (*receipt.Envelope, error) {
	s.endOnce.Do(func() {
		s.mu.Lock()
		ended := s.ended
		s.mu.Unlock()
		if !ended {
			select {
			case s.cmds <- command(protocol.Command{End: &protocol.End{Finish: finish}}):
			case <-ctx.Done():
			case <-s.done:
			}
		}
		select {
		case <-s.done:
		case <-ctx.Done():
			s.env, s.endErr = nil, ctx.Err()
			return
		}
		close(s.cmds)
		s.env, s.endErr = s.finish(ctx, s.res, s.err, s.next)
	})
	return s.env, s.endErr
}

func (s *Session) Ended() (bool, session.EndReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, s.reason
}
