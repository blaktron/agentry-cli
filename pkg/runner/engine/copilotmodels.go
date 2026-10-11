package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The model list of a Copilot plan (plan model-sign-ins.md §4.1, research
// copilot-cli.md R8; agentry-cli#808): a short-lived container per list,
// which is a run's agent container in all but the gateway, behind a run's
// egress proxy. Copilot CLI asks GitHub for the list itself; nothing of
// Agentry's calls the Copilot API.
//
//   - The container: the Copilot image the policy pins, with the run's
//     argv, environment, read-only root, empty home, docker.limits and
//     runtime (adapter.CopilotListContainer, hardenedAgent), on an internal
//     network of its own, nothing mounted.
//   - The route out: the run's llm proxy (startProxy), allowing
//     api.github.com and the Copilot API host of the person's plan alone
//     (policy.CopilotProxyAllow, I2).
//   - The token: handed through Copilot CLI's token callback over its
//     stdio (adapter.ListCopilotModels), never in a variable, a file or a
//     mount (I3, I4).
//   - Its end: the list's containers and network carry a run's labels and
//     are removed when it returns, and by the sweep if it was killed.

// CopilotModelsTimeout bounds one list: the containers' start, Copilot
// CLI's own start, GitHub's two answers.
const CopilotModelsTimeout = 3 * time.Minute

// CopilotModelsParams is one Copilot model list's.
type CopilotModelsParams struct {
	// Policy supplies the Copilot image (images.harness's copilot-image),
	// docker.limits, docker.runtime and docker.resolvers.
	Policy *policy.Policy
	// CopilotPlan is the person's Copilot plan (policy.CopilotPlans), which
	// names the Copilot API host the proxy allows.
	CopilotPlan string
	// RunnerImage is the runner image, which the proxy runs.
	RunnerImage string
	// SignIn and RenewSignIn are the Copilot sign-in and its renewal, as a
	// run's (Params.ModelSignIn, Params.RenewModelSignIn; CopilotSignIn
	// reads them from the person's store).
	SignIn      *ModelSignInGrant
	RenewSignIn func(ctx context.Context) (ConnectionToken, error)
	// Stderr receives Copilot CLI's own stderr; nil discards it.
	Stderr io.Writer
}

// ListCopilotModels lists the models the signed-in Copilot plan offers,
// through Copilot CLI in a run's container (see above). The Copilot image
// must be on the machine already (setup builds it).
func ListCopilotModels(ctx context.Context, d dockerx.Client, p CopilotModelsParams) (models []adapter.CopilotModel, err error) {
	if p.Policy == nil || p.RunnerImage == "" {
		return nil, errors.New("copilot models: no policy or runner image")
	}
	if p.SignIn == nil || p.SignIn.Kind != policy.KeyKindCopilotSignIn || p.SignIn.Token.AccessToken == "" {
		return nil, errors.New("copilot models: no Copilot sign-in to list the models of (agentry model-signins add copilot)")
	}
	allow, err := policy.CopilotProxyAllow(p.CopilotPlan)
	if err != nil {
		return nil, err
	}
	h, ok := p.Policy.Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	if !ok {
		return nil, errors.New("copilot models: the policy pins no Copilot image (images.harness." + adapter.CopilotImageAdapterName + ")")
	}
	have, err := d.ImageExists(ctx, h.Ref)
	if err != nil {
		return nil, fmt.Errorf("copilot models: %w", err)
	}
	if !have {
		return nil, fmt.Errorf("copilot models: the Copilot image %s is not on this machine (agentry setup builds it)", h.Ref)
	}
	plan := &spec.Plan{
		Proxy:   spec.ProxySpec{Image: p.RunnerImage, Allow: allow},
		Harness: spec.HarnessSpec{Adapter: adapter.CopilotImageAdapterName, Agent: adapter.CopilotListContainer(h.Ref)},
	}
	if rt := p.Policy.Docker.Runtime; rt != "" {
		// Under a runtime as a run is, the proxy too where the policy names
		// the name servers it resolves through (compile's docker block).
		plan.Docker = &spec.DockerSpec{Runtime: rt, Resolvers: slices.Clone(p.Policy.Docker.Resolvers)}
	}
	params := Params{Plan: plan, Policy: p.Policy}

	ctx, cancel := context.WithTimeout(ctx, CopilotModelsTimeout)
	defer cancel()
	runID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	td := &teardown{d: d, runID: runID, stderr: io.Discard}
	defer td.run()
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// The token is handed out as a run's is: renewed below signInMinLife
	// or when Copilot CLI asks, and masked out of every error.
	rd := &runDirs{redact: newRedactor("", nil, []string{p.SignIn.Token.AccessToken})}
	toks := &signInTokens{cur: p.SignIn.Token, renew: p.RenewSignIn, rd: rd, stderr: stderr}
	if proxySandboxed(plan) {
		// The sandboxed proxy reads the run's own resolv.conf.
		dir, err := os.MkdirTemp("", "agentry-copilot-models-")
		if err != nil {
			return nil, err
		}
		td.add(func(context.Context) { _ = os.RemoveAll(dir) })
		rd.cfg = dir
		if err := os.MkdirAll(filepath.Join(dir, "gateway"), 0o755); err != nil { //nolint:gosec // read by the proxy's user
			return nil, err
		}
		if err := writeResolvConf(filepath.Join(dir, "gateway"), plan); err != nil {
			return nil, err
		}
	}
	labels := runLabels(runID)
	netName := networkName(runID)
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return nil, fmt.Errorf("copilot models: create the list's network: %w", err)
	}
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, netName) })
	td.networks = append(td.networks, netName)
	if err := startProxy(ctx, d, params, rd, td, runID, netName, labels); err != nil {
		return nil, err
	}
	var hosts []string
	if plan.Runtime() != "" {
		// A sandboxed agent does not reach docker's DNS: the proxy is pinned
		// by address, as for a run (sandboxHosts).
		ip, err := d.ContainerIP(ctx, containerName(runID, "proxy"), netName)
		if err != nil {
			return nil, fmt.Errorf("copilot models: pin the proxy for the sandboxed list: %w", err)
		}
		hosts = []string{spec.ProxyAlias + ":" + ip}
	}
	a := plan.Harness.Agent
	env := make(map[string]string, len(a.Env))
	for _, e := range a.Env {
		env[e.Name] = e.Value
	}
	name := containerName(runID, "models")
	runSpec := hardenedAgent(dockerx.RunSpec{
		Name:        name,
		Image:       a.Image,
		Entrypoint:  a.Entrypoint,
		Argv:        a.Argv,
		Env:         env,
		Network:     netName,
		Labels:      labels,
		Interactive: true,
		AutoRemove:  true,
		Runtime:     plan.Runtime(),
		ExtraHosts:  hosts,
	}, params)
	td.add(func(c context.Context) { _ = d.RemoveForce(c, name) })

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	exited := make(chan error, 1)
	go func() {
		code, rerr := d.RunForeground(ctx, runSpec, stdinR, stdoutW, stderr)
		_ = stdoutW.Close()
		_ = stdinR.Close()
		if rerr == nil && code != 0 {
			rerr = fmt.Errorf("the Copilot CLI container exited %d", code)
		}
		exited <- rerr
	}()
	models, err = adapter.ListCopilotModels(ctx, rwPipe{Reader: stdoutR, WriteCloser: stdinW}, toks.token)
	// Its stdin closed, Copilot CLI ends; a list that failed is not waited
	// on past its context.
	select {
	case rerr := <-exited:
		if err != nil && rerr != nil {
			err = fmt.Errorf("%w (%v)", err, rerr)
		}
	case <-ctx.Done():
	}
	if err != nil {
		var refused *adapter.OrgPolicyRefusal
		if errors.As(err, &refused) {
			// Kept for what it is: an organization's policy, the setup's
			// answer to say (D29); the vendor's words masked.
			return nil, &adapter.OrgPolicyRefusal{Policy: refused.Policy, Text: rd.redact.String(refused.Text)}
		}
		return nil, rd.redact.Err(fmt.Errorf("copilot models: %w", err))
	}
	return models, nil
}

// CopilotSignIn is the person's Copilot sign-in from their store, for
// CopilotModelsParams: as a run on a Copilot plan is given it
// (ModelSignInFor). No sign-in is modelsignin.ErrNoSignIn, wrapped.
func CopilotSignIn(ctx context.Context, store *modelsignin.Store) (*ModelSignInGrant, func(context.Context) (ConnectionToken, error), error) {
	return ModelSignInFor(ctx, store, &spec.Plan{Harness: spec.HarnessSpec{ModelSignIn: &spec.ModelSignInSpec{
		Kind: policy.KeyKindCopilotSignIn, Provider: policy.ProviderCopilot, Delivery: spec.ModelSignInCallback,
	}}}, false)
}
