// An empty task runs the agent's own instruction (agentry-cli#449): the
// run setup promises it, and an autonomous harness refuses an empty stdin
// (docker-agent: "no message received on stdin", exit 14).
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnEmptyTaskSendsTheAgentsOwnInstruction(t *testing.T) {
	for name, task := range map[string]*strings.Reader{
		"no task":         nil,
		"an empty task":   strings.NewReader(""),
		"only whitespace": strings.NewReader("  \n\t\n"),
	} {
		t.Run(name, func(t *testing.T) {
			p, f := fixture(t)
			p.Task = nil
			if task != nil {
				p.Task = task
			}
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
}

// The instruction comes from the run's own copy of the workspace, the one
// CheckWorkspace held to the install's integrity record, never from the
// source directory after that check.
func TestTheOwnInstructionIsReadFromTheCheckedCopy(t *testing.T) {
	p, f := fixture(t)
	p.Task = nil
	p.CheckWorkspace = func(copyDir string) error {
		return os.WriteFile(filepath.Join(copyDir, "SKILL.md"), []byte("---\nname: x\n---\nThe checked copy's instruction.\n"), 0o644)
	}
	happyFake(f)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.Foreground["-agent"].GotStdin; got != "The checked copy's instruction.\n" {
		t.Errorf("agent stdin = %q", got)
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
	if !errors.As(err, &ce) || ce.Code != ExitVerify || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("want a verify refusal naming the instruction file, got %v", err)
	}
	if _, ran := f.FindRun("-agent"); ran {
		t.Error("the agent ran without its instruction")
	}
}

func TestAnEmptyTaskSendsTheOwnInstructionToAHostHarness(t *testing.T) {
	p, f, _, taskFile := hostFixture(t)
	p.Task = nil
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	task, _ := os.ReadFile(taskFile) //nolint:gosec // test file
	if string(task) != "Do the host test.\n" {
		t.Errorf("task on stdin = %q, want the agent's own instruction", task)
	}
}
