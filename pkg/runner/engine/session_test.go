package engine

import (
	"bufio"
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
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const sessionManifest = `schemaVersion: "2"
name: chatty
namespace: demo
modes: [autonomous, interactive]
version: 1.0.0
summary: "session engine fixture"
capabilities:
  secrets: [ENGINE_TEST_TOKEN]
interactive:
  opening: "What should I look at?"
publish:
  include: [agent.yaml, AGENT.md]
`

// sessionFixture compiles the interactive plan of a two-mode release.
func sessionFixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	return sessionFixtureEdited(t, nil)
}

// sessionFixtureEdited is sessionFixture with the manifest edited first.
func sessionFixtureEdited(t *testing.T, edit func(*manifest.Manifest)) (Params, *dockerx.Fake) {
	t.Helper()
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Chat helpfully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Exec.Packages = map[string]string{"echo": "busybox"} // the fixture declares exec: [echo]
	// The malware check whole, as a sandbox host takes it (fixtureEdited).
	on := true
	pol.MalwareCheck = &on
	pol.Sessions = policy.Sessions{IdleTimeoutSeconds: 3600, MaxWallSeconds: 3600, MaxUserTurns: 3}
	planDir := filepath.Join(installDir, "plan", "interactive")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, _ := adapter.Get(adapter.DefaultName)
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-9876")
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader(""), Adapter: ad, Policy: pol,
		Budgets: spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10}, Attestation: receipt.Attestation{State: "local"},
		Stderr: os.Stderr, RunID: "5e5510", KeepRunDir: true,
	}, &dockerx.Fake{}
}

// fakeHarness plays docker-agent's ACP server on the attached streams:
// it records every method it is asked, answers prompts with a tool call,
// a permission request (which the runner must deny), echoing text that
// leaks a secret (which receipts must redact), and exits on stdin EOF.
type fakeHarness struct {
	mu      sync.Mutex
	methods []string
	granted bool
	denied  bool
	// usage, when set, is written as one usage_update before the turn's
	// reply — the wire shape a harness that prices the session sends. Nil
	// sends none, as docker-agent 1.150.0 does for a model whose context
	// window it does not know; either way it reports no cost.
	usage map[string]any
	// bareResult answers a prompt with its stop reason alone, as
	// docker-agent does: its prompt result never carries usage.
	bareResult bool
	// gate, when set, holds every prompt's answer until a value arrives (or
	// the channel closes), so a turn stays in flight as long as a test needs.
	gate chan struct{}
	// exitOnPrompt makes the harness exit when a prompt arrives, as a
	// crashing harness does.
	exitOnPrompt bool
	// asks are more tools the harness requests permission for on each
	// prompt, after its two; answers records the option the runner chose
	// for each ("a" allow_once, "r" reject_once).
	asks    []string
	answers map[string]string
	// onPrompt, when set, sees each prompt's text as it arrives.
	onPrompt func(text string)
	// exitErr is what the harness exits with when it exits on a prompt
	// (exitOnPrompt): the container's own failure, as docker reports one.
	exitErr error
	// answer, when set, is the reply to a prompt in place of the echo.
	answer func(text string) string
	// loadSession advertises session/load, which replays a stored
	// conversation and continues it; loadFails answers it with an error.
	loadSession, loadFails bool
	// holdForCancel answers a prompt with a first chunk and then holds it
	// until session/cancel, which it answers with the stop reason
	// "cancelled"; cancels counts them.
	holdForCancel bool
	cancels       int
}

func (h *fakeHarness) handler(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if !strings.Contains(strings.Join(r.Argv, " "), "serve acp") {
		return 2, nil
	}
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = stdout.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(sc.Bytes(), &m)
		if m.Method != "" {
			h.mu.Lock()
			h.methods = append(h.methods, m.Method)
			h.mu.Unlock()
		}
		switch m.Method {
		case "initialize":
			res := map[string]any{"protocolVersion": 1}
			if h.loadSession {
				res["agentCapabilities"] = map[string]any{"loadSession": true}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": res})
		case "session/load":
			if h.loadFails {
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": "Internal error", "data": "session not found"}})
				continue
			}
			// The stored conversation, replayed before the answer.
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s0", "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "an earlier question"}}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s0", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "an earlier answer"}}}})
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
		case "session/new":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "s1"}})
		case "session/prompt":
			if h.exitOnPrompt {
				return 1, h.exitErr
			}
			if h.gate != nil {
				<-h.gate
			}
			h.mu.Lock()
			hold := h.holdForCancel
			h.mu.Unlock()
			if hold {
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Half an answer"}}}})
				for sc.Scan() {
					var c struct {
						Method string `json:"method"`
					}
					_ = json.Unmarshal(sc.Bytes(), &c)
					if c.Method == "session/cancel" {
						h.mu.Lock()
						h.cancels++
						h.mu.Unlock()
						break
					}
				}
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "cancelled"}})
				continue
			}
			text := ""
			if len(m.Params.Prompt) > 0 {
				text = m.Params.Prompt[0].Text
			}
			if h.onPrompt != nil {
				h.onPrompt(text)
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "fs_list", "status": "pending"}}})
			write(map[string]any{"jsonrpc": "2.0", "id": 76, "method": "session/request_permission", "params": map[string]any{"sessionId": "s1", "toolCall": map[string]any{"title": "write_report"}, "options": []any{map[string]any{"optionId": "a", "kind": "allow_once"}, map[string]any{"optionId": "r", "kind": "reject_once"}}}})
			if sc.Scan() {
				var ans struct {
					Result struct {
						Outcome struct {
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					} `json:"result"`
				}
				_ = json.Unmarshal(sc.Bytes(), &ans)
				h.mu.Lock()
				h.granted = ans.Result.Outcome.OptionID == "a"
				h.mu.Unlock()
			}
			write(map[string]any{"jsonrpc": "2.0", "id": 77, "method": "session/request_permission", "params": map[string]any{"sessionId": "s1", "toolCall": map[string]any{"title": "run_command"}, "options": []any{map[string]any{"optionId": "a", "kind": "allow_once"}, map[string]any{"optionId": "r", "kind": "reject_once"}}}})
			if sc.Scan() {
				var ans struct {
					Result struct {
						Outcome struct {
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					} `json:"result"`
				}
				_ = json.Unmarshal(sc.Bytes(), &ans)
				h.mu.Lock()
				h.denied = ans.Result.Outcome.OptionID == "r"
				h.mu.Unlock()
			}
			for i, tool := range h.asks {
				write(map[string]any{"jsonrpc": "2.0", "id": 100 + i, "method": "session/request_permission", "params": map[string]any{"sessionId": "s1", "toolCall": map[string]any{"title": tool}, "options": []any{map[string]any{"optionId": "a", "kind": "allow_once"}, map[string]any{"optionId": "r", "kind": "reject_once"}}}})
				if !sc.Scan() {
					break
				}
				var ans struct {
					Result struct {
						Outcome struct {
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					} `json:"result"`
				}
				_ = json.Unmarshal(sc.Bytes(), &ans)
				h.mu.Lock()
				if h.answers == nil {
					h.answers = map[string]string{}
				}
				h.answers[tool] = ans.Result.Outcome.OptionID
				h.mu.Unlock()
			}
			if h.usage != nil {
				upd := map[string]any{"sessionUpdate": "usage_update"}
				for k, v := range h.usage {
					upd[k] = v
				}
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": upd}})
			}
			reply := "You said: " + text + ". The token is sekret-value-1234 and the key is sk-test-key-9876."
			if h.answer != nil {
				reply = h.answer(text)
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": reply}}}})
			result := map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}
			if h.bareResult {
				delete(result, "usage")
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
	return 0, nil // stdin EOF: clean exit like the real harness
}

func startFixture(t *testing.T) (session.Session, *fakeHarness, Params, *dockerx.Fake) {
	t.Helper()
	return startFixtureEdited(t, nil)
}

// startFixtureEdited is startFixture with the manifest edited first.
func startFixtureEdited(t *testing.T, edit func(*manifest.Manifest)) (session.Session, *fakeHarness, Params, *dockerx.Fake) {
	t.Helper()
	p, f := sessionFixtureEdited(t, edit)
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return s, h, p, f
}

func collect(s session.Session) (func() []session.Event, chan struct{}) {
	var mu sync.Mutex
	var evs []session.Event
	done := make(chan struct{})
	go func() {
		for ev := range s.Events() {
			mu.Lock()
			evs = append(evs, ev)
			mu.Unlock()
		}
		close(done)
	}()
	return func() []session.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]session.Event(nil), evs...)
	}, done
}

func kinds(evs []session.Event) map[session.Kind]int {
	out := map[session.Kind]int{}
	for _, ev := range evs {
		out[ev.Kind]++
	}
	return out
}

// TestSessionTurnsInputsAndReceipts covers one conversation: turns
// reach the harness only as session/prompt, an input
// lands under the staged inputs and is announced, the permission request
// is denied and logged, and the receipts redact the secret and the key.
func TestSessionTurnsInputsAndReceipts(t *testing.T) {
	s, h, p, f := startFixture(t)
	get, done := collect(s)
	ctx := context.Background()
	// The gateway's and the proxy's logs, read at the end, hold the secret.
	f.LogsByName["agentry-5e5510-gateway"] = "- agentry-tools: fetch: Authorization: Bearer sekret-value-1234\n"
	f.LogsByName["agentry-5e5510-proxy"] = "deny api.example:443 token sekret-value-1234\n"

	if err := s.Send(ctx, "hello there"); err != nil {
		t.Fatalf("send: %v", err)
	}
	inputFile := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(inputFile, []byte("some notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInput(ctx, inputFile); err != nil {
		t.Fatalf("add input: %v", err)
	}
	if err := s.AddInput(ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory as an input: %v", err)
	}
	if err := s.AddInput(ctx, inputFile); err == nil || !strings.Contains(err.Error(), `an input named "notes.txt" is already staged — rename the file (inputs are basename-rooted under /inputs)`) {
		t.Errorf("a second file named notes.txt: %v", err)
	}
	if link := filepath.Join(t.TempDir(), "link.txt"); os.Symlink(inputFile, link) == nil {
		if err := s.AddInput(ctx, link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a link to a file as an input: %v", err)
		}
	}
	env, err := s.End(ctx, true)
	if err != nil {
		t.Fatalf("end: %v (envelope %+v)", err, env)
	}
	<-done

	// Only protocol turns reached the harness.
	h.mu.Lock()
	methods := strings.Join(h.methods, " ")
	denied := h.denied
	h.mu.Unlock()
	if methods != "initialize session/new session/prompt session/prompt" {
		t.Errorf("harness saw %q", methods)
	}
	if !denied {
		t.Error("permission request was not answered with the reject option (line 4)")
	}
	// The staged input, and the agent-side mount unchanged.
	staged, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", "5e5510", "inputs", "notes.txt"))
	if err != nil || string(staged) != "some notes" {
		t.Errorf("input not staged: %v", err)
	}
	for _, c := range f.Calls {
		if c.Op == "RunForeground" && strings.Contains(c.Arg, "-agent") {
			for _, mt := range c.Run.Mounts {
				if mt.Target == spec.SessionMount && mt.ReadOnly {
					t.Error("session store mounted read-only")
				}
				if mt.Target == spec.InputsMount {
					t.Error("the agent container must not mount /inputs (tools container only)")
				}
			}
		}
	}
	evs := get()
	k := kinds(evs)
	if k[session.KindUserTurn] != 2 || k[session.KindAssistantTurn] != 2 || k[session.KindInputAdded] != 1 || k[session.KindPermissionGranted] != 2 || k[session.KindPermissionDenied] != 2 || k[session.KindToolCall] != 2 || k[session.KindSessionEnd] != 1 {
		t.Errorf("event kinds = %v", k)
	}
	var synthetic bool
	for _, ev := range evs {
		if ev.Kind == session.KindUserTurn && ev.Synthetic && strings.Contains(ev.Text, "/inputs/notes.txt") {
			synthetic = true
		}
	}
	if !synthetic {
		t.Error("no synthetic turn announced the input")
	}
	// Envelope and receipts (redaction).
	if env.Status != "ok" || env.Harness.Mode != "interactive" || env.Usage.UserTurns != 1 || env.Usage.Turns != 2 || env.Usage.InputTokens != 20 {
		t.Errorf("envelope = status %s mode %s usage %+v", env.Status, env.Harness.Mode, env.Usage)
	}
	for name, want := range map[string]string{
		"gateway.log": "- agentry-tools: fetch: Authorization: Bearer [redacted]\n",
		"proxy.log":   "deny api.example:443 token [redacted]\n",
	} {
		if b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q", name, b, err, want)
		}
	}
	// The result is the last assistant message (the reply to the input
	// notice here), redacted.
	if !strings.Contains(string(env.Result), "You said:") || strings.Contains(string(env.Result), "sekret-value") {
		t.Errorf("result = %s", env.Result)
	}
	for _, name := range []string{"transcript.md", "events.ndjson", "result.json"} {
		var path string
		if name == "result.json" {
			path = filepath.Join(p.OutDir, name)
		} else {
			path = filepath.Join(env.Receipts.Dir, name)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, secret := range []string{"sekret-value-1234", "sk-test-key-9876"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s leaks %q", name, secret)
			}
		}
		if name != "result.json" && !strings.Contains(string(b), "[redacted]") {
			t.Errorf("%s has no redaction marker although the harness leaked", name)
		}
	}
	tr, _ := os.ReadFile(filepath.Join(env.Receipts.Dir, "transcript.md"))
	for _, want := range []string{"## User\n\nhello there", "## Runner\n\nA new input file is available at /inputs/notes.txt", "## Assistant", "- tool call: fs_list", "- permission granted: write_report", "- permission request denied: run_command", "- input added: /inputs/notes.txt (sha256:", "_Session ended: finished._"} {
		if !strings.Contains(string(tr), want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
	}
	// An error the session recorded is a line of the transcript.
	if got := transcript(env, []session.Event{{Kind: session.KindError, Text: "the harness said no"}}); !strings.Contains(got, "- error: the harness said no\n") {
		t.Errorf("an error event is missing from the transcript:\n%s", got)
	}
	if len(f.Leftover) != 0 {
		t.Errorf("leftovers after End: %v", f.Leftover)
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "5e5510", "cfg", "gateway", "secrets.env")); err == nil {
		t.Error("secrets.env survived teardown")
	}
	// A second End is idempotent; Send after End is refused.
	if env2, err2 := s.End(ctx, true); err2 != nil || env2 != env {
		t.Errorf("second End = %v, %v", env2, err2)
	}
	if err := s.Send(ctx, "again"); err != session.ErrEnded {
		t.Errorf("send after end = %v", err)
	}
}

// TestSessionRecordsTheLastTurnsEventsBeforeEnd: a turn's tool calls and
// permission decisions reach the log, events.ndjson and the transcript
// even when the pump lags the driver and End follows the turn at once.
// The lag is what a loaded runner produced.
func TestSessionRecordsTheLastTurnsEventsBeforeEnd(t *testing.T) {
	pumpEventHook = func() { time.Sleep(20 * time.Millisecond) }
	t.Cleanup(func() { pumpEventHook = nil })
	s, _, _, _ := startFixture(t)
	get, done := collect(s)
	ctx := context.Background()
	if err := s.Send(ctx, "hello there"); err != nil {
		t.Fatalf("send: %v", err)
	}
	env, err := s.End(ctx, true)
	if err != nil {
		t.Fatalf("end: %v (envelope %+v)", err, env)
	}
	<-done
	evs := get()
	k := kinds(evs)
	if k[session.KindToolCall] != 1 || k[session.KindPermissionGranted] != 1 || k[session.KindPermissionDenied] != 1 || k[session.KindAssistantTurn] != 1 {
		t.Errorf("event kinds = %v", k)
	}
	// The turn's own events come before the turn that closes it.
	var order []session.Kind
	for _, ev := range evs {
		switch ev.Kind {
		case session.KindToolCall, session.KindPermissionDenied, session.KindAssistantTurn:
			order = append(order, ev.Kind)
		}
	}
	if len(order) != 3 || order[2] != session.KindAssistantTurn {
		t.Errorf("order = %v, want the tool call and the denial before assistant_turn", order)
	}
	nd, _ := os.ReadFile(filepath.Join(env.Receipts.Dir, "events.ndjson"))
	if !strings.Contains(string(nd), `"kind":"permission_denied"`) {
		t.Errorf("events.ndjson lacks the denial:\n%s", nd)
	}
	tr, _ := os.ReadFile(filepath.Join(env.Receipts.Dir, "transcript.md"))
	for _, want := range []string{"- tool call: fs_list", "- permission granted: write_report", "- permission request denied: run_command"} {
		if !strings.Contains(string(tr), want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
	}
}

// TestSessionBudgetsEndTheSession: the user-turn cap and the
// idle timeout end the session with a session_end receipt, never a hang.
func TestSessionBudgetsEndTheSession(t *testing.T) {
	t.Run("max user turns", func(t *testing.T) {
		s, _, _, _ := startFixture(t) // MaxUserTurns: 3
		get, done := collect(s)
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			if err := s.Send(ctx, "turn"); err != nil {
				t.Fatalf("turn %d: %v", i+1, err)
			}
		}
		if err := s.Send(ctx, "one too many"); err != session.ErrEnded {
			t.Fatalf("4th turn = %v, want ErrEnded", err)
		}
		env, err := s.End(ctx, false)
		<-done
		var ce *ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitBudget || env.Status != "error" || !strings.Contains(err.Error(), "user-turn budget exceeded (3 turns)") {
			t.Fatalf("end = %v (%+v)", err, env)
		}
		last := get()[len(get())-1]
		if last.Kind != session.KindSessionEnd || last.Reason != session.EndUserTurns {
			t.Errorf("last event = %+v", last)
		}
	})
	t.Run("no user-turn cap", func(t *testing.T) {
		p, f := sessionFixture(t)
		p.Plan.Harness.Session.Budgets.MaxUserTurns = 0
		p.Budgets.MaxTurns = 0
		happyFake(f)
		h := &fakeHarness{}
		f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
		s, err := StartSession(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		_, done := collect(s)
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			if err := s.Send(ctx, "turn"); err != nil {
				t.Fatalf("turn %d: %v", i+1, err)
			}
		}
		env, err := s.End(ctx, true)
		<-done
		if err != nil || env.Usage.UserTurns != 5 || env.Usage.Turns != 5 {
			t.Fatalf("a session with no caps: %v (%+v)", err, env.Usage)
		}
	})
	t.Run("idle timeout", func(t *testing.T) {
		p, f := sessionFixture(t)
		p.Plan.Harness.Session.Budgets.IdleTimeoutSeconds = 1
		happyFake(f)
		h := &fakeHarness{}
		f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
		s, err := StartSession(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		get, done := collect(s)
		deadline := time.After(10 * time.Second)
		for {
			if ended, reason := s.Ended(); ended {
				if reason != session.EndIdle {
					t.Fatalf("ended for %s", reason)
				}
				break
			}
			select {
			case <-deadline:
				t.Fatal("idle timeout never fired")
			case <-time.After(100 * time.Millisecond):
			}
		}
		env, err := s.End(context.Background(), false)
		<-done
		var ce *ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitTimeout || env.Status != "error" || !strings.Contains(err.Error(), "session idle for more than 1s") {
			t.Fatalf("end = %v", err)
		}
		if k := kinds(get()); k[session.KindSessionEnd] != 1 {
			t.Errorf("kinds = %v", k)
		}
	})
}

// TestSessionCostBudgetNeedsACostTheHarnessReports: the ceiling fires on a
// harness that prices the session, and the receipt says plainly that it
// could not fire on one that does not. docker-agent sends `used` and no
// cost on its ACP usage_update, so a costUsd of 0 on the default adapter's
// interactive runs means "nothing was reported", not "the session was
// free".
func TestSessionCostBudgetNeedsACostTheHarnessReports(t *testing.T) {
	start := func(t *testing.T, ceiling float64, usage map[string]any) (session.Session, func() []session.Event, chan struct{}) {
		t.Helper()
		p, f := sessionFixture(t)
		p.Budgets.MaxCostUSD = ceiling
		happyFake(f)
		h := &fakeHarness{usage: usage}
		f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
		s, err := StartSession(context.Background(), f, p)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		get, done := collect(s)
		return s, get, done
	}

	t.Run("a reported cost above the ceiling ends the session", func(t *testing.T) {
		s, get, done := start(t, 1.0, map[string]any{
			"used": 1262, "size": 0,
			"cost": map[string]any{"amount": 4.5, "currency": "USD"},
		})
		ctx := context.Background()
		if err := s.Send(ctx, "turn"); err != nil && !errors.Is(err, session.ErrEnded) {
			t.Fatalf("send: %v", err)
		}
		env, err := s.End(ctx, false)
		<-done
		var ce *ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitBudget || env.Status != "error" || !strings.Contains(err.Error(), "cost budget exceeded (harness reported $4.5000, budget $1.00)") {
			t.Fatalf("end = %v (%+v)", err, env)
		}
		if env.Usage.CostUSD != 4.5 {
			t.Errorf("usage.costUsd = %v, want the 4.5 the harness reported", env.Usage.CostUSD)
		}
		if line := env.CapabilitiesEnforced["budgets.cost"]; !strings.Contains(line, "cost ceiling $1.00 enforced") || !strings.Contains(line, "$4.5000") {
			t.Errorf("budgets.cost = %q", line)
		}
		if last := get()[len(get())-1]; last.Kind != session.KindSessionEnd || last.Reason != session.EndCostBudget {
			t.Errorf("last event = %+v", last)
		}
	})

	t.Run("a cost that only reaches the ceiling does not end the session", func(t *testing.T) {
		s, _, done := start(t, 4.5, map[string]any{
			"used": 1262, "size": 0,
			"cost": map[string]any{"amount": 4.5, "currency": "USD"},
		})
		ctx := context.Background()
		if err := s.Send(ctx, "turn"); err != nil {
			t.Fatalf("send: %v", err)
		}
		env, err := s.End(ctx, true)
		<-done
		if err != nil || env.Usage.CostUSD != 4.5 {
			t.Fatalf("end = %v (%+v)", err, env)
		}
		if line := env.CapabilitiesEnforced["budgets.cost"]; line != "cost ceiling $4.50 enforced on the harness's reported running cost ($4.5000)" {
			t.Errorf("budgets.cost = %q", line)
		}
	})

	t.Run("a reported cost with no ceiling asked for is only reported", func(t *testing.T) {
		s, _, done := start(t, 0, map[string]any{
			"used": 1262, "size": 0,
			"cost": map[string]any{"amount": 4.5, "currency": "USD"},
		})
		ctx := context.Background()
		if err := s.Send(ctx, "turn"); err != nil {
			t.Fatalf("send: %v", err)
		}
		env, err := s.End(ctx, true)
		<-done
		if err != nil {
			t.Fatalf("end = %v", err)
		}
		if line := env.CapabilitiesEnforced["budgets.cost"]; line != "no cost ceiling requested; the harness reported a running cost of $4.5000" {
			t.Errorf("budgets.cost = %q", line)
		}
	})

	// A session paid by a person's plan counts tokens and never dollars: a cost
	// its harness reports is not recorded, so no ceiling can trip on it.
	t.Run("a plan-paid session records no dollars", func(t *testing.T) {
		usage := adapter.Event{Kind: adapter.EventUsage, InputTokens: 10, OutputTokens: 5, CostUSD: 4.5}
		for name, tc := range map[string]struct {
			signIn bool
			cost   float64
		}{"paid by a plan": {true, 0}, "paid by a key": {false, 4.5}} {
			s := &liveSession{
				p:      Params{Plan: &spec.Plan{}},
				events: make(chan session.Event, 4), done: make(chan struct{}),
			}
			if tc.signIn {
				s.p.Plan.Harness.ModelSignIn = &spec.ModelSignInSpec{}
			}
			s.handle(usage)
			if s.costUSD != tc.cost || s.inTok != 10 || s.outTok != 5 {
				t.Errorf("%s: cost %v and %d/%d tokens, want cost %v and 10/5 tokens", name, s.costUSD, s.inTok, s.outTok, tc.cost)
			}
		}
	})

	t.Run("a harness that reports no cost cannot trip it, and the receipt says so", func(t *testing.T) {
		s, _, done := start(t, 0.0001, map[string]any{"used": 1262, "size": 0})
		ctx := context.Background()
		if err := s.Send(ctx, "turn"); err != nil && !errors.Is(err, session.ErrEnded) {
			t.Fatalf("send: %v", err)
		}
		env, err := s.End(ctx, false)
		<-done
		if err != nil {
			t.Fatalf("end = %v, want the session to finish: a ceiling nothing can trip is not a breach", err)
		}
		if env.Usage.CostUSD != 0 {
			t.Errorf("usage.costUsd = %v, want 0", env.Usage.CostUSD)
		}
		line := env.CapabilitiesEnforced["budgets.cost"]
		if !strings.Contains(line, "requested but not enforceable") || !strings.Contains(line, "not because the session cost nothing") {
			t.Errorf("budgets.cost = %q", line)
		}
	})
}

// TestASessionWhoseHarnessReportsNoTokensSaysSo: docker-agent 1.150.0 sends
// its ACP usage_update only for a model whose context window it knows, so a
// session on any other model reports no tokens at all. Its receipt says the
// 0 is unreported rather than leaving it to read as a session that used
// none; a harness that reports keeps no such line.
func TestASessionWhoseHarnessReportsNoTokensSaysSo(t *testing.T) {
	end := func(t *testing.T, usage map[string]any) *receipt.Envelope {
		t.Helper()
		p, f := sessionFixture(t)
		happyFake(f)
		h := &fakeHarness{usage: usage, bareResult: true}
		f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
		s, err := StartSession(context.Background(), f, p)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		_, done := collect(s)
		ctx := context.Background()
		if err := s.Send(ctx, "turn"); err != nil {
			t.Fatalf("send: %v", err)
		}
		env, err := s.End(ctx, false)
		<-done
		if err != nil {
			t.Fatalf("end: %v", err)
		}
		return env
	}

	t.Run("no usage_update", func(t *testing.T) {
		env := end(t, nil)
		if env.Usage.TotalTokens != 0 {
			t.Errorf("usage.totalTokens = %d, want 0", env.Usage.TotalTokens)
		}
		line := env.CapabilitiesEnforced["usage.tokens"]
		if !strings.Contains(line, "reported no token usage") || !strings.Contains(line, "not because the session used none") {
			t.Errorf("usage.tokens = %q", line)
		}
		// With no ceiling asked for either, the cost line says the 0 is unreported.
		if line := env.CapabilitiesEnforced["budgets.cost"]; !strings.Contains(line, "reported no cost on its event stream") || !strings.Contains(line, "not because the session cost nothing") {
			t.Errorf("budgets.cost = %q", line)
		}
	})

	t.Run("a usage_update", func(t *testing.T) {
		env := end(t, map[string]any{"used": 1262, "size": 262144})
		if env.Usage.TotalTokens != 1262 {
			t.Errorf("usage.totalTokens = %d, want 1262", env.Usage.TotalTokens)
		}
		if line, ok := env.CapabilitiesEnforced["usage.tokens"]; ok {
			t.Errorf("usage.tokens = %q, want no line when the harness reported", line)
		}
	})
}

// TestStartSessionRefusesAutonomousPlan: the session API only drives a
// conversational plan, interactive or persistent.
func TestStartSessionRefusesAutonomousPlan(t *testing.T) {
	p, f := fixture(t) // autonomous plan
	if _, err := StartSession(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "not a conversational plan") {
		t.Fatalf("got %v", err)
	}
}

// TestIntakeStagedAndReceipted: intake answers reach the agent as
// /inputs/intake.json (secrets by name only), file answers are staged,
// the receipt copy is written, and a session records an intake event.
func TestIntakeStagedAndReceipted(t *testing.T) {
	p, f := sessionFixture(t)
	data := filepath.Join(t.TempDir(), "data.csv")
	if err := os.WriteFile(data, []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Intake = &intake.Answers{
		Values:  map[string]string{"region": "apac"},
		Files:   map[string]string{"dataset": data},
		Secrets: map[string]string{"api_token": "hush-hush-1234"},
	}
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	env, err := s.End(context.Background(), false)
	<-done
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", "5e5510", "inputs", "intake.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(staged, &doc)
	if doc["region"] != "apac" || doc["dataset"] != "/inputs/data.csv" {
		t.Errorf("intake.json = %s", staged)
	}
	if sec, _ := doc["api_token"].(map[string]any); sec["secret"] != "API_TOKEN" || strings.Contains(string(staged), "hush-hush") {
		t.Errorf("secret answer staged wrongly: %s", staged)
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "5e5510", "inputs", "data.csv")); err != nil {
		t.Errorf("file answer not staged: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, "intake.json")); err != nil || !strings.Contains(string(b), "apac") || strings.Contains(string(b), "hush-hush") {
		t.Errorf("intake receipt: %v %s", err, b)
	}
	if k := kinds(get()); k[session.KindIntake] != 1 {
		t.Errorf("kinds = %v", k)
	}
	// A session that took no turn does not claim its tokens went unreported.
	if line, has := env.CapabilitiesEnforced["usage.tokens"]; has {
		t.Errorf("usage.tokens = %q for a session with no turns", line)
	}

	// A file answer may not take the name of an input, and no input may take
	// the name the intake answers are staged under.
	for name, tc := range map[string]struct{ input, file, msg string }{
		"a file answer named like an input": {"data.csv", "data.csv", "intake file"},
		"an input named like the answers":   {"intake.json", "", "reserved for the intake answers"},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := sessionFixture(t)
			happyFake(f)
			p.InputPaths = []string{writeInput(t, t.TempDir(), tc.input, "x")}
			p.Intake = &intake.Answers{Values: map[string]string{"region": "apac"}}
			if tc.file != "" {
				p.Intake.Files = map[string]string{"dataset": writeInput(t, t.TempDir(), tc.file, "y")}
			}
			_, err := StartSession(context.Background(), f, p)
			var ce *ClassifiedError
			r := receipt.ReasonOf(err)
			if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), tc.msg) || r == nil || r.Code != receipt.ReasonInputRefused || r.Detail != receipt.InputNameClash {
				t.Fatalf("err = %v (reason %+v), want a name clash saying %q", err, r, tc.msg)
			}
		})
	}
}

// TestSessionHandshakeFailureMasksStderr: a harness that dies during the
// handshake, printing its environment on the way out, leaves neither the
// error nor the stderr receipt carrying a secret — the failure waits for
// the process to exit and the receipt to be masked before it returns.
func TestSessionHandshakeFailureMasksStderr(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	// A stderr receipt an earlier session left in the output folder is
	// replaced, not added to.
	if err := os.MkdirAll(filepath.Join(p.OutDir, "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.OutDir, "receipts", "agent-stderr.log"), []byte("stale line from an earlier session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The harness refuses the handshake and then hangs until it is
	// killed, dying with docker's usual latency: the receipt is masked
	// only once it has exited, so the failure has to wait for that.
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(ctx context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stderr, "boot: ENGINE_TEST_TOKEN=sekret-value-1234 ANTHROPIC_API_KEY=sk-test-key-9876\nfatal: no model\n")
		sc := bufio.NewScanner(stdin)
		if sc.Scan() {
			var m struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(sc.Bytes(), &m)
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "no model"}})
			_, _ = stdout.Write(append(b, '\n'))
		}
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond) // docker's kill latency: the file is still open here
		return 137, nil
	}}
	_, err := StartSession(context.Background(), f, p)
	if err == nil {
		t.Fatal("a harness that died in the handshake started a session")
	}
	for _, secret := range []string{"sekret-value-1234", "sk-test-key-9876"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error carries %q: %v", secret, err)
		}
	}
	b, rerr := os.ReadFile(filepath.Join(p.OutDir, "receipts", "agent-stderr.log"))
	if rerr != nil {
		t.Fatalf("stderr receipt: %v", rerr)
	}
	if got := string(b); got != "boot: ENGINE_TEST_TOKEN=[redacted] ANTHROPIC_API_KEY=[redacted]\nfatal: no model\n" {
		t.Errorf("agent-stderr.log = %q", got)
	}
	// Nothing holds the receipts: on Windows an open one could not be
	// moved or deleted until the process exits. The events receipt was
	// closed after the harness's session_end, not before it.
	assertReceiptsReleased(t, p.OutDir, "a session whose handshake failed")
	if ev, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson")); !strings.Contains(string(ev), `"session_end"`) {
		t.Errorf("events.ndjson lacks the session's end: %q", ev)
	}
}

// failingSessionAdapter is the default adapter with no interactive
// transport to give: its Session fails as a driver that cannot be made.
type failingSessionAdapter struct{ adapter.Adapter }

func (failingSessionAdapter) Session(context.Context, io.ReadWriteCloser, adapter.SessionOptions) (adapter.SessionDriver, error) {
	return nil, errors.New("no session transport")
}

// A driver that cannot be made fails the start with the harness running:
// its receipts are released once it has exited.
func TestSessionWhoseDriverCannotBeMadeClosesItsReceipts(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	p.Adapter = failingSessionAdapter{p.Adapter}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(ctx context.Context, _ dockerx.RunSpec, _ io.Reader, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		return 137, nil
	}}
	_, err := StartSession(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "no session transport") {
		t.Fatalf("err = %v, want the driver's failure", err)
	}
	assertReceiptsReleased(t, p.OutDir, "a session whose driver could not be made")
}

// A host-run harness that cannot be started fails the session before
// the goroutine that closes its stderr receipt exists: the start's
// failure closes both receipts itself.
func TestSessionWhoseHostHarnessCannotStartClosesItsReceipts(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	t.Setenv("PATH", t.TempDir())
	p.Plan.Harness.Host = &spec.HostProcessSpec{Command: "agentry-no-such-harness"}
	_, err := StartSession(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCompile || !strings.Contains(err.Error(), "agentry-no-such-harness") {
		t.Fatalf("err = %v, want the harness named as not runnable", err)
	}
	assertReceiptsReleased(t, p.OutDir, "a session whose harness could not start")
}

// TestSessionStoreIsARunVolume: an interactive run's session store is a
// per-run named volume labelled with the run id — created before the
// harness, given to the harness image's own user by a one-shot as root
// on no network, mounted at /session without a relabel, removed at
// teardown after the containers — and no host directory is made for it,
// world-writable or otherwise.
func TestSessionStoreIsARunVolume(t *testing.T) {
	s, _, p, f := startFixture(t)
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "5e5510", "session")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a host session directory exists: %v", err)
	}
	// The ownership one-shot: the harness image, root, no network, the
	// volume mounted, chown to the image's user.
	init, ok := f.FindRun("-session-init")
	if !ok {
		t.Fatal("no session-init container ran")
	}
	if init.Image != p.Plan.Harness.Agent.Image || init.User != "0:0" || init.Network != "none" || init.Entrypoint != "sh" ||
		len(init.Mounts) != 1 || init.Mounts[0].Source != "agentry-5e5510-session" || init.Mounts[0].Target != spec.SessionMount || init.Mounts[0].Relabel {
		t.Errorf("session-init spec = %+v", init)
	}
	if strings.Join(init.Argv, " ") != `-c chown -- "$1" "$2" sh docker-agent /session` {
		t.Errorf("session-init argv = %q", init.Argv)
	}
	// The container posture: the one-shot keeps CHOWN alone; the
	// session's agent is hardened and read-only like the exec path's,
	// and the receipt says both.
	if !init.AutoRemove || init.Labels["agentry.run"] != "5e5510" {
		t.Errorf("session-init is kept after it ends or carries no run label: auto-remove %v labels %v", init.AutoRemove, init.Labels)
	}
	if strings.Join(init.CapDrop, ",") != "ALL" || strings.Join(init.CapAdd, ",") != "CHOWN" || !init.NoNewPrivileges || init.PidsLimit == 0 || init.Memory == "" {
		t.Errorf("session-init posture: drop %v add %v nnp %v pids %d memory %q", init.CapDrop, init.CapAdd, init.NoNewPrivileges, init.PidsLimit, init.Memory)
	}
	if a, _ := f.FindRun("-agent"); strings.Join(a.CapDrop, ",") != "ALL" || len(a.CapAdd) != 0 || !a.NoNewPrivileges || !a.ReadOnly || len(a.Tmpfs) == 0 {
		t.Errorf("session agent posture: drop %v add %v nnp %v read-only %v tmpfs %v", a.CapDrop, a.CapAdd, a.NoNewPrivileges, a.ReadOnly, a.Tmpfs)
	}
	// The harness mounts the volume, not a path, and without a relabel.
	agent, ok := f.FindRun("-agent")
	if !ok {
		t.Fatal("no agent container ran")
	}
	var mounted bool
	for _, m := range agent.Mounts {
		if m.Target == spec.SessionMount {
			mounted = true
			if m.Source != "agentry-5e5510-session" || m.ReadOnly || m.Relabel {
				t.Errorf("session mount = %+v", m)
			}
		}
	}
	if !mounted {
		t.Error("the harness has no /session mount")
	}
	env, err := s.End(context.Background(), true)
	if err != nil {
		t.Fatalf("end: %v (%+v)", err, env)
	}
	// Order: the volume is created before the init and the harness, and
	// removed after the containers are gone; the sweep lists volumes by
	// the run label.
	ops := f.Ops()
	idx := func(op string) int {
		for i, o := range ops {
			if o == op {
				return i
			}
		}
		return -1
	}
	create, remove, agentRun, sweep := idx("VolumeCreate:agentry-5e5510-session"), idx("VolumeRemove:agentry-5e5510-session"), idx("RunForeground:agentry-5e5510-agent"), idx("VolumesByLabel:agentry.run=5e5510")
	if create < 0 || remove < 0 || agentRun < 0 || sweep < 0 || create > agentRun || remove < agentRun || sweep < remove {
		t.Errorf("order: create=%d agent=%d remove=%d sweep=%d in %v", create, agentRun, remove, sweep, ops)
	}
	for i, o := range ops {
		if strings.HasPrefix(o, "RemoveForce:agentry-5e5510-agent") && i > remove {
			t.Errorf("the volume was removed before its container: %v", ops)
		}
	}

	// A store that cannot be handed to the harness user fails the start.
	p2, f2 := sessionFixture(t)
	happyFake(f2)
	f2.Foreground["-session-init"] = &dockerx.FakeForeground{ExitCode: 1}
	var ce *ClassifiedError
	if _, err := StartSession(context.Background(), f2, p2); !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "give the session store to the harness user") {
		t.Errorf("a store the one-shot could not hand over: %v", err)
	}
}

// An interactive run's containers that hold the QuickAgent's code — the
// session store's ownership one-shot on the harness image and the agent
// — are created under the policy runtime too; the runner's own proxy and
// gateway are not.
func TestSessionContainersCarryTheRuntime(t *testing.T) {
	p, f := sessionFixture(t)
	p.Plan.Docker = &spec.DockerSpec{Runtime: "runsc"}
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatalf("end: %v", err)
	}
	for _, name := range []string{"-session-init", "-agent"} {
		c, ok := f.FindRun(name)
		if !ok {
			t.Errorf("no %s container ran", name)
			continue
		}
		if c.Runtime != "runsc" {
			t.Errorf("%s created without the policy runtime: %q", c.Name, c.Runtime)
		}
	}
	if agent, _ := f.FindRun("-agent"); len(agent.ExtraHosts) != 2 {
		t.Errorf("session agent extra hosts = %v, want the two aliases pinned", agent.ExtraHosts)
	}
	for _, name := range []string{"-proxy", "-gateway"} {
		if c, _ := f.FindRun(name); c.Runtime != "" {
			t.Errorf("%s is the runner's own and must stay on the daemon's default: %q", c.Name, c.Runtime)
		}
	}
}

// TestSessionEndIsSafeToCallConcurrently: two callers ending the session at
// once — the terminal's End and runInteractive's deferred one, or a window
// closing mid-run — both get the one envelope, and neither closes the event
// stream a second time.
func TestSessionEndIsSafeToCallConcurrently(t *testing.T) {
	s, _, _, _ := startFixture(t)
	_, done := collect(s)
	var wg sync.WaitGroup
	envs := make([]*receipt.Envelope, 4)
	panics := make([]any, 4)
	for i := range envs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { panics[i] = recover() }()
			envs[i], _ = s.End(context.Background(), false)
		}(i)
	}
	wg.Wait()
	<-done
	for i := range envs {
		if panics[i] != nil {
			t.Fatalf("End call %d panicked: %v", i, panics[i])
		}
		if envs[i] == nil || envs[i] != envs[0] {
			t.Errorf("End call %d returned %p, want the one envelope %p", i, envs[i], envs[0])
		}
	}
}

// An interactive session's envelope names the policy file the same way a
// run's does.
func TestSessionEnvelopeNamesThePolicyFile(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	p.PolicyFile = &policy.File{Path: "/etc/agentry/policy.yaml", Source: receipt.PolicySourceFlag}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, done := collect(s)
	env, err := s.End(context.Background(), true)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	<-done
	if env.Policy == nil || env.Policy.Path != "/etc/agentry/policy.yaml" || env.Policy.Source != receipt.PolicySourceFlag || env.Policy.SHA256 != p.Plan.PolicySHA256 {
		t.Fatalf("session policy = %+v", env.Policy)
	}
}

// A file added the moment a turn's assistant_turn event is seen gets its
// own notice turn. The session protocol's client reacts to that event at
// once, so a turn must not emit it while still holding the turn lock: an
// add in that window would find the session "busy" and its notice would
// wait for the next user turn, giving input_added and nothing more, and
// the protocol's events would differ from the API's. The test reacts to
// every assistant_turn from a user turn as that client does, many times,
// so the window is hit if it is still there.
func TestAnInputAddedAsATurnEndsGetsItsNoticeTurn(t *testing.T) {
	ctx := context.Background()
	const rounds = 200
	// Room for every round's two turns: the fixture's caps are 3 user
	// turns and 10 harness turns.
	s, _, _ := startAdapterSession(t, ctx, adapter.DefaultName, func(p *Params, _ *fakeHarness) {
		p.Plan.Harness.Session.Budgets.MaxUserTurns = rounds + 1
		p.Budgets.MaxTurns = 4 * rounds
	})
	inputs := t.TempDir()
	for i := 0; i < rounds; i++ {
		path := filepath.Join(inputs, fmt.Sprintf("notes-%d.txt", i))
		if err := os.WriteFile(path, []byte("notes"), 0o644); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(10 * time.Second)
		sent := make(chan error, 1)
		go func() { sent <- s.Send(ctx, fmt.Sprintf("turn %d", i)) }()
		// Read to this user turn's assistant_turn, as the protocol's reader
		// does, and add the file at once.
		for answered := false; !answered; {
			select {
			case ev := <-s.Events():
				answered = ev.Kind == session.KindAssistantTurn
			case <-deadline:
				t.Fatalf("round %d: the user turn was never answered", i)
			}
		}
		if err := s.AddInput(ctx, path); err != nil {
			t.Fatalf("round %d: add: %v", i, err)
		}
		select {
		case err := <-sent:
			if err != nil {
				t.Fatalf("round %d: send: %v", i, err)
			}
		case <-deadline:
			t.Fatalf("round %d: Send never returned", i)
		}
		// The add's own turn: its input_added, the synthetic user turn that
		// tells the agent, and that turn's answer, before anything else.
		want := []session.Kind{session.KindInputAdded, session.KindUserTurn, session.KindAssistantTurn}
		var got []session.Kind
		synthetic := false
		for len(got) < len(want) {
			select {
			case ev := <-s.Events():
				if ev.Kind != session.KindInputAdded && ev.Kind != session.KindUserTurn && ev.Kind != session.KindAssistantTurn {
					continue
				}
				if ev.Kind == session.KindUserTurn {
					synthetic = ev.Synthetic
				}
				got = append(got, ev.Kind)
			case <-deadline:
				t.Fatalf("round %d: the add's notice turn never came; read %v", i, got)
			}
		}
		if !slices.Equal(got, want) || !synthetic {
			t.Fatalf("round %d: after the add read %v (synthetic user turn %v), want %v", i, got, synthetic, want)
		}
	}
	if _, err := s.End(ctx, false); err != nil {
		t.Logf("end: %v", err)
	}
	// After End, an add starts no turn.
	path := filepath.Join(inputs, "late.txt")
	if err := os.WriteFile(path, []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInput(ctx, path); !errors.Is(err, session.ErrEnded) {
		t.Fatalf("an add after End: %v, want ErrEnded", err)
	}
}

// TestANoticeIsCarriedNotPutInThePersonsTurn: a notice that waited for the
// next turn (an input added while a turn was in flight) reaches the harness
// ahead of that turn, and the turn's user_turn event holds the person's text
// alone; the notice is recorded as carried.
func TestANoticeIsCarriedNotPutInThePersonsTurn(t *testing.T) {
	ctx := context.Background()
	arrived, release := make(chan string, 2), make(chan struct{}, 2)
	s, _, _ := startAdapterSession(t, ctx, adapter.DefaultName, func(p *Params, h *fakeHarness) {
		h.onPrompt = func(text string) { arrived <- text; <-release }
	})
	get, done := collect(s)
	sent := make(chan error, 1)
	go func() { sent <- s.Send(ctx, "first") }()
	first := <-arrived
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The first turn is in flight, so the notice waits for the next.
	if err := s.AddInput(ctx, path); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	if err := s.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	second := <-arrived
	if _, err := s.End(ctx, false); err != nil {
		t.Fatal(err)
	}
	<-done
	var turns []string
	carried := false
	for _, ev := range get() {
		if ev.Kind == session.KindUserTurn && !ev.Synthetic {
			turns = append(turns, ev.Text)
		}
		if ev.Kind == session.KindOther && strings.Contains(ev.Text, "carried into this turn") && strings.Contains(ev.Text, "notes.txt") {
			carried = true
		}
	}
	if !slices.Equal(turns, []string{"first", "second"}) || !carried {
		t.Errorf("the person's turns %q, the notice carried %v", turns, carried)
	}
	if strings.Contains(first, "notes.txt") || !strings.Contains(second, "notes.txt") || !strings.HasSuffix(second, "second") {
		t.Errorf("the harness was sent %q then %q", first, second)
	}
}

// jsonSession starts a session of a plan that declares an output schema, the
// harness answering the final-result prompt with the JSON the schema wants and
// every other turn with text.
func jsonSession(t *testing.T) (session.Session, chan string) {
	t.Helper()
	p, f := sessionFixture(t)
	happyFake(f)
	schema := []byte(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`)
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, compile.OutputSchemaFile), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Plan.Harness.ResultKind = "json"
	p.Plan.Output = spec.OutputSpec{SchemaFile: compile.OutputSchemaFile, SchemaSHA256: sha256Hex(schema)}
	prompts := make(chan string, 8)
	h := &fakeHarness{
		onPrompt: func(text string) { prompts <- text },
		answer: func(text string) string {
			if strings.HasPrefix(text, "Reply now with your final result as JSON") {
				return `{"answer":42}`
			}
			return "working on it"
		},
	}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return s, prompts
}

// TestASessionOfAJSONPlanAsksForItsResultOnlyWhenFinished: finishing a
// session of a plan with an output schema asks the agent for its final
// result as JSON and holds it to the schema; quitting asks nothing and holds
// the session to nothing.
func TestASessionOfAJSONPlanAsksForItsResultOnlyWhenFinished(t *testing.T) {
	ctx := context.Background()
	t.Run("finish", func(t *testing.T) {
		s, prompts := jsonSession(t)
		_, done := collect(s)
		if err := s.Send(ctx, "hello"); err != nil {
			t.Fatal(err)
		}
		env, err := s.End(ctx, true)
		<-done
		if err != nil || string(env.Result) != `{"answer":42}` || env.Status != "ok" {
			t.Fatalf("a finished session: %v, result %s (%+v)", err, env.Result, env)
		}
		close(prompts)
		var asked []string
		for text := range prompts {
			asked = append(asked, text)
		}
		if len(asked) != 2 || !strings.HasPrefix(asked[1], "Reply now with your final result as JSON") {
			t.Fatalf("the agent was prompted %q", asked)
		}
	})
	t.Run("quit", func(t *testing.T) {
		s, prompts := jsonSession(t)
		_, done := collect(s)
		if err := s.Send(ctx, "hello"); err != nil {
			t.Fatal(err)
		}
		env, err := s.End(ctx, false)
		<-done
		if err != nil || env.Status != "ok" || string(env.Result) != `"working on it"` {
			t.Fatalf("a session that quit: %v, result %s (%+v)", err, env.Result, env)
		}
		close(prompts)
		var asked []string
		for text := range prompts {
			asked = append(asked, text)
		}
		if len(asked) != 1 {
			t.Fatalf("the agent was prompted %q", asked)
		}
	})
}

// TestASessionWhoseEventPumpPanicsEndsAndNamesIt: a panic while the session
// records the harness's events ends the session, tears it down and is the
// error End returns, whatever reason the recovery ended it with.
func TestASessionWhoseEventPumpPanicsEndsAndNamesIt(t *testing.T) {
	pumpEventHook = func() { panic("the pump exploded") }
	t.Cleanup(func() { pumpEventHook = nil })
	s, _, _, f := startFixture(t)
	_, done := collect(s)
	ctx := context.Background()
	_ = s.Send(ctx, "hello") // the turn's events reach the pump, which panics
	env, err := s.End(ctx, false)
	<-done
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "session event pump panicked: the pump exploded") {
		t.Fatalf("End = %v, want the panic named under exit %d", err, ExitCLI)
	}
	if env == nil || env.Status != "error" || !strings.Contains(env.Error, "the pump exploded") {
		t.Fatalf("envelope = %+v", env)
	}
	if !strings.Contains(strings.Join(f.Ops(), "\n"), "NetworkRemove:agentry-5e5510-net") {
		t.Fatalf("teardown did not run: %v", f.Ops())
	}
}
