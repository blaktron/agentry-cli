package harness

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// Serve is the harness's session mode: any number of turns, on a
// conversation the runner opens, with events on stdout and anything else on
// stderr.
//
//	serve --config FILE
//
// Stdin carries the runner's commands, one JSON object a line. The first is
// an open, which names the conversation and is answered with a ready event.
// Each prompt then runs one turn, which ends with a turn_end event; between
// turns a compact replaces the conversation with a summary of it. While a
// turn is under way the runner may steer it or interrupt it, and answers
// the harness's questions: a permit or a deny for each permission event,
// and an accept or a continue for each turn_ending event. A command that
// cannot be taken as things stand is answered with a command_refused
// warning, and the session goes on.
//
// A stdin that ends is the end of the session: a turn under way is stopped,
// and Serve returns ExitAnswered.
func Serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("harness serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "the harness configuration file")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" || fs.NArg() != 0 {
		say(stderr, "usage: harness serve --config FILE  (the runner's commands on stdin)")
		return ExitUsage
	}
	h, code := build(*configPath, stdout, stderr, env)
	if h == nil {
		return code
	}
	defer func() { _ = h.Close() }()

	s := &server{h: h}
	h.Gate, h.Permit = s.askEnding, s.askPermission
	h.OnTurnEnd = func() { s.over.Store(true) }
	cmds := readCommands(stdin)

	// One turn at a time, on a goroutine of its own, so that the commands
	// that belong to it are read while it runs.
	var (
		turnDone chan struct{}
		stop     context.CancelCauseFunc
	)
	endTurn := func(cause error) {
		if turnDone != nil {
			stop(cause)
			<-turnDone
			turnDone, stop = nil, nil
		}
	}
	// idle reports whether no turn is under way. A turn whose turn_end is
	// written is over for the runner, which may send its next command at
	// once, so such a turn is waited out here: it has nothing left to do.
	idle := func() bool {
		if turnDone != nil && s.over.Load() {
			<-turnDone
			stop(nil)
			turnDone, stop = nil, nil
		}
		return turnDone == nil
	}
	for {
		select {
		case <-ctx.Done():
			endTurn(ctx.Err())
			return ExitFailed
		case <-turnDone:
			stop(nil)
			turnDone, stop = nil, nil
		case line, ok := <-cmds.lines:
			if !ok {
				endTurn(ErrStopped)
				return ExitAnswered
			}
			if line.err != nil {
				endTurn(ErrStopped)
				h.fail(proto.ClassHarness, line.err.Error())
				say(stderr, "harness:", line.err)
				return ExitFailed
			}
			cmd := line.cmd
			switch cmd.Type {
			case proto.CmdOpen:
				if !idle() {
					s.refuse(cmd, "a turn is under way")
					continue
				}
				// A conversation that cannot be opened is said so by an
				// error event, and the session stays where it was.
				if err := h.Start(ctx, cmd.Conversation); err == nil {
					s.open = true
				}
			case proto.CmdPrompt:
				switch {
				case !s.open:
					s.refuse(cmd, "no conversation is open")
					continue
				case !idle():
					s.refuse(cmd, "a turn is under way")
					continue
				case strings.TrimSpace(cmd.Text) == "":
					s.refuse(cmd, "the prompt is empty")
					continue
				}
				s.over.Store(false)
				turnCtx, cancel := context.WithCancelCause(ctx)
				stop = cancel
				done := make(chan struct{})
				turnDone = done
				go func() {
					defer close(done)
					// How the turn ended is in its turn_end event.
					_, _ = h.Prompt(turnCtx, cmd.Text)
				}()
			case proto.CmdCompact:
				switch {
				case !s.open:
					s.refuse(cmd, "no conversation is open")
					continue
				case !idle():
					s.refuse(cmd, "a turn is under way")
					continue
				}
				// A summary is a model request, which may take as long as
				// a turn: it has the turn's place, so an interrupt gives
				// it up and nothing else starts until it has answered.
				s.over.Store(false)
				workCtx, cancel := context.WithCancelCause(ctx)
				stop = cancel
				done := make(chan struct{})
				turnDone = done
				go func() {
					defer close(done)
					// How it went is in the warning it writes.
					_ = h.Compact(workCtx)
				}()
			case proto.CmdSteer:
				h.Steer(cmd.Text)
			case proto.CmdInterrupt:
				// A turn that has written its turn_end is past stopping,
				// and is not waited for here: an interrupt may come at
				// any moment, before the runner has read that event.
				if turnDone != nil && !s.over.Load() {
					stop(ErrStopped)
				}
			case proto.CmdAccept, proto.CmdContinue:
				text := ""
				if cmd.Type == proto.CmdContinue {
					text = cmd.Text
				}
				if !s.answerEnding(text) {
					s.refuse(cmd, "no turn is waiting to end")
				}
			case proto.CmdPermit, proto.CmdDeny:
				if !s.answerPermission(cmd.CallID, cmd.Type == proto.CmdPermit) {
					s.refuse(cmd, "no call of that id is waiting for permission")
				}
			default:
				s.refuse(cmd, "there is no such command")
			}
		}
	}
}

// server holds what a session's turn is waiting on the runner for.
type server struct {
	h *Harness
	// open says a conversation has been opened. Only the loop reads it.
	open bool
	// over says the turn last started has written its turn_end.
	over atomic.Bool

	mu sync.Mutex
	// ending receives the answer to the turn_ending event last written, nil
	// when none is waited for.
	ending chan string
	// permission receives the answer to the permission event last written,
	// for the call permissionFor names.
	permission    chan bool
	permissionFor string
}

// refuse tells the runner a command was not taken, and why.
func (s *server) refuse(cmd proto.Command, why string) {
	s.h.emit(proto.Event{
		Type: proto.TypeWarning, Code: proto.WarnCommandRefused, Reason: cmd.Type,
		Text: fmt.Sprintf("The command %q was not taken: %s.", cmd.Type, why),
	})
}

// askEnding is the harness's Gate: the turn waits for the runner's accept
// or continue.
func (s *server) askEnding() func(context.Context) (string, error) {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.ending = ch
	s.mu.Unlock()
	return func(ctx context.Context) (string, error) {
		select {
		case text := <-ch:
			return text, nil
		case <-ctx.Done():
			s.mu.Lock()
			if s.ending == ch {
				s.ending = nil
			}
			s.mu.Unlock()
			return "", ctx.Err()
		}
	}
}

// answerEnding hands the waiting turn the runner's answer. It reports
// whether a turn was waiting.
func (s *server) answerEnding(text string) bool {
	s.mu.Lock()
	ch := s.ending
	s.ending = nil
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- text
	return true
}

// askPermission is the harness's Permit: the call waits for the runner's
// permit or deny.
func (s *server) askPermission(callID string) func(context.Context) (bool, error) {
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.permission, s.permissionFor = ch, callID
	s.mu.Unlock()
	return func(ctx context.Context) (bool, error) {
		select {
		case ok := <-ch:
			return ok, nil
		case <-ctx.Done():
			s.mu.Lock()
			if s.permission == ch {
				s.permission, s.permissionFor = nil, ""
			}
			s.mu.Unlock()
			return false, ctx.Err()
		}
	}
}

// answerPermission hands the waiting call the runner's answer. It reports
// whether the call callID names was waiting.
func (s *server) answerPermission(callID string, permit bool) bool {
	s.mu.Lock()
	ch := s.permission
	if ch == nil || s.permissionFor != callID {
		s.mu.Unlock()
		return false
	}
	s.permission, s.permissionFor = nil, ""
	s.mu.Unlock()
	ch <- permit
	return true
}
