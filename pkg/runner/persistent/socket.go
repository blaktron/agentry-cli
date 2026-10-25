package persistent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// The owner socket speaks JSON lines. A command is one object naming
// exactly one of:
//
//	{"attach":{}}             every event of the segment from now on, as {"event":…}
//	{"send":{"text":"…"}}     one turn; refused as busy while another is in flight
//	{"status":{}}             answered with {"status":…}
//	{"roster":{}}             answered with {"roster":[…]}
//	{"change":{…}}            a Change; answered with {"roster":[…]}, or
//	                          refused with Consent naming what it turns on
//	{"stop":{}}               ends the segment; answered with {"stopped":…}
//	{"tasks":{}}              answered with {"tasks":[…]}, each with its next fire
//	{"task_change":{…}}       a TaskOp (pause, resume, remove); answered with
//	                          {"tasks":[…]}, or refused
//	{"compact":{}}            memory upkeep now, then a new conversation that
//	                          opens with the handoff; answered with
//	                          {"compacted":…} when it is done, or refused
//
// A refused command is answered with an event of kind error carrying
// Refused, as the session protocol answers one.

// Command is one line a client sends.
type Command struct {
	Attach *struct{}      `json:"attach,omitempty"`
	Send   *protocol.Send `json:"send,omitempty"`
	Status *struct{}      `json:"status,omitempty"`
	Roster *struct{}      `json:"roster,omitempty"`
	Change *Change        `json:"change,omitempty"`
	Stop   *struct{}      `json:"stop,omitempty"`
	Tasks  *struct{}      `json:"tasks,omitempty"`
	// TaskChange is a person's change to one of the agent's tasks.
	TaskChange *TaskOp `json:"task_change,omitempty"`
	// Compact asks for memory upkeep and a compaction now.
	Compact *struct{} `json:"compact,omitempty"`
}

// Reply is one line the supervisor sends.
type Reply struct {
	Event   *protocol.Event `json:"event,omitempty"`
	Status  *Status         `json:"status,omitempty"`
	Roster  Roster          `json:"roster,omitempty"`
	Stopped *Stopped        `json:"stopped,omitempty"`
	Tasks   *[]Task         `json:"tasks,omitempty"`
	// Compacted answers a compact: what the compaction did.
	Compacted *Compacted `json:"compacted,omitempty"`
	// Consent lists what a refused change would turn on, for the person to
	// agree to before it is sent again with Consent set.
	Consent []RosterEntry `json:"consent,omitempty"`
}

// Stopped is the answer to a stop: where the segment's envelope is and how
// it ended.
type Stopped struct {
	Status     string `json:"status"`
	ResultFile string `json:"resultFile,omitempty"`
	Error      string `json:"error,omitempty"`
}

// The refusal classes the owner socket adds to the session protocol's.
const (
	RefusedBusy                     = protocol.RefusedBusy
	RefusedEnded   protocol.Refusal = "ended"
	RefusedChange  protocol.Refusal = "change"
	RefusedConsent protocol.Refusal = "consent"
	RefusedTask    protocol.Refusal = "task"
	RefusedCompact protocol.Refusal = "compact"
)

// maxSocketPath is below the kernel's limit for a socket's name (108 bytes
// on Linux, 104 on macOS) with room for the terminator.
const maxSocketPath = 103

// listen serves the owner socket. The lock is held, so a socket file there
// is a dead supervisor's and is replaced.
func (s *Supervisor) listen() error {
	path := SocketPath(s.opts.Home, s.opts.Agent.Name)
	if len(path) > maxSocketPath {
		return fmt.Errorf("the %s agent's socket path %s is longer than a socket's name may be; use a shorter runner home", s.opts.Agent.Name, path)
	}
	// The folder is the runner's alone before the socket is made in it:
	// MkdirAll leaves a folder that exists as it is.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", s.opts.Agent.Name, err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", s.opts.Agent.Name, err)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("the %s agent's socket: %w", s.opts.Agent.Name, err)
	}
	_ = os.Chmod(path, 0o600)
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveConn(c)
		}
	}()
	return nil
}

// conn is one client connection: replies are written one line at a time.
type conn struct {
	c  net.Conn
	mu sync.Mutex
}

func (c *conn) reply(r Reply) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = c.c.Write(append(b, '\n'))
}

func (c *conn) refuse(class protocol.Refusal, why string) {
	c.reply(Reply{Event: &protocol.Event{Event: session.Event{Kind: session.KindError, Time: time.Now().UTC(), Text: why}, Refused: class}})
}

// serveConn answers one client until it goes.
func (s *Supervisor) serveConn(nc net.Conn) {
	c := &conn{c: nc}
	defer func() { _ = nc.Close() }()
	var detach func()
	defer func() {
		if detach != nil {
			detach()
		}
	}()
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxLine+1)
	for sc.Scan() {
		var cmd Command
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cmd); err != nil || commandsSet(cmd) != 1 {
			c.refuse(protocol.RefusedMalformed, "a command is one JSON object naming exactly one of attach, send, status, roster, change, stop, tasks, task_change or compact")
			continue
		}
		switch {
		case cmd.Attach != nil:
			if detach != nil {
				continue
			}
			events, cancel := s.Subscribe()
			detach = cancel
			go func() {
				for ev := range events {
					ev.Raw = nil
					c.reply(Reply{Event: &protocol.Event{Event: ev}})
				}
			}()
		case cmd.Send != nil:
			if strings.TrimSpace(cmd.Send.Text) == "" {
				c.refuse(protocol.RefusedMalformed, "send needs a text")
				continue
			}
			text := cmd.Send.Text
			go func() {
				switch err := s.Send(context.Background(), text); {
				case errors.Is(err, ErrBusy):
					c.refuse(RefusedBusy, err.Error())
				case errors.Is(err, session.ErrEnded):
					c.refuse(RefusedEnded, "the agent's segment has ended")
				}
			}()
		case cmd.Status != nil:
			st := s.Status()
			c.reply(Reply{Status: &st})
		case cmd.Roster != nil:
			c.reply(Reply{Roster: s.Roster()})
		case cmd.Change != nil:
			r, err := s.Change(*cmd.Change)
			var ce *ConsentError
			switch {
			case errors.As(err, &ce):
				c.reply(Reply{Consent: ce.Entries, Event: &protocol.Event{Event: session.Event{Kind: session.KindError, Time: time.Now().UTC(), Text: err.Error()}, Refused: RefusedConsent}})
			case err != nil:
				c.refuse(RefusedChange, err.Error())
			default:
				c.reply(Reply{Roster: r})
			}
		case cmd.Tasks != nil:
			ts, err := s.Tasks()
			if err != nil {
				c.refuse(RefusedTask, err.Error())
				continue
			}
			c.reply(Reply{Tasks: taskList(ts)})
		case cmd.Compact != nil:
			// An upkeep turn takes minutes: the answer comes when it is
			// done, and the connection reads on meanwhile. The compaction
			// is the agent's, not the connection's, so a client that goes
			// away does not cut it short.
			go func() {
				out, err := s.Compact(context.Background())
				switch {
				case errors.Is(err, ErrBusy):
					c.refuse(RefusedBusy, "a turn is in flight; compact once it ends")
				case errors.Is(err, ErrNoMemory):
					c.refuse(RefusedCompact, err.Error())
				default:
					c.reply(Reply{Compacted: &out})
				}
			}()
		case cmd.TaskChange != nil:
			ts, err := s.ChangeTask(*cmd.TaskChange)
			if err != nil {
				c.refuse(RefusedTask, err.Error())
				continue
			}
			c.reply(Reply{Tasks: taskList(ts)})
		case cmd.Stop != nil:
			env, err := s.Stop(context.Background())
			c.reply(Reply{Stopped: stoppedOf(env, err)})
			return
		}
	}
}

// commandsSet is how many commands one line names.
func commandsSet(c Command) int {
	n := 0
	for _, set := range []bool{c.Attach != nil, c.Send != nil, c.Status != nil, c.Roster != nil, c.Change != nil, c.Stop != nil, c.Tasks != nil, c.TaskChange != nil, c.Compact != nil} {
		if set {
			n++
		}
	}
	return n
}

func stoppedOf(env *receipt.Envelope, err error) *Stopped {
	st := &Stopped{Status: "error"}
	if env != nil {
		st.Status, st.ResultFile = env.Status, env.ResultFile
		if st.ResultFile == "" && env.OutDir != "" {
			st.ResultFile = filepath.Join(env.OutDir, "result.json")
		}
	}
	if err != nil {
		st.Error = err.Error()
	}
	return st
}

// ErrNotRunning says no supervisor answers on the agent's socket.
var ErrNotRunning = errors.New("not running")

// dial connects to the agent's owner socket.
func dial(ctx context.Context, home, name string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath(home, name))
	if err != nil {
		return nil, fmt.Errorf("the %s agent is %w", name, ErrNotRunning)
	}
	return c, nil
}

// request sends one command and reads replies until want takes one. A
// refused change that needs the person's consent is a ConsentError.
func request(ctx context.Context, home, name string, cmd Command, want func(Reply) bool) error {
	c, err := dial(ctx, home, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	b, _ := json.Marshal(cmd)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", name, err)
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var r Reply
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.Event != nil && r.Event.Refused == RefusedConsent {
			return &ConsentError{Agent: name, Entries: r.Consent}
		}
		if r.Event != nil && r.Event.Refused != "" {
			return errors.New(r.Event.Text)
		}
		if want(r) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("the %s agent's socket: %w", name, err)
	}
	return fmt.Errorf("the %s agent's supervisor closed the socket without answering", name)
}

// QueryStatus asks the running agent's supervisor for its status.
func QueryStatus(ctx context.Context, home, name string) (Status, error) {
	var st Status
	err := request(ctx, home, name, Command{Status: &struct{}{}}, func(r Reply) bool {
		if r.Status != nil {
			st = *r.Status
			return true
		}
		return false
	})
	return st, err
}

// QueryRoster asks the running agent's supervisor for its roster.
func QueryRoster(ctx context.Context, home, name string) (Roster, error) {
	var r Roster
	err := request(ctx, home, name, Command{Roster: &struct{}{}}, func(rep Reply) bool {
		if rep.Roster != nil {
			r = rep.Roster
			return true
		}
		return false
	})
	return r, err
}

// RequestChange sends a change to the running agent's supervisor, which
// writes it, and answers the roster after it.
func RequestChange(ctx context.Context, home, name string, ch Change) (Roster, error) {
	var r Roster
	err := request(ctx, home, name, Command{Change: &ch}, func(rep Reply) bool {
		if rep.Roster != nil {
			r = rep.Roster
			return true
		}
		return false
	})
	return r, err
}

// taskList is a reply's tasks, never nil, so an agent with none is
// answered with an empty list.
func taskList(ts []Task) *[]Task {
	if ts == nil {
		ts = []Task{}
	}
	return &ts
}

// RequestTasks asks the running agent's supervisor for its tasks.
func RequestTasks(ctx context.Context, home, name string) ([]Task, error) {
	return requestTasks(ctx, home, name, Command{Tasks: &struct{}{}})
}

// RequestTaskChange asks the running agent's supervisor to pause, resume or
// remove one of its tasks.
func RequestTaskChange(ctx context.Context, home, name string, op TaskOp) ([]Task, error) {
	return requestTasks(ctx, home, name, Command{TaskChange: &op})
}

func requestTasks(ctx context.Context, home, name string, cmd Command) ([]Task, error) {
	var ts []Task
	err := request(ctx, home, name, cmd, func(r Reply) bool {
		if r.Tasks != nil {
			ts = *r.Tasks
			return true
		}
		return false
	})
	return ts, err
}

// RequestCompact asks the running agent's supervisor for memory upkeep and a
// compaction now, and returns once it is done: an upkeep turn takes minutes,
// so ctx should allow for it.
func RequestCompact(ctx context.Context, home, name string) (Compacted, error) {
	var out Compacted
	err := request(ctx, home, name, Command{Compact: &struct{}{}}, func(r Reply) bool {
		if r.Compacted != nil {
			out = *r.Compacted
			return true
		}
		return false
	})
	return out, err
}

// RequestStop asks the running agent's supervisor to stop it, and returns
// once the segment's envelope is written.
func RequestStop(ctx context.Context, home, name string) (Stopped, error) {
	var st Stopped
	err := request(ctx, home, name, Command{Stop: &struct{}{}}, func(r Reply) bool {
		if r.Stopped != nil {
			st = *r.Stopped
			return true
		}
		return false
	})
	return st, err
}

// Attach connects a chat to the running agent: a session.Session whose
// events are the segment's from now on, after history, and whose End leaves
// the agent running. A file is never added this way.
func Attach(ctx context.Context, home, name string, history []session.Event) (session.Session, error) {
	c, err := dial(ctx, home, name)
	if err != nil {
		return nil, err
	}
	r := &remote{c: c, events: make(chan session.Event, 1024+len(history)), turnDone: make(chan error, 1)}
	for _, ev := range history {
		r.events <- ev
	}
	if err := r.write(Command{Attach: &struct{}{}}); err != nil {
		_ = c.Close()
		return nil, err
	}
	go r.read()
	return r, nil
}

// remote is a chat's view of a running agent over its owner socket.
type remote struct {
	c       net.Conn
	writeMu sync.Mutex
	events  chan session.Event

	mu       sync.Mutex
	ended    bool
	reason   session.EndReason
	inTurn   bool
	turnDone chan error
	// turnText is the turn in flight; turnSeen says its user_turn has come
	// back (ending in the text: notices the runner adds go before it), so
	// the next answer is its own and not the end of a turn another chat had
	// in flight when this one sent.
	turnText string
	turnSeen bool
}

func (r *remote) write(cmd Command) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = r.c.Write(append(b, '\n'))
	return err
}

// read takes the supervisor's lines until the connection ends.
func (r *remote) read() {
	defer close(r.events)
	defer r.finishTurn(session.ErrEnded)
	sc := bufio.NewScanner(r.c)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var rep Reply
		if json.Unmarshal(sc.Bytes(), &rep) != nil || rep.Event == nil {
			continue
		}
		ev := rep.Event.Event
		if rep.Event.Refused == RefusedBusy {
			// Another chat's turn is in flight: the send is answered busy,
			// which the chat says in its own words.
			r.finishTurn(ErrBusy)
			continue
		}
		if rep.Event.Refused != "" {
			r.finishTurn(errors.New(ev.Text))
			r.events <- ev
			continue
		}
		r.events <- ev
		switch ev.Kind {
		case session.KindUserTurn:
			r.mu.Lock()
			if r.inTurn && !ev.Synthetic && strings.HasSuffix(ev.Text, r.turnText) {
				r.turnSeen = true
			}
			r.mu.Unlock()
		case session.KindAssistantTurn:
			r.mu.Lock()
			own := r.turnSeen
			r.mu.Unlock()
			if own {
				r.finishTurn(nil)
			}
		case session.KindError:
			r.finishTurn(errors.New(ev.Text))
		case session.KindSessionEnd:
			r.mu.Lock()
			r.ended, r.reason = true, ev.Reason
			r.mu.Unlock()
			r.finishTurn(session.ErrEnded)
			return
		}
	}
	r.mu.Lock()
	if !r.ended {
		r.ended, r.reason = true, session.EndCancelled
	}
	r.mu.Unlock()
}

// finishTurn hands a turn in flight its outcome.
func (r *remote) finishTurn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.inTurn {
		return
	}
	r.inTurn = false
	r.turnDone <- err
}

// Events implements session.Session.
func (r *remote) Events() <-chan session.Event { return r.events }

// Send implements session.Session: the turn is the supervisor's, and Send
// returns when its answer arrives.
func (r *remote) Send(ctx context.Context, text string) error {
	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return session.ErrEnded
	}
	if r.inTurn {
		r.mu.Unlock()
		return ErrBusy
	}
	r.inTurn, r.turnText, r.turnSeen = true, text, false
	r.mu.Unlock()
	if err := r.write(Command{Send: &protocol.Send{Text: text}}); err != nil {
		r.finishTurn(err)
		return <-r.turnDone
	}
	select {
	case err := <-r.turnDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AddInput implements session.Session: a persistent agent takes no files
// through its chat.
func (r *remote) AddInput(context.Context, string) error {
	return errors.New("a persistent agent takes no files through its chat")
}

// End implements session.Session: the chat goes and the agent keeps running.
// It returns no envelope, since the segment has not ended.
func (r *remote) End(context.Context, bool) (*receipt.Envelope, error) {
	_ = r.c.Close()
	return nil, nil
}

// Ended implements session.Session.
func (r *remote) Ended() (bool, session.EndReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ended, r.reason
}

// HistoryEvents is the transcript as session events a chat shows before the
// live ones: each turn, tool call and error, and a line where each segment
// began.
func HistoryEvents(entries []Entry) []session.Event {
	var out []session.Event
	for _, e := range entries {
		switch e.Kind {
		case session.KindUserTurn, session.KindAssistantTurn, session.KindToolCall, session.KindError, session.KindNotice:
			out = append(out, e.Event)
		}
	}
	return out
}
