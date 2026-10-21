package toolsrv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Background programs: the Coding agent's run_command with background set
// starts a program that outlives the call, such as a dev server or a
// watcher. It is held as a program in the foreground is (the confinement,
// the person's ids, a process group of its own) but for the time limit; its
// output is kept as a bounded tail. background_output reads one and
// stop_background ends one; every one ends with this server, at the end of
// the segment. Each program runs beside the tools: in the folders a program
// can write (the workspace, the scratch folder) every tool opens a name
// beneath its root at the moment of use (package beneath), so a link a
// program swaps in leads nowhere a tool would not go, and the other roots
// (reports, inputs, knowledge) are folders no program can write.

// The tools that read and end a background program, granted with Exec.Any.
const (
	ToolBackgroundOutput = "background_output"
	ToolStopBackground   = "stop_background"
)

const (
	// maxBackground bounds the programs running in the background at once.
	maxBackground = 4
	// backgroundTail is how much of a program's latest output is kept.
	backgroundTail = 64 << 10
)

// backgroundSettle is how long a call that starts a program waits for its
// first output, or for its end; a variable so a test need not wait.
var backgroundSettle = 3 * time.Second

// bgProc is one program started in the background.
type bgProc struct {
	id      string
	command string
	started time.Time
	cmd     *exec.Cmd
	out     *tailWriter
	done    chan struct{}
	// Set before done closes.
	err    error
	failed string
}

func (p *bgProc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// status is the program's state, in a line.
func (p *bgProc) status() string {
	if p.running() {
		return fmt.Sprintf("%s: running since %s: %s", p.id, p.started.UTC().Format(time.RFC3339), p.command)
	}
	var exitErr *exec.ExitError
	switch {
	case p.failed != "":
		return fmt.Sprintf("%s: did not start (%s): %s", p.id, p.failed, p.command)
	case p.err == nil:
		return fmt.Sprintf("%s: ended, exit code 0: %s", p.id, p.command)
	case errors.As(p.err, &exitErr):
		return fmt.Sprintf("%s: ended, exit code %d: %s", p.id, exitErr.ExitCode(), p.command)
	default:
		return fmt.Sprintf("%s: ended (%v): %s", p.id, p.err, p.command)
	}
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	total int64
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += int64(len(p))
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	return len(p), nil
}

func (w *tailWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := string(w.buf)
	if dropped := w.total - int64(len(w.buf)); dropped > 0 {
		t = fmt.Sprintf("[%d earlier bytes of output dropped]\n", dropped) + t
	}
	return t
}

// background is the server's programs, by id.
type background struct {
	mu    sync.Mutex
	seq   int
	procs map[string]*bgProc
}

// startBackground starts cmd, already confined and in its own group, and
// waits up to backgroundSettle: a program that ends by then is answered as
// a foreground one is, and one still running is left to run.
func (s *server) startBackground(cmd *exec.Cmd, command string) (string, bool) {
	b := &s.bg
	b.mu.Lock()
	running := 0
	for _, p := range b.procs {
		if p.running() {
			running++
		}
	}
	if running >= maxBackground {
		b.mu.Unlock()
		return fmt.Sprintf("run_command: %d programs already run in the background, the most at once: end one with stop_background", maxBackground), true
	}
	b.seq++
	p := &bgProc{id: fmt.Sprintf("bg%d", b.seq), command: command, started: time.Now(), cmd: cmd,
		out: &tailWriter{max: backgroundTail}, done: make(chan struct{})}
	if b.procs == nil {
		b.procs = map[string]*bgProc{}
	}
	b.procs[p.id] = p
	b.mu.Unlock()

	cmd.Stdout, cmd.Stderr = p.out, p.out
	wait, err := s.confine.start(cmd)
	if err != nil {
		b.mu.Lock()
		delete(b.procs, p.id)
		b.mu.Unlock()
		return "run_command: " + err.Error(), true
	}
	go func() {
		failed, err := wait()
		// What the program left running in its group ends with it.
		_ = killGroup(cmd)
		p.failed, p.err = failed, err
		close(p.done)
	}()
	select {
	case <-p.done:
		if p.failed != "" {
			return "run_command: " + p.failed, true
		}
		return fmt.Sprintf("%s\nIt ended within %s, so nothing runs in the background.\n%s", p.status(), backgroundSettle, s.scrubSecrets(p.out.text())), false
	case <-time.After(backgroundSettle):
	}
	return fmt.Sprintf("started in the background as %s; it keeps running after this call until stop_background %s or the end of this conversation's segment. Output so far:\n%s",
		p.id, p.id, s.scrubSecrets(p.out.text())), false
}

// stopAllBackground ends every program still running: the server is going.
func (s *server) stopAllBackground() {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	for _, p := range s.bg.procs {
		if p.running() {
			_ = killGroup(p.cmd)
		}
	}
}

func backgroundOutputSchema() map[string]any {
	return objSchema(map[string]any{
		"id": strProp("the program's id (bg1, bg2…); empty lists every program started in the background"),
	})
}

func stopBackgroundSchema() map[string]any {
	return objSchema(map[string]any{
		"id": strProp("the program's id, as run_command named it"),
	}, "id")
}

const backgroundOutputDescription = "Read a program run_command started in the background: whether it is running or how it ended, and the latest of its output. Without an id, every such program, one a line."

const stopBackgroundDescription = "End a program run_command started in the background, and every process it started. Returns how it ended and the last of its output."

func (s *server) toolBackgroundOutput(raw json.RawMessage) (string, bool) {
	var a struct {
		ID string `json:"id"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "background_output: " + err.Error(), true
	}
	if strings.TrimSpace(a.ID) == "" {
		s.bg.mu.Lock()
		ids := make([]string, 0, len(s.bg.procs))
		for id := range s.bg.procs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return bgSeq(ids[i]) < bgSeq(ids[j]) })
		lines := make([]string, 0, len(ids))
		for _, id := range ids {
			lines = append(lines, s.bg.procs[id].status())
		}
		s.bg.mu.Unlock()
		if len(lines) == 0 {
			return "No program was started in the background.", false
		}
		return strings.Join(lines, "\n"), false
	}
	p, ok := s.bgProc(a.ID)
	if !ok {
		return fmt.Sprintf("background_output: no program %q was started in the background", a.ID), true
	}
	return p.status() + "\n" + s.scrubSecrets(p.out.text()), false
}

func (s *server) toolStopBackground(raw json.RawMessage) (string, bool) {
	var a struct {
		ID string `json:"id"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "stop_background: " + err.Error(), true
	}
	p, ok := s.bgProc(a.ID)
	if !ok {
		return fmt.Sprintf("stop_background: no program %q was started in the background", a.ID), true
	}
	if p.running() {
		_ = killGroup(p.cmd)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return fmt.Sprintf("stop_background: %s did not end within 5s of being killed", p.id), true
		}
	}
	return p.status() + "\n" + s.scrubSecrets(p.out.text()), false
}

func (s *server) bgProc(id string) (*bgProc, bool) {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	p, ok := s.bg.procs[strings.TrimSpace(id)]
	return p, ok
}

func bgSeq(id string) int {
	var n int
	_, _ = fmt.Sscanf(id, "bg%d", &n)
	return n
}

// start starts cmd and returns the wait for it. Under Landlock the
// trampoline that could not confine or start the command says why on
// confine.ErrorFD, which the wait returns as failed.
func (c confinement) start(cmd *exec.Cmd) (wait func() (string, error), err error) {
	if c.landlock == 0 {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return func() (string, error) { return "", cmd.Wait() }, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{w} // the first extra file is descriptor 3, confine.ErrorFD
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return func() (string, error) {
		err := cmd.Wait()
		msg, _ := io.ReadAll(io.LimitReader(r, 4096))
		_ = r.Close()
		return strings.TrimSpace(string(msg)), err
	}, nil
}
