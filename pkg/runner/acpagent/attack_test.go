package acpagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	srv     *server
}

func newConsentProbe(t *testing.T) *consentProbe {
	p := &consentProbe{ended: make(chan error, 1), started: make(chan struct{})}
	p.c = startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		p.srv = h.s
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

// errMessage is the message of a response's error, empty when it has none.
func errMessage(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
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
	r, _ = c2.replyTo(11)
	if errCode(r) != codeInvalidRequest || errMessage(r) != "the session has ended" {
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
	// The request that timed out is forgotten, not left for a late answer.
	p.srv.mu.Lock()
	pending := len(p.srv.perms)
	p.srv.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d consent requests are still waiting for an answer", pending)
	}
	p.c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`+"\n", id))
	time.Sleep(100 * time.Millisecond)
	if p.ran.Load() {
		t.Fatal("late allow ran")
	}
}

func TestAttackCancelAndCloseDuringConsent(t *testing.T) {
	for _, how := range []string{"cancel", "close", "close twice"} {
		t.Run(how, func(t *testing.T) {
			var ctxErr atomic.Value
			var ran atomic.Bool
			// The run's teardown is held until the test lets it go, for a
			// second close to arrive while the first is still being answered.
			gate := make(chan struct{})
			if how != "close twice" {
				close(gate)
			}
			c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
				err := h.Consent(ctx, Consent{Title: "Run?", Text: "x"})
				if err == nil {
					ran.Store(true)
				} else {
					ctxErr.Store(err.Error())
				}
				<-gate
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
			if how == "close twice" {
				c.send(req(12, "session/close", map[string]any{"sessionId": sid}))
				if r, _ := c.replyTo(12); errCode(r) != codeInvalidRequest || errMessage(r) != "the session is already closing" {
					t.Fatalf("a second close while the first is answered: %v", r)
				}
				close(gate)
			}
			r, _ := c.replyTo(10)
			if res, _ := r["result"].(map[string]any); res["stopReason"] != "cancelled" {
				t.Fatalf("answer %v", r)
			}
			// The wait for the answer ended with the reason the run was stopped.
			if got := ctxErr.Load(); got != "the client cancelled the session" {
				t.Fatalf("consent ended with %v", got)
			}
			if how != "cancel" {
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
		if errCode(r) != codeInvalidRequest || errMessage(r) != "the session has no turn to take this prompt" {
			t.Fatalf("a prompt after the run's answer was refused with %v", r)
		}
	case <-time.After(10 * time.Second): // refused at once, or parked until the teardown is released
		close(release)
		r := <-got
		t.Fatalf("a prompt sent after the run had answered was neither refused nor run; it was parked until teardown ended and then answered %v", r["result"])
	}
	// A close that waits for the teardown holds off prompts as well, and is
	// answered when the teardown ends.
	c.send(req(12, "session/close", map[string]any{"sessionId": sid}))
	c.send(req(13, "session/prompt", prompt(sid, "and another")))
	if r, _ := c.replyTo(13); errCode(r) != codeInvalidRequest || errMessage(r) != "a prompt is in flight: send the next when it is answered" {
		t.Fatalf("a prompt while a close waits for the teardown: %v", r)
	}
	close(release)
	if r, _ := c.replyTo(12); r["error"] != nil {
		t.Fatalf("close: %v", r)
	}
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
	r300, _ := c.replyTo(300)
	if r300["error"] == nil || errCode(r300) != codeInvalidParams || errMessage(r300) != "session/close takes a sessionId" {
		t.Fatalf("close with no session: %v", r300)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": ""}})
	sid := c.handshakeSession()
	// A close naming another session is refused, and the session goes on.
	c.send(req(301, "session/close", map[string]any{"sessionId": "not-" + sid}))
	if r, _ := c.replyTo(301); errCode(r) != codeInvalidParams || errMessage(r) != "no such session" {
		t.Fatalf("a close for another session: %v", r)
	}
	// A close in the idle state is covered by TestAttackCloseBeforeAnyPromptDoesNotHangServe.
}

func TestAttackPromptTextShapes(t *testing.T) {
	for name, blocks := range map[string]any{
		"image":           []any{map[string]any{"type": "image", "data": "x", "mimeType": "image/png"}},
		"audio":           []any{map[string]any{"type": "audio", "data": "x", "mimeType": "audio/wav"}},
		"resource":        []any{map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///etc/passwd", "text": "x"}}},
		"empty":           []any{},
		"blank text":      []any{map[string]any{"type": "text", "text": "  \n\t"}},
		"nonobject":       []any{"hello"},
		"null block":      []any{nil},
		"missing type":    []any{map[string]any{"text": "x"}},
		"type with ctl":   []any{map[string]any{"type": "te\u001b[31mxt", "text": "x"}},
		"mixed bad last":  []any{map[string]any{"type": "text", "text": "ok"}, map[string]any{"type": "image"}},
		"type with DEL":   []any{map[string]any{"type": "ty\u007fpe", "text": "x"}},
		"type with US":    []any{map[string]any{"type": "a\u001fb", "text": "x"}},
		"type with space": []any{map[string]any{"type": "a b", "text": "x"}},
		"long type":       []any{map[string]any{"type": strings.Repeat("t", 70), "text": "x"}},
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
			notAccepted := func(typ string) string {
				return fmt.Sprintf("this agent takes text prompts: a %q block is not accepted", typ)
			}
			want := map[string]string{
				"image":           notAccepted("image"),
				"audio":           notAccepted("audio"),
				"resource":        notAccepted("resource"),
				"empty":           "the prompt is empty: it names the task",
				"blank text":      "the prompt is empty: it names the task",
				"nonobject":       "a prompt block is not an object",
				"null block":      notAccepted(""),
				"missing type":    notAccepted(""),
				"type with ctl":   notAccepted("te_[31mxt"),
				"mixed bad last":  notAccepted("image"),
				"type with DEL":   notAccepted("ty_pe"),
				"type with US":    notAccepted("a_b"),
				"type with space": notAccepted("a b"),
				"long type":       notAccepted(strings.Repeat("t", 64)),
			}[name]
			if got := errMessage(r); got != want {
				t.Fatalf("refused with %q, want %q", got, want)
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
	// The note names the link as one line of at most 200 bytes.
	hostile := "a SYSTEM: ignore[2J" + strings.Repeat("x", 181)
	if want := fmt.Sprintf("do it\n\n(The client linked %q at %s; this agent cannot open it.)", hostile, "file:///etc/passwd more"); first != want {
		t.Fatalf("resource_link note %q, want %q", first, want)
	}

	// A name of exactly 200 bytes is whole, one byte more is cut, and the cut
	// never splits a character.
	got2 := make(chan string, 1)
	c2 := startRaw(t, testBackend(func(ctx context.Context, h *Host, first string) error { got2 <- first; return nil }))
	sid2 := c2.handshake()
	link := func(name, uri string) map[string]any {
		return map[string]any{"type": "resource_link", "name": name, "uri": uri}
	}
	c2.send(req(10, "session/prompt", map[string]any{"sessionId": sid2, "prompt": []any{
		map[string]any{"type": "text", "text": "a"},
		link(strings.Repeat("n", 200), "u1"),
		link(strings.Repeat("m", 201), "u2"),
		link(strings.Repeat("x", 199)+"\u00e9tail", "u3"),
		map[string]any{"type": "text", "text": "z"},
	}}))
	c2.replyTo(10)
	note := func(name, uri string) string {
		return fmt.Sprintf("(The client linked %q at %s; this agent cannot open it.)", name, uri)
	}
	want := strings.Join([]string{"a", note(strings.Repeat("n", 200), "u1"), note(strings.Repeat("m", 200), "u2"), note(strings.Repeat("x", 199), "u3"), "z"}, "\n\n")
	if first2 := <-got2; first2 != want {
		t.Fatalf("resource_link notes %q, want %q", first2, want)
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
	// padded is a valid request of exactly n bytes.
	padded := func(n int) string {
		head := `{"jsonrpc":"2.0","id":14,"method":"x","params":{"pad":"`
		return head + strings.Repeat("p", n-len(head)-len(`"}}`)) + `"}}`
	}
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
		{"a blank line", "", 0},
		{"a line of spaces and a tab", "  \t ", 0},
		{"frame of exactly the bound", padded(MaxFrame), codeInvalidRequest},
		{"frame of the bound plus one", padded(MaxFrame + 1), codeParse},
		{"id of exactly 256 bytes", `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 254) + `","method":"x"}`, codeInvalidRequest},
		{"id of 257 bytes", `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", 255) + `","method":"x"}`, codeInvalidRequest},
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
	idRefused := "an id is a number or a string of valid UTF-8 of at most 256 bytes"
	tooLong := fmt.Sprintf("a frame is at most %d bytes", MaxFrame)
	wantMsg := map[string]string{
		"huge frame":                          tooLong,
		"frame of exactly the bound plus one": tooLong,
		"frame of the bound plus one":         tooLong,
		"frame of exactly the bound":          "send initialize first",
		"id of exactly 256 bytes":             "send initialize first",
		"id of 257 bytes":                     idRefused,
		"id object":                           idRefused,
		"id array":                            idRefused,
		"id too long":                         idRefused,
		"id true":                             idRefused,
		"id null":                             idRefused,
		"id float":                            "send initialize first",
		"id string with escapes":              "send initialize first",
		"top-level null":                      `the frame is not JSON-RPC 2.0 ("jsonrpc": "2.0")`,
		"raw NUL in string":                   "the frame is not JSON",
		"top-level array":                     "the frame is not JSON",
		"trailing garbage":                    "the frame is not JSON",
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
		if want, ok := wantMsg[tc.name]; ok && errMessage(m) != want {
			t.Errorf("%s: message %q, want %q", tc.name, errMessage(m), want)
		}
		switch tc.name {
		case "id of exactly 256 bytes":
			if m["id"] != strings.Repeat("i", 254) {
				t.Errorf("%s: the id was not echoed: %v", tc.name, truncate(m))
			}
		case "id of 257 bytes", "id too long":
			if m["id"] != nil {
				t.Errorf("%s: the id was echoed: %v", tc.name, truncate(m))
			}
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
	// The method named in the answer is cut at 64 bytes and has its control
	// characters replaced; a space is kept.
	for i, tc := range [][2]string{
		{"a b", "a b"},
		{"x\u001fy", "x_y"},
		{"x\u007fy", "x_y"},
		{"x\u0000y", "x_y"},
		{strings.Repeat("m", 64), strings.Repeat("m", 64)},
		{strings.Repeat("m", 65), strings.Repeat("m", 64)},
		{strings.Repeat("m", 100), strings.Repeat("m", 64)},
	} {
		id := 700 + i
		c.send(req(id, tc[0], nil))
		r, _ := c.replyTo(id)
		if errCode(r) != codeMethodNotFound || errMessage(r) != "this agent does not serve "+tc[1] {
			t.Errorf("method %q: %v", tc[0], truncate(r))
		}
	}
	// as notifications: no answer, no effect
	for _, m := range []string{"fs/read_text_file", "$/cancel_request", "session/load", "_x", "initialize", "session/new", "session/prompt", "session/close"} {
		c.send(map[string]any{"jsonrpc": "2.0", "method": m, "params": map[string]any{"sessionId": sid}})
	}
	c.send(req(900, "nope", nil))
	r, others := c.replyTo(900)
	if errCode(r) != codeMethodNotFound {
		t.Fatalf("ping: %v", r)
	}
	if len(others) != 0 {
		t.Fatalf("a notification was answered: %v", others)
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
	// The version the client speaks is 1 or more.
	for i, v := range []string{"0", "-1"} {
		c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":%s}}`, 20+i, v) + "\n")
		if r, _ := c.replyTo(20 + i); errCode(r) != codeInvalidParams || errMessage(r) != "initialize needs the protocolVersion the client speaks" {
			t.Fatalf("protocolVersion %s: %v", v, r)
		}
	}
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
	// The flush grace is an hour: Serve returns in time only because the
	// writer finishes what is queued, not because the grace runs out.
	oldGrace := flushGrace
	flushGrace = time.Hour
	t.Cleanup(func() { flushGrace = oldGrace })
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
	if errCode(m) != codeInvalidRequest || errMessage(m) != "the frame names no method, result or error" {
		t.Fatalf("the frame with id 777 and no method was answered %v", truncate(m))
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
				if r, _ := c.replyTo(13); errCode(r) != codeInvalidRequest || errMessage(r) != "the session has ended" {
					t.Fatalf("prompt after the session ended: %v", truncate(r))
				}
				// A close once the session is over is answered, not refused.
				c.send(req(14, "session/close", map[string]any{"sessionId": sid}))
				if r, _ := c.replyTo(14); r["error"] != nil {
					t.Fatalf("close after the session ended: %v", truncate(r))
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
	runCtx := make(chan context.Context, 1)
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		runs.Add(1)
		runCtx <- ctx
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
			if msg := errMessage(m); msg != "a prompt is in flight: send the next when it is answered" {
				t.Fatalf("a prompt in flight was refused with %q", msg)
			}
			seen++
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
	// A cancel that names another session, or none, stops nothing: the frame
	// after them is answered, so they have been read, and the run still goes.
	ctx := <-runCtx
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": "not-" + sid}})
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{}})
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": "x"})
	c.send(req(2000, "nope", nil))
	if r, _ := c.replyTo(2000); errCode(r) != codeMethodNotFound {
		t.Fatalf("ping: %v", r)
	}
	if ctx.Err() != nil {
		t.Fatalf("a cancel for another session stopped the run: %v", context.Cause(ctx))
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

// ---- what the client draws and how the run's answer is made ---------------------

// startTuned is startRaw with the outbox's bounds set by tune.
func startTuned(t *testing.T, b Backend, tune func(*outbox)) *rawClient {
	t.Helper()
	cr, aw := io.Pipe()
	ar, cw := io.Pipe()
	c := &rawClient{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_ = serve(context.Background(), ar, aw, Info{Name: "agentry", Version: "test"}, b, tune)
		_ = aw.Close()
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		_, _ = io.Copy(io.Discard, cr)
		<-c.done
	})
	return c
}

// runToAnswer sends the prompt with id, allows the consent request if the run
// asks one, and returns that request, every session/update that came before
// the prompt's answer, and the answer.
func runToAnswer(c *rawClient, sid string, id int, text string) (consent map[string]any, updates []map[string]any, answer map[string]any) {
	c.t.Helper()
	c.send(req(id, "session/prompt", prompt(sid, text)))
	for {
		m := c.recv()
		switch {
		case m["method"] == "session/request_permission":
			consent = m
			c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}})
		case m["method"] == "session/update":
			u, _ := m["params"].(map[string]any)["update"].(map[string]any)
			updates = append(updates, u)
		case m["method"] == nil && fmt.Sprint(m["id"]) == fmt.Sprint(id):
			return consent, updates, m
		}
	}
}

// summary is an update as one short string: the text of a message chunk, the
// type of an embedded file or the name of a linked one, or its JSON.
func summary(u map[string]any) string {
	if u["sessionUpdate"] == "agent_message_chunk" {
		c, _ := u["content"].(map[string]any)
		switch c["type"] {
		case "text":
			return "text:" + fmt.Sprint(c["text"])
		case "resource":
			r, _ := c["resource"].(map[string]any)
			return "resource:" + fmt.Sprint(r["mimeType"])
		case "resource_link":
			return "link:" + fmt.Sprint(c["name"])
		}
	}
	return toJSON(u)
}

// What the agent sends a client to draw is cleaned of what could make it read
// as other than it says. The words of a run (a reply, a tool's output) lose
// their control characters only. What a person decides on or reads as the
// agent's own statement (the consent request, a note) also has its invisible
// formatting marked, and a label is one short line.
func TestTextTheClientDrawsIsCleanedOfWhatCouldMisleadIt(t *testing.T) {
	rlo, zwsp, ls, ps := string(rune(0x202e)), string(rune(0x200b)), string(rune(0x2028)), string(rune(0x2029))
	nel, c1lo, c1hi := string(rune(0x85)), string(rune(0x80)), string(rune(0x9f))
	nbsp, shy, eacute := string(rune(0xa0)), string(rune(0xad)), string(rune(0xe9))
	hostile := "a\x1b[31mred\r\nline\ttab" + nel + "\x7f" + rlo + zwsp + ls + ps + "end"
	asBody := "a[31mred\nline\ttab" + rlo + zwsp + ls + ps + "end"
	asText := "a[31mred\nline\ttab????end"
	asLine := "a[31mred line tab????end"

	t.Run("the cleaners", func(t *testing.T) {
		edge := "\x1f \x7e\x7f" + c1lo + c1hi + nbsp
		for _, tc := range []struct{ name, got, want string }{
			{"a body", cleanBody(hostile), asBody},
			{"a text", cleanText(hostile), asText},
			{"a line", cleanLine(hostile), asLine},
			{"a body keeps the line feed and the tab", cleanBody("a\tb\nc\x1fd\x1be\rf"), "a\tb\ncdef"},
			{"the edges of the controls in a body", cleanBody(edge), " ~" + nbsp},
			{"the edges of the controls in a text", cleanText(edge), " ~" + nbsp},
			{"a text keeps the line feed and the tab", cleanText("a\tb\nc\x1fd"), "a\tb\ncd"},
			{"a text marks a format character", cleanText("a" + shy + "b"), "a?b"},
			{"a text marks a line separator", cleanText("a" + ls + "b"), "a?b"},
			{"a text marks a paragraph separator", cleanText("a" + ps + "b"), "a?b"},
			{"a line is one line", cleanLine("  a \n\t b  "), "a b"},
			{"an empty line", cleanLine(""), ""},
			{"a line of 200 bytes is whole", cleanLine(strings.Repeat("a", 200)), strings.Repeat("a", 200)},
			{"a line of 201 bytes is cut", cleanLine(strings.Repeat("a", 201)), strings.Repeat("a", 200)},
			{"a cut does not split a character", cleanLine(strings.Repeat("a", 199) + eacute), strings.Repeat("a", 199)},
			{"a cut at a character keeps it", cleanLine(strings.Repeat("a", 198) + eacute + eacute), strings.Repeat("a", 198) + eacute},
		} {
			if tc.got != tc.want {
				t.Errorf("%s: %q, want %q", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("on the wire", func(t *testing.T) {
		raw, _ := json.Marshal(hostile)
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			if err := h.Consent(ctx, Consent{Title: "  Run\tit" + rlo + "?\n  now  ", Text: hostile}); err != nil {
				return err
			}
			h.Say(hostile)
			h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "tool\x1b[x" + rlo, CallID: "c1"})
			h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: "c1", Text: hostile})
			h.Finish(&receipt.Envelope{Result: raw}, nil)
			return nil
		}))
		sid := c.handshake()
		consent, updates, _ := runToAnswer(c, sid, 10, "go")
		tc, _ := consent["params"].(map[string]any)["toolCall"].(map[string]any)
		content, _ := tc["content"].([]any)
		if len(content) != 1 || fmt.Sprint(tc["title"]) != "Run it?? now" || toJSON(content[0]) != toJSON(map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": asText}}) {
			t.Errorf("consent request: title %q, content %v", tc["title"], content)
		}
		if len(updates) != 4 {
			t.Fatalf("%d updates, want 4: %v", len(updates), updates)
		}
		if got := summary(updates[0]); got != "text:"+asText {
			t.Errorf("a note: %q", got)
		}
		if fmt.Sprint(updates[1]["title"]) != "tool[x?" {
			t.Errorf("a tool's title: %q", updates[1]["title"])
		}
		if got := toJSON(updates[2]["content"]); got != toJSON([]any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": asBody}}}) {
			t.Errorf("a tool's output: %s", got)
		}
		if got := summary(updates[3]); got != "text:"+asBody {
			t.Errorf("the run's result: %q", got)
		}
	})
}

// A tool call is progress for the client: it starts as in progress, and its
// result names the call it answers and says whether it failed.
func TestAToolCallIsReportedAsProgressAndItsResultNamesTheCall(t *testing.T) {
	c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
		h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "read_input", CallID: "c1"})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, Tool: "read_input", CallID: "c1", Text: "ten lines"})
		h.Event(adapter.Event{Kind: adapter.EventToolCall, Text: "  search   the web ", CallID: "c2"})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: "c2", Failed: true, Text: "   "})
		h.Event(adapter.Event{Kind: adapter.EventToolCall})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: "unknown", Text: "x"})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, Text: "x"})
		h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "again", CallID: "c1"})
		h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: "c1", Text: "ok"})
		h.Event(adapter.Event{Kind: adapter.EventAssistantText, Text: "not a tool"})
		return nil
	}))
	sid := c.handshake()
	_, updates, answer := runToAnswer(c, sid, 10, "go")
	call := func(id, title string) string {
		return `{"kind":"other","sessionUpdate":"tool_call","status":"in_progress","title":"` + title + `","toolCallId":"` + id + `"}`
	}
	want := []string{
		call("call-1", "read_input"),
		`{"content":[{"content":{"text":"ten lines","type":"text"},"type":"content"}],"sessionUpdate":"tool_call_update","status":"completed","toolCallId":"call-1"}`,
		call("call-2", "search the web"),
		`{"sessionUpdate":"tool_call_update","status":"failed","toolCallId":"call-2"}`,
		call("call-3", "tool call"),
		call("call-4", "again"),
		`{"content":[{"content":{"text":"ok","type":"text"},"type":"content"}],"sessionUpdate":"tool_call_update","status":"completed","toolCallId":"call-4"}`,
	}
	var got []string
	for _, u := range updates {
		got = append(got, toJSON(u))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("updates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res, _ := answer["result"].(map[string]any); res["stopReason"] != "end_turn" {
		t.Fatalf("answer %v", answer)
	}
}

// Progress is what a client that reads slowly loses first: with the queue
// past its bound the tool calls, their results and an interactive session's
// replies are dropped, while a note, the run's result and the answer to the
// prompt are kept.
func TestProgressIsDroppedBeforeTheAnswersTheClientIsOwed(t *testing.T) {
	tune := func(o *outbox) { o.maxBehind, o.keepHeadroom = 1, 1<<30 }
	t.Run("an autonomous run", func(t *testing.T) {
		dropped := make(chan int, 1)
		c := startTuned(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			h.Event(adapter.Event{Kind: adapter.EventToolCall, Tool: "t", CallID: "c1"})
			h.Event(adapter.Event{Kind: adapter.EventToolResult, CallID: "c1", Text: "the output of the call"})
			h.Say("a note")
			h.Finish(&receipt.Envelope{Result: []byte(`"the result"`)}, nil)
			h.s.out.mu.Lock()
			dropped <- h.s.out.dropped
			h.s.out.mu.Unlock()
			return nil
		}), tune)
		sid := c.handshake()
		_, updates, answer := runToAnswer(c, sid, 10, "go")
		var got []string
		for _, u := range updates {
			got = append(got, summary(u))
		}
		if strings.Join(got, "|") != "text:a note|text:the result" {
			t.Fatalf("updates kept: %q", got)
		}
		if res, _ := answer["result"].(map[string]any); res["stopReason"] != "end_turn" {
			t.Fatalf("answer %v", answer)
		}
		if n := <-dropped; n != 2 {
			t.Fatalf("%d frames dropped, want the tool call and its result", n)
		}
	})
	t.Run("an interactive session", func(t *testing.T) {
		fs := newScriptSession(func(f *scriptSession, text string) error {
			f.events <- session.Event{Kind: session.KindToolCall, Text: "list_inputs"}
			f.events <- session.Event{Kind: session.KindAssistantText, Text: "hello"}
			f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "hello"}
			return nil
		})
		dropped := make(chan int, 1)
		c := startTuned(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			_ = h.Converse(ctx, fs)
			h.s.out.mu.Lock()
			dropped <- h.s.out.dropped
			h.s.out.mu.Unlock()
			env, err := fs.End(context.Background(), false)
			h.Finish(env, err)
			return err
		}), tune)
		sid := c.handshake()
		_, updates, answer := runToAnswer(c, sid, 10, "hi")
		if len(updates) != 0 {
			t.Fatalf("progress was kept: %v", updates)
		}
		if res, _ := answer["result"].(map[string]any); res["stopReason"] != "end_turn" {
			t.Fatalf("answer %v", answer)
		}
		c.send(req(11, "session/close", map[string]any{"sessionId": sid}))
		if r, _ := c.replyTo(11); r["error"] != nil {
			t.Fatalf("close: %v", r)
		}
		if n := <-dropped; n != 3 {
			t.Fatalf("%d frames dropped, want the tool call, the reply and the tool call's end", n)
		}
	})
}

// The run's result and its folder are the final answer: the result is the
// message (a JSON string as itself, anything else indented, none for null),
// bounded at InlineTotal bytes, the deliverables follow under a heading, and
// the answer says how the run ended.
func TestTheRunsFinalAnswerCarriesItsResultAndHowItEnded(t *testing.T) {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	cut := func(s, dir string) string {
		return s + fmt.Sprintf("\n\n[The result is longer than %d bytes and is cut here; all of it is in result.json in %s.]", InlineTotal, dir)
	}
	eacute := string(rune(0xe9))
	type tc struct {
		name string
		env  func(dir string, ds *receipt.Deliverables) *receipt.Envelope
		want func(dir string) []string
	}
	str := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	cases := []tc{
		{"a string result is the message itself",
			func(string, *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{Result: str("All quiet.")}
			},
			func(string) []string { return []string{"text:All quiet."} }},
		{"an object result is indented",
			func(string, *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{Result: json.RawMessage(`{"a":1,"b":[1,2]}`)}
			},
			func(string) []string { return []string{"text:{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2\n  ]\n}"} }},
		{"a null result is no message",
			func(string, *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{Result: json.RawMessage(`null`)}
			},
			func(string) []string { return nil }},
		{"no result is no message",
			func(string, *receipt.Deliverables) *receipt.Envelope { return &receipt.Envelope{} },
			func(string) []string { return nil }},
		{"no envelope is no message",
			func(string, *receipt.Deliverables) *receipt.Envelope { return nil },
			func(string) []string { return nil }},
		{"a result of exactly the bound is whole",
			func(string, *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{Result: str(strings.Repeat("x", InlineTotal))}
			},
			func(string) []string { return []string{"text:" + strings.Repeat("x", InlineTotal)} }},
		{"a result past the bound is cut and says where it is",
			func(dir string, _ *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{OutDir: dir, Result: str(strings.Repeat("x", InlineTotal+1))}
			},
			func(dir string) []string { return []string{"text:" + cut(strings.Repeat("x", InlineTotal), dir)} }},
		{"a cut does not split a character",
			func(dir string, _ *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{OutDir: dir, Result: str("a" + strings.Repeat(eacute, InlineTotal/2+1))}
			},
			func(dir string) []string {
				return []string{"text:" + cut("a"+strings.Repeat(eacute, InlineTotal/2-1), dir)}
			}},
		{"two deliverables are handed back under a plural heading",
			func(dir string, ds *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{OutDir: dir, Result: str("done"), Deliverables: ds}
			},
			func(string) []string {
				return []string{"text:done", "text:Deliverables (2):", "resource:text/markdown", "link:b.pdf"}
			}},
		{"one deliverable is handed back under a singular heading",
			func(dir string, ds *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{OutDir: dir, Deliverables: &receipt.Deliverables{Files: ds.Files[:1]}}
			},
			func(string) []string { return []string{"text:Deliverable:", "resource:text/markdown"} }},
		{"a file that is gone is named after the ones handed back",
			func(dir string, ds *receipt.Deliverables) *receipt.Envelope {
				gone := receipt.Deliverable{Path: "reports/gone.md", Title: "gone", SHA256: sum("x"), Bytes: 1, Type: "md"}
				return &receipt.Envelope{OutDir: dir, Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{ds.Files[0], gone}}}
			},
			func(string) []string {
				return []string{"text:Deliverable:", "resource:text/markdown", `text:"gone" was not handed back: it is not in the run's folder.`}
			}},
		{"files that are all withheld are named under no heading",
			func(dir string, _ *receipt.Deliverables) *receipt.Envelope {
				gone := receipt.Deliverable{Path: "reports/gone.md", Title: "gone", SHA256: sum("x"), Bytes: 1, Type: "md"}
				return &receipt.Envelope{OutDir: dir, Deliverables: &receipt.Deliverables{Files: []receipt.Deliverable{gone}}}
			},
			func(string) []string {
				return []string{`text:"gone" was not handed back: it is not in the run's folder.`}
			}},
		{"a truncated list says so",
			func(dir string, ds *receipt.Deliverables) *receipt.Envelope {
				return &receipt.Envelope{OutDir: dir, Deliverables: &receipt.Deliverables{Files: ds.Files[:1], Truncated: true}}
			},
			func(string) []string {
				return []string{"text:Deliverable:", "resource:text/markdown", fmt.Sprintf("text:The run had more than %d deliverables; the rest are not listed.", receipt.MaxDeliverables)}
			}},
		{"deliverables of a run with no folder are not listed",
			func(_ string, ds *receipt.Deliverables) *receipt.Envelope { return &receipt.Envelope{Deliverables: ds} },
			func(string) []string { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ds := &receipt.Deliverables{Files: []receipt.Deliverable{put(t, dir, "reports/a.md", "# a\n"), put(t, dir, "reports/b.pdf", "%PDF")}}
			env := c.env(dir, ds)
			cl := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
				h.Finish(env, nil)
				return nil
			}))
			sid := cl.handshake()
			_, updates, answer := runToAnswer(cl, sid, 10, "go")
			var got []string
			for _, u := range updates {
				got = append(got, summary(u))
			}
			if want := c.want(dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("updates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if res, _ := answer["result"].(map[string]any); res["stopReason"] != "end_turn" {
				t.Fatalf("answer %v", answer)
			}
		})
	}

	t.Run("the answer says how the run ended", func(t *testing.T) {
		dir := t.TempDir()
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			h.Finish(&receipt.Envelope{RunID: "r1", Status: "ok", ExitClass: "ok", OutDir: dir, Deliveries: []receipt.Delivery{
				{Destination: "folder", Kind: "folder", Status: "ok"},
				{Destination: "https://example.test/in", Kind: "post", Status: "failed", Error: "timeout"},
			}}, nil)
			return nil
		}))
		sid := c.handshake()
		_, _, answer := runToAnswer(c, sid, 10, "go")
		dirJSON, _ := json.Marshal(dir)
		want := `{"_meta":{"agentry":{"deliveries":[{"destination":"folder","error":"","kind":"folder","status":"ok"},` +
			`{"destination":"https://example.test/in","error":"timeout","kind":"post","status":"failed"}],` +
			`"exitClass":"ok","outDir":` + string(dirJSON) + `,"runId":"r1","status":"ok"}},"stopReason":"end_turn"}`
		if got := toJSON(answer["result"]); got != want {
			t.Fatalf("answer %s\nwant   %s", got, want)
		}
	})
	t.Run("a run with no deliveries does not list any", func(t *testing.T) {
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			h.Finish(&receipt.Envelope{RunID: "r2", Status: "failed", ExitClass: "model"}, nil)
			return nil
		}))
		sid := c.handshake()
		_, _, answer := runToAnswer(c, sid, 10, "go")
		want := `{"_meta":{"agentry":{"exitClass":"model","outDir":"","runId":"r2","status":"failed"}},"stopReason":"end_turn"}`
		if got := toJSON(answer["result"]); got != want {
			t.Fatalf("answer %s\nwant   %s", got, want)
		}
	})
	t.Run("a run that left no envelope says nothing of how it ended", func(t *testing.T) {
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			h.Finish(nil, nil)
			return nil
		}))
		sid := c.handshake()
		_, _, answer := runToAnswer(c, sid, 10, "go")
		if got := toJSON(answer["result"]); got != `{"stopReason":"end_turn"}` {
			t.Fatalf("answer %s", got)
		}
	})
	t.Run("a run that failed is answered with its error as one line", func(t *testing.T) {
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			h.Finish(&receipt.Envelope{Status: "failed"}, errors.New("the harness\nexited \x1b[31m1"))
			return nil
		}))
		sid := c.handshake()
		_, _, answer := runToAnswer(c, sid, 10, "go")
		if errCode(answer) != codeInternal || errMessage(answer) != "the harness exited [31m1" {
			t.Fatalf("answer %v", answer)
		}
	})
}

// scriptSession is an interactive session whose turns do what a test says.
type scriptSession struct {
	events chan session.Event
	mu     sync.Mutex
	ended  bool
	closed bool
	reason session.EndReason
	send   func(f *scriptSession, text string) error
}

func newScriptSession(send func(f *scriptSession, text string) error) *scriptSession {
	return &scriptSession{events: make(chan session.Event, 64), send: send}
}

func (f *scriptSession) Events() <-chan session.Event { return f.events }
func (f *scriptSession) Send(_ context.Context, text string) error {
	return f.send(f, text)
}
func (f *scriptSession) AddInput(context.Context, string) error { return nil }
func (f *scriptSession) End(context.Context, bool) (*receipt.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = true
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return &receipt.Envelope{}, nil
}
func (f *scriptSession) Ended() (bool, session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, f.reason
}

// endWith marks the session over, as a budget or the harness's exit does.
func (f *scriptSession) endWith(r session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended, f.reason = true, r
}

// answer is a turn that completes.
func (f *scriptSession) answer(text string) {
	f.events <- session.Event{Kind: session.KindUserTurn, Text: text}
	f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "ok"}
}

// An interactive turn is answered by how it ended: a turn that completed
// ends the turn, a session that ran out of turns or tools says it stopped
// there, any other end of the session is an error that names why, and a turn
// that failed leaves the session open for the next.
func TestATurnThatEndsTheSessionIsAnsweredWithWhy(t *testing.T) {
	budget := func(r session.EndReason) func(f *scriptSession, text string) error {
		return func(f *scriptSession, _ string) error { f.endWith(r); return session.ErrEnded }
	}
	cases := []struct {
		name string
		send func(f *scriptSession, text string) error
		want string // the answer to the first prompt, as JSON
	}{
		{"the tool budget", budget(session.EndToolBudget), `{"result":{"_meta":{"agentry":{"endReason":"tool_budget"}},"stopReason":"max_turn_requests"}}`},
		{"the cap on the person's turns", budget(session.EndUserTurns), `{"result":{"_meta":{"agentry":{"endReason":"max_user_turns"}},"stopReason":"max_turn_requests"}}`},
		{"the idle timeout", budget(session.EndIdle), `{"error":{"code":-32603,"message":"the session ended: idle_timeout"}}`},
		{"an end with no reason", budget(""), `{"error":{"code":-32603,"message":"the session ended: harness_exit"}}`},
		{"a failed turn that ended the session", func(f *scriptSession, _ string) error {
			f.events <- session.Event{Kind: session.KindError, Text: "the harness exited"}
			f.endWith(session.EndHarness)
			return errors.New("the harness exited")
		}, `{"error":{"code":-32603,"message":"the session ended: harness_exit"}}`},
		{"an end that comes with a completed turn", func(f *scriptSession, text string) error {
			f.answer(text)
			f.endWith(session.EndFinished)
			return nil
		}, `{"result":{"stopReason":"end_turn"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newScriptSession(tc.send)
			c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
				_ = h.Converse(ctx, fs)
				env, err := fs.End(context.Background(), false)
				h.Finish(env, err)
				return err
			}))
			sid := c.handshake()
			_, _, answer := runToAnswer(c, sid, 10, "hello")
			got := map[string]any{}
			for _, k := range []string{"result", "error"} {
				if v, ok := answer[k]; ok {
					got[k] = v
				}
			}
			if toJSON(got) != tc.want {
				t.Fatalf("answer %s, want %s", toJSON(got), tc.want)
			}
			// The session is over: the next prompt finds it so.
			c.send(req(11, "session/prompt", prompt(sid, "again")))
			if r, _ := c.replyTo(11); errCode(r) != codeInvalidRequest || errMessage(r) != "the session has ended" {
				t.Fatalf("a prompt after the end: %v", r)
			}
		})
	}

	t.Run("a turn that failed leaves the session open", func(t *testing.T) {
		turns := 0
		fs := newScriptSession(func(f *scriptSession, text string) error {
			turns++
			f.events <- session.Event{Kind: session.KindUserTurn, Text: text}
			if turns == 1 {
				f.events <- session.Event{Kind: session.KindToolCall, Text: "fetch"}
				f.events <- session.Event{Kind: session.KindError, Text: "the model said no"}
				return errors.New("the model said\nno \x1b[0m")
			}
			f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "ok"}
			return nil
		})
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			_ = h.Converse(ctx, fs)
			env, err := fs.End(context.Background(), false)
			h.Finish(env, err)
			return err
		}))
		sid := c.handshake()
		_, updates, first := runToAnswer(c, sid, 10, "hello")
		if errCode(first) != codeInternal || errMessage(first) != "the model said no [0m" {
			t.Fatalf("the failed turn was answered %v", first)
		}
		// The tool call it left open is closed before the answer.
		var got []string
		for _, u := range updates {
			got = append(got, toJSON(u))
		}
		if want := `{"kind":"other","sessionUpdate":"tool_call","status":"in_progress","title":"fetch","toolCallId":"call-1"}|{"sessionUpdate":"tool_call_update","status":"completed","toolCallId":"call-1"}`; strings.Join(got, "|") != want {
			t.Fatalf("updates of the failed turn:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
		}
		_, _, second := runToAnswer(c, sid, 11, "again")
		if res, _ := second["result"].(map[string]any); res["stopReason"] != "end_turn" {
			t.Fatalf("the turn after a failed one was answered %v", second)
		}
	})

	t.Run("a session that ends by itself between turns is over", func(t *testing.T) {
		fs := newScriptSession(func(f *scriptSession, text string) error { f.answer(text); return nil })
		over := make(chan struct{})
		c := startRaw(t, testBackend(func(ctx context.Context, h *Host, _ string) error {
			defer close(over)
			_ = h.Converse(ctx, fs)
			env, err := fs.End(context.Background(), false)
			h.Finish(env, err)
			return err
		}))
		sid := c.handshake()
		if _, _, answer := runToAnswer(c, sid, 10, "hello"); answer["error"] != nil {
			t.Fatalf("answer %v", answer)
		}
		fs.endWith(session.EndIdle)
		<-over
		c.send(req(11, "session/prompt", prompt(sid, "again")))
		if r, _ := c.replyTo(11); errCode(r) != codeInvalidRequest || errMessage(r) != "the session has ended" {
			t.Fatalf("a prompt after the end: %v", r)
		}
	})
}
