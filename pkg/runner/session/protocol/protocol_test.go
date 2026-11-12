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
	"slices"
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

	// A reader is dropped by the first event past the bound, not the last one
	// within it: three lines fit in exactly three lines' room. The drop is
	// said once, though the stuck write fails afterwards.
	ev := Event{Event: session.Event{Kind: session.KindError, Text: "x"}}
	b, _ := json.Marshal(ev)
	maxBehind = 3 * (len(b) + 1)
	var said atomic.Int32
	g := &gatedWriter{took: make(chan struct{}, 1), gate: make(chan struct{}), fail: true}
	o := newOutbox(g, func() { said.Add(1) })
	o.put(ev)
	<-g.took
	for i := 0; i < 3; i++ {
		o.put(ev)
	}
	if said.Load() != 0 {
		t.Fatal("a reader exactly at the bound was dropped")
	}
	o.put(ev)
	if said.Load() != 1 {
		t.Fatalf("the reader past the bound was dropped %d times", said.Load())
	}
	close(g.gate) // the write that was held fails: the drop is not said again
	<-o.done
	if said.Load() != 1 {
		t.Fatalf("the drop was said %d times", said.Load())
	}
}

// gatedWriter holds its writes until gate is closed, and says once that it
// has taken one; after that it writes to rec, or fails if fail is set.
type gatedWriter struct {
	took chan struct{}
	gate chan struct{}
	fail bool
	rec  recordingWriter
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	select {
	case g.took <- struct{}{}:
	default:
	}
	<-g.gate
	if g.fail {
		return 0, errors.New("the reader went away")
	}
	return g.rec.Write(p)
}

// recordingWriter keeps what it is given.
type recordingWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Write(p)
}

func (r *recordingWriter) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.b.Len() == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(r.b.String(), "\n"), "\n")
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
		{`{"answer":{"id":"q-1","yes":true}}`, ""},
		{`{"answer":{"id":"q-1"}}`, ""},
		{`{"answer":{"yes":true}}`, RefusedMalformed},
		{`{"answer":{"id":"q-1","yes":true,"all":true}}`, RefusedMalformed},
		{`{"send":{"text":"  \t"}}`, RefusedMalformed},
		{`{"answer":{"id":"  ","yes":true}}`, RefusedMalformed},
		{`{"answer":{"id":"q-1"},"end":{}}`, RefusedMalformed},
		{`{"steer":{"text":"and the date"}}`, ""},
		{`{"steer":{"text":" "}}`, RefusedMalformed},
		{`{"steer":{}}`, RefusedMalformed},
		{`{"steer":{"text":"a","turn":2}}`, RefusedMalformed},
		{`{"steer":{"text":"a"},"send":{"text":"b"}}`, RefusedMalformed},
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

	// What each refusal says.
	for text, why := range map[string]string{
		`null`:                           "a command is one JSON object per line",
		`nope`:                           "a command is one JSON object per line",
		`{}`:                             "a line names exactly one command: send, steer, add, end or answer",
		`{"stop":{}}`:                    `unknown command "stop" (want send, steer, add, end or answer)`,
		`{"send":{"text":""}}`:           "send needs a text",
		`{"send":{"text":"  \t"}}`:       "send needs a text",
		`{"answer":{"yes":true}}`:        "answer needs the question's id",
		`{"answer":{"id":" "}}`:          "answer needs the question's id",
		`{"add":{}}`:                     "add needs a path",
		`{"steer":{"text":""}}`:          "steer needs a text",
		`{"end":null}`:                   "end needs its object",
		`{"send":{"text":"a","x":1}}`:    `send: json: unknown field "x"`,
		`{"send":{"text":"a"},"end":{}}`: "a line names exactly one command: send, steer, add, end or answer",
	} {
		if _, _, got := parse(line{b: []byte(text)}); got != why {
			t.Errorf("%s: %q, want %q", text, got, why)
		}
	}
	if _, _, got := parse(line{tooLong: true}); got != "a command line is at most 1048576 bytes" {
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

	// A line of exactly MaxLine bytes is whole, one byte more is not, and an
	// overlong last line without its newline is one too.
	in = strings.NewReader(strings.Repeat("a", MaxLine) + "\n" + strings.Repeat("b", MaxLine+1) + "\n" + strings.Repeat("c", MaxLine+5))
	out = make(chan line)
	go readLines(in, out, make(chan struct{}))
	got = nil
	for l := range out {
		got = append(got, l)
	}
	if len(got) != 3 || got[0].tooLong || len(got[0].b) != MaxLine+1 || !got[1].tooLong || !got[2].tooLong {
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

// sharedSession is a session another driver holds a turn in: every send is
// answered busy.
type sharedSession struct{ *chattySession }

func (s sharedSession) Send(context.Context, string) error { return session.ErrBusy }

// TestASendTheSessionAnswersBusyIsRefusedBusy: a session other drivers
// share refuses a turn while one of theirs is in flight, and the protocol
// says so as its own busy refusal, so a driver tells it from a failed turn.
func TestASendTheSessionAnswersBusyIsRefusedBusy(t *testing.T) {
	s := sharedSession{newChatty(0)}
	r, w := io.Pipe()
	in := strings.NewReader(`{"send":{"text":"one"}}` + "\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Serve(context.Background(), s, in, w, Options{})
		_ = w.Close()
	}()
	var refused []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if c, ok := m["refused"].(string); ok {
			refused = append(refused, c)
			if m["text"] != session.ErrBusy.Error() {
				t.Errorf("busy text = %v", m["text"])
			}
		}
	}
	<-done
	if strings.Join(refused, " ") != "busy" {
		t.Errorf("refusals = %v, want one busy", refused)
	}
}

// refusingSession is a session shared through another process that
// refuses every send with a class of its own.
type refusingSession struct{ *chattySession }

func (s refusingSession) Send(context.Context, string) error {
	return &RefusedError{Class: "model", Text: "the saved model cannot be used"}
}

// TestASendTheSessionRefusesByClassIsRefusedByThatClass: a send the session
// refuses with a class of its own is refused by the protocol with that
// class and text, so a driver tells it from another chat's failed turn.
func TestASendTheSessionRefusesByClassIsRefusedByThatClass(t *testing.T) {
	s := refusingSession{newChatty(0)}
	r, w := io.Pipe()
	in := strings.NewReader(`{"send":{"text":"one"}}` + "\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Serve(context.Background(), s, in, w, Options{})
		_ = w.Close()
	}()
	var refused []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if c, ok := m["refused"].(string); ok {
			refused = append(refused, c+": "+m["text"].(string))
		}
	}
	<-done
	if strings.Join(refused, " | ") != "model: the saved model cannot be used" {
		t.Errorf("refusals = %v, want the one the session gave", refused)
	}
}

// scriptedSession is a session whose turns, files and end are what a test says.
type scriptedSession struct {
	events chan session.Event
	send   func(f *scriptedSession, text string) error

	mu     sync.Mutex
	closed bool
	sent   []string
	added  []string
	finish []bool // the argument of each End
	addErr error
	endErr error
}

func newScripted(send func(f *scriptedSession, text string) error) *scriptedSession {
	return &scriptedSession{events: make(chan session.Event, 64), send: send}
}

func (f *scriptedSession) Events() <-chan session.Event { return f.events }

func (f *scriptedSession) Send(_ context.Context, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	if f.send != nil {
		return f.send(f, text)
	}
	return nil
}

func (f *scriptedSession) AddInput(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, path)
	return f.addErr
}

func (f *scriptedSession) Ended() (bool, session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed, ""
}

func (f *scriptedSession) End(_ context.Context, finish bool) (*receipt.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finish = append(f.finish, finish)
	if !f.closed {
		f.closed = true
		f.events <- session.Event{Kind: session.KindSessionEnd, Reason: session.EndQuit}
		close(f.events)
	}
	return &receipt.Envelope{Status: "ok"}, f.endErr
}

func (f *scriptedSession) emit(ev session.Event) { f.events <- ev }

// turn is a send that completes: the person's turn and the agent's.
func turn(f *scriptedSession, text string) error {
	f.emit(session.Event{Kind: session.KindUserTurn, Text: text})
	f.emit(session.Event{Kind: session.KindAssistantTurn, Text: "done"})
	return nil
}

// answeringSession is a session that asks questions.
type answeringSession struct {
	*scriptedSession
	answers []string
	err     error
}

func (a *answeringSession) Answer(_ context.Context, id string, yes bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, fmt.Sprintf("%s:%v", id, yes))
	return a.err
}

// serveLines runs Serve over the commands in input, to their end, and returns
// the events it wrote.
func serveLines(t *testing.T, s session.Session, input string, opts Options) (events []map[string]any, env *receipt.Envelope, err error) {
	t.Helper()
	var out recordingWriter
	env, err = Serve(context.Background(), s, strings.NewReader(input), &out, opts)
	for _, l := range out.lines() {
		var m map[string]any
		if e := json.Unmarshal([]byte(l), &m); e != nil {
			t.Fatalf("event line %q: %v", l, e)
		}
		events = append(events, m)
	}
	return events, env, err
}

// refusals are the refusals among events, as "class: text".
func refusals(events []map[string]any) []string {
	var out []string
	for _, e := range events {
		if c, ok := e["refused"].(string); ok {
			out = append(out, c+": "+fmt.Sprint(e["text"]))
		}
	}
	return out
}

// live is Serve running over pipes a test writes commands to and reads events
// from one at a time.
type live struct {
	t      *testing.T
	in     *io.PipeWriter
	events chan map[string]any
	done   chan struct{}
	env    *receipt.Envelope
	err    error
}

func serveLive(t *testing.T, s session.Session, opts Options) *live {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	l := &live{t: t, in: inW, events: make(chan map[string]any), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		l.env, l.err = Serve(context.Background(), s, inR, outW, opts)
		_ = outW.Close()
	}()
	go func() {
		defer close(l.events)
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("event line %q: %v", sc.Bytes(), err)
				return
			}
			l.events <- m
		}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return l
}

func (l *live) cmd(line string) {
	l.t.Helper()
	if _, err := io.WriteString(l.in, line+"\n"); err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) next() map[string]any {
	l.t.Helper()
	m, ok := <-l.events
	if !ok {
		l.t.Fatal("the events ended")
	}
	return m
}

// nextOf is the next event, which must be of kind and say text.
func (l *live) nextOf(kind, text string) map[string]any {
	l.t.Helper()
	m := l.next()
	if m["kind"] != kind || (text != "" && m["text"] != text) {
		l.t.Fatalf("next event is %v, want a %s %q", m, kind, text)
	}
	return m
}

// busy is the next event, which must be the busy refusal of a send.
func (l *live) busy() {
	l.t.Helper()
	m := l.next()
	if m["kind"] != "error" || m["refused"] != "busy" || m["text"] != "a turn is in flight; send again when its assistant_turn arrives" || m["time"] == nil {
		l.t.Fatalf("next event is %v, want the busy refusal", m)
	}
}

// end ends the session and waits for Serve.
func (l *live) end() {
	l.t.Helper()
	l.cmd(`{"end":{"finish":false}}`)
	<-l.done
}

// TestAnAddIsHeldToTheInputsDirectory: an add names a regular file directly in
// the inputs directory, by a relative or an absolute path; anything else is
// refused before the session sees it, and the session's own refusal is passed
// on.
func TestAnAddIsHeldToTheInputsDirectory(t *testing.T) {
	dir := t.TempDir()
	put := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := put("ok.txt")
	put("sub/in.txt")
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(ok, filepath.Join(dir, "link.txt")) == nil
	add := func(path string) string {
		b, _ := json.Marshal(map[string]any{"add": map[string]any{"path": path}})
		return string(b) + "\n"
	}
	notRegular := func(p string) string {
		return "input_refused: input " + p + " is not a regular file (directories and links are not accepted mid-run)"
	}
	notDirect := func(p string) string {
		return "input_refused: input " + p + " is not directly in the inputs directory " + dir
	}

	fs := newScripted(nil)
	input := add("ok.txt") + add(ok) + add("sub/../ok.txt") + add(dir+"/sub/../ok.txt") + add("sub/in.txt") + add("../ok.txt") + add("gone.txt") + add("d")
	want := []string{
		notDirect(filepath.Join(dir, "sub", "in.txt")),
		notDirect(filepath.Join(filepath.Dir(dir), "ok.txt")),
		"input_refused: input file: lstat " + filepath.Join(dir, "gone.txt") + ": no such file or directory",
		notRegular(filepath.Join(dir, "d")),
	}
	if linked {
		input += add("link.txt")
		want = append(want, notRegular(filepath.Join(dir, "link.txt")))
	}
	events, _, err := serveLines(t, fs, input, Options{InputsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := refusals(events); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("refusals:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	slices.Sort(fs.added)
	if strings.Join(fs.added, "\n") != strings.Join([]string{ok, ok, ok, ok}, "\n") {
		t.Errorf("the session was given %q", fs.added)
	}

	// With no inputs directory every add is refused.
	fs = newScripted(nil)
	events, _, _ = serveLines(t, fs, add("ok.txt"), Options{})
	if got := refusals(events); strings.Join(got, "\n") != "input_refused: this session takes no files: it was started without an inputs directory" || len(fs.added) != 0 {
		t.Errorf("an add with no inputs directory: %q, session given %q", got, fs.added)
	}

	// A file the session will not take is refused with the session's words,
	// unless the session has ended, which is no refusal of the file.
	fs = newScripted(nil)
	fs.addErr = errors.New("the copy failed")
	events, _, _ = serveLines(t, fs, add("ok.txt"), Options{InputsDir: dir})
	if got := refusals(events); strings.Join(got, "\n") != "input_refused: the copy failed" {
		t.Errorf("an add the session refused: %q", got)
	}
	fs = newScripted(nil)
	fs.addErr = session.ErrEnded
	events, _, _ = serveLines(t, fs, add("ok.txt"), Options{InputsDir: dir})
	if got := refusals(events); len(got) != 0 {
		t.Errorf("an add to a session that had ended: %q", got)
	}
}

// TestTheEndCommandAndTheEndOfInputEndTheSession: an end command ends the
// session with or without its result, the end of input ends it without one,
// nothing after an end is read, the session's own end ends Serve, and what End
// returns is what Serve returns.
func TestTheEndCommandAndTheEndOfInputEndTheSession(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		finish      bool
		sent        []string
	}{
		{"an end with a result", `{"end":{"finish":true}}` + "\n", true, nil},
		{"an end without a result", `{"end":{"finish":false}}` + "\n", false, nil},
		{"an end naming nothing", `{"end":{}}` + "\n", false, nil},
		{"the end of input", "", false, nil},
		{"a command the end of input follows", `{"send":{"text":"one"}}` + "\n", false, []string{"one"}},
		{"a command after the end", `{"end":{"finish":true}}` + "\n" + `{"send":{"text":"late"}}` + "\n", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newScripted(nil)
			fs.endErr = errors.New("the teardown failed")
			_, env, err := serveLines(t, fs, tc.input, Options{})
			if err == nil || err.Error() != "the teardown failed" || env == nil || env.Status != "ok" {
				t.Errorf("Serve returned %v, %v", env, err)
			}
			if !slices.Equal(fs.finish, []bool{tc.finish}) {
				t.Errorf("End was called with finish %v, want [%v]", fs.finish, tc.finish)
			}
			if !slices.Equal(fs.sent, tc.sent) {
				t.Errorf("the session was sent %q, want %q", fs.sent, tc.sent)
			}
		})
	}

	t.Run("a session that ends by itself", func(t *testing.T) {
		fs := newScripted(nil)
		l := serveLive(t, fs, Options{})
		fs.emit(session.Event{Kind: session.KindSessionEnd, Reason: session.EndIdle})
		// No command or end of input arrives: the session's own end ends Serve.
		if m := l.nextOf("session_end", ""); m["reason"] != "idle_timeout" {
			t.Errorf("event %v", m)
		}
		<-l.done
		if !slices.Equal(fs.finish, []bool{false}) {
			t.Errorf("End was called with finish %v", fs.finish)
		}
	})
}

// TestASendIsRefusedBusyWhileATurnIsOnItsWayOrInFlight: a send is taken only
// when no turn is open and no other send is on its way to opening one; an
// error closes a turn as well as the assistant's answer does, and the opening
// task holds sends until its turn ends. The harness's raw message never leaves.
func TestASendIsRefusedBusyWhileATurnIsOnItsWayOrInFlight(t *testing.T) {
	send := func(text string) string {
		b, _ := json.Marshal(map[string]any{"send": map[string]any{"text": text}})
		return string(b)
	}
	t.Run("sends", func(t *testing.T) {
		holdA, holdB := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		fs := newScripted(func(f *scriptedSession, text string) error {
			switch {
			case calls.Add(1) == 1:
				<-holdA // taken, and the session has not opened its turn
				f.emit(session.Event{Kind: session.KindUserTurn, Text: text, Raw: json.RawMessage(`{"harness":"message"}`)})
				<-holdB // the turn is open
				f.emit(session.Event{Kind: session.KindAssistantTurn, Text: "done"})
			case text == "five":
				f.emit(session.Event{Kind: session.KindUserTurn, Text: text})
				f.emit(session.Event{Kind: session.KindError, Text: "the model said no"})
				return errors.New("the model said no")
			default:
				return turn(f, text)
			}
			return nil
		})
		l := serveLive(t, fs, Options{})
		// A line the protocol cannot read is refused, and the session goes on.
		l.cmd("not a command")
		if m := l.next(); m["kind"] != "error" || m["refused"] != "malformed" || m["text"] != "a command is one JSON object per line" {
			t.Errorf("a line that is not JSON: %v", m)
		}
		l.cmd(`{"stop":{}}`)
		if m := l.next(); m["kind"] != "error" || m["refused"] != "unknown_command" {
			t.Errorf("an unknown command: %v", m)
		}
		l.cmd(send("one"))
		l.cmd(send("two"))
		l.busy()
		close(holdA)
		if m := l.nextOf("user_turn", "one"); m["raw"] != nil {
			t.Errorf("the harness's raw message was written: %v", m)
		}
		l.cmd(send("three"))
		l.busy()
		close(holdB)
		l.nextOf("assistant_turn", "done")
		l.cmd(send("four"))
		l.nextOf("user_turn", "four")
		l.nextOf("assistant_turn", "done")
		l.cmd(send("five"))
		l.nextOf("user_turn", "five")
		if m := l.nextOf("error", "the model said no"); m["refused"] != nil {
			t.Errorf("the session's error was written as a refusal: %v", m)
		}
		l.cmd(send("six"))
		l.nextOf("user_turn", "six")
		l.nextOf("assistant_turn", "done")
		l.end()
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if !slices.Equal(fs.sent, []string{"one", "four", "five", "six"}) {
			t.Errorf("the session was sent %q", fs.sent)
		}
	})
	t.Run("an opening task", func(t *testing.T) {
		fs := newScripted(turn)
		l := serveLive(t, fs, Options{OpeningTurn: true})
		l.cmd(send("early"))
		l.busy()
		fs.emit(session.Event{Kind: session.KindUserTurn, Text: "the task"})
		fs.emit(session.Event{Kind: session.KindAssistantTurn, Text: "done"})
		l.nextOf("user_turn", "the task")
		l.nextOf("assistant_turn", "done")
		l.cmd(send("later"))
		l.nextOf("user_turn", "later")
		l.nextOf("assistant_turn", "done")
		l.end()
		if !slices.Equal(fs.sent, []string{"later"}) {
			t.Errorf("the session was sent %q", fs.sent)
		}
	})
	t.Run("a session that is busy", func(t *testing.T) {
		// A refusal is an error event of the protocol's own: it carries the
		// class, the session's words and a time, and is not a session event.
		fs := sharedSession{newChatty(0)}
		events, _, _ := serveLines(t, fs, send("one")+"\n", Options{})
		var found int
		for _, e := range events {
			if e["refused"] == "busy" {
				found++
				if e["kind"] != "error" || e["time"] == nil {
					t.Errorf("the busy refusal is %v", e)
				}
			}
		}
		if found != 1 {
			t.Errorf("%d busy refusals", found)
		}
	})
}

// TestAnAnswerReachesTheSessionThatAsksOrIsRefused: an answer is passed to a
// session that asks questions, with its yes or no; a session that asks none,
// or that will not take the answer, refuses it.
func TestAnAnswerReachesTheSessionThatAsksOrIsRefused(t *testing.T) {
	events, _, _ := serveLines(t, newScripted(nil), `{"answer":{"id":"q-1","yes":true}}`+"\n", Options{})
	if got := refusals(events); strings.Join(got, "\n") != "answer_refused: this session asks no questions" {
		t.Errorf("an answer to a session that asks nothing: %q", got)
	}

	asking := &answeringSession{scriptedSession: newScripted(nil)}
	events, _, _ = serveLines(t, asking, `{"answer":{"id":"q-1","yes":true}}`+"\n"+`{"answer":{"id":"q-2"}}`+"\n", Options{})
	if got := refusals(events); len(got) != 0 {
		t.Errorf("answers were refused: %q", got)
	}
	if !slices.Equal(asking.answers, []string{"q-1:true", "q-2:false"}) {
		t.Errorf("the session was answered %q", asking.answers)
	}

	asking = &answeringSession{scriptedSession: newScripted(nil), err: errors.New("no such question")}
	events, _, _ = serveLines(t, asking, `{"answer":{"id":"q-3","yes":true}}`+"\n", Options{})
	if got := refusals(events); strings.Join(got, "\n") != "answer_refused: no such question" {
		t.Errorf("an answer the session refused: %q", got)
	}
}

// TestUsageIsMeteredAsTheSessionNarratesIt: the totals handed to OnUsage after
// every usage event count the tool calls, sum the tokens and keep the highest
// cost.
func TestUsageIsMeteredAsTheSessionNarratesIt(t *testing.T) {
	fs := newScripted(func(f *scriptedSession, text string) error {
		f.emit(session.Event{Kind: session.KindUserTurn, Text: text})
		f.emit(session.Event{Kind: session.KindToolCall, Text: "a"})
		f.emit(session.Event{Kind: session.KindToolCall, Text: "b"})
		f.emit(session.Event{Kind: session.KindUsage, InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CostUSD: 0.01})
		f.emit(session.Event{Kind: session.KindUsage, InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostUSD: 0.03})
		f.emit(session.Event{Kind: session.KindUsage, InputTokens: 1, OutputTokens: 1, TotalTokens: 2, CostUSD: 0.02})
		f.emit(session.Event{Kind: session.KindAssistantTurn, Text: "done"})
		return nil
	})
	var mu sync.Mutex
	var got []receipt.Usage
	l := serveLive(t, fs, Options{OnUsage: func(u receipt.Usage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, u)
	}})
	l.cmd(`{"send":{"text":"go"}}`)
	for _, kind := range []string{"user_turn", "tool_call", "tool_call", "usage", "usage", "usage", "assistant_turn"} {
		l.nextOf(kind, "")
	}
	l.end()
	want := []receipt.Usage{
		{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CostUSD: 0.01, Turns: 2},
		{InputTokens: 110, OutputTokens: 55, TotalTokens: 165, CostUSD: 0.03, Turns: 2},
		{InputTokens: 111, OutputTokens: 56, TotalTokens: 167, CostUSD: 0.03, Turns: 2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("usage totals %+v, want %+v", got, want)
	}
}

// TestAClosedOutputStillWritesWhatIsQueued: closing the output stops it taking
// more but writes the events it holds.
func TestAClosedOutputStillWritesWhatIsQueued(t *testing.T) {
	g := &gatedWriter{took: make(chan struct{}, 1), gate: make(chan struct{})}
	o := newOutbox(g, nil)
	ev := func(text string) Event { return Event{Event: session.Event{Kind: session.KindError, Text: text}} }
	o.put(ev("one"))
	<-g.took
	o.put(ev("two"))
	o.put(ev("three"))
	o.close(time.Millisecond) // the writer still holds the first line
	o.put(ev("late"))
	close(g.gate)
	<-o.done
	var texts []string
	for _, l := range g.rec.lines() {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, m["text"].(string))
	}
	if !slices.Equal(texts, []string{"one", "two", "three"}) {
		t.Errorf("written %q", texts)
	}
}

// steeringSession is a session whose turn in flight takes a message, or
// refuses it with err.
type steeringSession struct {
	*scriptedSession
	steered []string
	err     error
}

func (s *steeringSession) Steer(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return session.ErrEnded
	}
	s.steered = append(s.steered, text)
	s.events <- session.Event{Kind: session.KindSteer, Text: text}
	return nil
}

// TestASteerReachesTheSessionOrIsRefusedByName: a steer goes to a session that
// takes one, which answers with its own steer event; any other outcome is a
// refusal whose class says which it was, and none of them ends the session.
func TestASteerReachesTheSessionOrIsRefusedByName(t *testing.T) {
	steer := `{"steer":{"text":"and the date"}}` + "\n"

	// A session with no way to take one.
	events, _, _ := serveLines(t, newScripted(nil), steer, Options{})
	if got := refusals(events); strings.Join(got, "\n") != "cannot_steer: this harness takes no message while a turn is in flight" {
		t.Errorf("a steer to a session that takes none: %q", got)
	}

	taking := &steeringSession{scriptedSession: newScripted(nil)}
	l := serveLive(t, taking, Options{})
	l.cmd(strings.TrimSpace(steer))
	l.nextOf("steer", "and the date")
	l.cmd(`{"end":{"finish":false}}`)
	for range l.events {
	}
	<-l.done
	if !slices.Equal(taking.steered, []string{"and the date"}) {
		t.Errorf("the session was steered %q", taking.steered)
	}

	for _, tc := range []struct {
		err  error
		want string
	}{
		{session.ErrNoTurn, "no_turn: no turn is in flight; send the text as a turn"},
		{fmt.Errorf("%w: the docker-agent harness takes one only between turns", session.ErrCannotSteer),
			"cannot_steer: this harness takes no message while a turn is in flight: the docker-agent harness takes one only between turns"},
		{errors.New("the session's limit of 3 messages is reached"), "steer_refused: the session's limit of 3 messages is reached"},
	} {
		refusing := &steeringSession{scriptedSession: newScripted(nil), err: tc.err}
		events, _, _ = serveLines(t, refusing, steer, Options{})
		if got := refusals(events); strings.Join(got, "\n") != tc.want {
			t.Errorf("a steer the session answered with %v: %q", tc.err, got)
		}
	}

	// A session that has ended says nothing of a steer that came too late.
	ended := &steeringSession{scriptedSession: newScripted(nil), err: session.ErrEnded}
	events, _, _ = serveLines(t, ended, steer, Options{})
	if got := refusals(events); len(got) != 0 {
		t.Errorf("a steer after the end was refused: %q", got)
	}
}

// TestASteerDoesNotTakeTheTurn: a steer during a turn leaves the turn in
// flight as it was, so a send is still refused busy until the turn ends, and
// taken after it.
func TestASteerDoesNotTakeTheTurn(t *testing.T) {
	hold := make(chan struct{})
	fs := &steeringSession{scriptedSession: newScripted(func(f *scriptedSession, text string) error {
		f.emit(session.Event{Kind: session.KindUserTurn, Text: text})
		if text == "first" {
			<-hold
		}
		f.emit(session.Event{Kind: session.KindAssistantTurn, Text: "done"})
		return nil
	})}
	l := serveLive(t, fs, Options{})
	l.cmd(`{"send":{"text":"first"}}`)
	l.nextOf("user_turn", "first")
	l.cmd(`{"steer":{"text":"and the date"}}`)
	l.nextOf("steer", "and the date")
	l.cmd(`{"send":{"text":"second"}}`)
	if m := l.next(); m["refused"] != "busy" {
		t.Fatalf("a send during the steered turn: %v", m)
	}
	close(hold)
	l.nextOf("assistant_turn", "done")
	l.cmd(`{"send":{"text":"second"}}`)
	l.nextOf("user_turn", "second")
	l.nextOf("assistant_turn", "done")
	l.cmd(`{"end":{"finish":false}}`)
	for range l.events {
	}
	<-l.done
}
