package main

import (
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// creditsClient is the credits-session client for a policy whose model
// is paid with Agentry credits (provider agentry): the registry its
// model endpoint is under, as the account this computer is signed in to
// there. Nil for any other provider, and for a registry this computer
// has no sign-in for (engine.InferenceFor then says to sign in). The
// sign-in is read at each call, so a renewal an hour on uses the sign-in
// as it is then; it never reaches the run.
func creditsClient(pol *policy.Policy) *inference.Client {
	if pol == nil || pol.Model.Provider != policy.ProviderAgentry {
		return nil
	}
	reg, err := policy.AgentryRegistry(pol.Model)
	if err != nil {
		return nil
	}
	if tok, err := credentialFor(reg); err != nil || tok == "" {
		return nil
	}
	return inference.New(reg, func() (string, error) { return credentialFor(reg) }, httpClient)
}
