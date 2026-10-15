package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// The session protocol is a fourth client of the session API. These tests
// drive the engine's real session — the real adapter's driver over each
// harness's wire shape, the real pump, budgets and teardown — once through
// the API and once through the protocol, and hold the two to the same
// events and the same envelope. No test here sets a field the production
// path would not.

// protocolAdapters are the three harnesses a session can run on.
var protocolAdapters = []string{adapter.DefaultName, adapter.ClaudeAdapterName, adapter.CodexAdapterName}

// startAdapterSession starts a real session of the two-mode fixture on the
// named adapter: docker-agent as the fake daemon's container speaking ACP,
// claude and codex as this test binary re-run as their host process
// (TestMain).
func startAdapterSession(t *testing.T, ctx context.Context, adapterName string, tune func(*Params, *fakeHarness)) (session.Session, Params, *dockerx.Fake) {
	t.Helper()
	p, f := sessionFixture(t)
	happyFake(f)
	h := &fakeHarness{}
	if adapterName != adapter.DefaultName {
		p = hostAdapterPlan(t, p, adapterName)
	}
	if tune != nil {
		tune(&p, h)
	}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(ctx, f, p)
	if err != nil {
		t.Fatalf("start %s: %v", adapterName, err)
	}
	return s, p, f
}

// hostAdapterPlan recompiles the fixture's interactive plan for a host
// adapter whose harness is this test binary.
func hostAdapterPlan(t *testing.T, p Params, adapterName string) Params {
	t.Helper()
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pol := p.Policy
	pol.Harness = map[string]policy.HarnessPolicy{adapterName: {Path: self}}
	ad, err := adapter.Get(adapterName)
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(p.RunRoot, "plan", adapterName)
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local", AdapterName: adapterName,
		WorkspaceDir: p.WorkspaceSrc, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatalf("compile %s: %v", adapterName, err)
	}
	p.Plan, p.PlanDir, p.Adapter, p.Policy = plan, planDir, ad, pol
	return p
}

// protocolDriver runs protocol.Serve over a session and gives the test its
// two ends: the command line it writes to and the events it reads.
type protocolDriver struct {
	t      *testing.T
	in     *io.PipeWriter
	events chan protocol.Event
	mu     sync.Mutex
	lines  []string // every event line, verbatim, in order
	seen   []protocol.Event
	done   chan struct{}
	env    *receipt.Envelope
	err    error
}

// allLines is every event line written, once Serve has returned.
func (d *protocolDriver) allLines() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.lines...)
}

func serveProtocol(t *testing.T, ctx context.Context, s session.Session, opts protocol.Options) *protocolDriver {
	t.Helper()
	outR, outW := io.Pipe()
	return serveProtocolTo(t, ctx, s, opts, outR, outW)
}

// serveProtocolTo is serveProtocol with the events stream given: outR is
// what the test reads, outW what Serve writes.
func serveProtocolTo(t *testing.T, ctx context.Context, s session.Session, opts protocol.Options, outR io.Reader, outW io.WriteCloser) *protocolDriver {
	t.Helper()
	inR, inW := io.Pipe()
	d := &protocolDriver{t: t, in: inW, events: make(chan protocol.Event, 1024), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
		for sc.Scan() {
			var ev protocol.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Errorf("an event line is not JSON: %q", sc.Text())
				continue
			}
			d.mu.Lock()
			d.lines = append(d.lines, sc.Text())
			d.mu.Unlock()
			d.events <- ev
		}
		close(d.events)
	}()
	go func() {
		d.env, d.err = protocol.Serve(ctx, s, inR, outW, opts)
		_ = outW.Close()
		_ = inR.Close()
		close(d.done)
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return d
}

// send writes one command line.
func (d *protocolDriver) send(line string) {
	d.t.Helper()
	if _, err := io.WriteString(d.in, line+"\n"); err != nil {
		d.t.Fatalf("write %q: %v", line, err)
	}
}

// until reads events until one satisfies ok, and returns everything read.
func (d *protocolDriver) until(ok func(protocol.Event) bool) []protocol.Event {
	d.t.Helper()
	var got []protocol.Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, open := <-d.events:
			if !open {
				d.t.Fatalf("the event stream closed first; read %v", eventKinds(got))
			}
			got = append(got, ev)
			d.seen = append(d.seen, ev)
			if ok(ev) {
				return got
			}
		case <-deadline:
			d.t.Fatalf("timed out; read %v", eventKinds(got))
		}
	}
}

// wait returns once Serve has, with every event it wrote after the last
// one read.
func (d *protocolDriver) wait() []protocol.Event {
	d.t.Helper()
	select {
	case <-d.done:
	case <-time.After(60 * time.Second):
		d.t.Fatal("Serve never returned")
	}
	var rest []protocol.Event
	for ev := range d.events {
		rest = append(rest, ev)
		d.seen = append(d.seen, ev)
	}
	return rest
}

func isKind(k session.Kind) func(protocol.Event) bool {
	return func(ev protocol.Event) bool { return ev.Kind == k && ev.Refused == "" }
}

func eventKinds(evs []protocol.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Kind)
		if ev.Refused != "" {
			out[i] += "/" + string(ev.Refused)
		}
	}
	return out
}

// sameEvent is what two runs of one conversation must agree on: every
// field but the time, and the raw message the protocol leaves out.
type sameEvent struct {
	Kind         session.Kind
	Text         string
	Synthetic    bool
	StopReason   string
	Path, Digest string
	Reason       session.EndReason
	In, Out, Tot int64
	Cost         float64
}

func asCompared(ev session.Event) sameEvent {
	return sameEvent{ev.Kind, ev.Text, ev.Synthetic, ev.StopReason, ev.Path, ev.Digest, ev.Reason, ev.InputTokens, ev.OutputTokens, ev.TotalTokens, ev.CostUSD}
}

// sameEnvelope is what two runs of one conversation must agree on in the
// envelope: all but the run's own identity, times and places.
type sameEnvelope struct {
	Status   string
	ExitCode int
	Error    string
	Result   string
	Usage    receipt.Usage
	Mode     string
	Enforced map[string]string
}

func comparableEnvelope(env *receipt.Envelope) sameEnvelope {
	u := env.Usage
	u.WallMS = 0
	enforced := map[string]string{}
	for k, v := range env.CapabilitiesEnforced {
		if k != "daemon" {
			enforced[k] = v
		}
	}
	return sameEnvelope{env.Status, env.ExitCode, env.Error, string(env.Result), u, env.Harness.Mode, enforced}
}

// stagedInput writes a file for an add in dir, the driver's staging
// directory, outside the run.
func stagedInput(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProtocolMatchesTheSessionAPI: on each adapter, one conversation — a
// turn, a file added while idle, a second turn, and the end with and
// without the final result — gives the same events in the same order and
// the same envelope whether the session API is called directly (as the
// TUI's /finish and /quit do) or driven through the protocol. The events
// the protocol writes are the lines of events.ndjson without the raw
// message: redacted exactly as the receipt is, never less.
func TestProtocolMatchesTheSessionAPI(t *testing.T) {
	for _, name := range protocolAdapters {
		for _, finish := range []bool{true, false} {
			label := name + "/quit"
			if finish {
				label = name + "/finish"
			}
			t.Run(label, func(t *testing.T) {
				inputs := t.TempDir()
				input := stagedInput(t, inputs, "notes.txt", "the notes")
				ctx := context.Background()

				// Through the API, as a chat surface calls it.
				s, _, _ := startAdapterSession(t, ctx, name, nil)
				get, drained := collect(s)
				if err := s.Send(ctx, "hello"); err != nil {
					t.Fatalf("send: %v", err)
				}
				if err := s.AddInput(ctx, input); err != nil {
					t.Fatalf("add: %v", err)
				}
				if err := s.Send(ctx, "and again"); err != nil {
					t.Fatalf("send: %v", err)
				}
				apiEnv, apiErr := s.End(ctx, finish)
				<-drained
				var apiEvents []sameEvent
				for _, ev := range get() {
					apiEvents = append(apiEvents, asCompared(ev))
				}

				// Through the protocol.
				s2, _, f2 := startAdapterSession(t, ctx, name, nil)
				d := serveProtocol(t, ctx, s2, protocol.Options{InputsDir: inputs})
				d.send(`{"send":{"text":"hello"}}`)
				d.until(isKind(session.KindAssistantTurn))
				d.send(`{"add":{"path":"notes.txt"}}`)
				d.until(isKind(session.KindAssistantTurn))
				d.send(`{"send":{"text":"and again"}}`)
				d.until(isKind(session.KindAssistantTurn))
				if finish {
					d.send(`{"end":{"finish":true}}`)
				} else {
					d.send(`{"end":{"finish":false}}`)
				}
				d.wait()
				lines := d.allLines()
				var protoEvents []sameEvent
				for _, l := range lines {
					var ev protocol.Event
					_ = json.Unmarshal([]byte(l), &ev)
					if ev.Refused != "" {
						t.Errorf("the protocol refused a command of a plain conversation: %s", l)
					}
					if len(ev.Raw) > 0 {
						t.Errorf("an event carried the harness's raw message: %s", l)
					}
					protoEvents = append(protoEvents, asCompared(ev.Event))
				}

				if !reflect.DeepEqual(apiEvents, protoEvents) {
					t.Errorf("events differ:\n api:      %+v\n protocol: %+v", apiEvents, protoEvents)
				}
				if (apiErr == nil) != (d.err == nil) {
					t.Errorf("End's error: api %v, protocol %v", apiErr, d.err)
				}
				if d.env == nil || apiEnv == nil {
					t.Fatalf("no envelope: api %v, protocol %v", apiEnv, d.env)
				}
				if a, b := comparableEnvelope(apiEnv), comparableEnvelope(d.env); !reflect.DeepEqual(a, b) {
					t.Errorf("envelopes differ:\n api:      %+v\n protocol: %+v", a, b)
				}
				wantReason := session.EndQuit
				if finish {
					wantReason = session.EndFinished
				}
				if last := protoEvents[len(protoEvents)-1]; last.Kind != session.KindSessionEnd || last.Reason != wantReason {
					t.Errorf("last event = %+v, want session_end %s", last, wantReason)
				}

				// The receipt's lines, raw left out, are the protocol's lines.
				receipt, err := os.ReadFile(filepath.Join(d.env.Receipts.Dir, "events.ndjson"))
				if err != nil {
					t.Fatal(err)
				}
				var fromReceipt []string
				for _, l := range strings.Split(strings.TrimSpace(string(receipt)), "\n") {
					var ev session.Event
					if err := json.Unmarshal([]byte(l), &ev); err != nil {
						t.Fatalf("receipt line %q: %v", l, err)
					}
					ev.Raw = nil
					b, _ := json.Marshal(ev)
					fromReceipt = append(fromReceipt, string(b))
				}
				if !reflect.DeepEqual(fromReceipt, lines) {
					t.Errorf("the protocol's lines are not the receipt's without raw:\n receipt:  %q\n protocol: %q", fromReceipt, lines)
				}
				joined := strings.Join(lines, "\n")
				for _, secret := range []string{"sekret-value-1234", "sk-test-key-9876"} {
					if strings.Contains(joined, secret) {
						t.Errorf("a protocol event carries %q", secret)
					}
				}
				if !strings.Contains(joined, "You said: hello") {
					t.Errorf("the harness's answer never reached the protocol: %s", joined)
				}
				assertTornDown(t, f2)
			})
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// assertTornDown: removing the run network is a teardown step the fake
// records, so its presence proves the session's teardown ran.
func assertTornDown(t *testing.T, f *dockerx.Fake) {
	t.Helper()
	if !strings.Contains(strings.Join(f.Ops(), "\n"), "NetworkRemove:agentry-5e5510-net") {
		t.Errorf("teardown did not run: %v", f.Ops())
	}
}

// TestProtocolReachesEveryEndReason: each of the session's nine end
// reasons ends a session driven through the protocol, Serve returns on
// its own when the session ends by itself (the driver never has to), the
// last event names the reason, and the sandbox is torn down.
func TestProtocolReachesEveryEndReason(t *testing.T) {
	cases := []struct {
		reason   session.EndReason
		tune     func(*Params, *fakeHarness)
		commands []string
		// closeInput ends the command stream instead of sending end.
		closeInput bool
		cancel     bool
		wantCode   int
	}{
		{reason: session.EndFinished, commands: []string{`{"send":{"text":"hi"}}`, `{"end":{"finish":true}}`}, wantCode: ExitOK},
		{reason: session.EndQuit, commands: []string{`{"send":{"text":"hi"}}`, `{"end":{"finish":false}}`}, wantCode: ExitOK},
		{reason: session.EndQuit, commands: []string{`{"send":{"text":"hi"}}`}, closeInput: true, wantCode: ExitOK},
		{reason: session.EndIdle, tune: func(p *Params, _ *fakeHarness) { p.Plan.Harness.Session.Budgets.IdleTimeoutSeconds = 1 }, wantCode: ExitTimeout},
		{reason: session.EndWall, tune: func(p *Params, _ *fakeHarness) { p.Plan.Harness.Session.Budgets.MaxWallSeconds = 1 }, wantCode: ExitTimeout},
		{reason: session.EndUserTurns, commands: []string{`{"send":{"text":"1"}}`, `{"send":{"text":"2"}}`, `{"send":{"text":"3"}}`, `{"send":{"text":"4"}}`}, wantCode: ExitBudget},
		{reason: session.EndToolBudget, tune: func(p *Params, _ *fakeHarness) { p.Budgets.MaxTurns = 1 }, commands: []string{`{"send":{"text":"1"}}`, `{"send":{"text":"2"}}`}, wantCode: ExitBudget},
		{reason: session.EndCostBudget, tune: func(p *Params, h *fakeHarness) {
			p.Budgets.MaxCostUSD = 1
			h.usage = map[string]any{"used": 1262, "size": 0, "cost": map[string]any{"amount": 4.5, "currency": "USD"}}
		}, commands: []string{`{"send":{"text":"1"}}`}, wantCode: ExitBudget},
		{reason: session.EndHarness, tune: func(_ *Params, h *fakeHarness) { h.exitOnPrompt = true }, commands: []string{`{"send":{"text":"1"}}`}, wantCode: ExitHarness},
		{reason: session.EndCancelled, cancel: true, wantCode: ExitCLI},
	}
	for _, tc := range cases {
		name := string(tc.reason)
		if tc.closeInput {
			name += "/end of input"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, _, f := startAdapterSession(t, ctx, adapter.DefaultName, tc.tune)
			d := serveProtocol(t, ctx, s, protocol.Options{})
			for _, c := range tc.commands {
				d.send(c)
				if strings.Contains(c, `"send"`) {
					// One turn at a time: the next command after this turn's
					// end (or the session's).
					d.until(func(ev protocol.Event) bool {
						return ev.Refused == "" && (ev.Kind == session.KindAssistantTurn || ev.Kind == session.KindSessionEnd || ev.Kind == session.KindError)
					})
				}
			}
			if tc.closeInput {
				_ = d.in.Close()
			}
			if tc.cancel {
				cancel()
			}
			d.wait()
			var ends []session.EndReason
			for _, ev := range d.seen {
				if ev.Kind == session.KindSessionEnd {
					ends = append(ends, ev.Reason)
				}
			}
			if len(ends) != 1 || ends[0] != tc.reason {
				t.Errorf("session_end events %v, want one, %q", ends, tc.reason)
			}
			if ended, reason := s.Ended(); !ended || reason != tc.reason {
				t.Errorf("session ended=%v reason=%q, want %q", ended, reason, tc.reason)
			}
			if d.env == nil || d.env.ExitCode != tc.wantCode {
				t.Errorf("envelope %+v, want exit %d", d.env, tc.wantCode)
			}
			if tc.wantCode == ExitOK && d.err != nil {
				t.Errorf("Serve = %v, want no error", d.err)
			}
			if tc.wantCode != ExitOK {
				var ce *ClassifiedError
				if !errors.As(d.err, &ce) || ce.Code != tc.wantCode {
					t.Errorf("Serve = %v, want exit class %d", d.err, tc.wantCode)
				}
			}
			assertTornDown(t, f)
		})
	}
}

// TestProtocolRefusesWithoutEndingTheSession: a malformed line, an unknown
// command, a second send while a turn is in flight, and an add naming
// anything but a regular file are each answered with an error event naming
// the refusal — and the session carries on, takes the next turn, and still
// tears down at the end.
func TestProtocolRefusesWithoutEndingTheSession(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	s, _, f := startAdapterSession(t, ctx, adapter.DefaultName, func(_ *Params, h *fakeHarness) { h.gate = gate })
	inputs := t.TempDir()
	d := serveProtocol(t, ctx, s, protocol.Options{InputsDir: inputs})

	sub := filepath.Join(inputs, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := stagedInput(t, t.TempDir(), "outside.txt", "not the driver's to add")
	nested := stagedInput(t, sub, "nested.txt", "one level down")
	link := filepath.Join(inputs, "link.txt")
	linkable := os.Symlink(outside, link) == nil
	refused := []struct {
		line  string
		class protocol.Refusal
	}{
		{`this is not json`, protocol.RefusedMalformed},
		{`["send"]`, protocol.RefusedMalformed},
		{`{}`, protocol.RefusedMalformed},
		{`{"stop":{}}`, protocol.RefusedUnknown},
		{`{"send":{"text":"a"},"end":{"finish":false}}`, protocol.RefusedMalformed},
		{`{"send":{"text":"  "}}`, protocol.RefusedMalformed},
		{`{"send":{"text":"a","as":"system"}}`, protocol.RefusedMalformed},
		{`{"send":null}`, protocol.RefusedMalformed},
		{`{"end":{"finish":"yes"}}`, protocol.RefusedMalformed},
		{`{"add":{"path":""}}`, protocol.RefusedMalformed},
		{`{"send":{"text":"` + strings.Repeat("x", protocol.MaxLine) + `"}}`, protocol.RefusedMalformed},
	}
	for _, r := range refused {
		d.send(r.line)
		got := d.until(func(ev protocol.Event) bool { return ev.Refused != "" })
		if last := got[len(got)-1]; last.Refused != r.class || last.Kind != session.KindError || last.Text == "" {
			t.Errorf("%.60s: refused %+v, want class %s", r.line, last, r.class)
		}
	}

	// A turn in flight (the harness holds its answer), then a second send.
	d.send(`{"send":{"text":"first"}}`)
	d.until(isKind(session.KindUserTurn))
	d.send(`{"send":{"text":"second"}}`)
	if got := d.until(func(ev protocol.Event) bool { return ev.Refused != "" }); got[len(got)-1].Refused != protocol.RefusedBusy {
		t.Errorf("a second send in flight: %v, want busy", eventKinds(got))
	}
	// An add naming anything but a regular file directly in the inputs
	// directory, while the turn is in flight: a directory, a missing file,
	// a file outside (by absolute path and by ..), one in a subdirectory,
	// and a link out.
	adds := []string{sub, "sub", "missing.txt", outside, "../" + filepath.Base(filepath.Dir(outside)) + "/outside.txt", nested, "sub/nested.txt"}
	if linkable {
		adds = append(adds, link, "link.txt")
	}
	for _, path := range adds {
		d.send(`{"add":{"path":` + jsonString(path) + `}}`)
		got := d.until(func(ev protocol.Event) bool { return ev.Refused != "" })
		if last := got[len(got)-1]; last.Refused != protocol.RefusedInput {
			t.Errorf("add %s: %+v, want input_refused", path, last)
		}
	}
	gate <- struct{}{}
	first := d.until(isKind(session.KindAssistantTurn))
	if !strings.Contains(first[len(first)-1].Text, "You said: first") {
		t.Errorf("the turn in flight answered %q", first[len(first)-1].Text)
	}
	if ended, _ := s.Ended(); ended {
		t.Fatal("a refusal ended the session")
	}

	// The session takes the next turn as if nothing had been refused.
	close(gate)
	d.send(`{"send":{"text":"after"}}`)
	after := d.until(isKind(session.KindAssistantTurn))
	if !strings.Contains(after[len(after)-1].Text, "You said: after") {
		t.Errorf("the turn after the refusals answered %q", after[len(after)-1].Text)
	}
	d.send(`{"end":{"finish":false}}`)
	d.wait()
	if d.err != nil || d.env == nil || d.env.Status != "ok" {
		t.Errorf("end after refusals: %v %+v", d.err, d.env)
	}
	// A refusal is the protocol's answer to its driver, not a session
	// event: the receipt has none.
	b, _ := os.ReadFile(filepath.Join(d.env.Receipts.Dir, "events.ndjson"))
	if strings.Contains(string(b), "refused") || strings.Contains(string(b), "unknown command") {
		t.Errorf("a refusal reached the receipt: %s", b)
	}
	assertTornDown(t, f)
}

// TestProtocolTakesNoFilesWithoutAnInputsDirectory: a driver that named no
// staging directory adds nothing, whatever path it sends.
func TestProtocolTakesNoFilesWithoutAnInputsDirectory(t *testing.T) {
	ctx := context.Background()
	s, _, f := startAdapterSession(t, ctx, adapter.DefaultName, nil)
	d := serveProtocol(t, ctx, s, protocol.Options{})
	file := stagedInput(t, t.TempDir(), "notes.txt", "the notes")
	d.send(`{"add":{"path":` + jsonString(file) + `}}`)
	got := d.until(func(ev protocol.Event) bool { return ev.Refused != "" })
	if last := got[len(got)-1]; last.Refused != protocol.RefusedInput || !strings.Contains(last.Text, "takes no files") {
		t.Errorf("add without an inputs directory: %+v", last)
	}
	d.send(`{"end":{"finish":false}}`)
	d.wait()
	for _, ev := range d.seen {
		if ev.Kind == session.KindInputAdded {
			t.Errorf("a file was added: %+v", ev)
		}
	}
	assertTornDown(t, f)
}

// TestProtocolNeverWaitsForTheReader: a driver that keeps descriptor 3 open
// and stops reading it does not hold the session up — the idle cap still
// ends it and the sandbox is still torn down — because the protocol writes
// events from its own queue and never makes the session wait (the session's
// events feed the pump that enforces the budgets).
func TestProtocolNeverWaitsForTheReader(t *testing.T) {
	ctx := context.Background()
	s, _, f := startAdapterSession(t, ctx, adapter.DefaultName, func(p *Params, _ *fakeHarness) {
		p.Plan.Harness.Session.Budgets.IdleTimeoutSeconds = 1
	})
	stuckR, stuckW := io.Pipe() // nothing ever reads stuckR
	defer func() { _ = stuckR.Close() }()
	done := make(chan struct{})
	var env *receipt.Envelope
	var err error
	inR, inW := io.Pipe() // the driver keeps its command stream open too
	defer func() { _ = inW.Close() }()
	go func() {
		env, err = protocol.Serve(ctx, s, inR, stuckW, protocol.Options{})
		close(done)
	}()
	// A turn whose events the reader will never take; after it, silence.
	go func() { _, _ = io.WriteString(inW, `{"send":{"text":"hello"}}`+"\n") }()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("Serve never returned: the session waited for a reader that stopped reading")
	}
	if ended, reason := s.Ended(); !ended || reason != session.EndIdle {
		t.Errorf("ended=%v reason=%q, want the idle cap", ended, reason)
	}
	var ce *ClassifiedError
	if env == nil || !errors.As(err, &ce) || ce.Code != ExitTimeout {
		t.Errorf("Serve = %v, %+v; want the idle cap's timeout", err, env)
	}
	assertTornDown(t, f)
}

// TestProtocolHoldsSendsDuringTheOpeningTurn: a session started with a task
// sends it as its first turn by itself; a send before that turn ends is
// refused as busy, as for any turn in flight.
func TestProtocolHoldsSendsDuringTheOpeningTurn(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	s, _, f := startAdapterSession(t, ctx, adapter.DefaultName, func(p *Params, h *fakeHarness) {
		h.gate = gate
		p.Task = strings.NewReader("the opening task")
	})
	d := serveProtocol(t, ctx, s, protocol.Options{OpeningTurn: true})
	d.send(`{"send":{"text":"too soon"}}`)
	if got := d.until(func(ev protocol.Event) bool { return ev.Refused != "" }); got[len(got)-1].Refused != protocol.RefusedBusy {
		t.Errorf("a send during the opening turn: %v, want busy", eventKinds(got))
	}
	close(gate)
	opening := d.until(isKind(session.KindAssistantTurn))
	if !strings.Contains(opening[len(opening)-1].Text, "the opening task") {
		t.Errorf("the opening turn answered %q", opening[len(opening)-1].Text)
	}
	d.send(`{"send":{"text":"now"}}`)
	d.until(isKind(session.KindAssistantTurn))
	d.send(`{"end":{"finish":false}}`)
	d.wait()
	if d.env == nil || d.env.Usage.UserTurns != 2 {
		t.Errorf("envelope %+v, want two user turns (the opening and one)", d.env)
	}
	assertTornDown(t, f)
}

// TestProtocolNarratesUsage: a driver meters a session as it meters a run —
// the running totals after every usage event, the envelope's figures at
// the end.
func TestProtocolNarratesUsage(t *testing.T) {
	ctx := context.Background()
	s, _, _ := startAdapterSession(t, ctx, adapter.ClaudeAdapterName, nil)
	var seen []receipt.Usage
	d := serveProtocol(t, ctx, s, protocol.Options{OnUsage: func(u receipt.Usage) { seen = append(seen, u) }})
	d.send(`{"send":{"text":"one"}}`)
	d.until(isKind(session.KindAssistantTurn))
	d.send(`{"send":{"text":"two"}}`)
	d.until(isKind(session.KindAssistantTurn))
	d.send(`{"end":{"finish":false}}`)
	d.wait()
	if len(seen) == 0 {
		t.Fatal("no usage was narrated")
	}
	last := seen[len(seen)-1]
	u := d.env.Usage
	if last.InputTokens != u.InputTokens || last.OutputTokens != u.OutputTokens || last.TotalTokens != u.TotalTokens || last.Turns != u.Turns || last.CostUSD != u.CostUSD {
		t.Errorf("last narrated %+v, envelope %+v", last, u)
	}
	if u.InputTokens == 0 {
		t.Errorf("the envelope counted no tokens: %+v", u)
	}
}
