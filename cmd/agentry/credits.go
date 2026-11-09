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
		return inference.New(reg, func() (string, error) { return tok, nil }, httpClient).
			WithSignInAdvice(runhostCreditsAdvice(runhostOwnName()))
	}
	// On a runhost, what runs there under its service (a persistent agent,
	// the save check) pays with the runhost's own credits token.
	if onRunhostServer() {
		return runhostCreditsClient(pol.Model)
	}
	if tok, err := credentialFor(reg); err != nil || tok == "" {
		return nil
	}
	return inference.New(reg, func() (string, error) { return credentialFor(reg) }, httpClient)
}

// runhostCreditsAdvice is what a run a remote runhost started says when the
// registry refuses the runhost's credits token: signing in again on a device
// changes nothing, since the runhost pays with a token of its own.
func runhostCreditsAdvice(name string) string {
	return "this runhost's credits token is revoked or expired: agentry runhost model " + name + " --provider agentry gives it a new one"
}

// runhostCreditsClient is a credits client paying with the runhost's own
// credits token for m's registry; nil when the runhost keeps none for it.
func runhostCreditsClient(m policy.Model) *inference.Client {
	reg, err := policy.AgentryRegistry(m)
	if err != nil {
		return nil
	}
	if _, err := runhostJobCredits(m); err != nil {
		return nil
	}
	return inference.New(reg, func() (string, error) { return runhostJobCredits(m) }, httpClient).
		WithSignInAdvice(runhostCreditsAdvice(runhostOwnName()))
}
