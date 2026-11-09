// Model sign-ins: a person's own grant that lets their local runs spend a
// model vendor's plan. `agentry providers` lists, adds and removes them,
// apart from `agentry connections` (MCP servers' grants), and the
// deprecated `agentry model-signins` still does under its own name. A run whose plan
// spends one is handed the access token alone, refreshed now and before it
// expires; a hosted run never spends one.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// hostedRun says this invocation is a hosted run's: a host hands it a run
// grant, the connections it fetched, or drives a session by protocol. Each
// is a hosted driver's alone.
func hostedRun(opts runOpts) bool {
	return os.Getenv(envRunGrant) != "" || opts.connectionsFile != "" || opts.sessionProtocol
}

// hostedProcess says this process runs for a sandbox host: it holds a run
// grant or a hosted job's credential, which only a host hands it. A
// built-in run in it never spends a model sign-in.
func hostedProcess() bool {
	return os.Getenv(envRunGrant) != "" || os.Getenv(envJobToken) != "" || os.Getenv(envHosted) != ""
}

// hostedFor says a run is a sandbox host's by any of its signs: a hosted
// driver's (hostedRun), the process's (hostedProcess), or input review
// through the registry, which only a host asks for. A target host is
// refused and a browser held to public sites by it.
func hostedFor(opts runOpts) bool {
	return hostedRun(opts) || hostedProcess() || opts.reviewInputs != ""
}

// envHosted marks a hosted run's CLI whatever else it was given: a public
// release's run with no inputs carries neither a grant nor a job token,
// so its host says so outright.
const envHosted = "AGENTRY_HOSTED"

// refuseSignInRun refuses a plan that spends a model sign-in where it
// cannot run, before anything is installed for it: a hosted run, and
// a --max-cost the run could not measure. Nil for a key plan.
func refuseSignInRun(plan *spec.Plan, opts runOpts) error {
	msi := plan.Harness.ModelSignIn
	switch {
	case msi == nil:
		return nil
	case hostedRun(opts):
		return hostedSignInRefusal(msi.Kind)
	case opts.maxCost > 0:
		return engine.Classify(engine.ExitUsage, errors.New("--max-cost cannot apply: "+policy.MaxCostSignInSentence))
	case opts.maxCost < 0 && plan.Budgets.MaxCostUSD > 0:
		// The policy's cost budget holds for every saved model a run is on,
		// and a sign-in has no dollars to hold it to.
		what := "this run's model"
		if sm := plan.Harness.SavedModel; sm != nil {
			what = fmt.Sprintf("the saved model %q", sm.Name)
		}
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("the policy's budgets.maxCostUsd cannot apply to %s, which spends %s: %s", what, policy.SignInSentence(msi.Kind), policy.MaxCostSignInSentence))
	}
	return nil
}

// hostedSignInRefusal is the refusal of a model sign-in for a hosted run.
func hostedSignInRefusal(kind string) error { return engine.HostedSignInRefusal(kind) }

// runModelSignIn is the model sign-in a plan spends, from the person's
// store, for engine.Params (engine.ModelSignInFor): nil when the plan
// spends a key; a hosted run is refused before the store is read.
func runModelSignIn(ctx context.Context, home string, plan *spec.Plan, hosted bool) (*engine.ModelSignInGrant, func(context.Context) (engine.ConnectionToken, error), error) {
	return engine.ModelSignInFor(ctx, modelsignin.Open(home, connectionKeys()), plan, hosted)
}

// newModelSignInsCmd is the deprecated alias of providers' sign-in half,
// kept for the scripts that call it. It is hidden from the help, and each
// use says on stderr which providers command replaces it. Cobra's own
// Deprecated note is not used: it is written to the command's output
// writer whenever one is set, which is where --json goes.
func newModelSignInsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "model-signins",
		Hidden: true,
		PersistentPreRun: func(c *cobra.Command, args []string) {
			on := map[string]bool{}
			c.Flags().Visit(func(f *pflag.Flag) { on[f.Name] = true })
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "Command \"model-signins\" is deprecated: use %s instead; model-signins will be removed in a later release\n", providersCommandFor(c.Name(), args, on))
		},
		Short: "Add, list and remove the model sign-ins your local runs can spend (your own ChatGPT, Claude or Copilot plan, or a connected key)",
		Long: "A model sign-in lets the runs on this computer spend a plan you hold instead of an API key.\n" +
			"Any number are kept; model setup (agentry setup) names the one in use, and switching signs\n" +
			"nothing out. A hosted run never spends one. These are not MCP connections (agentry connections).\n" +
			"Deprecated: agentry providers list, add and remove do all of this, and model-signins will be removed\n" +
			"in a later release.",
	}
	cmd.AddCommand(newModelSignInsAddCmd(), newModelSignInsListCmd(), newModelSignInsRemoveCmd())
	return cmd
}

// modelSignInOut is one row of `model-signins list --json`: never a token.
type modelSignInOut struct {
	Vendor    string `json:"vendor"`
	Kind      string `json:"kind"`
	Connected bool   `json:"connected,omitempty"`
	Account   string `json:"account,omitempty"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsedAt,omitempty"`
	KeptIn    string `json:"keptIn,omitempty"`
	// App is which of Agentry's GitHub apps a Copilot sign-in went through.
	App string `json:"app,omitempty"`
	// KeyExpiresAt is when a connected key that cannot be renewed expires.
	KeyExpiresAt string `json:"keyExpiresAt,omitempty"`
}

func newModelSignInsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List kept model sign-ins (never a token)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			list, err := modelsignin.Open(home, nil).List()
			if err != nil {
				return err
			}
			rows := make([]modelSignInOut, 0, len(list))
			for _, r := range list {
				rows = append(rows, modelSignInRow(*r))
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"modelSignIns": rows})
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no model sign-ins kept (agentry setup offers them)")
				return nil
			}
			for _, r := range rows {
				_, _ = fmt.Fprintln(out, signInLine(r))
			}
			return nil
		},
	}
}

// modelSignInRow is a kept sign-in as listed: never a token.
func modelSignInRow(r modelsignin.Record) modelSignInOut {
	state := "signed in"
	if r.Connected {
		state = "connected"
	}
	if r.Broken != "" {
		state = "broken: " + r.Broken
	}
	return modelSignInOut{Vendor: r.Vendor, Kind: r.Kind, Connected: r.Connected, Account: r.Label,
		State: state, CreatedAt: r.CreatedAt, LastUsed: r.LastUsedAt, KeptIn: r.SecretIn, App: r.App, KeyExpiresAt: r.KeyExpiresAt}
}

// signInLine is a kept sign-in in words.
func signInLine(r modelSignInOut) string {
	last := r.LastUsed
	if last == "" {
		last = "never used"
	}
	acct := ""
	if r.Account != "" {
		acct = " as " + r.Account
	}
	via := ""
	if r.App != "" {
		via = " (through Agentry's " + mcpauth.GitHubAppName(r.App) + ")"
	}
	expires := ""
	if r.KeyExpiresAt != "" {
		expires = " · key expires " + r.KeyExpiresAt
	}
	return fmt.Sprintf("%s%s%s · %s · %s%s · last used: %s", r.Vendor, acct, via, r.Kind, r.State, expires, last)
}

// providersCommandFor is the providers command that does what
// `model-signins <sub> <args>` does, with on its flags given.
func providersCommandFor(sub string, args []string, on map[string]bool) string {
	vendor := ""
	if len(args) > 0 {
		vendor = strings.ToLower(args[0])
	}
	switch sub {
	case "list":
		return "agentry providers list"
	case "add":
		if modelsignin.ProviderOfVendor(vendor) == "" {
			return "agentry providers add"
		}
		c := modelsignin.SignInCommand(vendor)
		for _, f := range []string{"new-account", "installed", "oauth-app"} {
			if on[f] {
				c += " --" + f
			}
		}
		return c
	case "remove":
		if p := modelsignin.ProviderOfVendor(vendor); p != "" {
			return "agentry providers remove " + p + " " + signInOnlyFlag(vendor)
		}
		return "agentry providers remove <provider id>"
	}
	return "agentry providers"
}

// removedModelSignIn is `model-signins remove --json`.
type removedModelSignIn struct {
	Vendor     string `json:"vendor"`
	Revocation string `json:"revocation"`
}

func newModelSignInsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <vendor>",
		Short: "Remove a model sign-in or disconnect an account (revoked at the vendor where this build can; for claude, Claude Code logs out and its directory is deleted; a connected key is forgotten, with where to delete it at the vendor)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			outcome, err := removeSignIn(cmd, home, args[0])
			if errors.Is(err, modelsignin.ErrNone) {
				return fmt.Errorf("no %s sign-in is kept on this computer", args[0])
			}
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), removedModelSignIn{Vendor: args[0], Revocation: outcome})
			}
			sayRemovedSignIn(cmd.OutOrStdout(), args[0], outcome)
			return nil
		},
	}
}
