package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
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
