// Sign in with ChatGPT (plan model-sign-ins.md §4.3, D8–D16;
// agentry-cli#739): `agentry model-signins add chatgpt` and the offer in
// `agentry setup`. The flow is mcpauth.ChatGPT, the store
// modelsignin.SignInChatGPT; this is the terminal around them.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// chatgptEndpoints points the flow somewhere else than OpenAI: nil in
// production, a test's fake otherwise.
var chatgptEndpoints func(*mcpauth.ChatGPT)

// signInChatGPT runs the sign-in in the browser, with the address shown
// and, where the browser is on another machine, the redirect pasted back
// (as runConsent does for a connection), and keeps the grant.
func signInChatGPT(cmd *cobra.Command, in *bufio.Reader, home string, newAccount bool) (*modelsignin.ChatGPTSignedIn, error) {
	errOut := cmd.ErrOrStderr()
	pasted := make(chan string)
	done := make(chan struct{})
	var finished chan struct{}
	flow := mcpauth.ChatGPT{
		HTTP:   vendorHTTP(),
		Pasted: pasted,
		Say:    func(s string) { _, _ = fmt.Fprintln(errOut, s) },
		Present: func(authURL string) {
			_, _ = fmt.Fprintf(errOut, "Sign in with ChatGPT in your browser:\n  %s\n", authURL)
			if os.Getenv("AGENTRY_NO_BROWSER") == "" {
				if err := openBrowser(authURL); err != nil {
					_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
				}
			}
			_, _ = fmt.Fprintln(errOut, "Waiting for the browser (up to 10 minutes). If it is on another machine and ends on a page that does not load, paste that page's address here and press Enter:")
			finished = make(chan struct{})
			go readPasted(in, pasted, done, finished)
		},
	}
	if chatgptEndpoints != nil {
		chatgptEndpoints(&flow)
	}
	got, err := modelsignin.Open(home, connectionKeys()).SignInChatGPT(cmd.Context(), flow, newAccount)
	close(done)
	if finished != nil {
		select {
		case <-finished:
		default:
			_, _ = fmt.Fprintln(errOut, "Press Enter to continue.")
			<-finished
		}
	}
	return got, err
}

// chatgptModels refreshes the kept ChatGPT sign-in and lists its plan's
// models: setup's choice when the person keeps the sign-in they have.
func chatgptModels(cmd *cobra.Command, home string) (string, []string, error) {
	s := modelsignin.Open(home, connectionKeys())
	r, err := s.Get(modelsignin.ChatGPT)
	if err != nil {
		return "", nil, err
	}
	a, err := s.Use(cmd.Context(), modelsignin.ChatGPT)
	if err != nil {
		return "", nil, err
	}
	flow := mcpauth.ChatGPT{HTTP: vendorHTTP()}
	if chatgptEndpoints != nil {
		chatgptEndpoints(&flow)
	}
	models, err := flow.Models(cmd.Context(), a.Token)
	if err != nil {
		return "", nil, err
	}
	return r.Label, models, nil
}

// chatgptOffer is what `agentry setup` offers of the ChatGPT sign-in.
func chatgptOffer(cmd *cobra.Command, in *bufio.Reader, home string) setup.ChatGPTOffer {
	return setup.ChatGPTOffer{
		Kept: func() string {
			r, err := modelsignin.Open(home, nil).Get(modelsignin.ChatGPT)
			if err != nil || r.Broken != "" {
				return ""
			}
			if r.Label == "" {
				return "an account whose address ChatGPT did not give"
			}
			return r.Label
		},
		SignIn: func(fresh bool) (string, []string, error) {
			if !fresh {
				return chatgptModels(cmd, home)
			}
			got, err := signInChatGPT(cmd, in, home, false)
			if err != nil {
				return "", nil, err
			}
			return got.Record.Label, got.Models, nil
		},
	}
}

// signedInJSON is `model-signins add --json`: never a token.
type signedInJSON struct {
	Vendor  string   `json:"vendor"`
	Kind    string   `json:"kind"`
	Account string   `json:"account,omitempty"`
	Models  []string `json:"models"`
}

func newModelSignInsAddCmd() *cobra.Command {
	var newAccount, installed, oauthApp bool
	cmd := &cobra.Command{
		Use:   "add <vendor>",
		Short: "Sign in with a model vendor so your local runs can spend your plan (chatgpt, claude, copilot)",
		Long: "add chatgpt signs in with ChatGPT in your browser. Your ChatGPT plan then pays for the runs on this\n" +
			"computer whose model setup names it (agentry setup → openai → chatgpt); a hosted run never spends it.\n" +
			"The sign-in is kept in the OS keychain; runs are handed a one-hour access token, never the sign-in.\n\n" +
			"add claude is " + setup.ClaudeSignInLabel + ": Claude Code signs you in itself, in Anthropic's own pages,\n" +
			"and keeps the sign-in in a directory of Agentry's own that Agentry never reads (never your ~/.claude).\n" +
			"It signs in the Claude Code image, built on this computer, which runs Claude runs by default; with\n" +
			"--installed, the Claude Code installed on this computer, for runs with --adapter claude.\n\n" +
			"add copilot is " + setup.CopilotSignInLabel + ": you enter a code on GitHub's own page for Agentry's GitHub App\n" +
			"(with --oauth-app, its OAuth App). Your Copilot plan then pays for the runs on this computer whose model\n" +
			"setup names it (agentry setup → copilot), through GitHub's Copilot CLI in an image built on this computer.\n" +
			"The sign-in is kept in the OS keychain; runs are handed an eight-hour access token, never the sign-in.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vendor := strings.ToLower(args[0])
			home, err := runnerHome()
			if err != nil {
				return err
			}
			in := bufio.NewReader(cmd.InOrStdin())
			if oauthApp && vendor != modelsignin.Copilot {
				return errors.New("--oauth-app is for copilot")
			}
			switch vendor {
			case modelsignin.ChatGPT:
				if installed {
					return errors.New("--installed is for claude")
				}
			case modelsignin.Claude:
				if newAccount {
					return errors.New("--new-account is for chatgpt")
				}
				return addClaude(cmd, in, home, installed)
			case modelsignin.Copilot:
				if newAccount || installed {
					return errors.New("--new-account is for chatgpt, --installed for claude")
				}
				return addCopilot(cmd, home, oauthApp)
			default:
				return fmt.Errorf("this build signs in with chatgpt, claude and copilot (not %q)", args[0])
			}
			got, err := signInChatGPT(cmd, in, home, newAccount)
			var ne *mcpauth.ChatGPTNotEligibleError
			if errors.As(err, &ne) || errors.Is(err, mcpauth.ErrChatGPTNoPlanScope) {
				return fmt.Errorf("nothing kept: %w", err)
			}
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), signedInJSON{Vendor: vendor, Kind: got.Record.Kind, Account: got.Record.Label, Models: got.Models})
			}
			who := ""
			if got.Record.Label != "" {
				who = " as " + got.Record.Label
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signed in with ChatGPT%s; your plan offers: %s\n", who, strings.Join(got.Models, ", "))
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "to run on it: agentry setup (provider openai, pay with chatgpt)")
			return nil
		},
	}
	cmd.Flags().BoolVar(&newAccount, "new-account", false, "sign in to another ChatGPT account than the one this computer is registered for")
	cmd.Flags().BoolVar(&installed, "installed", false, "claude: sign in the Claude Code installed on this computer rather than the Claude Code image")
	cmd.Flags().BoolVar(&oauthApp, "oauth-app", false, "copilot: sign in through Agentry's OAuth App rather than its GitHub App (the fallback, plan D26)")
	return cmd
}
