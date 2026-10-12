// Model sign-ins (plan model-sign-ins.md §4.1, §4.2; agentry-cli#738): a
// person's own grant that lets their local runs spend a model vendor's
// plan. `agentry model-signins` lists and removes them, apart from
// `agentry connections` (MCP servers' grants). A run whose plan spends one
// is handed the access token alone, refreshed now and before it expires;
// a hosted run never spends one (D2).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// hostedRun says this invocation is a hosted run's: a host hands it a run
// grant, the connections it fetched, or drives a session by protocol. Each
// is a hosted driver's alone (agentry-enterprise's runexec and runhost).
func hostedRun(opts runOpts) bool {
	return os.Getenv(envRunGrant) != "" || opts.connectionsFile != "" || opts.sessionProtocol
}

// hostedProcess says this process runs for a sandbox host: it holds a run
// grant or a hosted job's credential, which only a host hands it. A
// built-in run in it never spends a model sign-in (I5; agentry-cli#845).
func hostedProcess() bool {
	return os.Getenv(envRunGrant) != "" || os.Getenv(envJobToken) != "" || os.Getenv(envHosted) != ""
}

// envHosted marks a hosted run's CLI whatever else it was given: a public
// release's run with no inputs carries neither a grant nor a job token
// (agentry-enterprise#534), so its host says so outright.
const envHosted = "AGENTRY_HOSTED"

// refuseSignInRun refuses a plan that spends a model sign-in where it
// cannot run, before anything is installed for it: a hosted run (I5), and
// a --max-cost the run could not measure (D13). Nil for a key plan.
func refuseSignInRun(plan *spec.Plan, opts runOpts) error {
	msi := plan.Harness.ModelSignIn
	switch {
	case msi == nil:
		return nil
	case hostedRun(opts):
		return hostedSignInRefusal(msi.Kind)
	case opts.maxCost > 0:
		return engine.Classify(engine.ExitUsage, errors.New("--max-cost cannot apply: "+policy.MaxCostSignInSentence))
	}
	return nil
}

// hostedSignInRefusal is the refusal of a model sign-in for a hosted run.
func hostedSignInRefusal(kind string) error { return engine.HostedSignInRefusal(kind) }

// runModelSignIn is the model sign-in a plan spends, from the person's
// store, for engine.Params (engine.ModelSignInFor): nil when the plan
// spends a key; a hosted run is refused before the store is read (I5).
func runModelSignIn(ctx context.Context, home string, plan *spec.Plan, hosted bool) (*engine.ModelSignInGrant, func(context.Context) (engine.ConnectionToken, error), error) {
	return engine.ModelSignInFor(ctx, modelsignin.Open(home, connectionKeys()), plan, hosted)
}

func newModelSignInsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model-signins",
		Short: "Add, list and remove the model sign-ins your local runs can spend (your own ChatGPT, Claude or Copilot plan, or a connected key)",
		Long: "A model sign-in lets the runs on this computer spend a plan you hold instead of an API key.\n" +
			"Any number are kept; model setup (agentry setup) names the one in use, and switching signs\n" +
			"nothing out. A hosted run never spends one. These are not MCP connections (agentry connections).",
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
				state := "signed in"
				if r.Connected {
					state = "connected"
				}
				if r.Broken != "" {
					state = "broken: " + r.Broken
				}
				rows = append(rows, modelSignInOut{Vendor: r.Vendor, Kind: r.Kind, Connected: r.Connected, Account: r.Label,
					State: state, CreatedAt: r.CreatedAt, LastUsed: r.LastUsedAt, KeptIn: r.SecretIn, App: r.App, KeyExpiresAt: r.KeyExpiresAt})
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no model sign-ins kept (agentry setup offers them)")
				return nil
			}
			for _, r := range rows {
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
				_, _ = fmt.Fprintf(out, "%s%s%s · %s · %s%s · last used: %s\n", r.Vendor, acct, via, r.Kind, r.State, expires, last)
			}
			return nil
		},
	}
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
			s := connectStore(home)
			// A Claude sign-in is Claude Code's, in its own directories:
			// Claude Code logs out there, then they are deleted.
			s.EndClaude = endClaude(cmd.ErrOrStderr(), home)
			outcome, err := s.Remove(cmd.Context(), args[0], "removed by the person")
			if errors.Is(err, modelsignin.ErrNone) {
				return fmt.Errorf("no %s sign-in is kept on this computer", args[0])
			}
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), removedModelSignIn{Vendor: args[0], Revocation: outcome})
			}
			if modelsignin.IsConnectVendor(args[0]) {
				// The key is gone from the keychain: no sealed copy outlives it.
				resealSignedOut(cmd.ErrOrStderr())
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "disconnected your %s account (revocation: %s)\n", modelsignin.ConnectName(args[0]), outcome)
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed the %s sign-in (revocation: %s)\n", args[0], outcome)
			if args[0] == modelsignin.Copilot && outcome == modelsignin.RevokeSubmitted {
				// GitHub takes the tokens for revocation; the app's authorization,
				// which only a holder of its client secret could revoke, stays.
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "GitHub accepted the sign-in's tokens for revocation; Agentry's app stays in your authorized apps until you remove it there: https://github.com/settings/applications")
			}
			return nil
		},
	}
}
