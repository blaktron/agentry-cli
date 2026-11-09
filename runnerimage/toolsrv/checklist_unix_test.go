//go:build unix

package toolsrv

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTheChecklistNeverWaitsOnAPipe: a pipe a declared command puts where
// the file is is refused at once on a write and a read alike.
func TestTheChecklistNeverWaitsOnAPipe(t *testing.T) {
	spec := checklistSpec(t)
	if err := syscall.Mkfifo(spec.Checklist, 0o666); err != nil {
		t.Skip("no pipes here:", err)
	}
	s := newTestServer(t, spec)
	for _, steps := range [][]Step{nil, {{Text: "mine", Status: StepPending}}} {
		done := make(chan string, 1)
		go func() {
			got, _ := checklistCall(t, s, steps)
			done <- got
		}()
		select {
		case got := <-done:
			if !strings.Contains(got, "a link, a folder or a pipe") {
				t.Errorf("a pipe in the checklist's place answered %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the checklist waited on a pipe in its place")
		}
	}
}
