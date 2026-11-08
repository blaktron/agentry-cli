package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// TestNoFlagUsageCarriesABacktick holds every flag's help text free of
// backticks: pflag reads the first backticked words of a usage string as the
// value's placeholder, so a usage that quotes a command (`agentry run`)
// renders as "--target agentry run".
func TestNoFlagUsageCarriesABacktick(t *testing.T) {
	walkCommands(newRootCmd(), func(c *cobra.Command) {
		check := func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s: the usage carries a backtick: %q", c.CommandPath(), f.Name, f.Usage)
			}
		}
		c.LocalFlags().VisitAll(check)
		c.PersistentFlags().VisitAll(check)
	})
}

// TestRootHelpOpensWithThePurposeAndTheDocs: the root help's first line says
// what the CLI is, the docs link comes before the usage, the commands are
// in the newcomer's groups in order, and the credential prose comes last.
func TestRootHelpOpensWithThePurposeAndTheDocs(t *testing.T) {
	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	if !strings.HasPrefix(help, "Agentry runs QuickAgents") {
		t.Fatalf("the first line is not the purpose:\n%s", help)
	}
	docs, usage := strings.Index(help, docsURL), strings.Index(help, "Usage:")
	if docs < 0 || docs > usage {
		t.Fatalf("the docs link is not before the usage (docs at %d, usage at %d)", docs, usage)
	}
	last := usage
	for _, g := range commandGroups {
		i := strings.Index(help, "\n"+g.title+"\n")
		if i < last {
			t.Fatalf("group %q is missing or out of order", g.title)
		}
		last = i
	}
	more, env := strings.Index(help, `Use "agentry [command] --help"`), strings.Index(help, "Registry and credentials from the environment:")
	if more < 0 || env < more {
		t.Fatalf("the credential prose is not after the command list (more at %d, prose at %d)", more, env)
	}
	if strings.Contains(help, "Additional Commands:") {
		t.Fatal("a command is outside every group")
	}

	// A subcommand's help carries no root prose.
	var sub strings.Builder
	root = newRootCmd()
	root.SetOut(&sub)
	root.SetArgs([]string{"search", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sub.String(), "Registry and credentials") {
		t.Fatal("search --help carries the root's credential prose")
	}
}

// TestEveryRootCommandHasAGroup: a command added to the root without a
// place in commandGroups would land under "Additional Commands".
func TestEveryRootCommandHasAGroup(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		if !c.Hidden && c.GroupID == "" && c.Name() != "help" && c.Name() != "completion" {
			t.Errorf("%s has no group: add it to commandGroups", c.Name())
		}
	}
}

// TestTheNewcomersCommandsCarryExamples: each example command is the
// command's own.
func TestTheNewcomersCommandsCarryExamples(t *testing.T) {
	root := newRootCmd()
	for _, path := range []string{"search", "info", "install", "run", "schedule", "doctor"} {
		c := findCommand(root, path)
		if strings.TrimSpace(c.Example) == "" {
			t.Errorf("%s has no example", path)
			continue
		}
		for _, line := range strings.Split(c.Example, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.HasPrefix(line, "agentry "+path) {
				t.Errorf("%s's example runs something else: %q", path, line)
			}
		}
	}
}

// TestRunsDriverFlagsAreHiddenAndAccepted: a driver's flags are left out of
// run --help's flag list, named in its text, and still parse.
func TestRunsDriverFlagsAreHiddenAndAccepted(t *testing.T) {
	run := findCommand(newRootCmd(), "run")
	usage := run.UsageString()
	for _, name := range runDriverFlags {
		f := run.Flags().Lookup(strings.TrimPrefix(name, "--"))
		if f == nil {
			t.Fatalf("%s is not a flag of run", name)
		}
		if !f.Hidden {
			t.Errorf("%s is listed in run --help", name)
		}
		if strings.Contains(usage, name+" ") {
			t.Errorf("run's flag list shows %s", name)
		}
		if !strings.Contains(run.Long, name) {
			t.Errorf("run --help's text does not name %s", name)
		}
	}
	if err := run.ParseFlags([]string{"--session-protocol", "--session-inputs", "d", "--review-inputs", "https://r.example",
		"--connections-file", "c.json", "--destinations-file", "d.json", "--voice-file", "v.md"}); err != nil {
		t.Fatalf("the driver flags do not parse: %v", err)
	}
}

// TestUsageErrorsExitTwoWithAPointer: a flag the command does not take, the
// wrong arguments, a command that does not exist (at the root or under a
// group) and a run with no reference all exit with the usage code, followed
// by the line naming the command's help.
func TestUsageErrorsExitTwoWithAPointer(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"run", "--no-such-flag"}, "agentry run"},
		{[]string{"no-such-command"}, "agentry"},
		{[]string{"info", "a/b", "c/d"}, "agentry info"},
		{[]string{"agent", "no-such-command"}, "agentry agent"},
		{[]string{"run"}, "agentry run"},
	} {
		root := newRootCmd()
		var stderr strings.Builder
		root.SetErr(&stderr)
		root.SetOut(&stderr)
		root.SetArgs(tc.args)
		_, err := root.ExecuteC()
		err = asUsageError(root, err)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
			t.Errorf("%v: %v, want the usage class", tc.args, err)
			continue
		}
		var pointer strings.Builder
		printUsagePointer(&pointer, err)
		if want := "Run '" + tc.path + " --help' for usage.\n"; pointer.String() != want {
			t.Errorf("%v: pointer %q, want %q", tc.args, pointer.String(), want)
		}
	}
	// Any other failure has no pointer, a usage-class one raised while the
	// command works included: its line already says what to do.
	for _, err := range []error{
		errors.New("the registry refused"),
		engine.Classify(engine.ExitUsage, errors.New("no Writing Voice is kept on this computer")),
	} {
		var none strings.Builder
		printUsagePointer(&none, err)
		if none.Len() != 0 {
			t.Errorf("%v has a pointer: %q", err, none.String())
		}
	}
}
