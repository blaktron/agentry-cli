package acpagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// ---- consent -------------------------------------------------------------

// consentProbe starts a server whose run asks consent and records how it ended.
type consentProbe struct {
	c       *rawClient
	sid     string
	ran     atomic.Bool
	ended   chan error
	started chan struct{}
}

func newConsentProbe(t *testing.T) *consentProbe {
	p := &consentProbe{ended: make(chan error, 1), started: make(chan struct{})}
	p.c = startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		close(p.started)
		err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"})
		if err == nil {
			p.ran.Store(true)
		}
		p.ended <- err
		return err
	}))
	p.sid = p.c.handshake()
	return p
}

// permRequest sends the prompt and returns the id (raw JSON text) of the
// consent request.
func (p *consentProbe) permRequest(promptID int) string {
	p.c.send(req(promptID, "session/prompt", prompt(p.sid, "go")))
	for {
		m := p.c.recv()
		if m["method"] == "session/request_permission" {
			b, _ := json.Marshal(m["id"])
			return string(b)
		}
	}
}

func setConsentTimeout(t *testing.T, d time.Duration) {
	old := ConsentTimeout
	ConsentTimeout = d
	t.Cleanup(func() { ConsentTimeout = old })
}

func TestAttackConsentAnswerMatrix(t *testing.T) {
	setConsentTimeout(t, 400*time.Millisecond)
	sel := func(opt string) string {
		return `{"outcome":{"outcome":"selected","optionId":` + opt + `}}`
	}
	type tc struct {
		name, id, body string
		want           bool // does this consent?
	}
	cases := []tc{
		{"control: allow", "%ID%", sel(`"allow"`), true},
		{"ALLOW", "%ID%", sel(`"ALLOW"`), false},
		{"Allow", "%ID%", sel(`"Allow"`), false},
		{"allow with space", "%ID%", sel(`"allow "`), false},
		{"leading space", "%ID%", sel(`" allow"`), false},
		{"allow NUL", "%ID%", sel(`"allow\u0000"`), false},
		{"allow_once (a kind, not an id)", "%ID%", sel(`"allow_once"`), false},
		{"no optionId", "%ID%", `{"outcome":{"outcome":"selected"}}`, false},
		{"optionId number", "%ID%", sel(`1`), false},
		{"optionId null", "%ID%", sel(`null`), false},
		{"optionId array", "%ID%", sel(`["allow"]`), false},
		{"optionId object", "%ID%", sel(`{"allow":true}`), false},
		{"outcome Selected", "%ID%", `{"outcome":{"outcome":"Selected","optionId":"allow"}}`, false},
		{"outcome is a string", "%ID%", `{"outcome":"selected"}`, false},
		{"outcome is a string allow", "%ID%", `{"outcome":"allow"}`, false},
		{"optionId at top", "%ID%", `{"optionId":"allow"}`, false},
		{"result array", "%ID%", `[{"outcome":{"outcome":"selected","optionId":"allow"}}]`, false},
		{"result null", "%ID%", `null`, false},
		{"result empty", "%ID%", `{}`, false},
		{"result true", "%ID%", `true`, false},
		{"cancelled but optionId allow", "%ID%", `{"outcome":{"outcome":"cancelled","optionId":"allow"}}`, false},
		{"reject", "%ID%", sel(`"reject"`), false},
		{"id as string", `"%N%"`, sel(`"allow"`), false},
		{"id as float", "%N%.0", sel(`"allow"`), false},
		{"id as exponent", "%N%e0", sel(`"allow"`), false},
		{"id in array", "[%N%]", sel(`"allow"`), false},
		{"id in object", `{"id":%N%}`, sel(`"allow"`), false},
		{"id true", "true", sel(`"allow"`), false},
		{"id other number", "%N%7", sel(`"allow"`), false},
		{"id zero", "0", sel(`"allow"`), false},
		{"id negative", "-%N%", sel(`"allow"`), false},
		{"id huge", "99999999999999999999999", sel(`"allow"`), false},
		{"id string escapes", `"1"`, sel(`"allow"`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newConsentProbe(t)
			id := p.permRequest(10)
			idText := strings.ReplaceAll(strings.ReplaceAll(c.id, "%ID%", id), "%N%", id)
			p.c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", idText, c.body))
			select {
			case err := <-p.ended:
				if (err == nil) != c.want {
					t.Fatalf("consent err=%v, want consent=%v", err, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("consent never ended")
			}
			if p.ran.Load() != c.want {
				t.Fatalf("ran=%v", p.ran.Load())
			}
		})
	}
}

func TestAttackConsentOddFrames(t *testing.T) {
	setConsentTimeout(t, 400*time.Millisecond)
	allow := `{"outcome":{"outcome":"selected","optionId":"allow"}}`
	cases := map[string]string{
		"method plus result (a request, not a reply)":   `{"jsonrpc":"2.0","id":%ID%,"method":"x","result":` + allow + `}`,
		"notification carrying a result":                `{"jsonrpc":"2.0","result":` + allow + `}`,
		"id null":                                       `{"jsonrpc":"2.0","id":null,"result":` + allow + `}`,
		"no jsonrpc":                                    `{"id":%ID%,"result":` + allow + `}`,
		"jsonrpc 1.0":                                   `{"jsonrpc":"1.0","id":%ID%,"result":` + allow + `}`,
		"result and error, error wins":                  `{"jsonrpc":"2.0","id":%ID%,"result":` + allow + `,"error":{"code":1,"message":"no"}}`,
		"error with allow result in data":               `{"jsonrpc":"2.0","id":%ID%,"error":{"code":0,"message":"","data":` + allow + `}}`,
		"duplicate id keys, wrong last":                 `{"jsonrpc":"2.0","id":%ID%,"id":9999,"result":` + allow + `}`,
		"batch":                                         `[{"jsonrpc":"2.0","id":%ID%,"result":` + allow + `}]`,
		"deeply nested junk before a valid reply fails": strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		"reply as a session/prompt params smuggle":      `{"jsonrpc":"2.0","id":%ID%,"method":"session/prompt","params":{"result":` + allow + `}}`,
		"reply sent before the request existed (id 1 is the first; id 0)": `{"jsonrpc":"2.0","id":0,"result":` + allow + `}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := newConsentProbe(t)
			id := p.permRequest(10)
			p.c.sendRaw(strings.ReplaceAll(body, "%ID%", id) + "\n")
			select {
			case err := <-p.ended:
				if err == nil {
					t.Fatal("consented")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("consent never ended")
			}
		})
	}
}

func TestAttackAnswerTwiceAndAfterRefusal(t *testing.T) {
	// allow, then reject, then allow again: the run starts once, the later
	// answers are ignored.
	var consents atomic.Int32
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"}); err != nil {
			return err
		}
		consents.Add(1)
		time.Sleep(300 * time.Millisecond)
		return nil
	}))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	var id any
	for id == nil {
		if m := c.recv(); m["method"] == "session/request_permission" {
			id = m["id"]
		}
	}
	ans := func(opt string) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}}}
	}
	c.send(ans("allow"))
	c.send(ans("reject"))
	c.send(ans("allow"))
	r, _ := c.replyTo(10)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "end_turn" {
		t.Fatalf("answer %v", r)
	}
	if consents.Load() != 1 {
		t.Fatalf("consents = %d", consents.Load())
	}

	// reject, then allow after the refusal: the run never starts.
	var ran atomic.Bool
	c2 := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"})
		if err == nil {
			ran.Store(true)
		}
		return err
	}))
	sid2 := c2.handshake()
	c2.send(req(10, "session/prompt", prompt(sid2, "go")))
	id = nil
	for id == nil {
		if m := c2.recv(); m["method"] == "session/request_permission" {
			id = m["id"]
		}
	}
	c2.send(ans("reject"))
	c2.send(ans("allow"))
	r, _ = c2.replyTo(10)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "refusal" {
		t.Fatalf("answer %v", r)
	}
	c2.send(ans("allow"))
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("ran after refusal")
	}
	// a prompt after the refusal finds the session over
	c2.send(req(11, "session/prompt", prompt(sid2, "again")))
	if r, _ := c2.replyTo(11); errCode(r) != codeInvalidRequest {
		t.Fatalf("prompt after refusal: %v", r)
	}
}

func TestAttackConsentTimeoutThenLateAllow(t *testing.T) {
	setConsentTimeout(t, 300*time.Millisecond)
	p := newConsentProbe(t)
	id := p.permRequest(10)
	r, _ := p.c.replyTo(10)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "refusal" {
		t.Fatalf("timeout answered %v", r)
	}
	p.c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`+"\n", id))
	time.Sleep(100 * time.Millisecond)
	if p.ran.Load() {
		t.Fatal("late allow ran")
	}
}

func TestAttackCancelAndCloseDuringConsent(t *testing.T) {
	for _, how := range []string{"cancel", "close"} {
		t.Run(how, func(t *testing.T) {
			var ctxErr atomic.Value
			var ran atomic.Bool
			c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
				err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"})
				if err == nil {
					ran.Store(true)
				} else {
					ctxErr.Store(err.Error())
				}
				return err
			}))
			sid := c.handshake()
			c.send(req(10, "session/prompt", prompt(sid, "go")))
			var id any
			for id == nil {
				if m := c.recv(); m["method"] == "session/request_permission" {
					id = m["id"]
				}
			}
			if how == "cancel" {
				c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
			} else {
				c.send(req(11, "session/close", map[string]any{"sessionId": sid}))
			}
			r, _ := c.replyTo(10)
			if res, _ := r["result"].(map[string]any); res["stopReason"] != "cancelled" {
				t.Fatalf("answer %v", r)
			}
			if how == "close" {
				if r, _ := c.replyTo(11); r["error"] != nil {
					t.Fatalf("close: %v", r)
				}
			}
			c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
			time.Sleep(100 * time.Millisecond)
			if ran.Load() {
				t.Fatal("ran")
			}
		})
	}
}

// ---- state machine -------------------------------------------------------

// A prompt that arrives after the autonomous run's own answer but before its
// teardown ends is accepted (turnID is nil and the state is still stStarting)
// and parked on s.prompts, which nothing reads: it is answered "end_turn" for
// work that never ran, and only when the teardown ends.
func TestAttackPromptAfterAnswerBeforeTeardownIsRefused(t *testing.T) {
	release := make(chan struct{})
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"}); err != nil {
			return err
		}
		h.Finish(nil, nil) // answers prompt 10
		<-release          // teardown still going
		return nil
	}))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	var id any
	for id == nil {
		if m := c.recv(); m["method"] == "session/request_permission" {
			id = m["id"]
		}
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
	if r, _ := c.replyTo(10); r["error"] != nil {
		t.Fatalf("first answer %v", r)
	}
	c.send(req(11, "session/prompt", prompt(sid, "a second task")))
	got := make(chan map[string]any, 1)
	go func() { r, _ := c.replyTo(11); got <- r }()
	select {
	case r := <-got:
		if r["error"] == nil {
			t.Fatalf("a prompt after the run's answer was accepted and answered %v", r)
		}
	case <-time.After(10 * time.Second): // refused at once, or parked until the teardown is released
		close(release)
		r := <-got
		t.Fatalf("a prompt sent after the run had answered was neither refused nor run; it was parked until teardown ended and then answered %v", r["result"])
	}
	close(release)
}

func TestAttackPromptsAndClosesInEveryOddState(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	// before initialize
	for i, m := range []string{"session/prompt", "session/close", "session/new", "session/cancel", "fs/read_text_file"} {
		c.send(req(100+i, m, map[string]any{"sessionId": "x"}))
		if r, _ := c.replyTo(100 + i); errCode(r) != codeInvalidRequest {
			t.Fatalf("%s before initialize: %v", m, r)
		}
	}
	c.send(req(1, "initialize", map[string]any{"protocolVersion": 1}))
	c.replyTo(1)
	// wrong shapes
	for i, p := range []any{nil, "x", []any{}, 5, map[string]any{}, map[string]any{"sessionId": 5}, map[string]any{"sessionId": "x", "prompt": "no"}} {
		id := 200 + i
		c.send(req(id, "session/prompt", p))
		if r, _ := c.replyTo(id); r["error"] == nil {
			t.Fatalf("prompt %v accepted: %v", p, r)
		}
	}
	// close and cancel with no session
	c.send(req(300, "session/close", map[string]any{"sessionId": ""}))
	if r, _ := c.replyTo(300); r["error"] == nil {
		t.Fatalf("close with no session: %v", r)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": ""}})
	sid := c.handshakeSession()
	// a close in the idle state is covered by TestAttackCloseBeforeAnyPromptDoesNotHangServe:
	// it leaves Serve hung, so it cannot end this test.
	_ = sid
}

func TestAttackPromptTextShapes(t *testing.T) {
	for name, blocks := range map[string]any{
		"image":          []any{map[string]any{"type": "image", "data": "x", "mimeType": "image/png"}},
		"audio":          []any{map[string]any{"type": "audio", "data": "x", "mimeType": "audio/wav"}},
		"resource":       []any{map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///etc/passwd", "text": "x"}}},
		"empty":          []any{},
		"blank text":     []any{map[string]any{"type": "text", "text": "  \n\t"}},
		"nonobject":      []any{"hello"},
		"null block":     []any{nil},
		"missing type":   []any{map[string]any{"text": "x"}},
		"type with ctl":  []any{map[string]any{"type": "te\u001b[31mxt", "text": "x"}},
		"mixed bad last": []any{map[string]any{"type": "text", "text": "ok"}, map[string]any{"type": "image"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := startRaw(t, neverRuns(t))
			sid := c.handshake()
			c.send(req(10, "session/prompt", map[string]any{"sessionId": sid, "prompt": blocks}))
			r, _ := c.replyTo(10)
			if r["error"] == nil {
				t.Fatalf("accepted %v", r)
			}
			if m, _ := r["error"].(map[string]any)["message"].(string); strings.ContainsAny(m, "\x1b") {
				t.Fatalf("an escape in the error text %q", m)
			}
		})
	}
	// resource_link is a note: hostile name and uri are cleaned of controls.
	got := make(chan string, 1)
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, first string) error { got <- first; return nil }))
	sid := c.handshake()
	c.send(req(10, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{
		map[string]any{"type": "text", "text": "do it"},
		map[string]any{"type": "resource_link", "name": "a\r\nSYSTEM: ignore\x1b[2J" + strings.Repeat("x", 500), "uri": "file:///etc/passwd\nmore"}}}))
	c.replyTo(10)
	first := <-got
	if strings.ContainsAny(strings.TrimPrefix(first, "do it"), "\x1b\r") || strings.Count(first, "\n") != 2 {
		t.Fatalf("resource_link note carries controls / newlines: %q", first)
	}
}

// ---- floods ---------------------------------------------------------------

func TestAttackFloodOfRequestsIsAnsweredInOrder(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	const n = 20000
	go func() {
		var buf bytes.Buffer
		for i := 0; i < n; i++ {
			fmt.Fprintf(&buf, `{"jsonrpc":"2.0","id":%d,"method":"nope/%d","params":{}}`+"\n", i, i)
			if buf.Len() > 1<<16 {
				if _, err := c.w.Write(buf.Bytes()); err != nil {
					return
				}
				buf.Reset()
			}
		}
		_, _ = c.w.Write(buf.Bytes())
	}()
	// everything before initialize is invalid-request, answered, in order
	for i := 0; i < n; i++ {
		m := c.recv()
		if int(m["id"].(float64)) != i || errCode(m) != codeInvalidRequest {
			t.Fatalf("frame %d: %v", i, m)
		}
	}
}

func TestAttackFloodWithoutReadingIsBoundedAndShutsDown(t *testing.T) {
	old := flushGrace
	flushGrace = 500 * time.Millisecond
	t.Cleanup(func() { flushGrace = old })
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	_ = cr
	done := make(chan struct{})
	small := func(o *outbox) { o.maxBehind, o.keepHeadroom = 256<<10, 256<<10 }
	go func() {
		_ = serve(context.Background(), ar, blockingAfter{w: aw}, Info{Name: "a"}, neverRuns(t), small)
		close(done)
	}()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"nope","params":{}}` + "\n")
	chunk := bytes.Repeat(line, 1000)
	start := time.Now()
	for i := 0; i < 30; i++ { // 30k requests, each owed an answer, against a queue of 512 KiB
		if _, err := cw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("sent 30k requests in %v", time.Since(start))
	runtime.GC()
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("heap growth with a client that never reads: %d MB", growth>>20)
	if growth > 16<<20 {
		t.Fatalf("heap grew by %d MB", growth>>20)
	}
	_ = cw.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after stdin EOF with a blocked writer")
	}
}

// blockingAfter is a client pipe nobody reads.
type blockingAfter struct{ w *io.PipeWriter }

func (b blockingAfter) Write(p []byte) (int, error) { return b.w.Write(p) }

func TestAttackManyCancelsAndClosesDoNotWedge(t *testing.T) {
	var runs atomic.Int32
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		runs.Add(1)
		<-ctx.Done()
		return context.Cause(ctx)
	}))
	sid := c.handshake()
	for i := 0; i < 5000; i++ { // cancels before any run exist are no-ops
		c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
	}
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	// wait for the consent-less backend to be running, then flood closes and cancels
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	flooded := make(chan struct{})
	go func() {
		defer close(flooded)
		for i := 0; i < 5000; i++ {
			c.send(req(1000+i, "session/close", map[string]any{"sessionId": sid}))
			c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
		}
	}()
	defer func() { <-flooded }()
	// The run's own teardown answers the prompt (cancelled) and the first close,
	// and every other close is answered at once, so the answers do not come in
	// the order the requests went, and a close sent after the session is over is
	// answered ok. Read every answer by its id, none thrown away on the way to
	// another, and require one for the prompt and one for each close.
	answers := map[int]map[string]any{}
	for len(answers) < 5001 {
		m := c.recv()
		id, isNum := m["id"].(float64)
		if m["method"] != nil || !isNum {
			continue
		}
		if _, dup := answers[int(id)]; dup {
			t.Fatalf("request %d was answered twice", int(id))
		}
		answers[int(id)] = m
		if len(answers)%1000 == 0 {
			t.Logf("%d answered", len(answers))
		}
	}
	if res, _ := answers[10]["result"].(map[string]any); res["stopReason"] != "cancelled" {
		t.Fatalf("the prompt was answered %v", answers[10])
	}
	ok := 0
	for i := 0; i < 5000; i++ {
		m, answered := answers[1000+i]
		if !answered {
			t.Fatalf("close %d was never answered", 1000+i)
		}
		if m["error"] == nil {
			ok++
		}
	}
	if ok < 1 {
		t.Fatal("no close was answered ok")
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

// ---- slow and silent clients -------------------------------------------------

func TestAttackClientThatNeverReadsWhileRunMakesMegabytes(t *testing.T) {
	old := flushGrace
	flushGrace = 300 * time.Millisecond
	t.Cleanup(func() { flushGrace = old })
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	_ = cr
	done := make(chan struct{})
	finished := make(chan time.Duration, 1)
	backend := testBackend(func(ctx context.Context, h *Host, _ string) error {
		start := time.Now()
		if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err == nil {
			t.Error("consent was given by a client that does not read")
		}
		finished <- time.Since(start)
		return nil
	})
	// This client never reads, so the consent request itself sits in the queue
	// unread. The run can only end on the timeout or the EOF.
	go func() {
		_ = Serve(context.Background(), ar, aw, Info{Name: "a"}, backend)
		close(done)
	}()
	_, _ = cw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"))
	// the answers are not read: the pipe blocks the outbox's writer
	time.Sleep(200 * time.Millisecond)
	_, _ = cw.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}` + "\n"))
	time.Sleep(200 * time.Millisecond)
	_ = cw.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestAttackRunProducesMegabytesOfProgressForSlowReader(t *testing.T) {
	// A reader that takes one byte per millisecond while the run writes 40 MB of
	// progress and then finishes: the run must not wait for the reader.
	old := flushGrace
	flushGrace = 500 * time.Millisecond
	t.Cleanup(func() { flushGrace = old })
	runDone := make(chan time.Duration, 1)
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		start := time.Now()
		big := strings.Repeat("x", 1<<20)
		for i := 0; i < 40; i++ {
			id := fmt.Sprint(i)
			h.Event(adapter.Event{Kind: adapter.EventToolCall, CallID: id, Tool: "t"})
			h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: id, Text: big})
		}
		runDone <- time.Since(start)
		return nil
	}))
	sid := c.handshake()
	go func() { // one byte per millisecond
		b := make([]byte, 1)
		for {
			if _, err := c.r.Read(b); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	select {
	case d := <-runDone:
		// One byte a millisecond would take hours to read 40 MB: a run that waited
		// for the reader never finishes, so finishing at all is the proof, and how
		// long it took on a loaded machine is only logged.
		t.Logf("run took %v", d)
	case <-time.After(90 * time.Second):
		t.Fatal("the run waited for the reader")
	}
}

// ---- frames -----------------------------------------------------------------

func TestAttackFrameShapes(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	big := strings.Repeat("a", MaxFrame) // with the envelope this is over the bound
	cases := []struct {
		name, line string
		wantCode   int // 0: no response expected
	}{
		{"huge frame", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"` + big + `"}}`, codeParse},
		{"frame of exactly the bound plus one", strings.Repeat("a", MaxFrame+1), codeParse},
		{"deeply nested", `{"jsonrpc":"2.0","id":2,"method":"x","params":` + strings.Repeat("[", 500000) + strings.Repeat("]", 500000) + `}`, codeParse},
		{"invalid utf8 in method", "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"a\xff\xfe\"}", codeInvalidRequest},
		{"raw NUL in string", "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"a\x00b\"}", codeParse},
		{"escaped NUL method", `{"jsonrpc":"2.0","id":5,"method":"a\u0000b"}`, codeInvalidRequest},
		{"id object", `{"jsonrpc":"2.0","id":{"a":[1,2,{"b":null}]},"method":"x"}`, codeInvalidRequest},
		{"id array", `{"jsonrpc":"2.0","id":[1,2],"method":"x"}`, codeInvalidRequest},
		{"id float", `{"jsonrpc":"2.0","id":1.5e300,"method":"x"}`, codeInvalidRequest},
		{"id string with escapes", `{"jsonrpc":"2.0","id":"a\"\\\n\u2028\ud800","method":"x"}`, codeInvalidRequest},
		{"id too long", `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 300) + `","method":"x"}`, codeInvalidRequest},
		{"id true", `{"jsonrpc":"2.0","id":true,"method":"x"}`, codeInvalidRequest},
		{"id null", `{"jsonrpc":"2.0","id":null,"method":"x"}`, codeInvalidRequest},
		{"uppercase keys", `{"JSONRPC":"2.0","ID":6,"METHOD":"x"}`, codeInvalidRequest},
		{"top-level array", `[1,2,3]`, codeParse},
		{"top-level null", `null`, codeInvalidRequest},
		{"top-level string", `"x"`, codeParse},
		{"bare number", `42`, codeParse},
		{"trailing garbage", `{"jsonrpc":"2.0","id":7,"method":"x"} trailing`, codeParse},
		{"two objects on a line", `{"jsonrpc":"2.0","id":8,"method":"x"}{"jsonrpc":"2.0","id":9,"method":"x"}`, codeParse},
		{"method not a string", `{"jsonrpc":"2.0","id":10,"method":5}`, codeParse},
		{"BOM first", "\xef\xbb\xbf" + `{"jsonrpc":"2.0","id":11,"method":"x"}`, codeParse},
		{"CR only line ends", `{"jsonrpc":"2.0","id":12,"method":"x"}` + "\r", codeInvalidRequest},
		{"lone U+2028 in a string", "{\"jsonrpc\":\"2.0\",\"id\":13,\"method\":\"a\u2028b\"}", codeInvalidRequest},
	}
	for _, tc := range cases {
		c.sendRaw(tc.line + "\n")
		if tc.wantCode == 0 {
			continue
		}
		m := c.recv()
		if errCode(m) != tc.wantCode {
			t.Errorf("%s: want %d, got %v", tc.name, tc.wantCode, truncate(m))
		}
		// an echoed id must still be valid JSON on the wire (recv parsed it) and no
		// larger than the bound
		if b, _ := json.Marshal(m); len(b) > 1024 {
			t.Errorf("%s: a response of %d bytes", tc.name, len(b))
		}
	}
	// the connection still works
	c.send(req(100, "initialize", map[string]any{"protocolVersion": 1}))
	if r, _ := c.replyTo(100); r["error"] != nil {
		t.Fatalf("initialize after the abuse: %v", r)
	}
}

func truncate(m map[string]any) string {
	b, _ := json.Marshal(m)
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

func TestAttackInvalidUTF8IDIsEchoedAsValidJSON(t *testing.T) {
	// An id string with invalid UTF-8 is echoed raw: the response is then not
	// valid UTF-8. A strict client rejects the frame.
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	go func() {
		_ = Serve(context.Background(), ar, aw, Info{Name: "a"}, neverRuns(t))
		_ = aw.Close()
	}()
	go func() {
		_, _ = cw.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":\"a\xff\",\"method\":\"x\"}\n"))
		time.Sleep(200 * time.Millisecond)
		_ = cw.Close()
	}()
	b, _ := io.ReadAll(cr)
	if !isValidUTF8(b) {
		t.Fatalf("the agent wrote invalid UTF-8 to the client: %q", b)
	}
}

func isValidUTF8(b []byte) bool { return strings.ToValidUTF8(string(b), "\ufffd") == string(b) }

func TestAttackExtensionAndClientSideMethods(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	sid := c.handshake()
	for i, m := range []string{
		"fs/read_text_file", "fs/write_text_file", "terminal/create", "terminal/output", "terminal/kill", "terminal/release", "terminal/wait_for_exit",
		"session/load", "session/set_mode", "session/set_model", "session/set_config_option", "session/list", "session/fork", "session/resume",
		"authenticate", "$/cancel_request", "_vendor/custom", "session/request_permission", "session/update", "initialize/", " ", "SESSION/PROMPT", "session/prompt ", "session/prompt\u0000",
	} {
		id := 500 + i
		c.send(req(id, m, map[string]any{"sessionId": sid, "path": "/etc/passwd"}))
		r, _ := c.replyTo(id)
		want := codeMethodNotFound
		if errCode(r) != want {
			t.Errorf("method %q: %v", m, truncate(r))
		}
		if msg, _ := r["error"].(map[string]any)["message"].(string); strings.ContainsAny(msg, "\x00\x1b") {
			t.Errorf("method %q echoed a control character: %q", m, msg)
		}
	}
	// as notifications: no answer, no effect
	for _, m := range []string{"fs/read_text_file", "$/cancel_request", "session/load", "_x", "initialize", "session/new", "session/prompt", "session/close"} {
		c.send(map[string]any{"jsonrpc": "2.0", "method": m, "params": map[string]any{"sessionId": sid}})
	}
	c.send(req(900, "nope", nil))
	if r, _ := c.replyTo(900); errCode(r) != codeMethodNotFound {
		t.Fatalf("ping: %v", r)
	}
}

func TestAttackSessionNewShapes(t *testing.T) {
	for name, params := range map[string]string{
		"mcpServers object":      `{"cwd":"/","mcpServers":{}}`,
		"mcpServers one":         `{"cwd":"/","mcpServers":[{"name":"x"}]}`,
		"mcpServers null items":  `{"cwd":"/","mcpServers":[null]}`,
		"additionalDirectories":  `{"cwd":"/","mcpServers":[],"additionalDirectories":["/"]}`,
		"additionalDirs null el": `{"cwd":"/","additionalDirectories":[null]}`,
		"cwd number":             `{"cwd":5}`,
		"params array":           `[]`,
		"params string":          `"x"`,
	} {
		t.Run(name, func(t *testing.T) {
			c := startRaw(t, neverRuns(t))
			c.send(req(1, "initialize", map[string]any{"protocolVersion": 1}))
			c.replyTo(1)
			c.sendRaw(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":` + params + "}\n")
			if r, _ := c.replyTo(2); r["error"] == nil {
				t.Fatalf("accepted %s", params)
			}
			// the failed attempt did not burn the one session
			c.sendRaw(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}` + "\n")
			if r, _ := c.replyTo(3); r["error"] != nil {
				t.Fatalf("a good session/new after: %v", r)
			}
		})
	}
	// null params / missing params for initialize and new
	c := startRaw(t, neverRuns(t))
	c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	if r, _ := c.replyTo(1); errCode(r) != codeInvalidParams {
		t.Fatalf("initialize without params: %v", r)
	}
	c.sendRaw(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1.0}}` + "\n")
	r, _ := c.replyTo(2)
	t.Logf("protocolVersion 1.0 -> %v", truncate(r))
	c.sendRaw(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":1}}` + "\n")
	c.replyTo(3)
	c.sendRaw(`{"jsonrpc":"2.0","id":4,"method":"session/new"}` + "\n")
	if r, _ := c.replyTo(4); r["error"] == nil {
		t.Logf("session/new without params accepted (cwd is unused): %v", truncate(r))
	}
}

// ---- shutdown -----------------------------------------------------------------

var ownFuncs = []string{"acpagent.Serve", "(*outbox).loop", "(*server).readLoop", "(*server).runBackend", "(*forwarder).run"}

func ownGoroutines() []string {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	var out []string
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		for _, f := range ownFuncs {
			if strings.Contains(g, f) && !strings.Contains(g, "testing.tRunner") {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func acpGoroutines() int { return len(ownGoroutines()) }

func waitNoLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if acpGoroutines() <= base {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines left (%d, base %d):\n%s", acpGoroutines(), base, strings.Join(ownGoroutines(), "\n\n"))
}

func TestAttackEOFInEveryStateEndsCleanly(t *testing.T) {
	setConsentTimeout(t, time.Minute)
	type stage struct {
		name string
		run  func(c *rawClient, started <-chan struct{}) // drive to the state, then EOF follows
	}
	stages := []stage{
		{"before initialize", func(c *rawClient, _ <-chan struct{}) {}},
		{"after initialize", func(c *rawClient, _ <-chan struct{}) {
			c.send(req(1, "initialize", map[string]any{"protocolVersion": 1}))
			c.replyTo(1)
		}},
		{"idle", func(c *rawClient, _ <-chan struct{}) { c.handshake() }},
		{"during consent", func(c *rawClient, started <-chan struct{}) {
			sid := c.handshake()
			c.send(req(10, "session/prompt", prompt(sid, "go")))
			<-started
		}},
		{"mid-run", func(c *rawClient, started <-chan struct{}) {
			sid := c.handshake()
			c.send(req(10, "session/prompt", prompt(sid, "go")))
			<-started
			for {
				m := c.recv()
				if m["method"] == "session/request_permission" {
					c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
					return
				}
			}
		}},
		{"after the session ended", func(c *rawClient, _ <-chan struct{}) {
			sid := c.handshake()
			c.send(req(10, "session/close", map[string]any{"sessionId": sid}))
			c.replyTo(10)
		}},
	}
	for _, st := range stages {
		t.Run(st.name, func(t *testing.T) {
			base := acpGoroutines()
			started := make(chan struct{}, 1)
			var ranTeardown atomic.Bool
			cr, aw := io.Pipe()
			ar, cw := io.Pipe()
			c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan struct{})}
			go func() {
				defer close(c.done)
				_ = Serve(context.Background(), ar, aw, Info{Name: "a"}, testBackend(func(ctx context.Context, h *Host, _ string) error {
					select {
					case started <- struct{}{}:
					default:
					}
					if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err != nil {
						ranTeardown.Store(true) // teardown after refusal
						return err
					}
					<-ctx.Done() // mid-run: wait to be stopped
					time.Sleep(50 * time.Millisecond)
					ranTeardown.Store(true)
					return context.Cause(ctx)
				}))
				_ = aw.Close()
			}()
			st.run(c, started)
			_ = cw.Close()
			go func() { _, _ = io.Copy(io.Discard, cr) }()
			select {
			case <-c.done:
			case <-time.After(10 * time.Second):
				t.Fatal("Serve did not return on stdin EOF")
			}
			if st.name == "during consent" || st.name == "mid-run" {
				if !ranTeardown.Load() {
					t.Fatal("Serve returned before the run's teardown finished")
				}
			}
			waitNoLeak(t, base)
		})
	}
}

// ---- deliverables through the Host -------------------------------------------

func TestAttackTheFinalAnswerFollowsEveryUpdate(t *testing.T) {
	dir := t.TempDir()
	ds := &receipt.Deliverables{Files: []receipt.Deliverable{put(t, dir, "reports/a.md", "# a\n"), put(t, dir, "reports/b.md", "# b\n")}}
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err != nil {
			return err
		}
		for i := 0; i < 50; i++ {
			h.Say(fmt.Sprintf("narration %d", i))
		}
		h.Finish(&receipt.Envelope{Result: json.RawMessage(`"done"`), OutDir: dir, Deliverables: ds}, nil)
		return nil
	}))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	for {
		m := c.recv()
		if m["method"] == "session/request_permission" {
			c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
			break
		}
	}
	updates := 0
	for {
		m := c.recv()
		if m["method"] == "session/update" {
			updates++
			continue
		}
		if fmt.Sprint(m["id"]) == "10" {
			break
		}
	}
	if updates < 50+1+1+2 {
		t.Fatalf("the answer overtook updates: %d seen first", updates)
	}
}

func TestAttackHugeResultKillsTheConnectionSilently(t *testing.T) {
	// A result text above the outbox's keep-limit (24 MiB) makes the connection
	// "dead": everything queued after it, the answer to the prompt included, is
	// dropped, and the client is never told.
	if testing.Short() {
		t.Skip()
	}
	huge := strings.Repeat("x", 25<<20)
	raw, _ := json.Marshal(huge)
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err != nil {
			return err
		}
		h.Finish(&receipt.Envelope{Result: raw}, nil)
		return nil
	}))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "go")))
	for {
		m := c.recv()
		if m["method"] == "session/request_permission" {
			c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
			break
		}
	}
	got := make(chan bool, 1)
	go func() {
		for {
			m := c.recvLong()
			if m == nil {
				got <- false
				return
			}
			if fmt.Sprint(m["id"]) == "10" {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the connection ended without the prompt's answer")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the prompt was never answered: a large result silently killed the outbox")
	}
}

func (c *rawClient) recvLong() map[string]any {
	l, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(l, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// A session/close in the idle state (no prompt, no run) marks the session over
// without a backend ever starting; Serve then waits for a backendDone that is
// never closed (st >= stStarting is read as "a run was started"), so the
// process never exits, whether the client closes stdin or not.
func TestAttackCloseBeforeAnyPromptDoesNotHangServe(t *testing.T) {
	old := lingerAfterEnd
	lingerAfterEnd = 200 * time.Millisecond
	t.Cleanup(func() { lingerAfterEnd = old })
	for _, eof := range []bool{true, false} {
		t.Run(fmt.Sprintf("client closes stdin=%v", eof), func(t *testing.T) {
			cr, aw := io.Pipe()
			ar, cw := io.Pipe()
			c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan struct{})}
			go func() {
				defer close(c.done)
				_ = Serve(context.Background(), ar, aw, Info{Name: "a"}, neverRuns(t))
				_ = aw.Close()
			}()
			sid := c.handshake()
			c.send(req(10, "session/close", map[string]any{"sessionId": sid}))
			if r, _ := c.replyTo(10); r["error"] != nil {
				t.Fatalf("close: %v", r)
			}
			if eof {
				_ = cw.Close()
			}
			go func() { _, _ = io.Copy(io.Discard, cr) }()
			select {
			case <-c.done:
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return after a session/close in the idle state")
			}
		})
	}
}

// A frame with an id but no method, result or error is neither a request nor a
// reply: JSON-RPC says Invalid Request, the agent says nothing.
func TestAttackIDWithoutMethodIsAnswered(t *testing.T) {
	c := startRaw(t, neverRuns(t))
	c.handshake()
	c.sendRaw(`{"jsonrpc":"2.0","id":777}` + "\n")
	c.sendRaw(`{"jsonrpc":"2.0","id":778,"method":"nope"}` + "\n")
	m := c.recv()
	if fmt.Sprint(m["id"]) != "777" {
		t.Fatalf("the frame with id 777 and no method was not answered; the next frame is %v", truncate(m))
	}
}

// ---- interactive sessions -----------------------------------------------------

type slowSession struct {
	events  chan session.Event
	mu      sync.Mutex
	ended   bool
	endCall atomic.Int32
	inSend  chan struct{}
	sends   atomic.Int32
	silent  bool // a cancelled turn emits no closing event (the engine's session emits KindError)
}

func newSlowSession() *slowSession {
	return &slowSession{events: make(chan session.Event, 64), inSend: make(chan struct{}, 8)}
}
func (f *slowSession) Events() <-chan session.Event { return f.events }
func (f *slowSession) Send(ctx context.Context, text string) error {
	f.sends.Add(1)
	if text == "quick" {
		f.events <- session.Event{Kind: session.KindAssistantText, Text: "hi"}
		f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "hi"}
		return nil
	}
	f.inSend <- struct{}{}
	<-ctx.Done() // a turn that never ends by itself
	if !f.silent {
		f.events <- session.Event{Kind: session.KindError, Text: ctx.Err().Error()}
	}
	return ctx.Err()
}
func (f *slowSession) AddInput(context.Context, string) error { return nil }
func (f *slowSession) End(context.Context, bool) (*receipt.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endCall.Add(1)
	if !f.ended {
		f.ended = true
		close(f.events)
	}
	return &receipt.Envelope{}, nil
}
func (f *slowSession) Ended() (bool, session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, session.EndQuit
}

func interactiveBackend(fs *slowSession, finished *atomic.Bool) testBackend {
	return func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err != nil {
			return err
		}
		_ = h.Converse(ctx, fs)
		env, err := fs.End(context.Background(), false)
		h.Finish(env, err)
		finished.Store(true)
		return err
	}
}

func allowConsent(c *rawClient) {
	for {
		m := c.recv()
		if m["method"] == "session/request_permission" {
			c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
			return
		}
	}
}

func TestAttackInteractiveCancelCloseAndEOFMidTurn(t *testing.T) {
	for _, how := range []string{"cancel", "close", "eof", "second prompt then cancel"} {
		t.Run(how, func(t *testing.T) {
			fs := newSlowSession()
			var finished atomic.Bool
			cr, aw := io.Pipe()
			ar, cw := io.Pipe()
			c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan struct{})}
			go func() {
				defer close(c.done)
				_ = Serve(context.Background(), ar, aw, Info{Name: "a"}, interactiveBackend(fs, &finished))
				_ = aw.Close()
			}()
			sid := c.handshake()
			c.send(req(10, "session/prompt", prompt(sid, "long")))
			allowConsent(c)
			<-fs.inSend
			switch how {
			case "cancel":
				c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
			case "close":
				c.send(req(11, "session/close", map[string]any{"sessionId": sid}))
			case "eof":
				_ = cw.Close()
			case "second prompt then cancel":
				c.send(req(12, "session/prompt", prompt(sid, "quick")))
				if r, _ := c.replyTo(12); r["error"] == nil {
					t.Fatalf("a second prompt during a turn: %v", r)
				}
				c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
			}
			if how != "eof" {
				r, _ := c.replyTo(10)
				if res, _ := r["result"].(map[string]any); res["stopReason"] != "cancelled" {
					t.Fatalf("the turn was answered %v", truncate(r))
				}
				if how == "close" {
					if r, _ := c.replyTo(11); r["error"] != nil {
						t.Fatalf("close: %v", r)
					}
				}
				// prompt afterwards: the session is over
				c.send(req(13, "session/prompt", prompt(sid, "quick")))
				if r, _ := c.replyTo(13); errCode(r) != codeInvalidRequest {
					t.Fatalf("prompt after the session ended: %v", truncate(r))
				}
				_ = cw.Close()
			}
			go func() { _, _ = io.Copy(io.Discard, cr) }()
			select {
			case <-c.done:
			case <-time.After(10 * time.Second):
				t.Fatal("Serve did not return")
			}
			if fs.endCall.Load() != 1 || !finished.Load() {
				t.Fatalf("teardown: End called %d times, finished=%v", fs.endCall.Load(), finished.Load())
			}
		})
	}
}

func TestAttackInteractiveCloseBetweenTurnsAnswersAfterTheTeardownUpdates(t *testing.T) {
	dir := t.TempDir()
	ds := &receipt.Deliverables{Files: []receipt.Deliverable{put(t, dir, "reports/a.md", "# a\n")}}
	fs := newSlowSession()
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		if err := h.Consent(ctx, Consent{Title: "t", Text: "x"}); err != nil {
			return err
		}
		_ = h.Converse(ctx, fs)
		env, err := fs.End(context.Background(), false)
		env.OutDir, env.Deliverables = dir, ds
		h.Finish(env, err)
		return err
	}))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "quick")))
	allowConsent(c)
	r, _ := c.replyTo(10)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "end_turn" {
		t.Fatalf("turn: %v", truncate(r))
	}
	c.send(req(11, "session/close", map[string]any{"sessionId": sid}))
	r, others := c.replyTo(11)
	if r["error"] != nil {
		t.Fatalf("close: %v", r)
	}
	res := 0
	for _, m := range others {
		if m["method"] == "session/update" {
			b, _ := json.Marshal(m)
			if strings.Contains(string(b), `"resource"`) {
				res++
			}
		}
	}
	if res != 1 {
		t.Fatalf("the deliverable was not handed back before the close was answered (%d)", res)
	}
}

func TestAttackConcurrentPromptsAreOneRun(t *testing.T) {
	var runs atomic.Int32
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		runs.Add(1)
		<-ctx.Done()
		return context.Cause(ctx)
	}))
	sid := c.handshake()
	for i := 0; i < 200; i++ {
		c.send(req(1000+i, "session/prompt", prompt(sid, "go")))
	}
	// 199 refusals arrive; the run's own answer is pending
	seen := 0
	for seen < 199 {
		m := c.recv()
		if m["error"] != nil && errCode(m) == codeInvalidRequest {
			seen++
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
	r, _ := c.replyTo(1000)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "cancelled" {
		t.Fatalf("answer %v", r)
	}
}

// A cancel during a turn is answered at once: the turn's ctx ends, Send
// returns, and the answer is "cancelled". turn() first waits up to 5 s for an
// assistant_turn event that a cancelled turn never produces.
func TestAttackCancelMidTurnIsAnsweredPromptly(t *testing.T) {
	for _, silent := range []bool{false, true} {
		t.Run(fmt.Sprintf("silent=%v", silent), func(t *testing.T) { cancelMidTurn(t, silent) })
	}
}

func cancelMidTurn(t *testing.T, silent bool) {
	fs := newSlowSession()
	fs.silent = silent
	var finished atomic.Bool
	c := startRaw(t, interactiveBackend(fs, &finished))
	sid := c.handshake()
	c.send(req(10, "session/prompt", prompt(sid, "long")))
	allowConsent(c)
	<-fs.inSend
	start := time.Now()
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
	r, _ := c.replyTo(10)
	d := time.Since(start)
	if res, _ := r["result"].(map[string]any); res["stopReason"] != "cancelled" {
		t.Fatalf("answer %v", r)
	}
	// The fault was a wait of five seconds for an event that does not come.
	if d > 4*time.Second {
		t.Fatalf("the cancel was answered after %v", d)
	}
}
