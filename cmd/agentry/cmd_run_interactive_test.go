package main

import (
	"context"
	"errors"
	"io"
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
}

func (r *endRecorder) Events() <-chan session.Event           { return nil }
func (r *endRecorder) Send(context.Context, string) error     { return nil }
func (r *endRecorder) AddInput(context.Context, string) error { return nil }
func (r *endRecorder) Ended() (bool, session.EndReason)       { return false, "" }
func (r *endRecorder) End(context.Context, bool) (*receipt.Envelope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	return &receipt.Envelope{OutDir: "/out"}, nil
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
	withInteractiveSeams(t, rec, func(context.Context, session.Session, tui.Options, io.Reader, io.Writer) (tui.Outcome, error) {
		panic("chat blew up")
	})
	cmd, params, m, plan := interactiveFixture()
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
	if got := rec.count(); got < 1 {
		t.Errorf("End never ran after the chat returned early")
	}
}
