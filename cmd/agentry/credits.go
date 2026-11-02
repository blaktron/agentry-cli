package main

import (
	"os"

	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// envRunhostCreditsToken carries a remote runhost's credits-only token from
// its service to the run it starts. That token can open a credits session
// and nothing else, so the run uses it for that alone, never as a sign-in.
const envRunhostCreditsToken = "AGENTRY_RUNHOST_CREDITS_TOKEN"

// creditsClient is the credits-session client for a policy whose model
// is paid with Agentry credits (provider agentry): the registry its
// model endpoint is under, as the account this computer is signed in to
// there, or, in a run a remote runhost started, with the runhost's
// credits-only token. Nil for any other provider, and for a registry this
// computer has no sign-in for (engine.InferenceFor then says to sign in).
// The sign-in is read at each call, so a renewal an hour on uses the
// sign-in as it is then; it never reaches the run.
func creditsClient(pol *policy.Policy) *inference.Client {
	if pol == nil || pol.Model.Provider != policy.ProviderAgentry {
		return nil
	}
	reg, err := policy.AgentryRegistry(pol.Model)
	if err != nil {
		return nil
	}
	if tok := os.Getenv(envRunhostCreditsToken); tok != "" {
		return inference.New(reg, func() (string, error) { return tok, nil }, httpClient)
	}
	if tok, err := credentialFor(reg); err != nil || tok == "" {
		return nil
	}
	return inference.New(reg, func() (string, error) { return credentialFor(reg) }, httpClient)
}
