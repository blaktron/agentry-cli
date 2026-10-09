// Sign in with GitHub for a Copilot plan (plan model-sign-ins.md §4.5,
// D25–D29; agentry-cli#741): `agentry model-signins add copilot` and the
// offer in `agentry setup`. The flow is mcpauth.GitHub, the store
// modelsignin.SignInCopilot; this is the terminal around them.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// githubEndpoints points the flow somewhere else than GitHub: nil in
// production, a test's fake otherwise.
var githubEndpoints func(*mcpauth.GitHub)

// signInCopilot runs GitHub's device flow through app (Agentry's GitHub
// App, or its OAuth App), showing the code and opening GitHub's page, and
// keeps the grant.
func signInCopilot(cmd *cobra.Command, home, app string) (*modelsignin.CopilotSignedIn, error) {
	errOut := cmd.ErrOrStderr()
	flow := mcpauth.GitHub{
		HTTP: vendorHTTP(),
		Present: func(page, code string) {
			_, _ = fmt.Fprintf(errOut, "Sign in with GitHub: enter the code %s at\n  %s\n", code, page)
			if os.Getenv("AGENTRY_NO_BROWSER") == "" {
				if err := openBrowser(page); err != nil {
					_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
				}
			}
			_, _ = fmt.Fprintln(errOut, "Waiting for GitHub (the code lasts 15 minutes)…")
		},
	}
	if githubEndpoints != nil {
		githubEndpoints(&flow)
	}
	return modelsignin.Open(home, connectionKeys()).SignInCopilot(cmd.Context(), flow, app)
}

// copilotOffer is what `agentry setup` offers of the Copilot sign-in.
func copilotOffer(cmd *cobra.Command, home string, oauthApp bool) setup.CopilotOffer {
	app := mcpauth.GitHubApp
	if oauthApp {
		app = mcpauth.GitHubOAuthApp
	}
	return setup.CopilotOffer{
		Kept: func() string {
			r, err := modelsignin.Open(home, nil).Get(modelsignin.Copilot)
			if err != nil || r.Broken != "" {
				return ""
			}
			if r.Label == "" {
				return "a GitHub account"
			}
			return r.Label
		},
		SignIn: func() (string, error) {
			got, err := signInCopilot(cmd, home, app)
			if err != nil {
				return "", err
			}
			return got.Record.Label, nil
		},
	}
}

// addCopilot is `agentry model-signins add copilot`.
func addCopilot(cmd *cobra.Command, home string, oauthApp bool) error {
	app := mcpauth.GitHubApp
	if oauthApp {
		app = mcpauth.GitHubOAuthApp
	}
	got, err := signInCopilot(cmd, home, app)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), signedInJSON{Vendor: modelsignin.Copilot, Kind: got.Record.Kind, Account: got.Record.Label, Models: []string{}})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signed in with GitHub as %s (through Agentry's %s)\n", got.Record.Label, mcpauth.GitHubAppName(app))
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "to run on it: agentry setup (provider copilot); your Copilot plan then pays for the runs on this computer")
	return nil
}
