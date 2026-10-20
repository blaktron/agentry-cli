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
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Assistant is the Assistant (pkg/persistent/assistant), the first built-in
// that runs as a persistent agent. A file in its share is read only once
// ClamAV has checked it.
func Assistant() Builtin {
	return Builtin{namespace: assistant.Namespace, name: assistant.Name, version: assistant.Version, fsys: assistant.FS(), persistent: true, checkShare: true}
}

// PersistentBuiltins are the built-ins that run as persistent agents.
func PersistentBuiltins() []Builtin { return []Builtin{Assistant()} }

// Persistent says the built-in runs as a persistent agent: only a Segment
// starts it, and Execute refuses it.
func (b Builtin) Persistent() bool { return b.persistent }

// ChecksShare says a file in the persistent built-in's share folder is read
// only once ClamAV has checked it; otherwise the share is read as it is.
func (b Builtin) ChecksShare() bool { return b.checkShare }

// Grants are what a persistent agent's roster turns on for one segment,
// beside what its built-in declares: the person's share folder, the
// knowledge packs it searches, and the MCP servers the person added. The
// persistent package's Apply is the one function that makes them.
type Grants struct {
	// Share is the share folder, absolute; empty when the agent has it off.
	Share string
	// Knowledge are the packs attached to the segment, opened by the
	// caller.
	Knowledge []engine.AttachedPack
	// Servers are the MCP servers the person added and has on, in the
	// manifest's shape.
	Servers []manifest.MCPServer
	// ServersDir is the agent's own store a package server is installed
	// into, as a release's declared server is into its profile's.
	ServersDir string
}

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
	// Grants are what the agent's roster turns on (Apply).
	Grants Grants
}

// Start starts the segment: materialize, validate, plan the persistent mode,
// ensure the images, start the session. A persistent agent always runs its
// harness in a container: a host adapter is replaced by its image
// counterpart, and a plan that would run the harness on the host is refused.
// It has no session caps and no tool-call or cost budget: the person stops
// it.
func (s Segment) Start(ctx context.Context, d dockerx.Client) (engine.PersistentSession, error) {
	logw := s.Stderr
	if logw == nil {
		logw = io.Discard
	}
	sp, err := s.Plan(ctx, logw)
	if err != nil {
		return nil, err
	}
	plan, planDir, meta, workspace, installDir, resolved, ad := sp.Plan, sp.Dir, sp.meta, sp.workspace, sp.installDir, sp.resolved, sp.adapter
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
	if len(resolved) > 0 {
		if err := mcpinstall.EnsureInstalled(ctx, d, resolved, s.Grants.ServersDir, s.Policy.MCPRegistries, s.Policy.Images, s.Policy.Docker, logw); err != nil {
			return nil, engine.Classify(engine.ExitGateway, err)
		}
	}
	seg := s.Persistent
	seg.Share = s.Grants.Share
	sess, err := engine.StartSession(ctx, d, engine.Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: workspace, RunRoot: installDir, RunnerHome: s.Home,
		OutDir: s.OutDir, TextInputs: true, Knowledge: s.Grants.Knowledge, ServersDir: s.Grants.ServersDir,
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

// agentFiles are the folders the segment's plan mounts: the data volume,
// and the share when the roster has it on.
func (s Segment) agentFiles() *spec.AgentFiles {
	return &spec.AgentFiles{Share: s.Grants.Share != "", CheckShare: s.Grants.Share != "" && s.Builtin.checkShare}
}

// withServers is the built-in's manifest with the servers the person added
// and has on declared beside its own, so they compile, are consented and
// are held as a release's declared servers are. The built-in's own files
// are unchanged: the segment's receipt names what the roster added.
func (s Segment) withServers(m *manifest.Manifest) *manifest.Manifest {
	if len(s.Grants.Servers) == 0 {
		return m
	}
	out := *m
	out.Capabilities.MCPServers = append(append([]manifest.MCPServer(nil), m.Capabilities.MCPServers...), s.Grants.Servers...)
	return &out
}

// resolveServers resolves the package servers among the manifest's, from
// the agent's store when it already holds each pin, and returns them with
// the plan's server specs.
func (s Segment) resolveServers(ctx context.Context, m *manifest.Manifest, logw io.Writer) ([]mcpinstall.Resolved, []spec.MCPServerSpec, error) {
	if len(m.Capabilities.MCPServers) == 0 {
		return nil, nil, nil
	}
	if len(mcpinstall.Packages(m.Capabilities.MCPServers)) > 0 && s.Grants.ServersDir == "" {
		return nil, nil, engine.Classify(engine.ExitCLI, errors.New("the agent has a package server on, and no store to install it into"))
	}
	resolved, err := mcpinstall.StoredOrResolved(ctx, logw, s.Grants.ServersDir, s.Policy.MCPRegistries, m.Capabilities.MCPServers)
	if err != nil {
		return nil, nil, engine.Classify(engine.ExitVerify, err)
	}
	return resolved, mcpinstall.Specs(resolved, m.Capabilities.MCPServers, s.Policy.Images), nil
}

// SegmentPlan is a segment's compiled plan and what starting it needs.
type SegmentPlan struct {
	Plan *spec.Plan
	// Dir is the plan's folder: its gateway and harness files.
	Dir        string
	meta       *installdir.Meta
	workspace  string
	installDir string
	resolved   []mcpinstall.Resolved
	adapter    adapter.Adapter
}

// Plan materializes the built-in and compiles the segment's plan from it and
// the roster's grants: the servers the person has on are declared beside the
// built-in's own, and its folders are mounted, the share when it is on. It
// touches no container engine; a package server is resolved from the agent's
// store, or from the registries when the store does not hold its pin.
func (s Segment) Plan(ctx context.Context, logw io.Writer) (*SegmentPlan, error) {
	if !s.Builtin.persistent {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("%s is not a persistent agent", s.Builtin.Ref()))
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
	m = s.withServers(m)
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
	resolved, servers, err := s.resolveServers(ctx, m, logw)
	if err != nil {
		return nil, err
	}
	plan, planDir, err := EnsurePlan(PlanInputs{
		Home: s.Home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspace,
		Policy: s.Policy, PolicySHA: s.PolicySHA, Adapter: adapterName, Mode: manifest.ModePersistent,
		Servers: servers, AgentFiles: s.agentFiles(), CompiledBy: s.Build, Log: logw,
	})
	if err != nil {
		return nil, err
	}
	if plan.Harness.Host != nil {
		return nil, engine.Classify(engine.ExitPolicy, errors.New("a persistent agent runs its harness in a container, and this plan runs it on the host"))
	}
	return &SegmentPlan{Plan: plan, Dir: planDir, meta: meta, workspace: workspace, installDir: installDir, resolved: resolved, adapter: ad}, nil
}
