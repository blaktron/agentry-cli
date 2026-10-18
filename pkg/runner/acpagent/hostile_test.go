package acpagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// rawClient speaks to the agent in raw frames, as a hostile or broken client
// would.
type rawClient struct {
	t    *testing.T
	w    *io.PipeWriter
	r    *bufio.Reader
	done chan struct{}
}

type testBackend func(ctx context.Context, h *Host, first string) error

func (f testBackend) Run(ctx context.Context, h *Host, first string) error { return f(ctx, h, first) }

func startRaw(t *testing.T, b Backend) *rawClient {
	t.Helper()
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_ = Serve(context.Background(), ar, aw, Info{Name: "agentry", Version: "test"}, b)
		_ = aw.Close()
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		_, _ = io.Copy(io.Discard, cr)
		select {
		case <-c.done:
		case <-time.After(15 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	return c
}

func (c *rawClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.sendRaw(string(b) + "\n")
}

func (c *rawClient) sendRaw(s string) {
	c.t.Helper()
	if _, err := c.w.Write([]byte(s)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawClient) recv() map[string]any {
	c.t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := c.r.ReadBytes('\n')
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("read: %v", r.err)
		}
		var m map[string]any
		if err := json.Unmarshal(r.line, &m); err != nil {
			c.t.Fatalf("the agent wrote a non-JSON line %q", r.line)
		}
		return m
	case <-time.After(10 * time.Second):
		c.t.Fatal("no frame from the agent")
		return nil
	}
}

// replyTo reads frames until the response to id, returning it; requests and
// notifications met on the way are returned too.
func (c *rawClient) replyTo(id any) (resp map[string]any, others []map[string]any) {
	c.t.Helper()
	for {
		m := c.recv()
		if m["method"] == nil && fmt.Sprint(m["id"]) == fmt.Sprint(id) {
			return m, others
		}
		others = append(others, m)
	}
}

func req(id any, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func errCode(m map[string]any) int {
	e, ok := m["error"].(map[string]any)
	if !ok {
		return 0
	}
	return int(e["code"].(float64))
}

func (c *rawClient) handshake() string {
	c.t.Helper()
	c.send(req(1, "initialize", map[string]any{"protocolVersion": 1}))
	if r, _ := c.replyTo(1); r["error"] != nil {
		c.t.Fatalf("initialize: %v", r)
	}
	c.send(req(2, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	r, _ := c.replyTo(2)
	res, _ := r["result"].(map[string]any)
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		c.t.Fatalf("session/new: %v", r)
	}
	return sid
}

func prompt(sid, text string) map[string]any {
	return map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": text}}}
}

func neverRuns(t *testing.T) testBackend {
	return func(context.Context, *Host, string) error {
		t.Error("the run started")
		return nil
	}
}

func TestNothingIsServedBeforeInitializeOrBeforeASession(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	c.send(req(1, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	if r, _ := c.replyTo(1); errCode(r) != codeInvalidRequest {
		t.Fatalf("session/new before initialize: %v", r)
	}
	c.send(req(2, "session/prompt", prompt("agentry-x", "go")))
	if r, _ := c.replyTo(2); errCode(r) != codeInvalidRequest {
		t.Fatalf("prompt before initialize: %v", r)
	}
	c.send(req(3, "initialize", map[string]any{"protocolVersion": 1}))
	c.replyTo(3)
	c.send(req(4, "initialize", map[string]any{"protocolVersion": 1}))
	if r, _ := c.replyTo(4); errCode(r) != codeInvalidRequest {
		t.Fatalf("a second initialize: %v", r)
	}
	c.send(req(5, "session/prompt", prompt("agentry-x", "go")))
	if r, _ := c.replyTo(5); errCode(r) != codeInvalidParams {
		t.Fatalf("a prompt before session/new: %v", r)
	}
	sid := c.handshakeSession()
	c.send(req(7, "session/prompt", prompt("not-"+sid, "go")))
	if r, _ := c.replyTo(7); errCode(r) != codeInvalidParams {
		t.Fatalf("a prompt for another session: %v", r)
	}
}

func (c *rawClient) handshakeSession() string {
	c.send(req(6, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	r, _ := c.replyTo(6)
	return r["result"].(map[string]any)["sessionId"].(string)
}

func TestASessionTakesNoClientToolsAndOnlyOneSession(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	c.send(req(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
		"fs": map[string]any{"readTextFile": true, "writeTextFile": true}, "terminal": true}}))
	c.replyTo(1)
	c.send(req(2, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{map[string]any{"name": "shell", "command": "sh", "args": []any{}, "env": []any{}}}}))
	if r, _ := c.replyTo(2); errCode(r) != codeInvalidParams {
		t.Fatalf("a client's MCP server was taken: %v", r)
	}
	c.send(req(3, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}, "additionalDirectories": []any{"/home"}}))
	if r, _ := c.replyTo(3); errCode(r) != codeInvalidParams {
		t.Fatalf("a client's directory was taken: %v", r)
	}
	c.send(req(4, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	if r, _ := c.replyTo(4); r["error"] != nil {
		t.Fatalf("session/new: %v", r)
	}
	c.send(req(5, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	if r, _ := c.replyTo(5); errCode(r) != codeInvalidRequest {
		t.Fatalf("a second session: %v", r)
	}
	// A client calling what only a client serves, or what the agent does not.
	for i, m := range []string{"fs/read_text_file", "fs/write_text_file", "terminal/create", "terminal/output", "session/load", "session/set_mode", "session/list", "authenticate", "nonsense"} {
		id := 10 + i
		c.send(req(id, m, map[string]any{"path": "/etc/passwd", "sessionId": "x"}))
		if r, _ := c.replyTo(id); errCode(r) != codeMethodNotFound {
			t.Fatalf("%s: %v", m, r)
		}
	}
}

func TestPromptsBeforeConsentAreRefusedAndThePendingOneIsTheOnlyOne(t *testing.T) {
	asked := make(chan struct{})
	release := make(chan struct{})
	var ran atomic.Bool
	b := testBackend(func(ctx context.Context, h *Host, _ string) error {
		close(asked)
		err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"})
		<-release
		if err == nil {
			ran.Store(true)
		}
		return err
	})
	c := startRaw(t, b)
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "first")))
	<-asked
	// While the consent request is out, more prompts, a second close and a
	// cancel of nothing in particular change nothing.
	for i := 0; i < 5; i++ {
		c.send(req(20+i, "session/prompt", prompt(sid, "second")))
	}
	seen := 0
	var permID any
	for seen < 5 || permID == nil {
		m := c.recv()
		switch {
		case m["method"] == "session/request_permission":
			permID = m["id"]
		case m["id"] != nil && m["error"] != nil:
			if errCode(m) != codeInvalidRequest {
				t.Fatalf("a prompt in flight was answered %v", m)
			}
			seen++
		}
	}
	// The client answers the request with a response whose id is another's:
	// it is not the consent.
	c.send(map[string]any{"jsonrpc": "2.0", "id": 9999, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
	c.send(map[string]any{"jsonrpc": "2.0", "id": "allow", "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a reply to another id consented")
	}
	// The real answer, declining.
	c.send(map[string]any{"jsonrpc": "2.0", "id": permID, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject"}}})
	close(release)
	r, _ := c.replyTo(10)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "refusal" {
		t.Fatalf("the declined run was answered %v", r)
	}
	if ran.Load() {
		t.Fatal("the declined run ran")
	}
}

func TestAMalformedOrHugeFrameDoesNotEndTheConnection(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	c.sendRaw("this is not json\n")
	if r := c.recv(); errCode(r) != codeParse {
		t.Fatalf("garbage: %v", r)
	}
	c.sendRaw(`{"jsonrpc":"1.0","id":1,"method":"initialize"}` + "\n")
	if r := c.recv(); errCode(r) != codeInvalidRequest {
		t.Fatalf("a wrong version: %v", r)
	}
	c.sendRaw(`{"jsonrpc":"2.0"}` + "\n")
	if r := c.recv(); errCode(r) != codeInvalidRequest {
		t.Fatalf("no method: %v", r)
	}
	c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize"}`, strings.Repeat("i", 1000)) + "\n")
	if r := c.recv(); errCode(r) != codeInvalidRequest || r["id"] != nil {
		t.Fatalf("a huge id was echoed: %v", r)
	}
	// 3 MiB on one line, then a valid request on the next.
	big := `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"pad":"` + strings.Repeat("x", 3<<20) + `"}}` + "\n"
	go func() { _, _ = c.w.Write([]byte(big)) }()
	if r := c.recv(); errCode(r) != codeParse {
		t.Fatalf("a huge frame: %v", r)
	}
	c.send(req(4, "initialize", map[string]any{"protocolVersion": 1}))
	if r, _ := c.replyTo(4); r["error"] != nil {
		t.Fatalf("the connection did not survive a huge frame: %v", r)
	}
	// A prompt with a block the agent does not take, and an empty one.
	sid := c.handshakeSession()
	for i, p := range []any{
		map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}}},
		map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///etc/passwd", "text": "x"}}}},
		map[string]any{"sessionId": sid, "prompt": []any{}},
		map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "  \n"}}},
		map[string]any{"sessionId": sid, "prompt": "text"},
	} {
		id := 100 + i
		c.send(req(id, "session/prompt", p))
		if r, _ := c.replyTo(id); errCode(r) != codeInvalidParams {
			t.Fatalf("prompt %d: %v", i, r)
		}
	}
}

func TestAFrameWithoutItsNewlineAtTheEndIsStillAFrame(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	_ = c.w.Close()
	if r := c.recv(); r["error"] != nil || r["id"].(float64) != 1 {
		t.Fatalf("%v", r)
	}
}

func TestAClientThatNeverReadsCostsTheRunNothing(t *testing.T) {
	old := flushGrace
	flushGrace = 200 * time.Millisecond
	defer func() { flushGrace = old }()
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	var dropped int
	finished := make(chan time.Duration, 1)
	b := testBackend(func(ctx context.Context, h *Host, _ string) error {
		pad := strings.Repeat("x", 4096)
		start := time.Now()
		for i := 0; i < 6000; i++ { // 24 MB of progress
			h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "t", CallID: fmt.Sprint(i)})
			h.Event(adapter.Event{Kind: adapter.EventToolResult, Tool: "t", CallID: fmt.Sprint(i), Text: pad})
		}
		finished <- time.Since(start)
		h.Finish(&receipt.Envelope{Status: "ok", Result: []byte(`"done"`)}, nil)
		h.s.out.mu.Lock()
		dropped = h.s.out.dropped
		queued := h.s.out.queued
		h.s.out.mu.Unlock()
		if queued > MaxBehind+mustKeepHeadroom {
			t.Errorf("%d bytes queued for a client that does not read", queued)
		}
		return nil
	})
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), ar, aw, Info{Name: "agentry"}, b)
	}()
	r := bufio.NewReader(cr)
	write := func(v any) {
		bb, _ := json.Marshal(v)
		_, _ = cw.Write(append(bb, '\n'))
	}
	write(req(1, "initialize", map[string]any{"protocolVersion": 1}))
	_, _ = r.ReadBytes('\n')
	write(req(2, "session/new", map[string]any{"cwd": "/w", "mcpServers": []any{}}))
	line, _ := r.ReadBytes('\n')
	var ns struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &ns); err != nil || ns.Result.SessionID == "" {
		t.Fatalf("session/new: %s", line)
	}
	// From here the client reads nothing at all.
	write(req(3, "session/prompt", prompt(ns.Result.SessionID, "go")))
	select {
	case d := <-finished:
		if d > 20*time.Second {
			t.Fatalf("the run was held up %s by a client that does not read", d)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run was held up by a client that does not read")
	}
	_ = cw.Close()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not stop for a client that never reads")
	}
	if dropped == 0 {
		t.Fatal("24 MB of progress was queued whole")
	}
}

type blockedWriter struct{}

func (blockedWriter) Write([]byte) (int, error) { select {} }

func TestTheQueueGivesUpOnAClientThatCannotTakeTheAnswersItIsOwed(t *testing.T) {
	o := newOutbox(blockedWriter{})
	big := make([]byte, 1<<20)
	for i := 0; i < 40; i++ {
		o.send(big, false)
	}
	o.mu.Lock()
	dead, queued := o.dead, o.queued
	o.mu.Unlock()
	if !dead || queued != 0 {
		t.Fatalf("dead=%v queued=%d: the connection was not given up", dead, queued)
	}
	start := time.Now()
	o.close(200 * time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("close waited on a writer that never returns")
	}
}

func TestADeadReaderDoesNotStopTheRunsEvents(t *testing.T) {
	// The client closed its read side: writes fail, and Event stays cheap.
	ar, cw := io.Pipe()
	cr, aw := io.Pipe()
	_ = cr.Close()
	ran := make(chan struct{})
	b := testBackend(func(ctx context.Context, h *Host, _ string) error {
		defer close(ran)
		for i := 0; i < 1000; i++ {
			h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "t", CallID: fmt.Sprint(i)})
		}
		return nil
	})
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), ar, aw, Info{Name: "agentry"}, b)
	}()
	bb, _ := json.Marshal(req(1, "initialize", map[string]any{"protocolVersion": 1}))
	_, _ = cw.Write(append(bb, '\n'))
	_ = cw.Close()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent hung on a client that closed its reading side")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAStopSignalEndsTheRunWhileTheClientStillHoldsTheConnection(t *testing.T) {
	old := flushGrace
	flushGrace = 200 * time.Millisecond
	defer func() { flushGrace = old }()
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	ctx, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	torn := make(chan struct{})
	b := testBackend(func(rctx context.Context, h *Host, _ string) error {
		close(started)
		<-rctx.Done()
		close(torn)
		return context.Cause(rctx)
	})
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(ctx, ar, aw, Info{Name: "agentry"}, b)
		_ = aw.Close()
	}()
	c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: served}
	sid := c.handshake()
	c.send(req(9, "session/prompt", prompt(sid, "go")))
	<-started
	stop()
	select {
	case <-torn:
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not stopped")
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept serving after the stop")
	}
	_ = cw.Close()
	_, _ = io.Copy(io.Discard, cr)
}
