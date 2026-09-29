package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// These tests hold the protocol's own machinery — the reader it writes to,
// the command lines it parses — to its contract with a session that only
// emits. The protocol over the engine's real session, on every adapter, is
// pkg/runner/engine/protocol_test.go.

// chattySession answers each send with n assistant_text events and a turn.
type chattySession struct {
	n      int
	events chan session.Event
	mu     sync.Mutex
	ended  bool
}

func newChatty(n int) *chattySession {
	return &chattySession{n: n, events: make(chan session.Event, 16)}
}

func (c *chattySession) Events() <-chan session.Event { return c.events }
func (c *chattySession) Send(_ context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return session.ErrEnded
	}
	c.events <- session.Event{Kind: session.KindUserTurn, Text: text}
	for i := 0; i < c.n; i++ {
		c.events <- session.Event{Kind: session.KindAssistantText, Text: strings.Repeat("x", 512)}
	}
	c.events <- session.Event{Kind: session.KindAssistantTurn, Text: "done"}
	return nil
}
func (c *chattySession) AddInput(context.Context, string) error { return nil }
func (c *chattySession) Ended() (bool, session.EndReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended, ""
}
func (c *chattySession) End(context.Context, bool) (*receipt.Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ended {
		c.ended = true
		c.events <- session.Event{Kind: session.KindSessionEnd, Reason: session.EndQuit}
		close(c.events)
	}
	return &receipt.Envelope{Status: "ok"}, nil
}

// brokenPipe fails every write, as a reader that closed its end does.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestAReaderThatGoesAwayIsDroppedOnce: the first failed write gives up on
// the reader, says so once, and the session carries on to its end.
func TestAReaderThatGoesAwayIsDroppedOnce(t *testing.T) {
	s := newChatty(3)
	var drops atomic.Int32
	in := strings.NewReader(`{"send":{"text":"one"}}` + "\n" + `{"send":{"text":"two"}}` + "\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := Serve(context.Background(), s, in, brokenPipe{}, Options{OnDropped: func() { drops.Add(1) }})
		if err != nil || env == nil {
			t.Errorf("Serve = %v, %v", env, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Serve never returned after its reader went away")
	}
	if got := drops.Load(); got != 1 {
		t.Errorf("OnDropped ran %d times, want once", got)
	}
	if ended, _ := s.Ended(); !ended {
		t.Error("the session was not ended")
	}
}

// TestAReaderTooFarBehindIsDropped: a reader that stops reading gets the
// events up to MaxBehind queued and nothing after; Serve still returns,
// bounded by the flush grace, and the drop is said once.
func TestAReaderTooFarBehindIsDropped(t *testing.T) {
	old := maxBehind
	maxBehind = 4 << 10
	t.Cleanup(func() { maxBehind = old })

	s := newChatty(64) // 64 × 512 bytes of text: past 4 KiB
	var drops atomic.Int32
	stuckR, stuckW := io.Pipe()
	defer func() { _ = stuckR.Close() }()
	inR, inW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Serve(context.Background(), s, inR, stuckW, Options{OnDropped: func() { drops.Add(1) }})
	}()
	_, _ = io.WriteString(inW, `{"send":{"text":"talk a lot"}}`+"\n")
	deadline := time.Now().Add(10 * time.Second)
	for drops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if drops.Load() != 1 {
		t.Fatalf("the reader 4 KiB behind was not dropped (drops=%d)", drops.Load())
	}
	_ = inW.Close() // end of input: the session ends
	select {
	case <-done:
	case <-time.After(flushGrace + 10*time.Second):
		t.Fatal("Serve waited on a reader that stopped reading")
	}
}

// TestCommandLines: what parse accepts and refuses, class by class.
func TestCommandLines(t *testing.T) {
	cases := []struct {
		line string
		want Refusal
	}{
		{`{"send":{"text":"hi"}}`, ""},
		{`  {"end":{"finish":true}}  `, ""},
		{`{"end":{}}`, ""},
		{`{"add":{"path":"a.txt"}}`, ""},
		{`nope`, RefusedMalformed},
		{`null`, RefusedMalformed},
		{`[]`, RefusedMalformed},
		{`{}`, RefusedMalformed},
		{`{"stop":{}}`, RefusedUnknown},
		{`{"send":{"text":"a"},"stop":{}}`, RefusedUnknown},
		{`{"send":{"text":"a"},"add":{"path":"b"}}`, RefusedMalformed},
		{`{"send":{"text":""}}`, RefusedMalformed},
		{`{"send":{}}`, RefusedMalformed},
		{`{"send":"hi"}`, RefusedMalformed},
		{`{"send":{"text":"a","role":"system"}}`, RefusedMalformed},
		{`{"add":{}}`, RefusedMalformed},
		{`{"end":null}`, RefusedMalformed},
	}
	for _, tc := range cases {
		_, got, why := parse(line{b: []byte(tc.line)})
		if got != tc.want {
			t.Errorf("%s: refused %q (%s), want %q", tc.line, got, why, tc.want)
		}
		if got != "" && why == "" {
			t.Errorf("%s: refused without a reason", tc.line)
		}
	}
	if _, got, _ := parse(line{tooLong: true}); got != RefusedMalformed {
		t.Errorf("an overlong line: %q", got)
	}
}

// TestReadLinesBoundsALineAndReadsOn: a line past MaxLine is one refusal,
// and the next line is read whole.
func TestReadLinesBoundsALineAndReadsOn(t *testing.T) {
	long := strings.Repeat("x", MaxLine+10)
	in := strings.NewReader(long + "\n" + `{"end":{"finish":false}}` + "\n" + "last, unterminated")
	out := make(chan line)
	go readLines(in, out, make(chan struct{}))
	var got []line
	for l := range out {
		got = append(got, l)
	}
	if len(got) != 3 || !got[0].tooLong || string(got[1].b) != `{"end":{"finish":false}}`+"\n" || string(got[2].b) != "last, unterminated" {
		t.Errorf("lines = %d: %v", len(got), summarize(got))
	}
}

func summarize(ls []line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		if l.tooLong {
			out[i] = "<too long>"
		} else {
			out[i] = string(l.b)
		}
	}
	return out
}

// TestAnEventLineIsTheSessionEventWithoutRaw: the wire shape a host parses.
func TestAnEventLineIsTheSessionEventWithoutRaw(t *testing.T) {
	r, w := io.Pipe()
	o := newOutbox(w, nil)
	o.put(Event{Event: session.Event{Kind: session.KindToolCall, Text: "read_input"}})
	o.put(Event{Event: session.Event{Kind: session.KindError, Text: "no"}, Refused: RefusedBusy})
	sc := bufio.NewScanner(r)
	var lines []map[string]any
	for i := 0; i < 2 && sc.Scan(); i++ {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	o.close(time.Second)
	if len(lines) != 2 || lines[0]["kind"] != "tool_call" || lines[0]["refused"] != nil || lines[1]["refused"] != "busy" {
		t.Errorf("lines = %v", lines)
	}
}
