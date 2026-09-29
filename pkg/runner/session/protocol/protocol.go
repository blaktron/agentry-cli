// Package protocol is the session protocol (hosted sessions plan D4,
// agentry-cli#477, DR-51): an interactive session driven by a program
// rather than a person, over pipes and with no terminal. Commands arrive as
// JSON lines on one stream, the session's events leave as JSON lines on
// another, and the session itself is the unchanged session.Session every
// other surface drives — the TUI, the desktop, agentry-mcp — so the
// protocol can say nothing a chat could not.
//
// The commands, one JSON object per line, each exactly one of:
//
//	{"send":{"text":"…"}}       one user turn
//	{"add":{"path":"…"}}        a regular file the driver staged in Options.InputsDir
//	{"end":{"finish":true}}     ask for the final result, then end (the TUI's /finish)
//	{"end":{"finish":false}}    end without one (the TUI's /quit)
//
// End of input is an end without a result. Every event line is one
// session.Event exactly as the session emitted it, redacted as it reaches
// events.ndjson, with the harness's raw message left out. A command the
// protocol refuses is answered with an event of kind "error" carrying
// Refused, the class of the refusal; a refusal never ends the session and
// is not a session event, so the receipts do not record it.
package protocol

import (
	"bufio"
	"bytes"
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

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// Command is one line of input: exactly one of its members is set.
type Command struct {
	Send *Send `json:"send,omitempty"`
	Add  *Add  `json:"add,omitempty"`
	End  *End  `json:"end,omitempty"`
}

// Send is one user turn.
type Send struct {
	Text string `json:"text"`
}

// Add names a regular file, directly in Options.InputsDir, to copy into
// the run's read-only inputs. A relative path is taken in that directory.
type Add struct {
	Path string `json:"path"`
}

// End ends the session; Finish asks the agent for its final result first.
type End struct {
	Finish bool `json:"finish"`
}

// Refusal classifies a refused command, the value of Event.Refused.
type Refusal string

const (
	// RefusedMalformed: the line is not one JSON object naming exactly one
	// command with its fields, or it is longer than MaxLine.
	RefusedMalformed Refusal = "malformed"
	// RefusedUnknown: the line names a command the protocol does not have.
	RefusedUnknown Refusal = "unknown_command"
	// RefusedBusy: a send arrived while a turn was in flight.
	RefusedBusy Refusal = "busy"
	// RefusedInput: the named file is not a regular file directly in the
	// inputs directory, the session was given no inputs directory, or the
	// session would not take it (a name already staged, unreadable).
	RefusedInput Refusal = "input_refused"
)

// Event is one line of output: a session event, or a refusal.
type Event struct {
	session.Event
	// Refused is set on an error event the protocol wrote about a command
	// it refused; it is empty on every event the session emitted.
	Refused Refusal `json:"refused,omitempty"`
}

// Options configure Serve.
type Options struct {
	// OpeningTurn says the session was started with a task, which it sends
	// as its first user turn by itself: a send is refused until that turn
	// ends, as it would be for a turn the driver sent. Set it only when the
	// session does send one (a non-blank task), or no send is taken until
	// the session's first turn of any kind.
	OpeningTurn bool
	// InputsDir is where the driver stages the files it adds. An add must
	// name a regular file directly in it; with none, every add is refused.
	// The TUI's /add takes any path a person types; a path a program sends
	// is held to the directory that program said it would use.
	InputsDir string
	// OnUsage, when set, is called with the session's running usage totals
	// after every usage event — the autonomous run's engine.Params.OnUsage
	// contract, so a driver meters a session as it meters a run.
	OnUsage func(receipt.Usage)
	// OnDropped, when set, is called once if the events reader fell
	// MaxBehind or failed, after which it is written nothing more.
	OnDropped func()
}

const (
	// MaxLine bounds one command line. A longer line is refused, and the
	// protocol reads on from the next one.
	MaxLine = 1 << 20
	// MaxBehind is how far the events reader may fall behind before the
	// protocol stops writing to it. The session never waits for the reader:
	// its events channel feeds the pump that enforces the budgets, and a
	// session held up by a reader that stopped reading would never end.
	MaxBehind = 16 << 20
	// cancelGrace is how long a cancelled Serve waits for the session to
	// end itself as cancelled before ending it.
	cancelGrace = 5 * time.Second
	// flushGrace is how long Serve waits, after the session's end, for the
	// reader to take the last events; the teardown has run by then.
	flushGrace = 10 * time.Second
)

// Serve drives s from the commands on in and writes its events to events
// until the session is over, then ends it and returns End's envelope and
// error. It returns when an end command arrives, when in reaches its end,
// when the session ends by itself (a budget, the idle or wall cap, the
// harness exiting), or when ctx is done. Every event is written by the time
// it returns, unless the reader fell MaxBehind or stopped taking them;
// Options.OnDropped says so when that happens.
func Serve(ctx context.Context, s session.Session, in io.Reader, events io.Writer, opts Options) (*receipt.Envelope, error) {
	sv := &server{s: s, onUsage: opts.OnUsage, out: newOutbox(events, opts.OnDropped)}
	if opts.OpeningTurn {
		sv.seq = 1
		sv.pending = 1
	}
	inputsDir := ""
	if opts.InputsDir != "" {
		if abs, err := filepath.Abs(opts.InputsDir); err == nil {
			inputsDir = filepath.Clean(abs)
		}
	}
	ended := make(chan struct{})
	forwarded := make(chan struct{})
	go sv.forward(ended, forwarded)

	stop := make(chan struct{})
	lines := make(chan line)
	go readLines(in, lines, stop)

	var turns sync.WaitGroup
	finish := false
loop:
	for {
		select {
		case <-ctx.Done():
			// The session's own context is normally this one's child, and
			// it ends the session as cancelled; wait a moment for that, so
			// the End below does not record the cancellation as a quit.
			select {
			case <-ended:
			case <-time.After(cancelGrace):
			}
			break loop
		case <-ended:
			break loop
		case l, ok := <-lines:
			if !ok {
				break loop // end of input: an end without a result
			}
			c, refused, why := parse(l)
			if refused != "" {
				sv.refuse(refused, why)
				continue
			}
			switch {
			case c.End != nil:
				finish = c.End.Finish
				break loop
			case c.Send != nil:
				seq, ok := sv.take()
				if !ok {
					sv.refuse(RefusedBusy, "a turn is in flight; send again when its assistant_turn arrives")
					continue
				}
				turns.Add(1)
				go func(text string) {
					defer turns.Done()
					// A turn's failure is the session's to report: it emits
					// the error event itself, and a turn cut short by the
					// session's end is not a failure.
					_ = s.Send(context.Background(), text)
					sv.settle(seq)
				}(c.Send.Text)
			case c.Add != nil:
				path, err := staged(inputsDir, c.Add.Path)
				if err != nil {
					sv.refuse(RefusedInput, err.Error())
					continue
				}
				// Adding while idle opens a turn of its own (the notice to
				// the agent), which its events mark in flight; adding during
				// a turn only queues the notice. Either way the add runs
				// beside the command loop.
				turns.Add(1)
				go func() {
					defer turns.Done()
					if err := s.AddInput(context.Background(), path); err != nil && !errors.Is(err, session.ErrEnded) {
						sv.refuse(RefusedInput, err.Error())
					}
				}()
			}
		}
	}
	close(stop)
	env, err := s.End(context.WithoutCancel(ctx), finish)
	// End has closed the events channel; the forwarder has queued the last
	// of them once it returns, and every turn goroutine has returned
	// because the session is over.
	<-forwarded
	turns.Wait()
	sv.out.close(flushGrace)
	return env, err
}

// server is Serve's state shared with its goroutines.
type server struct {
	s       session.Session
	onUsage func(receipt.Usage)
	out     *outbox

	mu sync.Mutex
	// turnOpen is true from a user_turn event to the event that closes its
	// turn: the session's own record of a turn in flight, the opening task's
	// and an add's notice included.
	turnOpen bool
	// pending is the send taken and not yet seen as a user_turn (0: none);
	// seq numbers the sends.
	pending, seq uint64
	usage        receipt.Usage
}

// take claims the turn for a send; false when a turn is in flight or a
// send is on its way to becoming one.
func (sv *server) take() (uint64, bool) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.turnOpen || sv.pending != 0 {
		return 0, false
	}
	sv.seq++
	sv.pending = sv.seq
	return sv.seq, true
}

// settle drops a send's claim if its user_turn never came (the session
// refused the turn); a later send's claim is left alone.
func (sv *server) settle(seq uint64) {
	sv.mu.Lock()
	if sv.pending == seq {
		sv.pending = 0
	}
	sv.mu.Unlock()
}

// forward queues every session event until the session closes its stream,
// closing ended after the session_end event so Serve stops reading
// commands. It never waits for the reader.
func (sv *server) forward(ended, done chan<- struct{}) {
	defer close(done)
	closed := false
	for ev := range sv.s.Events() {
		sv.observe(ev)
		ev.Raw = nil
		sv.out.put(Event{Event: ev})
		if ev.Kind == session.KindSessionEnd && !closed {
			close(ended)
			closed = true
		}
	}
	if !closed {
		close(ended)
	}
}

// observe keeps the turn in flight and the usage totals current.
func (sv *server) observe(ev session.Event) {
	sv.mu.Lock()
	switch ev.Kind {
	case session.KindUserTurn:
		sv.turnOpen, sv.pending = true, 0
	case session.KindAssistantTurn, session.KindError, session.KindSessionEnd:
		// A turn ends in its assistant_turn, or in the error the session
		// emits when the prompt fails. A harness error in the middle of a
		// turn frees it early; a send then waits for the turn instead of
		// being refused, which is the only cost.
		sv.turnOpen = false
		if ev.Kind == session.KindSessionEnd {
			sv.pending = 0
		}
	case session.KindToolCall:
		sv.usage.Turns++
	case session.KindUsage:
		// The session's own counting (engine liveSession.handle), which the
		// envelope's usage is read from; TestProtocolNarratesUsage holds the
		// last narration to the envelope, so the two cannot drift silently.
		sv.usage.InputTokens += ev.InputTokens
		sv.usage.OutputTokens += ev.OutputTokens
		sv.usage.TotalTokens += ev.TotalTokens
		if ev.CostUSD > sv.usage.CostUSD {
			sv.usage.CostUSD = ev.CostUSD
		}
	}
	u, report := sv.usage, ev.Kind == session.KindUsage && sv.onUsage != nil
	sv.mu.Unlock()
	if report {
		sv.onUsage(u)
	}
}

// refuse writes a refusal as an error event.
func (sv *server) refuse(class Refusal, why string) {
	sv.out.put(Event{Event: session.Event{Kind: session.KindError, Time: time.Now().UTC(), Text: why}, Refused: class})
}

// maxBehind is MaxBehind, a variable so a test can fall behind cheaply.
var maxBehind = MaxBehind

// outbox writes event lines to the reader in order, from a goroutine of its
// own, so the session never waits for the reader. A reader MaxBehind behind
// gets nothing more; a reader that has gone (a broken pipe) gets nothing
// more either. The receipts have every event whatever the reader took.
type outbox struct {
	w         io.Writer
	onDropped func()
	mu        sync.Mutex
	cond      *sync.Cond
	q         [][]byte
	size      int
	// dropped: the reader fell behind or failed, and no more is written.
	dropped, closed bool
	done            chan struct{}
}

func newOutbox(w io.Writer, onDropped func()) *outbox {
	o := &outbox{w: w, onDropped: onDropped, done: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	go o.run()
	return o
}

// put queues one event.
func (o *outbox) put(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	b = append(b, '\n')
	o.mu.Lock()
	if o.dropped || o.closed {
		o.mu.Unlock()
		return
	}
	if o.size+len(b) > maxBehind {
		o.cond.Signal()
		o.mu.Unlock()
		o.drop()
		return
	}
	o.q = append(o.q, b)
	o.size += len(b)
	o.cond.Signal()
	o.mu.Unlock()
}

// drop gives up on the reader, once.
func (o *outbox) drop() {
	o.mu.Lock()
	first := !o.dropped
	o.dropped, o.q, o.size = true, nil, 0
	o.cond.Signal()
	o.mu.Unlock()
	if first && o.onDropped != nil {
		o.onDropped()
	}
}

// run writes what is queued until the outbox closes or the reader fails.
func (o *outbox) run() {
	defer close(o.done)
	for {
		o.mu.Lock()
		for len(o.q) == 0 && !o.closed && !o.dropped {
			o.cond.Wait()
		}
		if o.dropped || (o.closed && len(o.q) == 0) {
			o.mu.Unlock()
			return
		}
		batch := o.q
		o.q, o.size = nil, 0
		o.mu.Unlock()
		for _, b := range batch {
			if _, err := o.w.Write(b); err != nil {
				o.drop()
				return
			}
		}
	}
}

// close takes no more events and waits up to grace for the queued ones to
// be written; a reader still not reading by then is left behind.
func (o *outbox) close(grace time.Duration) {
	o.mu.Lock()
	o.closed = true
	o.cond.Signal()
	o.mu.Unlock()
	select {
	case <-o.done:
	case <-time.After(grace):
	}
}

// line is one line of input, or the note that it was too long.
type line struct {
	b       []byte
	tooLong bool
}

// readLines sends every line of in, bounded by MaxLine, until the end of
// in, a read error, or stop. A read already blocked on in when stop closes
// stays blocked until in yields; the caller owns in.
func readLines(in io.Reader, out chan<- line, stop <-chan struct{}) {
	defer close(out)
	send := func(l line) bool {
		select {
		case out <- l:
			return true
		case <-stop:
			return false
		}
	}
	r := bufio.NewReaderSize(in, 64*1024)
	for {
		var buf []byte
		tooLong := false
		for {
			frag, err := r.ReadSlice('\n')
			if !tooLong {
				if len(buf)+len(frag) > MaxLine+1 { // +1: the newline
					tooLong, buf = true, nil
				} else {
					buf = append(buf, frag...)
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if len(buf) > 0 || tooLong {
					send(line{b: buf, tooLong: tooLong})
				}
				return
			}
			break
		}
		if !send(line{b: buf, tooLong: tooLong}) {
			return
		}
	}
}

// parse reads one command line: the command, or the refusal and why.
func parse(l line) (Command, Refusal, string) {
	if l.tooLong {
		return Command{}, RefusedMalformed, fmt.Sprintf("a command line is at most %d bytes", MaxLine)
	}
	b := bytes.TrimSpace(l.b)
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil || members == nil {
		return Command{}, RefusedMalformed, "a command is one JSON object per line"
	}
	var names []string
	for k := range members {
		switch k {
		case "send", "add", "end":
			names = append(names, k)
		default:
			return Command{}, RefusedUnknown, fmt.Sprintf("unknown command %q (want send, add or end)", k)
		}
	}
	if len(names) != 1 {
		return Command{}, RefusedMalformed, "a line names exactly one command: send, add or end"
	}
	var c Command
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Command{}, RefusedMalformed, fmt.Sprintf("%s: %v", names[0], err)
	}
	switch {
	case c.Send != nil && strings.TrimSpace(c.Send.Text) == "":
		return Command{}, RefusedMalformed, "send needs a text"
	case c.Add != nil && c.Add.Path == "":
		return Command{}, RefusedMalformed, "add needs a path"
	case c.Send == nil && c.Add == nil && c.End == nil:
		return Command{}, RefusedMalformed, fmt.Sprintf("%s needs its object", names[0])
	}
	return c, "", ""
}

// staged is the file an add names, held to the inputs directory: directly
// in it (no subdirectory, no way out of it) and a regular file, not a link.
// The check is made here so a refusal never waits for a turn; the session
// checks the file again when it copies it.
func staged(inputsDir, path string) (string, error) {
	if inputsDir == "" {
		return "", errors.New("this session takes no files: it was started without an inputs directory")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(inputsDir, path)
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) != inputsDir {
		return "", fmt.Errorf("input %s is not directly in the inputs directory %s", path, inputsDir)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("input file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("input %s is not a regular file (directories and links are not accepted mid-run)", path)
	}
	return path, nil
}
