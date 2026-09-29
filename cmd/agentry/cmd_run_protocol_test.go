package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// The command's half of the session protocol (agentry-cli#477): the gates
// it lifts and keeps, and what it puts on each stream. The protocol over
// the engine's real session, on all three adapters, is
// pkg/runner/engine/protocol_test.go.

// withProtocolEvents stands in for descriptor 3.
func withProtocolEvents(t *testing.T, w io.Writer, err error) {
	t.Helper()
	old := protocolEventsOut
	protocolEventsOut = func() (io.Writer, error) { return w, err }
	t.Cleanup(func() { protocolEventsOut = old })
}

// TestSessionProtocolGates: the flag lifts the terminal rule and the JSON
// refusal for an interactive run and nothing else — --events stays
// refused, an autonomous run refuses the flag, and a driver that did not
// open descriptor 3 is told so before anything runs.
func TestSessionProtocolGates(t *testing.T) {
	isolateEnv(t)
	_, pol := isolateRunnerHome(t)
	fixture := repoPath(t, "testdata/runner/full-caps") // declares both modes, and exec: [jq]
	catalogued := policy.Default()
	catalogued.Exec.Packages = map[string]string{"jq": "jq"}
	if err := policy.Save(pol, catalogued); err != nil {
		t.Fatal(err)
	}
	base := []string{"run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol}

	_, _, err := runCLI(t, strings.NewReader(""), append(base, "--mode", "autonomous", "--session-protocol")...)
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "drives an interactive session") {
		t.Errorf("autonomous with the flag: %v", err)
	}

	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--mode", "interactive", "--session-protocol", "--events", "x.ndjson")...)
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "--events") {
		t.Errorf("--events with the flag: %v", err)
	}

	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--mode", "interactive", "--session-inputs", t.TempDir())...)
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "--session-inputs is for --session-protocol") {
		t.Errorf("--session-inputs alone: %v", err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--mode", "interactive", "--session-protocol", "--session-inputs", filepath.Join(t.TempDir(), "nowhere"))...)
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("--session-inputs missing: %v", err)
	}

	// No terminal and --output-format json pass; what stops the run is the
	// missing descriptor, before docker is touched.
	withProtocolEvents(t, nil, errors.New("--session-protocol writes its events to descriptor 3, and it is not open"))
	_, _, err = runCLI(t, strings.NewReader(""), append(base, "--mode", "interactive", "--session-protocol", "--output-format", "json")...)
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "descriptor 3") {
		t.Errorf("no descriptor 3: %v, want the descriptor's refusal (the terminal rule lifted)", err)
	}
}

// TestSessionProtocolChangesNoPlan: the flag is the driver's, not the
// plan's — the interactive plan it runs is byte for byte the one a
// terminal session runs (DR-15's goldens hold the rest).
func TestSessionProtocolChangesNoPlan(t *testing.T) {
	isolateEnv(t)
	_, pol := isolateRunnerHome(t)
	fixture := repoPath(t, "testdata/runner/full-caps")
	catalogued := policy.Default()
	catalogued.Exec.Packages = map[string]string{"jq": "jq"}
	if err := policy.Save(pol, catalogued); err != nil {
		t.Fatal(err)
	}
	base := []string{"run", "--local", fixture, "--allow-unsigned", "--yes", "--policy", pol, "--mode", "interactive", "--dry-run"}
	plain, _, err := runCLI(t, strings.NewReader(""), base...)
	if err != nil {
		t.Fatal(err)
	}
	withProtocol, _, err := runCLI(t, strings.NewReader(""), append(base, "--session-protocol")...)
	if err != nil {
		t.Fatal(err)
	}
	if plain != withProtocol || !strings.Contains(plain, `"mode": "interactive"`) {
		t.Errorf("the plan differs under the flag:\n%s\n---\n%s", plain, withProtocol)
	}
}

// TestSessionProtocolReadsNoTaskFromStdin: stdin is the command stream, so
// the task is --input's alone.
func TestSessionProtocolReadsNoTaskFromStdin(t *testing.T) {
	cmd, _, _, _ := interactiveFixture()
	cmd.SetIn(strings.NewReader(`{"send":{"text":"a command, not a task"}}` + "\n"))
	if r := runTask(cmd, runOpts{sessionProtocol: true}); r != nil {
		t.Errorf("the task reader is stdin under the protocol")
	}
	if r := runTask(cmd, runOpts{}); r == nil {
		t.Errorf("without the flag, piped stdin is the task")
	}
}

// TestSessionProtocolOpeningTurnFollowsTheTask: the protocol holds sends
// for an opening turn only when the session will send one — a task that is
// not blank.
func TestSessionProtocolOpeningTurnFollowsTheTask(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank.txt")
	task := filepath.Join(dir, "task.txt")
	if err := os.WriteFile(blank, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(task, []byte("look at this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		input string
		want  bool
	}{{"", false}, {blank, false}, {task, true}, {filepath.Join(dir, "missing.txt"), false}} {
		if got := sendsOpeningTurn(c.input); got != c.want {
			t.Errorf("sendsOpeningTurn(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestProtocolEventsDescriptor runs the real check of descriptor 3 in a
// child process, because what fd 3 is depends on what the process was
// started with: a pipe the driver opened for writing is taken; the read end
// of one, /dev/null, and nothing at all (where the Go runtime puts a file
// of its own) are refused.
func TestProtocolEventsDescriptor(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := openProtocolEvents(); err == nil {
			t.Fatal("Windows accepted descriptor 3")
		}
		return
	}
	if os.Getenv("AGENTRY_TEST_FD3_CHILD") == "1" {
		if _, err := openProtocolEvents(); err != nil {
			fmt.Println("refused:", err)
		} else {
			fmt.Println("accepted")
		}
		return
	}
	child := func(extra *os.File) string {
		t.Helper()
		c := exec.Command(os.Args[0], "-test.run=^TestProtocolEventsDescriptor$", "-test.count=1") //nolint:gosec // this test binary
		c.Env = append(os.Environ(), "AGENTRY_TEST_FD3_CHILD=1")
		if extra != nil {
			c.ExtraFiles = []*os.File{extra}
		}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return string(out)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	for _, c := range []struct {
		name string
		fd   *os.File
		want string
	}{
		{"a pipe open for writing", w, "accepted"},
		{"the read end of a pipe", r, "refused: --session-protocol writes its events to descriptor 3, and it is open for reading only"},
		{"/dev/null", devNull, "refused: --session-protocol writes its events to descriptor 3, and it is not a pipe or a socket the driver opened"},
		{"nothing", nil, "refused: --session-protocol writes its events to descriptor 3"},
	} {
		if got := child(c.fd); !strings.Contains(got, c.want) {
			t.Errorf("%s: %s, want %q", c.name, got, c.want)
		}
	}
}

// scriptedSession answers each turn with a usage event and an assistant
// turn and ends when asked: enough to see where runProtocol puts what.
type scriptedSession struct {
	events chan session.Event
	mu     sync.Mutex
	ended  bool
	ends   int
	finish bool
}

func newScriptedSession() *scriptedSession {
	return &scriptedSession{events: make(chan session.Event, 64)}
}

func (s *scriptedSession) Events() <-chan session.Event { return s.events }
func (s *scriptedSession) Send(_ context.Context, text string) error {
	// Under the lock End takes, so an end never closes the stream under a
	// turn (the engine's session has its done channel for this).
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return session.ErrEnded
	}
	s.events <- session.Event{Kind: session.KindUserTurn, Text: text}
	s.events <- session.Event{Kind: session.KindUsage, InputTokens: 7, OutputTokens: 3}
	s.events <- session.Event{Kind: session.KindAssistantTurn, Text: "ok: " + text, Raw: json.RawMessage(`{"secret":"raw"}`)}
	return nil
}
func (s *scriptedSession) AddInput(context.Context, string) error { return nil }
func (s *scriptedSession) Ended() (bool, session.EndReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, ""
}
func (s *scriptedSession) End(_ context.Context, finish bool) (*receipt.Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ends++
	if !s.ended {
		s.ended, s.finish = true, finish
		s.events <- session.Event{Kind: session.KindSessionEnd, Reason: session.EndFinished}
		close(s.events)
	}
	return &receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", Result: receipt.TextResult("the result"), OutDir: "/out",
		Usage: receipt.Usage{InputTokens: 7, OutputTokens: 3}}, nil
}

// TestRunProtocolStreams: under --output-format json, stdout carries the
// envelope and nothing else, stderr the usage narration a host meters by,
// descriptor 3 one line per event without the raw message — and End runs.
func TestRunProtocolStreams(t *testing.T) {
	s := newScriptedSession()
	withInteractiveSeams(t, s, nil)
	cmd, params, m, plan := interactiveFixture()
	inR, inW := io.Pipe()
	cmd.SetIn(inR)
	evR, evW := io.Pipe()
	opts := runOpts{sessionProtocol: true, protocolEvents: evW, outputFormat: "json", progress: &runProgress{}}

	// The driver: one turn, and the end once the turn has ended.
	var lines []string
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(evR)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if strings.Contains(sc.Text(), `"kind":"assistant_turn"`) {
				_, _ = io.WriteString(inW, `{"end":{"finish":true}}`+"\n")
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- runInteractive(cmd, nil, params, m, plan, opts) }()
	go func() { _, _ = io.WriteString(inW, `{"send":{"text":"hello"}}`+"\n") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInteractive: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runInteractive never returned")
	}
	_ = evW.Close()
	<-read

	stdout := cmd.OutOrStdout().(*strings.Builder).String()
	var env receipt.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.Status != "ok" {
		t.Errorf("stdout is not the envelope alone: %q (%v)", stdout, err)
	}
	if !opts.progress.printed {
		t.Error("the envelope was printed but not recorded as printed; a failure envelope would follow it")
	}
	stderr := cmd.ErrOrStderr().(*strings.Builder).String()
	if !strings.Contains(stderr, "usage: input=7 output=3 total=10 turns=0") {
		t.Errorf("stderr narrated %q", stderr)
	}
	var kinds []session.Kind
	for _, l := range lines {
		var ev protocol.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("event line %q: %v", l, err)
		}
		if strings.Contains(l, `"raw"`) {
			t.Errorf("an event carries the raw message: %s", l)
		}
		kinds = append(kinds, ev.Kind)
	}
	want := []session.Kind{session.KindUserTurn, session.KindUsage, session.KindAssistantTurn, session.KindSessionEnd}
	if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(want), ",") {
		t.Errorf("events = %v, want %v", kinds, want)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ends < 1 || !s.finish {
		t.Errorf("End ran %d times, finish=%v; want the end command's finish", s.ends, s.finish)
	}
}

func kindStrings(ks []session.Kind) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

// TestBrokenPipesDoNotKillAProtocolSession: a driver that closes its end of
// stderr must not kill the CLI mid-session (Go dies of SIGPIPE on a write to
// a broken fd 2, and a process killed so skips its teardown). The child
// writes to a stderr whose reader is gone, with and without the flag's
// signal handling, and says whether it lived.
func TestBrokenPipesDoNotKillAProtocolSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGPIPE on Windows")
	}
	if mode := os.Getenv("AGENTRY_TEST_SIGPIPE_CHILD"); mode != "" {
		if mode == "ignore" {
			ignoreBrokenPipes()
		}
		_, _ = fmt.Fprintln(os.Stderr, "usage: input=1 output=1 total=2 turns=0")
		os.Exit(7) // alive after the write
	}
	for _, c := range []struct {
		mode  string
		lives bool
	}{{"default", false}, {"ignore", true}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()                                                                                            // the reader has gone
		cmd := exec.Command(os.Args[0], "-test.run=^TestBrokenPipesDoNotKillAProtocolSession$", "-test.count=1") //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), "AGENTRY_TEST_SIGPIPE_CHILD="+c.mode)
		cmd.Stderr = w
		err = cmd.Run()
		_ = w.Close()
		var exitErr *exec.ExitError
		lived := errors.As(err, &exitErr) && exitErr.ExitCode() == 7
		if lived != c.lives {
			t.Errorf("%s: lived=%v (%v), want %v", c.mode, lived, err, c.lives)
		}
	}
}
