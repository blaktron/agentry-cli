// Narration, stage lines and colour: silent under --quiet, stage lines only
// on a terminal, colour off under NO_COLOR, off the terminal and under
// --json.
package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestStageLogSpeaksOnlyOnATerminal: the lines a pull or a build reports
// become stages with their duration on a terminal, and nothing elsewhere or
// under --quiet.
func TestStageLogSpeaksOnlyOnATerminal(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := &stageLog{w: &out, now: clock}
	_, _ = io.WriteString(s, "pulling ghcr.io/x/gateway:1 (first use)\n")
	now = now.Add(3 * time.Second)
	_, _ = io.WriteString(s, "building agentry-runner:abc from embedded context (first use)\n")
	now = now.Add(90 * time.Second)
	s.Close()
	if want := "pulling ghcr.io/x/gateway:1 (first use)… 3s\nbuilding agentry-runner:abc from embedded context (first use)… 1m30s\n"; out.String() != want {
		t.Errorf("stages:\n%q\nwant\n%q", out.String(), want)
	}
	out.Reset()
	s = &stageLog{w: &out, now: clock}
	s.Stage("pulling x")
	now = now.Add(time.Second)
	s.Fail()
	if !strings.Contains(out.String(), "pulling x… failed after 1s\n") {
		t.Errorf("failed stage: %q", out.String())
	}
	// A line that arrives in pieces opens one stage when it is complete, several
	// in one write open one each, and a stage is closed once.
	out.Reset()
	s = &stageLog{w: &out, now: clock}
	_, _ = io.WriteString(s, "  pulling a")
	if out.Len() != 0 {
		t.Errorf("half a line opened a stage: %q", out.String())
	}
	_, _ = io.WriteString(s, "b  \nbuilding c\npulling")
	now = now.Add(2 * time.Second)
	_, _ = io.WriteString(s, " d\n")
	s.Close()
	s.Close()
	s.Fail()
	if want := "pulling ab… 0ms\nbuilding c… 2s\npulling d… 0ms\n"; out.String() != want {
		t.Errorf("stages:\n%q\nwant\n%q", out.String(), want)
	}
	// Off a terminal, or under --quiet, the log is silent and the writes
	// succeed.
	silent := &stageLog{w: nil, now: clock}
	if n, err := io.WriteString(silent, "pulling x\n"); err != nil || n != len("pulling x\n") {
		t.Errorf("silent write: %d %v", n, err)
	}
	silent.Stage("y")
	silent.Close()
	silent.Fail()
}

// TestNewStageLogIsSilentOffATerminalAndUnderQuiet: the command's stderr
// is a buffer here, never a terminal, so the log is silent; a terminal
// writer under --quiet is silent too.
func TestNewStageLogIsSilentOffATerminalAndUnderQuiet(t *testing.T) {
	root := newRootCmd()
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	s := newStageLog(root)
	_, _ = io.WriteString(s, "pulling x\n")
	s.Close()
	if errOut.Len() != 0 {
		t.Errorf("a buffer is not a terminal, yet: %q", errOut.String())
	}
	prev := terminalWriter
	terminalWriter = func(io.Writer) bool { return true }
	t.Cleanup(func() { terminalWriter = prev })
	s = newStageLog(root)
	_, _ = io.WriteString(s, "pulling x\n")
	s.Close()
	if !strings.HasPrefix(errOut.String(), "pulling x… ") {
		t.Errorf("on a terminal: %q", errOut.String())
	}
	errOut.Reset()
	flags.quiet = true
	t.Cleanup(func() { flags.quiet = false })
	s = newStageLog(root)
	_, _ = io.WriteString(s, "pulling x\n")
	s.Close()
	if errOut.Len() != 0 {
		t.Errorf("--quiet, yet: %q", errOut.String())
	}
	if narration(root) != io.Discard {
		t.Error("--quiet narration is not discarded")
	}
	flags.quiet = false
	if narration(root) != io.Writer(&errOut) {
		t.Error("narration is not on stderr")
	}
}

// TestColorFollowsNoColorAndTheTerminal: colour needs a terminal, no
// NO_COLOR and no --json.
func TestColorFollowsNoColorAndTheTerminal(t *testing.T) {
	var buf bytes.Buffer
	if paint(&buf, colorRed, "x") != "x" {
		t.Error("a buffer took colour")
	}
	// Only this process's stderr or stdout is a terminal, and only when it is
	// a character device: another open file is not, however it looks, nor is
	// a stream that is a regular file.
	chardev, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chardev.Close() }()
	regular, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = regular.Close() }()
	if terminalWriter(chardev) {
		t.Error("an unrelated character device is a terminal")
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = prevOut, prevErr }()
	os.Stdout, os.Stderr = chardev, regular
	if !terminalWriter(chardev) {
		t.Error("a stdout that is a character device is not a terminal")
	}
	if terminalWriter(regular) {
		t.Error("a stderr that is a regular file is a terminal")
	}
	os.Stdout, os.Stderr = regular, chardev
	if !terminalWriter(chardev) {
		t.Error("a stderr that is a character device is not a terminal")
	}
	closed, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	os.Stdout = closed
	if terminalWriter(closed) {
		t.Error("a stdout that cannot be stated is a terminal")
	}
	os.Stdout, os.Stderr = prevOut, prevErr
	prev := terminalWriter
	terminalWriter = func(io.Writer) bool { return true }
	t.Cleanup(func() { terminalWriter = prev })
	t.Setenv("NO_COLOR", "")
	if got := paint(&buf, colorRed, "x"); got != "x" {
		t.Errorf("NO_COLOR set (empty) still coloured: %q", got)
	}
	_ = os.Unsetenv("NO_COLOR") // t.Setenv above restores it after the test
	if got := paint(&buf, colorRed, "x"); got != "\x1b[31mx\x1b[0m" {
		t.Errorf("terminal without NO_COLOR: %q", got)
	}
	flags.jsonOut = true
	t.Cleanup(func() { flags.jsonOut = false })
	if got := paint(&buf, colorRed, "x"); got != "x" {
		t.Errorf("--json still coloured: %q", got)
	}
}

// TestQuietSilencesNextStepsAndKeepsResults: under --quiet a clean
// validate prints its result and no next steps; a warning still reaches
// stderr.
func TestQuietSilencesNextStepsAndKeepsResults(t *testing.T) {
	isolateEnv(t)
	dir := writeTree(t, publishableSkillFiles("demo", ""))
	stdout, stderr, err := runCLI(t, nil, "--quiet", "validate", dir)
	if err != nil || !strings.Contains(stdout, "validate: PASSED") {
		t.Fatalf("validate: %v\n%s", err, stdout)
	}
	if strings.Contains(stderr, "next steps:") {
		t.Errorf("--quiet printed next steps:\n%s", stderr)
	}
	_, policyPath := isolateRunnerHome(t)
	t.Setenv("OPENAI_API_KEY", "")
	_, stderr, err = runCLI(t, strings.NewReader(""), "--quiet", "setup", "--skip-images", "--policy", policyPath, "--yes", "--provider", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning: $OPENAI_API_KEY is not set") || strings.Contains(stderr, "policy written:") || strings.Contains(stderr, "next steps:") {
		t.Errorf("--quiet setup stderr:\n%s", stderr)
	}
}
