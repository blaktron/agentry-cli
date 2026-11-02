package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/tui"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A session on a remote runhost, as this device's chat or session protocol
// drives it: the session itself runs on the runhost under its session
// protocol, its commands go over the job's input and its events come back as
// frames, so the TUI and the session protocol drive it as they drive a local
// session.Session.

// remoteSession is a session.Session whose session runs on a runhost.
type remoteSession struct {
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
	res  runhostJobResult
	err  error
	next int

	finish  func(ctx context.Context, res runhostJobResult, err error, next int) (*receipt.Envelope, error)
	endOnce sync.Once
	env     *receipt.Envelope
	endErr  error
}

func newRemoteSession() *remoteSession {
	return &remoteSession{events: make(chan session.Event, 256), cmds: make(chan []byte, 16), done: make(chan struct{}), pending: map[string]chan error{}}
}

// input writes the session's commands to the job's input until End.
func (s *remoteSession) input(w io.Writer) error {
	for line := range s.cmds {
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// frame takes one event frame from the runhost.
func (s *remoteSession) frame(f runhost.Frame) {
	var ev protocol.Event
	if json.Unmarshal(f.Result, &ev) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Refused != "" {
		// A command the runhost's session protocol refused: the call in
		// flight that it answers learns why.
		if ev.Refused == protocol.RefusedInput {
			s.settle("input", refusalError(ev))
		} else {
			s.settle("turn", refusalError(ev))
		}
		return
	}
	switch ev.Kind {
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
	default:
		return fmt.Errorf("the runhost's session refused it (%s): %s", ev.Refused, ev.Text)
	}
}

// settle hands the pending call of kind what its outcome; s.mu is held.
func (s *remoteSession) settle(what string, err error) {
	if ch := s.pending[what]; ch != nil {
		ch <- err
		delete(s.pending, what)
	}
}

// over marks the call ended: the job's result, or the connection lost.
func (s *remoteSession) over(res runhostJobResult, err error, next int) {
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
	s.closed = true
	close(s.events)
	s.mu.Unlock()
	close(s.done)
}

func (s *remoteSession) Events() <-chan session.Event { return s.events }

func (s *remoteSession) begin(what string, line []byte) (chan error, error) {
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

func (s *remoteSession) await(ctx context.Context, what string, ch chan error) error {
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
	line, _ := json.Marshal(runhostSessionInput{Command: (*json.RawMessage)(&raw)})
	return line
}

func (s *remoteSession) Send(ctx context.Context, text string) error {
	ch, err := s.begin("turn", command(protocol.Command{Send: &protocol.Send{Text: text}}))
	if err != nil {
		return err
	}
	return s.await(ctx, "turn", ch)
}

// AddInput sends a regular file of this device's to the session's inputs
// folder on the runhost, then has the session add it.
func (s *remoteSession) AddInput(ctx context.Context, hostPath string) error {
	fi, err := os.Lstat(hostPath)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", hostPath)
	}
	if fi.Size() > runhostMaxSessionFile {
		return fmt.Errorf("%s is over the %d bytes a session on a runhost takes", hostPath, runhostMaxSessionFile)
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
			line, _ := json.Marshal(runhostSessionInput{File: &runhostSessionFile{Name: name, Data: buf[:n], More: more}})
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
func (s *remoteSession) End(ctx context.Context, finish bool) (*receipt.Envelope, error) {
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

func (s *remoteSession) Ended() (bool, session.EndReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, s.reason
}

// runOnSession runs an interactive session of the release on the runhost:
// the terminal chat drives it, or the session protocol does with
// --session-protocol, exactly as they drive a local session. A connection
// lost mid-session leaves the session on the runhost to its idle timeout, as
// a local session whose chat goes quiet; the device follows it to its end and
// collects it.
func runOnSession(cmd *cobra.Command, client *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, j *job.Job, archive []byte,
	files *payload.Packed, lj runhostLocalJob, m *manifest.Manifest, pol *policy.Policy, opts runOpts, carried *runOnCarried, say func(string)) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	s := newRemoteSession()
	var started bool
	go func() {
		res, st, next, err := runOnStart(ctx, client, j, archive, files, false, say, s.frame, s.input)
		started = st
		s.over(res, err, next)
	}()
	s.finish = func(ctx context.Context, res runhostJobResult, err error, next int) (*receipt.Envelope, error) {
		carried.finishProfile(errOut)
		c := client
		if err != nil && !started {
			if home, herr := runnerHome(); herr == nil {
				dropLocalJob(home, j.ID)
			}
			return nil, engine.Classify(runOnSendClass(err), err)
		}
		if err != nil {
			var c2 *runhost.Client
			res, c2, err = runOnReattach(ctx, rec, keys, j.ID, next, errOut, say, nil)
			if err != nil {
				return nil, engine.Classify(engine.ExitGateway, fmt.Errorf("%w: %v; the session goes on there to its idle timeout: agentry runhost attach %s %s collects it", errSessionLost, err, rec.Name, j.ID))
			}
			defer func() { _ = c2.Close() }()
			c = c2
		}
		return runOnFinishEnv(cmd, c, rec, keys, lj, res.ExitCode)
	}
	defer func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = s.End(endCtx, false)
	}()
	if opts.sessionProtocol {
		return runProtocol(cmd, s, opts)
	}
	topts := tui.Options{
		Ref: lj.Ref, Model: pol.Model.Provider + "/" + pol.Model.Name, Caps: capabilityLines(m.Contract()),
		MaxTurns: opts.maxTurns, MaxCostUSD: opts.maxCost, HasTask: strings.TrimSpace(j.Run.Task) != "",
		Budgets: spec.SessionBudgets{IdleTimeoutSeconds: pol.Sessions.IdleTimeoutSeconds, MaxWallSeconds: pol.Sessions.MaxWallSeconds, MaxUserTurns: pol.Sessions.MaxUserTurns},
	}
	if m.Interactive != nil {
		topts.Opening = m.Interactive.Opening
	}
	outcome, uiErr := runChat(ctx, s, topts, cmd.InOrStdin(), cmd.OutOrStdout())
	if uiErr != nil {
		_, _ = fmt.Fprintf(errOut, "chat closed with an error: %v\n", uiErr)
	}
	env, runErr := s.End(ctx, outcome.Finish)
	if env != nil {
		if _, reason := s.Ended(); reason != "" {
			_, _ = fmt.Fprintf(errOut, "session ended: %s\n", reason)
		}
		_, _ = fmt.Fprintf(errOut, "transcript, reports and receipts: %s\n", env.OutDir)
		printDeliverySummary(errOut, env)
	}
	if runErr == nil && uiErr != nil {
		return engine.Classify(engine.ExitCLI, uiErr)
	}
	return runErr
}

var errSessionLost = errors.New("the connection to the runhost was lost during the session")
