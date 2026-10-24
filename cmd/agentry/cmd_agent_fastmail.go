package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// agentFastmailLong is configure's help for Fastmail.
const agentFastmailLong = "\n\n--fastmail-token gives the Assistant's Fastmail server your Fastmail API\n" +
	"token, read from the terminal without echoing it, or from stdin. Make one of\n" +
	"the MCP type in Fastmail's settings (Privacy & Security, Connected apps & API\n" +
	"tokens), Read only unless you want the Assistant to change or send things:\n" +
	"the server can do whatever the token allows. Fastmail is asked whether it\n" +
	"accepts the token before it is kept, in your system keychain, and the\n" +
	"fastmail mail provider is enabled. --no-fastmail removes the token. Both are\n" +
	"refused while the agent runs; switch the server on with\n" +
	"agentry agent grant assistant server:url-api-fastmail-com-mcp on.\n\n" +
	"  agentry agent configure assistant --fastmail-token < token.txt"

// configureFastmail keeps the Fastmail token (on) or removes it (off).
func configureFastmail(cmd *cobra.Command, a persistent.Agent, home string, on, yes bool) error {
	if !a.Builtin.KeepsSecrets() || !slices.ContainsFunc(a.Builtin.OfferedServers(nil), func(s offer.Server) bool { return s.MailProvider == assistant.MailFastmail }) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no Fastmail", a.Name))
	}
	if held, _ := persistent.Held(home, a.Name); held {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running: stop it first (agentry agent stop %s)", a.Name, a.Name))
	}
	account := persistent.SecretAccount(a.Name, assistant.FastmailToken)
	if !on {
		c, err := persistent.LoadConfig(home, a.Name)
		if err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		if !slices.ContainsFunc(c.Secrets, func(s persistent.StoredSecret) bool { return s.Name == assistant.FastmailToken }) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent keeps no Fastmail token", a.Name))
		}
		r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveSecret: assistant.FastmailToken}, false)
		if err != nil {
			return err
		}
		if err := agentSecrets.Delete(account); err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		printChanged(cmd.OutOrStdout(), a, r, running)
		return nil
	}
	token, err := readSecretValue(cmd, "Fastmail API token", "the Fastmail API token")
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if err := persistent.FastmailTokenAccepted(cmd.Context(), token); err != nil {
		if errors.Is(err, persistent.ErrFastmailRefused) {
			return engine.Classify(engine.ExitUsage, err)
		}
		return engine.Classify(engine.ExitCLI, err)
	}
	previous, _ := agentSecrets.Get(account)
	if err := agentSecrets.Set(account, token); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	undo := func() {
		if previous != "" {
			_ = agentSecrets.Set(account, previous)
		} else {
			_ = agentSecrets.Delete(account)
		}
	}
	// The provider is enabled first, so the agreement asked for names the
	// server the token goes to; a change refused or failed disables it
	// again if it was not enabled before.
	c, err := persistent.LoadConfig(home, a.Name)
	if err != nil {
		undo()
		return engine.Classify(engine.ExitCLI, err)
	}
	wasOn := slices.Contains(c.MailProviders, assistant.MailFastmail)
	if err := persistent.SetMailProvider(home, a, persistent.ClientCLI, assistant.MailFastmail, true); err != nil {
		undo()
		return engine.Classify(engine.ExitCLI, err)
	}
	r, running, err := configureAgent(cmd, home, a, persistent.Change{SetSecret: &persistent.StoredSecret{Name: assistant.FastmailToken, Set: time.Now().UTC()}}, yes)
	if err != nil {
		undo()
		if !wasOn {
			_ = persistent.SetMailProvider(home, a, persistent.ClientCLI, assistant.MailFastmail, false)
		}
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Fastmail accepted the token, and it is kept in your system keychain. Switch the server on with agentry agent grant %s server:url-api-fastmail-com-mcp on, if it is not.\n", a.Name)
	if r2, err := readAgentState(cmd.Context(), home, a); err == nil {
		r = r2.Roster
	}
	printChanged(cmd.OutOrStdout(), a, r, running)
	return nil
}
