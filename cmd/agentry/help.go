// How the CLI explains itself: the root help's purpose line, docs link and
// command groups, with the credential prose after the command list, and one
// path for every usage error (a flag the command does not take, the wrong
// number of arguments, a command that does not exist), which prints the
// error, a pointer to the command's help, and exits with the usage code.
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// docsURL is the CLI's page on the docs site.
const docsURL = "https://docs.secureagentry.net/runner/"

// rootLong is the root help's opening: what the CLI is and where its docs
// are. The credential prose follows the command list (rootEnvHelp).
const rootLong = "Agentry runs QuickAgents, single-purpose AI agents, in isolated containers on\n" +
	"this computer, and finds, installs, builds and publishes them.\n\n" +
	"Docs: " + docsURL + "\n" +
	"New here? agentry setup, then agentry search <words>, then agentry run ns/name."

// commandGroups are the root help's sections, in the order a newcomer meets
// them, and the commands each holds. Every visible root command is in one
// (TestEveryRootCommandHasAGroup).
var commandGroups = []struct {
	id, title string
	commands  []string
}{
	{"start", "Start here:", []string{"setup", "doctor", "login", "whoami"}},
	{"find", "Find and install:", []string{"search", "info", "install", "list", "uninstall", "verify", "builtins", "fork"}},
	{"run", "Run:", []string{"run", "runs", "open", "deliver", "destinations", "schedule", "runhost", "connect", "connections", "acp", "browser", "files"}},
	{"publish", "Publish:", []string{"init", "draft", "validate", "review", "publish", "yank"}},
	{"knowledge", "Knowledge and voice:", []string{"knowledge", "voice", "report-design"}},
	{"agents", "Persistent agents:", []string{"agent"}},
	{"config", "Configuration:", []string{"models", "providers", "model-signins", "logout", "managed", "profiles"}},
}

// rootEnvHelp is the credential prose, printed after the root help's
// command list and flags.
func rootEnvHelp() string {
	return "Registry and credentials from the environment:\n" +
		"  " + envRegistry + "         the registry to use when --registry is not passed (ahead\n" +
		"                           of the config file's defaultRegistry), and the self-hosted\n" +
		"                           registry " + envToken + " is for.\n" +
		"  " + envSecureAgentryToken + "      a personal access token sent only to the SecureAgentry\n" +
		"                           registry this build is for (agentry --version names it);\n" +
		"                           to any other registry it is not sent, and a build whose\n" +
		"                           default is not SecureAgentry sends it nowhere.\n" +
		"  " + envToken + "            a personal access token sent only to the self-hosted\n" +
		"                           registry " + envRegistry + " names, else the config file's\n" +
		"                           defaultRegistry. It is never sent to SecureAgentry, even\n" +
		"                           when --registry names it; set with no self-hosted registry\n" +
		"                           configured, it is refused.\n" +
		"An environment token is used in place of the stored sign-in for its own\n" +
		"registry; for any other, the CLI says why it is not sent and uses that\n" +
		"registry's stored sign-in, if any. Sign in with agentry login.\n"
}

// applyHelp gives root its groups and help, and routes every usage error
// through usageError. It runs once the whole tree is built.
func applyHelp(root *cobra.Command) {
	for _, g := range commandGroups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
		for _, name := range g.commands {
			c := findCommand(root, name)
			if c == nil {
				panic(fmt.Sprintf("commandGroups names %q, which the command tree does not have", name))
			}
			c.GroupID = g.id
		}
	}
	root.SetHelpCommandGroupID("config")
	root.SetCompletionCommandGroupID("config")

	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		defaultHelp(c, args)
		if c == root {
			_, _ = fmt.Fprint(c.OutOrStdout(), "\n"+rootEnvHelp())
		}
	})
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return usageError(c, err)
	})
	walkCommands(root, func(c *cobra.Command) {
		if args := c.Args; args != nil {
			c.Args = func(cmd *cobra.Command, a []string) error {
				if err := args(cmd, a); err != nil {
					return usageError(cmd, err)
				}
				return nil
			}
		}
		// A command that only groups others shows its help when called
		// bare, and refuses a subcommand it does not have instead of
		// showing the help and succeeding.
		if c != root && c.HasSubCommands() && c.Run == nil && c.RunE == nil {
			c.Args = cobra.ArbitraryArgs
			c.RunE = func(cmd *cobra.Command, a []string) error {
				if len(a) > 0 {
					return usageError(cmd, fmt.Errorf("unknown command %q for %q", a[0], cmd.CommandPath()))
				}
				return cmd.Help()
			}
		}
	})
}

// walkCommands calls fn on root and every command beneath it.
func walkCommands(root *cobra.Command, fn func(*cobra.Command)) {
	fn(root)
	for _, c := range root.Commands() {
		walkCommands(c, fn)
	}
}

// usageErr is a command used the wrong way: it exits with the usage code,
// and the line is followed by a pointer to the command's help.
type usageErr struct {
	path string
	err  error
}

func (e *usageErr) Error() string { return e.err.Error() }
func (e *usageErr) Unwrap() error { return e.err }

// usageError marks err as a usage error of c, classified with the usage
// exit code.
func usageError(c *cobra.Command, err error) error {
	var u *usageErr
	if errors.As(err, &u) {
		return err
	}
	return engine.Classify(engine.ExitUsage, &usageErr{path: c.CommandPath(), err: err})
}

// asUsageError recognises the usage errors cobra reports before any hook
// of ours runs: a command root does not have. It returns err otherwise.
func asUsageError(root *cobra.Command, err error) error {
	if err != nil && strings.HasPrefix(err.Error(), "unknown command ") {
		return usageError(root, err)
	}
	return err
}

// printUsagePointer writes the one line after a command-line mistake that
// says where the command's usage is: a flag the command does not take, the
// wrong arguments, a command that does not exist, or a missing reference
// (the errors usageError marks). A failure of the usage class raised while
// the command works (no Writing Voice kept, say) already says what to do,
// and gets no pointer.
func printUsagePointer(w io.Writer, err error) {
	var u *usageErr
	if errors.As(err, &u) {
		_, _ = fmt.Fprintf(w, "Run '%s --help' for usage.\n", u.path)
	}
}
