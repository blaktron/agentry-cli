package localrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Assistant is the Assistant (pkg/persistent/assistant), the first built-in
// that runs as a persistent agent.
func Assistant() Builtin {
	return Builtin{namespace: assistant.Namespace, name: assistant.Name, version: assistant.Version, fsys: assistant.FS(), persistent: true}
}

// PersistentBuiltins are the built-ins that run as persistent agents.
func PersistentBuiltins() []Builtin { return []Builtin{Assistant()} }

// Persistent says the built-in runs as a persistent agent: only a Segment
// starts it, and Execute refuses it.
func (b Builtin) Persistent() bool { return b.persistent }

// Segment is one start of a persistent built-in: the session a persistent
// agent's supervisor owns until it stops it.
type Segment struct {
	Builtin    Builtin
	Home       string
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	// Build is the client's build identity, recorded in the receipt's
	// attestation and the plan.
	Build string
	// OutDir is the segment's folder (its receipts and envelope), absolute.
	OutDir string
	RunID  string
	Stderr io.Writer
	// Persistent is the agent's part of the engine's parameters: its name,
	// the segment's number, its volumes and the conversation to continue.
	Persistent engine.PersistentSegment
}

// Start starts the segment: materialize, validate, plan the persistent mode,
// ensure the images, start the session. A persistent agent always runs its
// harness in a container: a host adapter is replaced by its image
// counterpart, and a plan that would run the harness on the host is refused.
// It has no session caps and no tool-call or cost budget: the person stops
// it.
func (s Segment) Start(ctx context.Context, d dockerx.Client) (engine.PersistentSession, error) {
	if !s.Builtin.persistent {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("%s is not a persistent agent", s.Builtin.Ref()))
	}
	logw := s.Stderr
	if logw == nil {
		logw = io.Discard
	}
	meta, installDir, err := s.Builtin.Materialize(s.Home, s.Build)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	workspace := filepath.Join(installDir, "workspace")
	m, err := ManifestFrom(workspace)
	if err != nil {
		return nil, engine.Classify(engine.ExitCompile, err)
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		msgs := make([]string, 0, len(blocking))
		for _, e := range blocking {
			msgs = append(msgs, e.Error())
		}
		return nil, engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid:\n  %s", meta.Ref, strings.Join(msgs, "\n  ")))
	}
	adapterName := ImageAdapter(adapter.DefaultFor(s.Policy))
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	plan, planDir, err := EnsurePlan(PlanInputs{
		Home: s.Home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspace,
		Policy: s.Policy, PolicySHA: s.PolicySHA, Adapter: adapterName, Mode: manifest.ModePersistent,
		CompiledBy: s.Build, Log: logw,
	})
	if err != nil {
		return nil, err
	}
	if plan.Harness.Host != nil {
		return nil, engine.Classify(engine.ExitPolicy, errors.New("a persistent agent runs its harness in a container, and this plan runs it on the host"))
	}
	signIns := modelsignin.Open(s.Home, connection.OSKeychain{})
	if err := signIns.ReadyForRun(ctx, plan); err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	signIn, renewSignIn, err := engine.ModelSignInFor(ctx, signIns, plan, false)
	if err != nil {
		return nil, err
	}
	runnerTag := s.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(s.Home, "image")
	if err := gateway.EnsureInstalledFor(ctx, d, s.Policy, runnerTag, ctxDir, logw, plan); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	var execPackages []string
	if plan.ExecImage != nil {
		if execPackages, err = gateway.EnsureExecImage(ctx, d, runnerTag, *plan.ExecImage, ctxDir, logw); err != nil {
			return nil, engine.Classify(engine.ExitGateway, err)
		}
	}
	seg := s.Persistent
	sess, err := engine.StartSession(ctx, d, engine.Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: workspace, RunRoot: installDir, RunnerHome: s.Home,
		OutDir: s.OutDir, TextInputs: true,
		Adapter: ad, Policy: s.Policy, PolicyFile: s.PolicyFile, Budgets: spec.Budgets{},
		Attestation: meta.Attestation, ExecPackages: execPackages,
		CheckWorkspace: builtinCopyCheck(meta.Digest),
		RunID:          s.RunID, Stderr: logw,
		ModelSignIn: signIn, RenewModelSignIn: renewSignIn,
		Persistent: &seg,
	})
	if err != nil {
		return nil, err
	}
	ps, ok := sess.(engine.PersistentSession)
	if !ok {
		_, _ = sess.End(context.WithoutCancel(ctx), false)
		return nil, engine.Classify(engine.ExitCLI, errors.New("the engine's session is not a persistent agent's segment"))
	}
	return ps, nil
}

// ImageAdapter is the adapter a persistent agent runs on for the machine's
// adapter: the same harness in a container, for one that would run it on the
// host.
func ImageAdapter(name string) string {
	switch name {
	case adapter.ClaudeAdapterName:
		return adapter.ClaudeImageAdapterName
	case adapter.CodexAdapterName:
		return adapter.CodexImageAdapterName
	}
	return name
}
