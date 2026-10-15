// An empty task runs the agent's own instruction: the run setup promises
// it, and an autonomous harness refuses an empty stdin (docker-agent: "no
// message received on stdin", exit 14).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnEmptyTaskSendsTheAgentsOwnInstruction(t *testing.T) {
	for name, task := range map[string]io.Reader{
		"no task":         nil,
		"an empty task":   strings.NewReader(""),
		"only whitespace": strings.NewReader("  \n\t\n"),
	} {
		t.Run(name, func(t *testing.T) {
			p, f := fixture(t)
			p.Task = task
			happyFake(f)
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if env.Status != "ok" {
				t.Fatalf("status = %s", env.Status)
			}
			if got := f.Foreground["-agent"].GotStdin; got != "Do the engine test.\n" {
				t.Errorf("agent stdin = %q, want the agent's own instruction", got)
			}
			// The receipts say what went in the task's place.
			b, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "meta.json")) //nolint:gosec // test file
			if err != nil {
				t.Fatal(err)
			}
			var meta struct{ Task string }
			if err := json.Unmarshal(b, &meta); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(meta.Task, "own instruction, SKILL.md (sha256:"+p.Plan.Instruction.SHA256) {
				t.Errorf("meta.json task = %q", meta.Task)
			}
		})
	}
}

func TestAGivenTaskIsSentAsItIs(t *testing.T) {
	p, f := fixture(t)
	p.Task = strings.NewReader("  just this  \n")
	happyFake(f)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.Foreground["-agent"].GotStdin; got != "  just this  \n" {
		t.Errorf("agent stdin = %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "meta.json")) //nolint:gosec // test file
	if strings.Contains(string(b), `"task"`) {
		t.Errorf("meta.json claims a substitute for a given task: %s", b)
	}
}

// The instruction comes from the run's own copy of the workspace, the one
// CheckWorkspace held to the install's integrity record, frontmatter
// stripped as the compile strips it.
func TestTheOwnInstructionIsReadFromTheCheckedCopy(t *testing.T) {
	p, f := fixture(t)
	p.Task = nil
	p.CheckWorkspace = func(copyDir string) error {
		return os.WriteFile(filepath.Join(copyDir, "SKILL.md"), []byte("---\nname: x\n---\nDo the engine test.\n"), 0o644)
	}
	happyFake(f)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.Foreground["-agent"].GotStdin; got != "Do the engine test.\n" {
		t.Errorf("agent stdin = %q", got)
	}
}

// A client that checks no copy (the desktop hands the engine none) still
// sends only the instruction the plan was compiled from: the text is held
// to the plan's digest.
func TestAnInstructionChangedAfterTheCompileIsRefused(t *testing.T) {
	p, f := fixture(t)
	p.Task = nil
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, "SKILL.md"), []byte("Do something else.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	happyFake(f)
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitVerify || !strings.Contains(err.Error(), "SKILL.md changed after the plan was compiled") {
		t.Fatalf("want a verify refusal, got %v", err)
	}
	if _, ran := f.FindRun("-agent"); ran {
		t.Error("the agent ran an instruction its plan was not compiled from")
	}
}

func TestAnEmptyTaskWithTheInstructionGoneIsRefused(t *testing.T) {
	p, f := fixture(t)
	p.Task = nil
	p.CheckWorkspace = func(copyDir string) error {
		return os.Remove(filepath.Join(copyDir, "SKILL.md"))
	}
	happyFake(f)
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitVerify || !strings.Contains(err.Error(), "SKILL.md is missing from the package") {
		t.Fatalf("want a verify refusal naming the instruction file, got %v", err)
	}
	if _, ran := f.FindRun("-agent"); ran {
		t.Error("the agent ran without its instruction")
	}
}
