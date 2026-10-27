// agentry agent secret: the secrets the person stores for a persistent agent,
// each kept in the system keychain and given, while it is on, to the servers
// that declare it and to the Coding agent's programs (pkg/runner/persistent).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

var agentSecretLong = "A stored secret is a value you keep for a persistent agent, such as an API\n" +
	"key, under the environment variable name a program or a server reads it by.\n" +
	"Its value stays in your system keychain, never in the agent's configuration,\n" +
	"and is never listed. While it is on, the agent's segment is given it: an MCP\n" +
	"server you added that declares a secret of that name receives it, and the\n" +
	"Coding agent's programs find it in their environment, where they can read it\n" +
	"and send it anywhere they reach. The Assistant runs no programs, so its\n" +
	"secrets reach its servers alone.\n\n" +
	"Switched off (`agentry agent grant <name> secret:<NAME> off`), it is withheld:\n" +
	"a server that declares it is given an empty value. Its value is masked\n" +
	"wherever it appears whole in the receipts, the transcript and the results of\n" +
	"the runner's own tools; what a server returns is the server's own.\n\n" +
	"set reads the value from the terminal without echoing it, or from stdin when\n" +
	"stdin is not a terminal, and asks you to agree to what it gives the agent.\n" +
	"A running agent takes a change at its next turn.\n\n" +
	"  agentry agent secret set coder GITHUB_TOKEN\n" +
	"  agentry agent secret set assistant BRAVE_API_KEY < key.txt\n" +
	"  agentry agent secret list coder\n" +
	"  agentry agent secret remove coder GITHUB_TOKEN"

// agentSecrets is where the stored secrets' values are kept: the keychain,
// but in a test.
var agentSecrets persistent.Secrets = persistent.KeychainSecrets{}

func newAgentSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Set, remove and list the secrets a persistent agent keeps",
		Long:  agentSecretLong,
	}
	cmd.AddCommand(newAgentSecretListCmd(), newAgentSecretSetCmd(), newAgentSecretRemoveCmd())
	return cmd
}

// secretAgent is the agent args names, refused when it keeps no secrets.
func secretAgent(name string) (persistent.Agent, error) {
	a, err := cliAgent(name)
	if err != nil {
		return a, err
	}
	if !a.Builtin.KeepsSecrets() {
		return a, engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent keeps no secrets", a.Name))
	}
	return a, nil
}

func newAgentSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <name>",
		Short: "List a persistent agent's stored secrets by name, and whether each is on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := secretAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			st, err := readAgentState(cmd.Context(), home, a)
			if err != nil {
				return err
			}
			secrets := persistent.Roster{}
			for _, e := range st.Roster {
				if e.Kind == persistent.KindSecret {
					secrets = append(secrets, e)
				}
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent   string            `json:"agent"`
					Secrets persistent.Roster `json:"secrets"`
				}{a.Name, secrets})
			}
			printSecrets(cmd.OutOrStdout(), a, secrets)
			return nil
		},
	}
}

func newAgentSecretSetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "set <name> <NAME>",
		Short: "Store a secret for a persistent agent, or set its value again",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := secretAgent(args[0])
			if err != nil {
				return err
			}
			name := args[1]
			if err := persistent.CheckSecretName(name); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			c, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			existed := slices.ContainsFunc(c.Secrets, func(s persistent.StoredSecret) bool { return s.Name == name })
			value, err := readSecretValue(cmd, "Value of "+name, "the value of "+name)
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			account := persistent.SecretAccount(a.Name, name)
			// The value goes into the keychain before the change records
			// it, so no segment reads a secret it has no value for. A change
			// that fails leaves the keychain as it was.
			previous := ""
			if existed {
				previous, _ = agentSecrets.Get(account)
			}
			if err := agentSecrets.Set(account, value); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{SetSecret: &persistent.StoredSecret{Name: name, Set: time.Now().UTC()}}, yes)
			if err != nil {
				if previous != "" {
					_ = agentSecrets.Set(account, previous)
				} else {
					_ = agentSecrets.Delete(account)
				}
				return err
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what storing the secret gives the agent, without asking")
	return cmd
}

func newAgentSecretRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <NAME>",
		Short: "Remove a stored secret from a persistent agent and from the keychain",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := secretAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveSecret: args[1]}, false)
			if err != nil {
				return err
			}
			if err := agentSecrets.Delete(persistent.SecretAccount(a.Name, args[1])); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
}

// readSecretValue reads a secret's value: from the terminal without echo
// when stdin is one, asking for it as prompt, else from stdin, one trailing
// line break dropped. what names it in an error.
func readSecretValue(cmd *cobra.Command, prompt, what string) (string, error) {
	return readSecretInput(cmd, prompt, what, false)
}

// readSecretInput is readSecretValue, taking an empty value as "" when
// empty is allowed.
func readSecretInput(cmd *cobra.Command, prompt, what string, empty bool) (string, error) {
	in := cmd.InOrStdin()
	var v string
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		got, err := askHidden(cmd, prompt+" (not shown): ")
		if err != nil {
			return "", fmt.Errorf("read %s: %w", what, err)
		}
		v = got
	} else {
		// Room for a line break after the longest value, and one byte
		// more, so a value past the bound is refused, not cut.
		b, err := io.ReadAll(io.LimitReader(in, persistent.MaxSecretBytes+3))
		if err != nil {
			return "", fmt.Errorf("read %s from stdin: %w", what, err)
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if v == "" {
		if empty {
			return "", nil
		}
		return "", errors.New("the value is empty: nothing was stored")
	}
	if err := persistent.CheckSecretValue(v); err != nil {
		return "", err
	}
	return v, nil
}

// printSecrets prints the stored secrets by name, one a line, never a value.
func printSecrets(w io.Writer, a persistent.Agent, secrets persistent.Roster) {
	if len(secrets) == 0 {
		_, _ = fmt.Fprintf(w, "The %s agent has no stored secrets. Store one: agentry agent secret set %s <NAME>\n", a.Name, a.Name)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SECRET\tSTATE\tWHAT")
	for _, e := range secrets {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.TrimPrefix(e.ID, persistent.SecretEntryID("")), e.State, safeTerminalText(e.Line, 800))
	}
	_ = tw.Flush()
}
