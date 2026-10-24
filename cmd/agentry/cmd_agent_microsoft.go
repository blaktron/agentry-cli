package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// agentMicrosoftLong is configure's help for Microsoft 365.
const agentMicrosoftLong = "\n\n--microsoft-client gives the Assistant's Microsoft 365 and Outlook.com server\n" +
	"your own Entra app: its application (client) id, of an app registered as a\n" +
	"public client (\"Mobile and desktop applications\", Allow public client flows)\n" +
	"with the delegated Microsoft Graph permissions the server asks for. The\n" +
	"Assistant's microsoft-365-setup skill walks you through it.\n" +
	"--microsoft-tenant names the tenant to sign in on: consumers for a personal\n" +
	"account (Outlook.com, Hotmail, Live), organizations for a work or school\n" +
	"one, or your organization's tenant id; without it, Microsoft is asked which\n" +
	"your address is. --microsoft-sign-in then signs it in by a device code: it\n" +
	"shows a code and opens Microsoft's sign-in page, where you enter it, and\n" +
	"waits up to 15 minutes. The sign-in reaches Microsoft's sign-in host alone,\n" +
	"asks only for reading unless \"allow changes\" is on, and is kept sealed on\n" +
	"the server's volume under a key in your system keychain.\n" +
	"--microsoft-signoff sets a line every mail it sends or drafts ends with, and\n" +
	"--no-microsoft-signoff clears it. All are refused while the agent runs;\n" +
	"switch the server on with agentry agent grant assistant server:ms365 on.\n\n" +
	"  agentry agent configure assistant --microsoft-client 11111111-2222-3333-4444-555555555555\n" +
	"  agentry agent configure assistant --microsoft-sign-in you@outlook.com"

// configureMicrosoft runs --microsoft-client (with --microsoft-tenant),
// --microsoft-signoff and --microsoft-sign-in, in that order.
func configureMicrosoft(cmd *cobra.Command, a persistent.Agent, home, policyPath, clientID, tenant, email string, signoff *string) error {
	if !hasImageServer(a, assistant.MS365Key) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no Microsoft 365", a.Name))
	}
	if held, _ := persistent.Held(home, a.Name); held {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running, and its Microsoft 365 server holds its sign-in: stop it first (agentry agent stop %s)", a.Name, a.Name))
	}
	pol, _, _, err := agentPolicy(policyPath)
	if err != nil {
		return err
	}
	m := persistent.Microsoft{Home: home, Agent: a, Policy: pol, Keys: persistent.KeychainSecrets{}, Say: cmd.ErrOrStderr()}
	if clientID != "" {
		if err := m.SetClient(clientID, tenant); err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--microsoft-client: %w", err))
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Microsoft 365 has your Entra app. Sign it in with --microsoft-sign-in <email>.")
	}
	if signoff != nil {
		if err := m.SetSignoff(*signoff); err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--microsoft-signoff: %w", err))
		}
		if *signoff == "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Mail Microsoft 365 sends or drafts ends with no signoff.")
		} else {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Mail Microsoft 365 sends or drafts ends with your signoff, from its next start.")
		}
	}
	if email == "" {
		return nil
	}
	d := agentDocker()
	if err := d.Available(cmd.Context()); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := ensureImage(cmd, d, pol.Images.MS365Image()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureRunnerImage(cmd.Context(), d, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	m.Docker, m.RunnerImage = d, runnerTag
	browse := openBrowser
	if os.Getenv("AGENTRY_NO_BROWSER") != "" {
		browse = nil
	}
	account, err := m.SignIn(cmd.Context(), email, browse)
	if err != nil {
		return engine.Classify(engine.ExitCLI, fmt.Errorf("--microsoft-sign-in: %w", err))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Microsoft 365 is signed in as %s. Switch it on with agentry agent grant %s server:ms365 on, if it is not.\n", account, a.Name)
	return nil
}

// hasImageServer says the agent offers the image server key.
func hasImageServer(a persistent.Agent, key string) bool {
	for _, s := range a.Builtin.ImageServers(policy.Images{}) {
		if s.Key == key {
			return true
		}
	}
	return false
}
