// How the CLI speaks beside its result: narration on stderr that --quiet
// silences, stage lines for long operations that show only on a terminal,
// and colour that is off under NO_COLOR, off the terminal and under --json.
// The result itself (stdout, or the JSON a --json asks for) is untouched by
// any of it.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// narration is where a command's progress and advice go: stderr, or
// nowhere under --quiet. Warnings and errors do not go through it.
func narration(cmd *cobra.Command) io.Writer {
	if flags.quiet {
		return io.Discard
	}
	return cmd.ErrOrStderr()
}

// terminalWriter reports whether w is this process's stderr or stdout on an
// interactive terminal: the only place stage lines and colour belong.
var terminalWriter = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || (f != os.Stderr && f != os.Stdout) {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ANSI colours, as a few names.
const (
	colorRed    = "31"
	colorYellow = "33"
	colorGreen  = "32"
	colorBold   = "1"
)

// colorOn reports whether colour may be written to w: never under NO_COLOR
// (any value), never under --json, and only on a terminal.
func colorOn(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set || flags.jsonOut {
		return false
	}
	return terminalWriter(w)
}

// paint is s in the given colour when w takes colour, s itself otherwise.
func paint(w io.Writer, color, s string) string {
	if !colorOn(w) {
		return s
	}
	return "\x1b[" + color + "m" + s + "\x1b[0m"
}

// stageLog renders the lines a long operation reports (a pull, a build, a
// model call) as stage lines on stderr: each line as it arrives, and when
// the next arrives or the log is closed, how long the stage took. It writes
// nothing when stderr is not a terminal or under --quiet, so a log or a
// pipe never fills with progress; the operation's result is reported by its
// caller as before. It is an io.Writer, so a package that narrates to one
// needs no other seam.
type stageLog struct {
	w       io.Writer
	started time.Time
	open    bool
	partial strings.Builder
	now     func() time.Time
}

// newStageLog is the stage log for cmd: live on a terminal, silent
// otherwise.
func newStageLog(cmd *cobra.Command) *stageLog {
	w := cmd.ErrOrStderr()
	if flags.quiet || !terminalWriter(w) {
		w = nil
	}
	return &stageLog{w: w, now: time.Now}
}

// Write takes narration lines; each complete line opens a stage.
func (s *stageLog) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	s.partial.Write(p)
	for {
		text := s.partial.String()
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		line := text[:i]
		s.partial.Reset()
		s.partial.WriteString(text[i+1:])
		s.Stage(line)
	}
	return len(p), nil
}

// Stage closes the open stage with its duration and opens label.
func (s *stageLog) Stage(label string) {
	if s.w == nil {
		return
	}
	s.Close()
	_, _ = fmt.Fprintf(s.w, "%s…", strings.TrimSpace(label))
	s.started, s.open = s.now(), true
}

// Close ends the open stage, saying how long it took.
func (s *stageLog) Close() {
	if s.w == nil || !s.open {
		return
	}
	_, _ = fmt.Fprintf(s.w, " %s\n", humanDuration(s.now().Sub(s.started)))
	s.open = false
}

// Fail ends the open stage as failed.
func (s *stageLog) Fail() {
	if s.w == nil || !s.open {
		return
	}
	_, _ = fmt.Fprintf(s.w, " %s after %s\n", paint(s.w, colorRed, "failed"), humanDuration(s.now().Sub(s.started)))
	s.open = false
}
