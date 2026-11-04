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
const agentMailLong = "\n\n--mail-provider enables one of an agent's mail providers (the Assistant's, or\n" +
	"one a published agent declares), the services whose mail and calendar it can\n" +
	"reach (google: Google Workspace through gog),\n" +
	"and --no-mail-provider disables one. Only an enabled provider's servers are\n" +
	"on its roster, to switch on with agentry agent grant. Disabling keeps the\n" +
	"provider's sign-in, so enabling it again needs no new one; agentry agent reset\n" +
	"removes it. A running agent takes either at its next turn. Setting one up\n" +
	"(--google-client, --google-sign-in) enables it.\n\n" +
	"  agentry agent configure assistant --mail-provider google\n" +
	"  agentry agent configure assistant --no-mail-provider google"

// configureMailProviders runs --mail-provider and --no-mail-provider, then
// prints the agent's providers. A provider both enabled and disabled is
// refused before anything changes.
func configureMailProviders(cmd *cobra.Command, a persistent.Agent, home string, on, off []string, yes bool) error {
	for _, k := range on {
		if slices.Contains(off, k) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--mail-provider and --no-mail-provider both name %s", k))
		}
	}
	running := false
	for _, set := range []struct {
		keys []string
		on   bool
	}{{on, true}, {off, false}} {
		for _, k := range set.keys {
			ch := persistent.Change{MailProvider: &persistent.MailProviderSwitch{Key: strings.ToLower(strings.TrimSpace(k)), On: set.on}}
			_, r, err := configureAgent(cmd, home, a, ch, yes)
			if err != nil {
				return err
			}
			running = running || r
		}
	}
	if !flags.jsonOut {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Saved: %s.\n", whenTaken(running))
	}
	c, err := persistent.LoadConfig(home, a.Name)
	if err != nil {
		return err
	}
	ps := persistent.MailProviders(a, c)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"providers": ps})
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
		case p.Enabled && len(p.SetUp) == 0:
			line += ", set up"
		case p.Enabled:
			line += ", not set up yet"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
