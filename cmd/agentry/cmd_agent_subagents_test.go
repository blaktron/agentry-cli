package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestASubagentRunIsAnOrdinaryAutonomousRun: the CLI runs a subagent as
// `agentry run` of its exact version in the autonomous mode, with the task
// file, the staged files and the answers alone: never persistent, never
// another agent's state, never a destination or a Writing Voice.
func TestASubagentRunIsAnOrdinaryAutonomousRun(t *testing.T) {
	args := cliSubagents{policyPath: "/p/policy.yaml"}.runArgs(persistent.SubagentRunSpec{
		Registry: "https://registry.test", Exact: "acme/pdf-summary@1.4.2", Task: "Summarise",
		Answers: map[string]string{"length": "short", "audience": "me"}, Files: []string{"/s/in/report.pdf"}, OutDir: "/s/out",
	}, "/s/task.txt")
	want := []string{"run", "acme/pdf-summary@1.4.2", "--mode", "autonomous", "--yes", "--output-format", "json", "--out", "/s/out",
		"--input", "/s/task.txt", "--no-destination", "--no-voice", "--quiet", "--registry", "https://registry.test", "--policy", "/p/policy.yaml",
		"--file", "/s/in/report.pdf", "--intake", "audience=me", "--intake", "length=short"}
	if !slices.Equal(args, want) {
		t.Fatalf("args:\n%q\nwant\n%q", args, want)
	}
	if j := strings.Join(args, " "); strings.Contains(j, "persistent") || strings.Contains(j, "agent ") {
		t.Errorf("a subagent's run names a persistent agent: %s", j)
	}
}

// TestDesktopUseIsTheInstallsWords: a subagent's usage names each
// application and computer use as the install's consent does, and a
// release with no desktop line names nothing.
func TestDesktopUseIsTheInstallsWords(t *testing.T) {
	if got := desktopUse(nil); got != nil {
		t.Errorf("no line: %q", got)
	}
	d := &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "kcalc", Use: manifest.DesktopUseComputer}}, ComputerUse: manifest.ComputerUseApp}
	want := []string{manifest.DescribeDesktopServer(d.Servers[0]), manifest.DescribeComputerUse(d)}
	if got := desktopUse(d); !slices.Equal(got, want) {
		t.Errorf("desktopUse = %q, want %q", got, want)
	}
	if !strings.Contains(want[1], "kcalc") {
		t.Errorf("computer use does not name the window: %q", want[1])
	}
}
