package toolsrv

// The checklist: the steps of the run's task, kept by the agent in one file
// (Spec.Checklist) so that they outlast a summary of earlier work, a reload
// of the harness and a start again on the stored conversation. One call
// replaces the whole list, a call without steps reads it, and every answer
// carries the list and how many steps are still open. The tool holds the
// list's shape: one step in progress at most, and a reason for every step
// dropped. The file is Markdown, one step a line, so it reads as it is
// with scratch_read; a list the agent wrote there by hand reads back as
// far as its lines keep that shape.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/beneath"
)

// The statuses a step may have.
const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepDone       = "done"
	StepDropped    = "dropped"
)

// ChecklistFile is the checklist's file name in its folder.
const ChecklistFile = "checklist.md"

const (
	// maxChecklistSteps and maxStepBytes bound a list: a run's steps,
	// not its findings, which go into the report.
	maxChecklistSteps = 100
	maxStepBytes      = 500
	// maxChecklistRead bounds the file read back, a full list well inside it.
	maxChecklistRead = 256 << 10
	checklistHead    = "# Checklist\n\n"
	droppedMark      = " (dropped: "
)

// checklistMarks are each status's box in the file, in the order a reader
// meets them.
var checklistMarks = []struct{ status, box string }{
	{StepPending, "- [ ] "},
	{StepInProgress, "- [>] "},
	{StepDone, "- [x] "},
	{StepDropped, "- [-] "},
}

// Step is one step of a checklist.
type Step struct {
	Text   string `json:"text"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

const checklistDescription = "Keep this run's checklist: the steps of your task, in order. Pass steps to replace the whole list; call it without steps to read the list. " +
	"Each step has text and a status: pending, in_progress, done or dropped. At most one step is in_progress, and a dropped step needs a reason. " +
	"Every answer shows the list and how many steps are still open. The list is kept in " + ChecklistFile + ", a working note and never a deliverable."

func checklistSchema() map[string]any {
	step := objSchema(map[string]any{
		"text":   strProp("the step, one line, something you can check is done"),
		"status": map[string]any{"type": "string", "enum": []string{StepPending, StepInProgress, StepDone, StepDropped}, "description": "pending, in_progress (at most one step), done, or dropped (with a reason)"},
		"reason": strProp("why the step was dropped, one line; only for a dropped step"),
	}, "text", "status")
	return objSchema(map[string]any{
		"steps": map[string]any{"type": "array", "items": step,
			"description": fmt.Sprintf("the whole list, in order, replacing the one kept (at most %d steps); omit it to read the list", maxChecklistSteps)},
	})
}

func (s *server) toolChecklist(raw json.RawMessage) (string, bool) {
	var a struct {
		Steps []Step `json:"steps"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return ToolChecklist + ": " + err.Error(), true
	}
	if len(a.Steps) == 0 {
		steps, err := s.readChecklist()
		if errors.Is(err, fs.ErrNotExist) {
			return "No checklist yet. Start one with the steps of your task, in order.", false
		}
		if errors.Is(err, beneath.ErrNotRegular) {
			return ToolChecklist + ": " + ChecklistFile + " is a link, a folder or a pipe, not a file; write the list again to replace it", true
		}
		if err != nil {
			return s.failed(ToolChecklist, err)
		}
		s.copyChecklist(renderChecklist(steps))
		return checklistAnswer(steps, false), false
	}
	if msg := checkSteps(a.Steps); msg != "" {
		return ToolChecklist + ": " + msg, true
	}
	text := renderChecklist(a.Steps)
	if env := s.holdsRunSecret(text); env != "" {
		return ToolChecklist + ": a step holds the value of this run's secret " + env + "; write what it is and where it was found, not the value", true
	}
	if err := s.writeChecklist(text); err != nil {
		if errors.Is(err, beneath.ErrNotRegular) {
			return ToolChecklist + ": " + ChecklistFile + " is a link, a folder or a pipe, not a file, so the list was not saved; remove what is in its place", true
		}
		return s.failed(ToolChecklist, err)
	}
	s.copyChecklist(text)
	return checklistAnswer(a.Steps, true), false
}

// checkSteps is why steps cannot be the list, or "".
func checkSteps(steps []Step) string {
	if len(steps) > maxChecklistSteps {
		return fmt.Sprintf("%d steps is more than the %d a checklist holds; group them", len(steps), maxChecklistSteps)
	}
	inProgress := 0
	for i := range steps {
		st := &steps[i]
		st.Text, st.Reason = strings.TrimSpace(st.Text), strings.TrimSpace(st.Reason)
		n := i + 1
		switch {
		case st.Text == "":
			return fmt.Sprintf("step %d has no text", n)
		case strings.ContainsAny(st.Text+st.Reason, "\r\n"):
			return fmt.Sprintf("step %d is more than one line; a step's text and reason are one line each", n)
		case len(st.Text)+len(st.Reason) > maxStepBytes:
			return fmt.Sprintf("step %d is longer than %d bytes; keep a step short and put findings in your report", n, maxStepBytes)
		case strings.Contains(st.Text, droppedMark):
			return fmt.Sprintf("step %d's text holds %q, which the list uses to mark a drop", n, strings.TrimSpace(droppedMark))
		}
		switch st.Status {
		case StepPending, StepDone:
		case StepInProgress:
			inProgress++
		case StepDropped:
			if st.Reason == "" {
				return fmt.Sprintf("step %d is dropped without a reason; give the reason in one line", n)
			}
		default:
			return fmt.Sprintf("step %d has status %q; a status is pending, in_progress, done or dropped", n, st.Status)
		}
		if st.Reason != "" && st.Status != StepDropped {
			return fmt.Sprintf("step %d has a reason but is %s; only a dropped step has one", n, st.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Sprintf("%d steps are in_progress; keep one in progress and the others pending or done", inProgress)
	}
	return ""
}

// renderChecklist is the file's text for steps.
func renderChecklist(steps []Step) string {
	var b strings.Builder
	b.WriteString(checklistHead)
	for _, st := range steps {
		for _, m := range checklistMarks {
			if m.status == st.Status {
				b.WriteString(m.box)
			}
		}
		b.WriteString(st.Text)
		if st.Status == StepDropped {
			reason := st.Reason
			if reason == "" { // a list written by hand
				reason = "no reason given"
			}
			b.WriteString(droppedMark + reason + ")")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// parseChecklist reads the steps back from the file's text: every line that
// starts with a status's box, in order; any other line is passed over.
func parseChecklist(text string) []Step {
	var steps []Step
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, m := range checklistMarks {
			rest, ok := strings.CutPrefix(line, m.box)
			if !ok {
				continue
			}
			st := Step{Text: strings.TrimSpace(rest), Status: m.status}
			if m.status == StepDropped {
				if i := strings.LastIndex(rest, droppedMark); i >= 0 && strings.HasSuffix(rest, ")") {
					st.Text = strings.TrimSpace(rest[:i])
					st.Reason = strings.TrimSpace(rest[i+len(droppedMark) : len(rest)-1])
				}
			}
			if st.Text != "" {
				steps = append(steps, st)
			}
			break
		}
	}
	return steps
}

// checklistAnswer is the tool's answer: the list, then the open steps.
func checklistAnswer(steps []Step, saved bool) string {
	var b strings.Builder
	if saved {
		b.WriteString("Saved. ")
	}
	open, current := 0, ""
	for _, st := range steps {
		if st.Status == StepPending || st.Status == StepInProgress {
			open++
		}
		if st.Status == StepInProgress {
			current = st.Text
		}
	}
	switch {
	case len(steps) == 0:
		b.WriteString("The checklist holds no steps.")
		return b.String()
	case open == 0:
		fmt.Fprintf(&b, "All %d steps are done or dropped; give the reason for each dropped one in your answer.\n\n", len(steps))
	case current != "":
		fmt.Fprintf(&b, "%d of %d steps open; in progress: %s\n\n", open, len(steps), current)
	default:
		fmt.Fprintf(&b, "%d of %d steps open, none in progress.\n\n", open, len(steps))
	}
	b.WriteString(strings.TrimPrefix(renderChecklist(steps), checklistHead))
	if open > 0 {
		b.WriteString("\nBefore your final answer every step is done, or dropped with its reason.")
	}
	return b.String()
}

// checklistDir opens the checklist's folder through a beneath.Dir, which
// resolves the file's name beneath it at the moment of use: a declared
// command may write the scratch folder too, and could put a link where the
// file is.
func (s *server) checklistDir() (*beneath.Dir, error) {
	if s.spec.Checklist == "" {
		return nil, errors.New("this run keeps no checklist")
	}
	return beneath.Open(path.Dir(s.spec.Checklist))
}

func (s *server) readChecklist() ([]Step, error) {
	root, err := s.checklistDir()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenRegular(path.Base(s.spec.Checklist))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxChecklistRead))
	if err != nil {
		return nil, err
	}
	return parseChecklist(string(data)), nil
}

// writeChecklist replaces the file with text. In the scratch folder, which
// a declared command may write too, it is written only as a regular file:
// a link, a folder or a pipe put in its place is refused, never written
// through or waited on. In a persistent agent's data folder it is written as
// fs_write writes there, owned as the agent's other files are. Anywhere else
// it is not written at all.
func (s *server) writeChecklist(text string) error {
	if rel, ok := strings.CutPrefix(s.spec.Checklist, s.spec.WorkspaceDir+"/"); ok {
		if !s.spec.FSWrite.Enabled {
			return errors.New("the checklist's folder is not writable in this run")
		}
		return s.writeWorkspace(rel, text)
	}
	if path.Dir(s.spec.Checklist) != s.spec.ScratchDir {
		return errors.New("the checklist is neither in the scratch folder nor in the agent's folders")
	}
	root, err := s.checklistDir()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.CreateRegular(path.Base(s.spec.Checklist), scratchFileMode)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	if werr == nil {
		werr = f.Chmod(scratchFileMode) // past the umask, on the file opened
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// ParseChecklist reads a checklist's steps from its file's text, as the
// tool reads its own file back.
func ParseChecklist(text string) []Step { return parseChecklist(text) }

// OpenSteps are the steps still pending or in progress.
func OpenSteps(steps []Step) []Step {
	var open []Step
	for _, st := range steps {
		if st.Status == StepPending || st.Status == StepInProgress {
			open = append(open, st)
		}
	}
	return open
}

func validateChecklistCopy(spec *Spec) error {
	l := spec.ChecklistCopy
	if l == "" {
		return nil
	}
	if spec.Checklist == "" {
		return errors.New("checklistCopy is set but the run keeps no checklist")
	}
	if !filepath.IsAbs(l) {
		return fmt.Errorf("checklistCopy %q must be an absolute container path", l)
	}
	if rel, err := filepath.Rel(spec.OutDir, l); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("checklistCopy %q must be under outDir %q", l, spec.OutDir)
	}
	return nil
}

// copyChecklist replaces the runner's copy of the list with text, the list
// the tool has just saved or read. The copy is how the runner learns the
// run's steps; a copy that cannot be written is logged and the agent's call
// still answers, since the list itself is kept. It is written only as a
// regular file, as the list is: a link, a folder or a pipe put in its place
// is refused, never written through or waited on.
func (s *server) copyChecklist(text string) {
	l := s.spec.ChecklistCopy
	if l == "" {
		return
	}
	if err := s.writeChecklistCopy(l, text); err != nil {
		s.logf("checklist copy: %v", err)
	}
}

func (s *server) writeChecklistCopy(l, text string) error {
	root, err := beneath.Open(filepath.Dir(l))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.CreateRegular(filepath.Base(l), 0o644)
	if err != nil {
		return err
	}
	s.ownIn(root, filepath.Base(l))
	_, werr := f.WriteString(text)
	return errors.Join(werr, f.Close())
}
