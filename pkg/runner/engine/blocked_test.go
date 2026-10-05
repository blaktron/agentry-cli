package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// blockedRun runs the fixture with end_blocked granted; ledger, when not
// nil, is what the tools server wrote while the agent ran.
func blockedRun(t *testing.T, ledger []string) (*receipt.Envelope, Params, error) {
	t.Helper()
	p, f := fixture(t)
	happyFake(f)
	p.EndBlocked = true
	p.KeepRunDir = true
	f.OnForeground = func(r dockerx.RunSpec) {
		if strings.HasSuffix(r.Name, "-agent") && ledger != nil {
			_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", CallsLedgerFile), []byte(strings.Join(ledger, "\n")+"\n"), 0o644)
		}
	}
	env, err := Run(context.Background(), f, p)
	return env, p, err
}

const startLine = `{"start":true}`

func isBlocked(t *testing.T, env *receipt.Envelope, err error, code receipt.ReasonCode, detail string) {
	t.Helper()
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBlocked {
		t.Fatalf("err %v, want the blocked class", err)
	}
	b, _ := env.Marshal()
	var got struct {
		ExitClass string          `json:"exitClass"`
		Stage     string          `json:"stage"`
		Reason    *receipt.Reason `json:"reason"`
	}
	_ = json.Unmarshal(b, &got)
	if env.Status != "error" || got.ExitClass != "blocked" || got.Stage != receipt.StageAgent ||
		got.Reason == nil || got.Reason.Code != code || got.Reason.Detail != detail || len(got.Reason.Names) != 0 {
		t.Fatalf("envelope status=%s %+v", env.Status, got)
	}
	// The agent's last message is still the run's answer.
	if string(env.Result) != `"final answer"` {
		t.Fatalf("result %s", env.Result)
	}
	if _, rerr := os.Stat(filepath.Join(env.OutDir, "result.json")); rerr != nil {
		t.Fatalf("no result.json: %v", rerr)
	}
}

// The agent's own end_blocked ends the run blocked with its sentence in the
// error, masked like any receipt, and a reason that carries none of it.
func TestAnAgentsEndBlockedEndsTheRunBlocked(t *testing.T) {
	env, p, err := blockedRun(t, []string{
		startLine,
		`{"tool":"list_inputs","ok":true}`,
		`{"tool":"read_input","ok":true}`,
		`{"tool":"end_blocked","ok":true,"blocked":"the archive is encrypted and the key sekret-value-1234 did not open it"}`,
		`{"tool":"end_blocked","ok":true,"blocked":"a second word"}`,
	})
	isBlocked(t, env, err, receipt.ReasonAgentBlocked, "")
	if !strings.Contains(env.Error, "blocked: the agent could not do the task: the archive is encrypted") ||
		strings.Contains(env.Error, "sekret-value-1234") || strings.Contains(env.Error, "second word") {
		t.Fatalf("error %q", env.Error)
	}
	gw := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway")
	b, rerr := os.ReadFile(filepath.Join(gw, "toolspec.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Calls.Ledger != "/out/receipts/"+CallsLedgerFile || !slices.Contains(ts.GrantedTools(), toolsrv.ToolEndBlocked) {
		t.Fatalf("calls %+v granted %v", ts.Calls, ts.GrantedTools())
	}
	if tools, _ := os.ReadFile(filepath.Join(gw, "tools.yaml")); !strings.Contains(string(tools), toolsrv.ToolEndBlocked) {
		t.Fatalf("tools.yaml lacks end_blocked:\n%s", tools)
	}
}

// The runner's own evidence, each a fault of the run's machinery; it comes
// before the agent's word, since a failing tool is why an agent says it is
// blocked.
func TestTheRunnersEvidenceEndsTheRunBlocked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger []string
		detail string
	}{
		{"no ledger", nil, receipt.BlockedToolsUnrecorded},
		{"no start line", []string{`{"tool":"read_input","ok":true}`}, receipt.BlockedToolsUnrecorded},
		{"every input call failed", []string{startLine,
			`{"tool":"list_inputs"}`, `{"tool":"read_input"}`, `{"tool":"search_inputs"}`, `{"tool":"write_report","ok":true}`,
			`{"tool":"end_blocked","ok":true,"blocked":"nothing to read"}`}, receipt.BlockedInputsUnread},
		{"every report failed", []string{startLine,
			`{"tool":"read_input","ok":true}`, `{"tool":"write_report"}`, `{"tool":"write_report"}`}, receipt.BlockedReportUnwritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _, err := blockedRun(t, tc.ledger)
			isBlocked(t, env, err, receipt.ReasonRunnerBlocked, tc.detail)
			if strings.Contains(env.Error, "nothing to read") {
				t.Fatalf("the runner's evidence carried the agent's sentence: %q", env.Error)
			}
		})
	}
}

// A run whose tools answered is ok: one failed call among good ones, a run
// that never touched the inputs or the reports, and an unreadable line.
func TestARunWhoseToolsAnsweredIsOK(t *testing.T) {
	for _, ledger := range [][]string{
		{startLine},
		{startLine, `{"tool":"read_input"}`, `{"tool":"read_input","ok":true}`, `{"tool":"write_report"}`, `{"tool":"write_report","ok":true}`},
		{startLine, `{"tool":"fetch"}`, `{"tool":`},
	} {
		env, _, err := blockedRun(t, ledger)
		if err != nil || env.ExitCode != ExitOK || env.Reason != nil {
			t.Fatalf("%v: err %v envelope %+v", ledger, err, env)
		}
	}
}

// Without the grant (a built-in's run) nothing changes: no ledger in the
// tools spec, no end_blocked, and a run with no ledger is ok.
func TestWithoutEndBlockedARunIsAsBefore(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	env, err := Run(context.Background(), f, p)
	if err != nil || env.ExitCode != ExitOK {
		t.Fatalf("%v %+v", err, env)
	}
	b, _ := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
	if strings.Contains(string(b), `"calls"`) || strings.Contains(string(b), toolsrv.ToolEndBlocked) {
		t.Fatalf("toolspec:\n%s", b)
	}
}

// A session does not take end_blocked: its person sees the agent stop.
func TestASessionRefusesEndBlocked(t *testing.T) {
	p, f := sessionFixture(t)
	p.EndBlocked = true
	_, err := StartSession(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitUsage || !strings.Contains(err.Error(), "end_blocked") {
		t.Fatalf("%v", err)
	}
}

// A harness failure keeps its own class, whatever the ledger says.
func TestAHarnessFailureIsNotBlocked(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.EndBlocked = true
	f.Foreground["-agent"].ExitCode = 3
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness {
		t.Fatalf("%v", err)
	}
}
