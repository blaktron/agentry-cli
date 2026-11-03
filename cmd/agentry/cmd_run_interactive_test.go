package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/tui"
)

// endRecorder is a session whose End is what tears the sandbox down; the
// test asserts it ran, however the chat left.
type endRecorder struct {
	mu   sync.Mutex
	ends int
	// finish and ctxErr are what the first End was given.
	finish bool
	ctxErr error
	// result and reason are what the session answers once it is ended.
	result json.RawMessage
	reason session.EndReason
}

func (r *endRecorder) Events() <-chan session.Event           { return nil }
func (r *endRecorder) Send(context.Context, string) error     { return nil }
func (r *endRecorder) AddInput(context.Context, string) error { return nil }
func (r *endRecorder) Ended() (bool, session.EndReason)       { return false, r.reason }
func (r *endRecorder) End(ctx context.Context, finish bool) (*receipt.Envelope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	if r.ends == 1 {
		r.finish, r.ctxErr = finish, ctx.Err()
	}
	return &receipt.Envelope{OutDir: "/out", Result: r.result}, nil
}

func (r *endRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ends
}

// withInteractiveSeams swaps the session start and the chat for one test.
func withInteractiveSeams(t *testing.T, s session.Session, chat func(context.Context, session.Session, tui.Options, io.Reader, io.Writer) (tui.Outcome, error)) {
	t.Helper()
	oldStart, oldChat := startSession, runChat
	startSession = func(context.Context, dockerx.Client, engine.Params) (session.Session, error) { return s, nil }
	runChat = chat
	t.Cleanup(func() { startSession, runChat = oldStart, oldChat })
}

func interactiveFixture() (*cobra.Command, engine.Params, *manifest.Manifest, *spec.Plan) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetIn(strings.NewReader(""))
	return cmd, engine.Params{}, &manifest.Manifest{}, &spec.Plan{Ref: "acme/agent@1.0.0"}
}

// TestRunInteractiveTearsDownWhenTheChatPanics: a panic in the terminal
// chat still ends the session — the deferred End runs while the panic
// unwinds, so the sandbox, the gateway and the model key do not outlive
// the process.
func TestRunInteractiveTearsDownWhenTheChatPanics(t *testing.T) {
	rec := &endRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	withInteractiveSeams(t, rec, func(context.Context, session.Session, tui.Options, io.Reader, io.Writer) (tui.Outcome, error) {
		cancel() // the command was interrupted, and the chat fell over with it
		panic("chat blew up")
	})
	cmd, params, m, plan := interactiveFixture()
	cmd.SetContext(ctx)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed; it must still reach the caller")
			}
		}()
		_ = runInteractive(cmd, nil, params, m, plan, runOpts{})
	}()
	if got := rec.count(); got != 1 {
		t.Errorf("End ran %d times after a chat panic, want 1 (the deferred teardown)", got)
	}
	if rec.ctxErr != nil {
		t.Errorf("the teardown ran with a context that was already done: %v", rec.ctxErr)
	}
	if rec.finish {
		t.Error("the teardown asked for a final result")
	}
}

// TestRunInteractiveTearsDownWhenTheChatReturnsEarly: a chat that fails
// and returns at once still leaves through End, and its error is the
// command's.
func TestRunInteractiveTearsDownWhenTheChatReturnsEarly(t *testing.T) {
	rec := &endRecorder{}
	withInteractiveSeams(t, rec, func(context.Context, session.Session, tui.Options, io.Reader, io.Writer) (tui.Outcome, error) {
		return tui.Outcome{}, errors.New("no terminal")
	})
	cmd, params, m, plan := interactiveFixture()
	err := runInteractive(cmd, nil, params, m, plan, runOpts{})
	if err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("runInteractive = %v, want the chat's error", err)
	}
	wantClassified(t, err, engine.ExitCLI)
	if got := cmd.ErrOrStderr().(*strings.Builder).String(); !strings.Contains(got, "chat closed with an error: no terminal\n") {
		t.Errorf("stderr = %q, want the chat's error said", got)
	}
	if got := rec.count(); got < 1 {
		t.Errorf("End never ran after the chat returned early")
	}
}

// TestRunInteractiveHandsTheChatWhatItNeeds: the chat is told the release, its
// model, capabilities and budgets; the result of a chat the person finished is
// printed on stdout and the session's end and transcript on stderr; the
// person's choice to finish reaches the session's end.
func TestRunInteractiveHandsTheChatWhatItNeeds(t *testing.T) {
	t.Run("the options of the chat", func(t *testing.T) {
		rec := &endRecorder{}
		var got tui.Options
		withInteractiveSeams(t, rec, func(_ context.Context, _ session.Session, o tui.Options, _ io.Reader, _ io.Writer) (tui.Outcome, error) {
			got = o
			return tui.Outcome{}, nil
		})
		cmd, params, m, plan := interactiveFixture()
		params.Budgets = spec.Budgets{MaxTurns: 9, MaxCostUSD: 2.5}
		plan.Harness.Model = "openai/gpt-5"
		plan.Harness.ModelSignIn = &spec.ModelSignInSpec{}
		plan.Harness.Session = &spec.SessionSpec{Budgets: spec.SessionBudgets{IdleTimeoutSeconds: 61, MaxWallSeconds: 62, MaxUserTurns: 63}}
		m.Interactive = &manifest.InteractiveHints{Opening: "Ask me what the workspace holds."}
		if err := runInteractive(cmd, nil, params, m, plan, runOpts{input: "task.txt"}); err != nil {
			t.Fatal(err)
		}
		want := tui.Options{Ref: "acme/agent@1.0.0", Model: "openai/gpt-5", Caps: capabilityLines(m.Contract()), MaxTurns: 9, MaxCostUSD: 2.5, PlanRun: true,
			Opening: "Ask me what the workspace holds.", HasTask: true, Budgets: spec.SessionBudgets{IdleTimeoutSeconds: 61, MaxWallSeconds: 62, MaxUserTurns: 63}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chat options\n got %+v\nwant %+v", got, want)
		}
		if len(got.Caps) == 0 {
			t.Error("the chat was told no capabilities")
		}
	})

	for _, c := range []struct {
		name       string
		finish     bool
		result     string
		reason     session.EndReason
		wantStdout string
	}{
		{"a finished chat prints its text result", true, `"the answer"`, "", "the answer\n"},
		{"a finished chat with an empty result prints nothing", true, `""`, "", ""},
		{"a finished chat prints a structured result as it is", true, `{"k":1}`, "", "{\"k\":1}\n"},
		{"a chat left without finishing prints no result", false, `"the answer"`, "", ""},
		{"the reason an engine ended the session is said", false, `""`, session.EndIdle, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &endRecorder{result: json.RawMessage(c.result), reason: c.reason}
			withInteractiveSeams(t, rec, func(context.Context, session.Session, tui.Options, io.Reader, io.Writer) (tui.Outcome, error) {
				return tui.Outcome{Finish: c.finish}, nil
			})
			cmd, params, m, plan := interactiveFixture()
			if err := runInteractive(cmd, nil, params, m, plan, runOpts{}); err != nil {
				t.Fatal(err)
			}
			if got := cmd.OutOrStdout().(*strings.Builder).String(); got != c.wantStdout {
				t.Errorf("stdout = %q, want %q", got, c.wantStdout)
			}
			stderr := cmd.ErrOrStderr().(*strings.Builder).String()
			if !strings.Contains(stderr, "transcript, reports and receipts: /out\n") {
				t.Errorf("stderr = %q, want the transcript's place said", stderr)
			}
			if said := strings.Contains(stderr, "session ended: idle_timeout\n"); said != (c.reason == session.EndIdle) {
				t.Errorf("stderr = %q, the end reason said: %v", stderr, said)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.finish != c.finish {
				t.Errorf("the session was ended with finish=%v, the person chose %v", rec.finish, c.finish)
			}
		})
	}
}
