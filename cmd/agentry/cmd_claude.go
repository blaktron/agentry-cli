// Use your Claude subscription (runs Claude Code) (plan model-sign-ins.md
// §4.4, D19–D24; agentry-cli#740): `agentry model-signins add claude`, its
// removal, and the offer in `agentry setup`. The sign-in and its removal
// are pkg/runner/claudesignin, which the desktop calls too
// (agentry-cli#844); this is the terminal around them.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/claudesignin"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// claudeDocker is the daemon the image's sign-in runs on: a seam for tests.
var claudeDocker = func() dockerx.Client { return dockerx.New() }

// claudeVolume is the Claude Code image's config directory.
const claudeVolume = claudesignin.Volume

// claudeHostDir is the installed Claude Code's config directory.
func claudeHostDir(home string) string { return claudesignin.HostDir(home) }

// claudeStdin is what Claude Code's prompt reads: the terminal itself when
// nothing of it is buffered here, so the code the person pastes goes from
// the terminal to Claude Code without passing through Agentry.
func claudeStdin(cmd *cobra.Command, in *bufio.Reader) io.Reader {
	if f, ok := cmd.InOrStdin().(*os.File); ok && in.Buffered() == 0 {
		return f
	}
	return in
}

// claudeSignIn is the package's sign-in on this CLI's home, daemon and
// keychain.
func claudeSignIn(home string) claudesignin.SignIn {
	return claudesignin.SignIn{Home: home, Docker: claudeDocker, Keys: connectionKeys()}
}

// signInClaude signs in the image's Claude Code, or the installed one, with
// the terminal as the person's side: Claude Code's output on stderr, the
// browser opened unless AGENTRY_NO_BROWSER, and the terminal's input.
func signInClaude(cmd *cobra.Command, in *bufio.Reader, home string, installed bool) (*modelsignin.Record, error) {
	errOut := cmd.ErrOrStderr()
	open := func(u string) {
		if os.Getenv("AGENTRY_NO_BROWSER") != "" {
			return
		}
		if err := openBrowser(u); err != nil {
			_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
		}
	}
	return claudeSignIn(home).Run(cmd.Context(), claudesignin.Person{In: claudeStdin(cmd, in), Out: errOut, Open: open}, installed)
}

// addClaude is `agentry model-signins add claude [--installed]`.
func addClaude(cmd *cobra.Command, in *bufio.Reader, home string, installed bool) error {
	rec, err := signInClaude(cmd, in, home, installed)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), signedInJSON{Vendor: modelsignin.Claude, Kind: rec.Kind, Account: rec.Label, Models: []string{}})
	}
	who := ""
	if rec.Label != "" {
		who = " as " + rec.Label
	}
	where, how := "the Claude Code image", "agentry setup (provider anthropic, pay with claude)"
	if installed {
		where, how = "the Claude Code installed on this computer", "agentry setup (provider anthropic, pay with claude), then agentry run --adapter claude"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signed in to Claude%s in %s\n", who, where)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "to run on it: "+how)
	return nil
}

// endClaude is the store's EndClaude (claudesignin.SignIn.End).
func endClaude(errOut io.Writer, home string) func(context.Context, modelsignin.Record) (string, error) {
	return claudeSignIn(home).End(errOut)
}

// claudeOffer is what `agentry setup` offers of the Claude sign-in: the
// image's, the default adapter's (D23).
func claudeOffer(cmd *cobra.Command, in *bufio.Reader, home string) setup.ClaudeOffer {
	return setup.ClaudeOffer{
		Kept: func() string { return claudesignin.Kept(home) },
		SignIn: func() (string, error) {
			rec, err := signInClaude(cmd, in, home, false)
			if err != nil {
				return "", err
			}
			return rec.Label, nil
		},
	}
}
