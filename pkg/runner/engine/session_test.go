package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
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
	denied  bool
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
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": "s1"}})
		case "session/prompt":
			text := ""
			if len(m.Params.Prompt) > 0 {
				text = m.Params.Prompt[0].Text
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "fs_list", "status": "pending"}}})
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
			reply := "You said: " + text + ". The token is sekret-value-1234 and the key is sk-test-key-9876."
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": reply}}}})
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
	return 0, nil // stdin EOF: clean exit like the real harness
}

func startFixture(t *testing.T) (session.Session, *fakeHarness, Params, *dockerx.Fake) {
	t.Helper()
	p, f := sessionFixture(t)
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

// TestSessionTurnsInputsAndReceipts covers plan §6.10 lines 2-5 in one
// conversation: turns reach the harness only as session/prompt, an input
// lands under the staged inputs and is announced, the permission request
// is denied and logged, and the receipts redact the secret and the key.
func TestSessionTurnsInputsAndReceipts(t *testing.T) {
	s, h, p, f := startFixture(t)
	get, done := collect(s)
	ctx := context.Background()

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
	if err := s.AddInput(ctx, t.TempDir()); err == nil {
		t.Error("a directory was accepted as an input")
	}
	env, err := s.End(ctx, true)
	if err != nil {
		t.Fatalf("end: %v (envelope %+v)", err, env)
	}
	<-done

	// Only protocol turns reached the harness (line 2).
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
	// The staged input, and the agent-side mount unchanged (line 3).
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
	if k[session.KindUserTurn] != 2 || k[session.KindAssistantTurn] != 2 || k[session.KindInputAdded] != 1 || k[session.KindPermissionDenied] != 2 || k[session.KindToolCall] != 2 || k[session.KindSessionEnd] != 1 {
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
	// Envelope and receipts (line 5: redaction).
	if env.Status != "ok" || env.Harness.Mode != "interactive" || env.Usage.UserTurns != 1 || env.Usage.Turns != 2 || env.Usage.InputTokens != 20 {
		t.Errorf("envelope = status %s mode %s usage %+v", env.Status, env.Harness.Mode, env.Usage)
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
	for _, want := range []string{"## User\n\nhello there", "## Assistant", "- tool call: fs_list", "- permission request denied: run_command", "- input added: /inputs/notes.txt (sha256:", "_Session ended: finished._"} {
		if !strings.Contains(string(tr), want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
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

// TestSessionBudgetsEndTheSession covers line 6: the user-turn cap and the
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
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitBudget || env.Status != "error" {
			t.Fatalf("end = %v (%+v)", err, env)
		}
		last := get()[len(get())-1]
		if last.Kind != session.KindSessionEnd || last.Reason != session.EndUserTurns {
			t.Errorf("last event = %+v", last)
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
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitTimeout || env.Status != "error" {
			t.Fatalf("end = %v", err)
		}
		if k := kinds(get()); k[session.KindSessionEnd] != 1 {
			t.Errorf("kinds = %v", k)
		}
	})
}

// TestStartSessionRefusesAutonomousPlan: the session API only drives an
// interactive plan.
func TestStartSessionRefusesAutonomousPlan(t *testing.T) {
	p, f := fixture(t) // autonomous plan
	if _, err := StartSession(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "not an interactive plan") {
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
}
