package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
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
	// An autonomous run keeps a long task's notes, and its harness's
	// compaction summary points back to them.
	if !strings.Contains(rendered, "finish."+compile.LongTaskNotes) || strings.Contains(rendered, spec.PHLongTask) {
		t.Errorf("the run's System Prompt lacks the long-task sentences:\n%s", rendered)
	}
	if !strings.Contains(rendered, "compaction_notes: '"+compile.NotesFileForHarness+"'") || strings.Contains(rendered, spec.PHNotesFile) {
		t.Errorf("the run's harness config does not name the notes file:\n%s", rendered)
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
	// A session talks with the person; it has no long-task sentences and no
	// notes file for its harness.
	if strings.Contains(rendered, "A task that reads more than a few pages") || strings.Contains(rendered, spec.PHLongTask) ||
		strings.Contains(rendered, compile.NotesFileForHarness) || strings.Contains(rendered, spec.PHNotesFile) {
		t.Errorf("the session's harness files carry the long-task notes:\n%s", rendered)
	}
}
