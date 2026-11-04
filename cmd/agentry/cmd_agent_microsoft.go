package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
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
	"Assistant's microsoft-365-setup skill walks you through it, and writes the\n" +
	"request a work or school account's administrator usually has to approve.\n" +
	"In Agentry Desktop the same set-up is the Microsoft card's Set up, under\n" +
	"Mail and calendar in the Assistant's Configuration.\n" +
	"--microsoft-tenant names the tenant to sign in on: consumers for a personal\n" +
	"account (Outlook.com, Hotmail, Live), organizations for a work or school\n" +
	"one, or your organization's tenant id; without it, Microsoft is asked which\n" +
	"your address is. --microsoft-sign-in then signs it in by a device code: it\n" +
	"shows a code and opens Microsoft's sign-in page, where you enter it, and\n" +
	"waits up to 15 minutes. The sign-in reaches Microsoft's sign-in host alone,\n" +
	"asks only for reading unless \"allow changes\" is on, and is kept sealed on\n" +
	"the server's volume under a key in your system keychain.\n" +
	"Without --microsoft-client, the sign-in uses Agentry's own Microsoft app\n" +
	"where your Agentry registry offers one, and only with Agentry's image of the\n" +
	"server; your own app, once given, comes first.\n" +
	"--microsoft-signoff sets a line every mail it sends or drafts ends with, and\n" +
	"--no-microsoft-signoff clears it. A running agent takes the app, the tenant\n" +
	"and the signoff at its next turn; the sign-in pauses it between turns while\n" +
	"the server writes its volume, then the agent goes on with it.\n" +
	"Switch the server on with agentry agent grant assistant server:ms365 on.\n\n" +
	"  agentry agent configure assistant --microsoft-client 11111111-2222-3333-4444-555555555555\n" +
	"  agentry agent configure assistant --microsoft-sign-in you@outlook.com"

// configureMicrosoft runs --microsoft-client (with --microsoft-tenant),
// --microsoft-signoff and --microsoft-sign-in, in that order.
func configureMicrosoft(cmd *cobra.Command, a persistent.Agent, home, policyPath, clientID, tenant, email string, signoff *string, yes bool) error {
	if !hasImageServer(a, assistant.MS365Key) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no Microsoft 365", a.Name))
	}
	pol, _, _, err := agentPolicy(policyPath)
	if err != nil {
		return err
	}
	m := persistent.Microsoft{Home: home, Agent: a, Policy: pol, Keys: persistent.KeychainSecrets{}, Say: cmd.ErrOrStderr(),
		AgentryClient: func(ctx context.Context) string {
			return connection.OpenPublicClients(home, registryURL(), httpClient).ForConnector(ctx, persistent.MicrosoftConnector)
		},
	}
	if clientID != "" {
		before, err := persistent.LoadConfig(home, a.Name)
		if err != nil {
			return engine.Classify(engine.ExitCLI, err)
		}
		_, running, err := configureAgent(cmd, home, a, persistent.Change{MicrosoftClient: &persistent.MicrosoftClient{ClientID: clientID, Tenant: tenant}}, yes)
		if err != nil {
			return fmt.Errorf("--microsoft-client: %w", err)
		}
		if !slices.Contains(before.MailProviders, assistant.MailMicrosoft) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Microsoft is now one of the %s agent's mail providers (agentry agent configure %s --no-mail-provider %s takes it off its roster).\n", a.Name, a.Name, assistant.MailMicrosoft)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Microsoft 365 has your Entra app (%s). Sign it in with --microsoft-sign-in <email>.\n", whenTaken(running))
	}
	if signoff != nil {
		_, running, err := configureAgent(cmd, home, a, persistent.Change{MicrosoftSignoff: signoff}, yes)
		if err != nil {
			return fmt.Errorf("--microsoft-signoff: %w", err)
		}
		what := "your signoff"
		if strings.TrimSpace(*signoff) == "" {
			what = "no signoff"
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Mail Microsoft 365 sends or drafts ends with %s (%s).\n", what, whenTaken(running))
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
	for _, s := range a.ImageServers(policy.Images{}) {
		if s.Key == key {
			return true
		}
	}
	return false
}
