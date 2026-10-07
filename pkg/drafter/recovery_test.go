package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

func TestARecoveredAnswerStillGetsItsRulesRepair(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		wantRepair    bool
	}{
		{"reports written", "Both reports are written. Here is the draft.\n" + postRepaired, false},
		{"rules failure", "Done.\n" + postDraft, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, "reports", "AGENT.md"), []byte("# Meeting actions\n\nRead the notes and list decisions.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := PostStep{OutDir: out, Scan: webscan.Result{Verdict: webscan.Pass}, Checker: &postChecker{}}
			p.Repair = func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
				if !tc.wantRepair || !strings.Contains(rt.Task, "V10 intake[0].choices") {
					t.Fatalf("unexpected repair task: %s", rt.Task)
				}
				return &receipt.Envelope{Status: "ok", Result: json.RawMessage(postRepaired)}, nil
			}
			env := firstTurn(t, out, postRepaired)
			env.Result, _ = json.Marshal(tc.message)
			env.Status, env.ExitCode, env.ExitClass, env.Error, env.Stage = "error", 17, "outputSchema", "not JSON", "output"
			d, got, err := p.Run(context.Background(), env, engine.Classify(engine.ExitOutputSchema, errors.New("not JSON")))
			if err != nil {
				t.Fatal(err)
			}
			if !d.Recovered || !d.Passed() || d.IsRepaired() != tc.wantRepair || !strings.Contains(d.Notes, "## Recovered") ||
				got.Status != "ok" || got.ExitCode != 0 || got.Error != "" || got.Stage != "" || got.Usage.InputTokens != 1000 {
				t.Fatalf("draft %+v, notes %s, envelope %+v", d, d.Notes, got)
			}
			if !strings.Contains(d.Notes, "characters of text before the answer") || d.AIReview.Check.Verdict() != CheckPass {
				t.Fatalf("notes %s, check %+v", d.Notes, d.AIReview.Check)
			}
		})
	}
}

func TestTheRecordedVultrAnswerIsRecovered(t *testing.T) {
	message, err := os.ReadFile("testdata/vultr-revision-last-message.txt")
	if err != nil {
		t.Fatal(err)
	}
	env := &receipt.Envelope{}
	env.Result, _ = json.Marshal(string(message))
	p := PostStep{OutDir: t.TempDir(), Scan: webscan.Result{Verdict: webscan.Pass}}
	d, got, err := p.Run(context.Background(), env, engine.Classify(engine.ExitOutputSchema, errors.New("invalid character 'A'")))
	if err != nil || d == nil || !d.Recovered || got.Status != "ok" || !strings.Contains(d.Notes, "50 characters") {
		t.Fatalf("draft %+v, envelope %+v, error %v", d, got, err)
	}
	if !strings.Contains(string(d.Manifest), "api.github.com") || !strings.Contains(string(d.Manifest), "optionalSecrets") || len(d.Corrections()) == 0 {
		t.Fatalf("recovered manifest %s, corrections %v", d.Manifest, d.Corrections())
	}
}

// Replay the actual refused message through the engine's harness boundary
// and the post-step, retaining the raw receipt and reports throughout.
func TestTheRecordedAnswerSurvivesTheRunnerAndPostStep(t *testing.T) {
	message, err := os.ReadFile("testdata/vultr-revision-last-message.txt")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	for _, name := range []string{"agent.yaml", "AGENT.md", compile.OutputSchemaFile} {
		data, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := fs.ReadFile(FS(), "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	planDir := t.TempDir()
	plan, err := compile.Compile(compile.Inputs{Manifest: m, Digest: "sha256:test", Source: "builtin", WorkspaceDir: workspace,
		Policy: pol, PolicySHA: "sha", RunnerImage: pol.Images.Runner}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"type": "agent_choice", "message": map[string]any{"content": string(message)}})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "reports", "AGENT.md"), []byte("# Vultr setup\n\nRead the project and help with Vultr setup.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-model-key")
	fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0}, "-agent": {Stdout: string(line) + "\n"},
	}}
	env, runErr := engine.Run(context.Background(), fake, engine.Params{Plan: plan, PlanDir: planDir, WorkspaceSrc: workspace,
		RunRoot: t.TempDir(), OutDir: out, Task: strings.NewReader("Revise the Vultr wizard."), Adapter: ad, Policy: pol,
		Budgets: spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10}, RunID: "replay711"})
	var ce *engine.ClassifiedError
	if !errors.As(runErr, &ce) || ce.Code != engine.ExitOutputSchema {
		t.Fatalf("runner error %v", runErr)
	}
	d, got, err := (PostStep{OutDir: out, Checker: &postChecker{}, Scan: webscan.Result{Verdict: webscan.Pass}}).Run(context.Background(), env, runErr)
	if err != nil || d == nil || !d.Recovered || !d.Passed() || got.ExitCode != 0 || d.Instructions == "" {
		t.Fatalf("draft %+v, envelope %+v, error %v", d, got, err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "receipts", "events.ndjson"))
	if err != nil || !strings.Contains(string(raw), "AGENT.md is restored. Returning the final draft.") {
		t.Fatalf("raw message missing from receipts: %v", err)
	}
}

func TestAnUnparsedAnswersErrorSaysWhyTheRepairFailed(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		repair     func(context.Context, RepairTurn) (*receipt.Envelope, error)
	}{
		{"timeout", "its run ended with exit 15 (run timed out after 300s)", func(context.Context, RepairTurn) (*receipt.Envelope, error) {
			return &receipt.Envelope{Usage: receipt.Usage{InputTokens: 200}}, engine.Classify(engine.ExitTimeout, errors.New("run timed out after 300s"))
		}},
		{"bad answer", "its answer was not a draft", func(context.Context, RepairTurn) (*receipt.Envelope, error) {
			return &receipt.Envelope{Result: receipt.TextResult("still no answer")}, nil
		}},
		{"no repair", "this client runs no repair turn", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PostStep{OutDir: t.TempDir(), Repair: tc.repair}
			original := engine.Classify(engine.ExitOutputSchema, errors.New("first parse failure"))
			_, _, err := p.Run(context.Background(), &receipt.Envelope{Result: receipt.TextResult("truncated {\"manifest\":")}, original)
			if !errors.Is(err, original) || !strings.Contains(err.Error(), "first parse failure") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v", err)
			}
		})
	}
}
