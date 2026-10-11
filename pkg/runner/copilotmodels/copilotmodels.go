// Package copilotmodels lists the models a person's Copilot plan offers
// (plan model-sign-ins.md §4.1, research copilot-cli.md R8;
// agentry-cli#808), for `agentry setup` and the desktop's model setup, so
// there is one implementation per machine: the Copilot sign-in kept on this
// computer, the images a Copilot run needs made present as setup makes them,
// then Copilot CLI's own list in a run's container
// (engine.ListCopilotModels). Nothing of Agentry's calls the Copilot API.
package copilotmodels

import (
	"context"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Request is one list's.
type Request struct {
	// Home is the runner home, whose model sign-in store keeps the Copilot
	// sign-in; Keys its keychain (the sign-in's refresh token).
	Home string
	Keys connection.Keychain
	// Policy supplies the images, docker.limits and docker.runtime; its
	// model block is not read.
	Policy *policy.Policy
	// CopilotPlan is the person's Copilot plan (policy.CopilotPlans).
	CopilotPlan string
	// RunnerImage is the runner image's tag and ImageCtxDir where its
	// context is unpacked to build it, as setup builds it.
	RunnerImage, ImageCtxDir string
	// Log receives the image builds' lines and Copilot CLI's stderr; nil
	// discards them.
	Log io.Writer
}

// List returns the models the Copilot plan offers the signed-in account,
// in Copilot CLI's order. No Copilot sign-in kept here is
// modelsignin.ErrNoSignIn, wrapped; an organization's policy that turns
// Copilot CLI off for the account is an *adapter.OrgPolicyRefusal.
func List(ctx context.Context, d dockerx.Client, r Request) ([]adapter.CopilotModel, error) {
	log := r.Log
	if log == nil {
		log = io.Discard
	}
	grant, renew, err := engine.CopilotSignIn(ctx, modelsignin.Open(r.Home, r.Keys))
	if err != nil {
		return nil, err
	}
	// The proxy runs the runner image; the list, the Copilot image.
	if err := gateway.EnsureInstalled(ctx, d, r.Policy, r.RunnerImage, r.ImageCtxDir, log, false); err != nil {
		return nil, err
	}
	if h, ok := r.Policy.Images.HarnessImageFor(adapter.CopilotImageAdapterName); ok {
		if err := gateway.EnsureHarnessImage(ctx, d, adapter.CopilotImageAdapterName, h, log); err != nil {
			return nil, err
		}
	}
	return engine.ListCopilotModels(ctx, d, engine.CopilotModelsParams{
		Policy: r.Policy, CopilotPlan: r.CopilotPlan, RunnerImage: r.RunnerImage,
		SignIn: grant, RenewSignIn: renew, Stderr: log,
	})
}
