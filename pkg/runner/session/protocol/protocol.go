// Package protocol is the session protocol (hosted sessions plan D4,
// agentry-cli#477): an interactive session driven by a program rather than a
// person, over pipes and with no terminal. Commands arrive as JSON lines on
// one stream, the session's events leave as JSON lines on another, and the
// session itself is the unchanged session.Session every other surface
// drives — the TUI, the desktop, agentry-mcp — so the protocol can say
// nothing a chat could not.
//
// The commands, one JSON object per line, each exactly one of:
//
//	{"send":{"text":"…"}}       one user turn
//	{"add":{"path":"…"}}        a regular host file the driver already staged
//	{"end":{"finish":true}}     ask for the final result, then end (the TUI's /finish)
//	{"end":{"finish":false}}    end without one (the TUI's /quit)
//
// End of input is an end without a result. Every event line is one
// session.Event exactly as the session emitted it, redacted as it reaches
// events.ndjson, with the harness's raw message left out. A command the
// protocol refuses is answered with an event of kind "error" carrying
// Refused, the class of the refusal; a refusal never ends the session.
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

// Add names a regular host file to copy into the run's read-only inputs.
type Add struct {
	Path string `json:"path"`
}

// End ends the session; Finish asks the agent for its final result first.
type End struct {
	Finish bool `json:"finish"`
}

// Refusal classes, the value of Event.Refused.
const (
	// RefusedMalformed: the line is not one JSON object naming exactly one
	// command with its fields, or it is longer than MaxLine.
	RefusedMalformed = "malformed"
	// RefusedUnknown: the line names a command the protocol does not have.
	RefusedUnknown = "unknown_command"
	// RefusedBusy: a send arrived while a turn was in flight.
	RefusedBusy = "busy"
	// RefusedInput: the session would not take the named file (not a
	// regular file, a name already staged, unreadable).
	RefusedInput = "input_refused"
)

// MaxLine bounds one command line. A longer line is refused, and the
// protocol reads on from the next one.
const MaxLine = 1 << 20

// Event is one line of output: a session event, or a refusal.
type Event struct {
	session.Event
	// Refused is set on an error event the protocol wrote about a command
	// it refused; it is empty on every event the session emitted.
	Refused string `json:"refused,omitempty"`
}

// Options are what Serve reports besides the events.
type Options struct {
	// OpeningTurn says the session was started with a task, which it sends
	// as its first user turn by itself: a send is refused until that turn
	// ends, as it would be for a turn the driver sent.
	OpeningTurn bool
	// OnUsage, when set, is called with the session's running usage totals
	// after every usage event — the autonomous run's engine.Params.OnUsage
	// contract, so a driver meters a session as it meters a run.
	OnUsage func(receipt.Usage)
}

// Serve drives s from the commands on in and writes its events to events
// until the session is over, then ends it and returns End's envelope and
// error. It returns when an end command arrives, when in reaches its end,
// when the session ends by itself (a budget, the idle or wall cap, the
// harness exiting), or when ctx is done. Every event the session emitted
// has been written by the time it returns.
//
// Serve never reads in again after it returns, but a read already blocked
// on in stays blocked until in closes: the caller owns in.
func Serve(ctx context.Context, s session.Session, in io.Reader, events io.Writer, opts Options) (*receipt.Envelope, error) {
	sv := &server{s: s, w: events, onUsage: opts.OnUsage}
	if opts.OpeningTurn {
		sv.opening = true
		sv.inflight = 1
	}
	ended := make(chan struct{})
	forwarded := make(chan struct{})
	go sv.forward(ended, forwarded)

	lines := make(chan line)
	go readLines(in, lines)

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
			case <-time.After(5 * time.Second):
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
				if !sv.take() {
					sv.refuse(RefusedBusy, "a turn is in flight; send again when its assistant_turn arrives")
					continue
				}
				turns.Add(1)
				go func(text string) {
					defer turns.Done()
					defer sv.release()
					// A turn's failure is the session's to report: it emits
					// the error event itself, and a turn cut short by the
					// session's end is not a failure.
					_ = s.Send(context.Background(), text)
				}(c.Send.Text)
			case c.Add != nil:
				if err := statRegular(c.Add.Path); err != nil {
					sv.refuse(RefusedInput, err.Error())
					continue
				}
				// Adding while idle opens a turn of its own (the notice to
				// the agent), so it holds the turn slot until that turn
				// ends; adding during a turn only queues the notice.
				sv.hold()
				turns.Add(1)
				go func(path string) {
					defer turns.Done()
					defer sv.release()
					if err := s.AddInput(context.Background(), path); err != nil && !errors.Is(err, session.ErrEnded) {
						sv.refuse(RefusedInput, err.Error())
					}
				}(c.Add.Path)
			}
		}
	}
	env, err := s.End(context.WithoutCancel(ctx), finish)
	// End has closed the events channel; the forwarder has written the
	// last of them once it returns, and every turn goroutine has returned
	// because the session is over.
	<-forwarded
	turns.Wait()
	return env, err
}

// server is Serve's state shared with its goroutines.
type server struct {
	s       session.Session
	onUsage func(receipt.Usage)

	wmu sync.Mutex // one line at a time on w
	w   io.Writer

	mu       sync.Mutex
	inflight int  // turns (or adds that may open one) in flight
	opening  bool // the opening task's turn holds one of them
	usage    receipt.Usage
}

// take claims the turn slot for a send; false when a turn is in flight.
func (sv *server) take() bool {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.inflight > 0 {
		return false
	}
	sv.inflight++
	return true
}

// hold claims a share of the turn slot whatever else holds it.
func (sv *server) hold() {
	sv.mu.Lock()
	sv.inflight++
	sv.mu.Unlock()
}

// release gives back a claim from take or hold.
func (sv *server) release() {
	sv.mu.Lock()
	sv.inflight--
	sv.mu.Unlock()
}

// forward writes every session event until the session closes its stream,
// closing ended at the session_end event so Serve stops reading commands.
func (sv *server) forward(ended, done chan<- struct{}) {
	defer close(done)
	closed := false
	for ev := range sv.s.Events() {
		sv.observe(ev)
		ev.Raw = nil
		sv.write(Event{Event: ev})
		if ev.Kind == session.KindSessionEnd && !closed {
			close(ended)
			closed = true
		}
	}
	if !closed {
		close(ended)
	}
}

// observe keeps the opening turn's claim and the usage totals current.
func (sv *server) observe(ev session.Event) {
	sv.mu.Lock()
	switch ev.Kind {
	case session.KindAssistantTurn, session.KindError, session.KindSessionEnd:
		// The opening turn ends in its assistant_turn, or in the error the
		// session emits when the prompt fails. A harness error in the
		// middle of that turn frees the slot early; a send then waits for
		// the turn instead of being refused, which is the only cost.
		if sv.opening {
			sv.opening = false
			sv.inflight--
		}
	case session.KindToolCall:
		sv.usage.Turns++
	case session.KindUsage:
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
func (sv *server) refuse(class, why string) {
	sv.write(Event{Event: session.Event{Kind: session.KindError, Time: timeNow(), Text: why}, Refused: class})
}

// write puts one event on its own line. A driver that stopped reading does
// not stop the session: the error is dropped, and the receipts still have
// every event.
func (sv *server) write(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	sv.wmu.Lock()
	defer sv.wmu.Unlock()
	_, _ = sv.w.Write(append(b, '\n'))
}

// timeNow stamps a refusal as the session stamps its own events.
func timeNow() time.Time { return time.Now().UTC() }

// line is one line of input, or the note that it was too long.
type line struct {
	b       []byte
	tooLong bool
}

// readLines sends every line of in, bounded by MaxLine, and closes out at
// the end of in or on a read error.
func readLines(in io.Reader, out chan<- line) {
	defer close(out)
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
					out <- line{b: buf, tooLong: tooLong}
				}
				return
			}
			break
		}
		out <- line{b: buf, tooLong: tooLong}
	}
}

// parse reads one command line: the command, or the refusal class and why.
func parse(l line) (Command, string, string) {
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

// statRegular refuses anything but a regular file before the session is
// asked, so a refusal never waits for a turn. The session checks again
// when it copies the file.
func statRegular(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("input file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("input %s is not a regular file (directories and links are not accepted mid-run)", path)
	}
	return nil
}
