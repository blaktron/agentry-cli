package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// agentMailLong is configure's help for the mail providers.
const agentMailLong = "\n\n--mail-provider enables one of the Assistant's mail providers, the services\n" +
	"whose mail and calendar it can reach (google: Google Workspace through gog),\n" +
	"and --no-mail-provider disables one. Only an enabled provider's servers are\n" +
	"on its roster, to switch on with agentry agent grant. Disabling keeps the\n" +
	"provider's sign-in, so enabling it again needs no new one; agentry agent reset\n" +
	"removes it. Both are refused while the agent runs. Setting a provider up\n" +
	"(--google-client, --google-sign-in) enables it.\n\n" +
	"  agentry agent configure assistant --mail-provider google\n" +
	"  agentry agent configure assistant --no-mail-provider google"

// configureMailProviders runs --mail-provider and --no-mail-provider, then
// prints the agent's providers. A provider both enabled and disabled is
// refused before anything changes.
func configureMailProviders(cmd *cobra.Command, a persistent.Agent, home string, on, off []string) error {
	for _, k := range on {
		if slices.Contains(off, k) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--mail-provider and --no-mail-provider both name %s", k))
		}
	}
	for _, set := range []struct {
		keys []string
		on   bool
	}{{on, true}, {off, false}} {
		for _, k := range set.keys {
			if err := persistent.SetMailProvider(home, a, persistent.ClientCLI, strings.ToLower(strings.TrimSpace(k)), set.on); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
		}
	}
	c, err := persistent.LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	ps := persistent.MailProviders(a, c)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), ps)
	}
	printMailProviders(cmd.OutOrStdout(), ps)
	return nil
}

// printMailProviders lists an agent's mail providers, one a line.
func printMailProviders(w io.Writer, ps []persistent.MailProviderState) {
	for _, p := range ps {
		state := "disabled"
		if p.Enabled {
			state = "enabled"
		}
		line := fmt.Sprintf("    %s\t%s: %s", p.Key, p.Name, state)
		switch {
		case len(p.Accounts) > 0:
			line += ", signed in as " + safeTerminalText(strings.Join(p.Accounts, ", "), 300)
		case p.Enabled:
			line += ", not signed in"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
