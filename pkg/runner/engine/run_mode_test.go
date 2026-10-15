package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The System Prompt names the run's mode: the compiled text carries
// spec.PHRunMode, so a release's two plans keep one instruction, and the
// engine fills it in per run.

// TestARunsSystemPromptSaysItIsAutonomous goes through a whole Run.
func TestARunsSystemPromptSaysItIsAutonomous(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	rendered := renderedText(t, filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", "harness"))
	if !strings.Contains(rendered, "This run's mode is autonomous.") || strings.Contains(rendered, spec.PHRunMode) {
		t.Fatalf("the run's System Prompt does not name its mode:\n%s", rendered)
	}
}

// TestASessionsSystemPromptSaysItIsInteractive goes through StartSession:
// the mode is set before the harness files are rendered.
func TestASessionsSystemPromptSaysItIsInteractive(t *testing.T) {
	s, _, p, _ := startFixture(t)
	defer func() { _, _ = s.End(context.Background(), true) }()
	dirs, err := filepath.Glob(filepath.Join(p.RunRoot, "runs", "*", "cfg", "harness"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("the session's harness files: %v %v", dirs, err)
	}
	rendered := renderedText(t, dirs[0])
	if !strings.Contains(rendered, "This run's mode is interactive.") || strings.Contains(rendered, spec.PHRunMode) {
		t.Fatalf("the session's System Prompt does not name its mode:\n%s", rendered)
	}
}
