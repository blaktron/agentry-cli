package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// agentGoogleLong is configure's help for Google Workspace.
const agentGoogleLong = "\n\n--google-client gives the Assistant's Google Workspace server (gog) your own\n" +
	"Google Cloud OAuth client: the JSON file of a client ID of type Desktop app,\n" +
	"read here and handed to gog, which keeps its id on the server's own volume\n" +
	"and its secret in its encrypted keyring, whose password is kept in your\n" +
	"system keychain. --google-sign-in then signs it in to your Google account:\n" +
	"your browser opens Google's consent, its answer comes back to this computer\n" +
	"at 127.0.0.1, and gog exchanges it reaching Google's token endpoint alone.\n" +
	"It asks for read-only access unless the server's \"allow changes\" is on.\n" +
	"Both are refused while the agent runs; switch the server on with agentry\n" +
	"agent grant assistant server:gog on.\n\n" +
	"  agentry agent configure assistant --google-client ~/Downloads/client_secret.json\n" +
	"  agentry agent configure assistant --google-sign-in you@example.com"

// configureGoogle runs --google-client and --google-sign-in, in that order.
func configureGoogle(cmd *cobra.Command, a persistent.Agent, home, policyPath, clientFile, email string) error {
	if len(a.Builtin.ImageServers(policy.Images{})) == 0 {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent has no Google Workspace", a.Name))
	}
	if held, _ := persistent.Held(home, a.Name); held {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is running, and its Google Workspace server holds its sign-in: stop it first (agentry agent stop %s)", a.Name, a.Name))
	}
	pol, _, _, err := agentPolicy(policyPath)
	if err != nil {
		return err
	}
	d := agentDocker()
	if err := d.Available(cmd.Context()); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := ensureImage(cmd, d, pol.Images.GogImage()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	g := persistent.Google{Docker: d, Home: home, Agent: a, Policy: pol, RunnerImage: runnerTag, Keys: persistent.KeychainSecrets{}, Say: cmd.ErrOrStderr()}
	if clientFile != "" {
		b, err := readBounded(clientFile, 64<<10)
		if err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--google-client: %w", err))
		}
		if err := g.SetClient(cmd.Context(), b); err != nil {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("--google-client: %w", err))
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Google Workspace has your Google Cloud client. Sign it in with --google-sign-in <email>.")
	}
	if email == "" {
		return nil
	}
	if err := gateway.EnsureRunnerImage(cmd.Context(), d, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	browse := openBrowser
	if os.Getenv("AGENTRY_NO_BROWSER") != "" {
		browse = nil
	}
	account, err := g.SignIn(cmd.Context(), email, browse)
	if err != nil {
		return engine.Classify(engine.ExitCLI, fmt.Errorf("--google-sign-in: %w", err))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Google Workspace is signed in as %s. Switch it on with agentry agent grant %s server:gog on, if it is not.\n", account, a.Name)
	return nil
}

// ensureImage pulls a pinned image the engine lacks, by its digest.
func ensureImage(cmd *cobra.Command, d dockerx.Client, ref string) error {
	ok, err := d.ImageExists(cmd.Context(), ref)
	if err != nil || ok {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "pulling %s (first use)\n", ref)
	return d.Pull(cmd.Context(), ref)
}
