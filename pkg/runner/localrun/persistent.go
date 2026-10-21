package localrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/coder"
	mainagent "github.com/blaktron/agentry-cli/pkg/persistent/main"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Assistant is the Assistant (pkg/persistent/assistant), the first built-in
// that runs as a persistent agent. A file in its share is read only once
// ClamAV has checked it, and its app link is self_configure alone.
func Assistant() Builtin {
	return Builtin{namespace: assistant.Namespace, name: assistant.Name, version: assistant.Version, fsys: assistant.FS(),
		persistent: true, runnable: true, checkShare: true, appLink: applink.SetAssistant}
}

// Main is Agentry Helper (pkg/persistent/main), the persistent agent of Agentry
// Desktop's Home: its app link is the application's own operations, and a
// file in its share is read only once ClamAV has checked it.
func Main() Builtin {
	return Builtin{namespace: mainagent.Namespace, name: mainagent.Name, version: mainagent.Version, fsys: mainagent.FS(),
		persistent: true, runnable: true, checkShare: true, appLink: applink.SetMain}
}

// Coder is the Coding agent (pkg/persistent/coder): its programs run in its
// own toolchain image, it may reach the internet, a browser and the person's
// dev server, each as its roster has them on, and it reads its share as it
// is: it builds and runs software.
func Coder() Builtin {
	return Builtin{namespace: coder.Namespace, name: coder.Name, version: coder.Version, fsys: coder.FS(),
		persistent: true, runnable: true, coder: true}
}

// PersistentBuiltins are the built-ins that run as persistent agents.
func PersistentBuiltins() []Builtin { return []Builtin{Main(), Assistant(), Coder()} }

// Codes says the persistent built-in is given the Coding agent's grant
// (Grants.Coder).
func (b Builtin) Codes() bool { return b.coder }

// Runnable says a persistent built-in runs as an agent; one that is not is
// published for reading alone.
func (b Builtin) Runnable() bool { return b.runnable }

// AppLink is the persistent built-in's app link tool set; empty for none.
func (b Builtin) AppLink() string { return b.appLink }

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
	// AppLink is the socket the supervisor answers the agent's app link on;
	// empty gives the segment no app link.
	AppLink string
	// Coder is what the Coding agent's roster has on of its grant; nil for
	// any other agent, which is refused it.
	Coder *spec.CoderGrant
	// DevHost is the Coding agent's dev server, readied (its address, its
	// pinned host key, its sign-in): set exactly when Coder.DevHost is.
	DevHost *targethost.Host
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
	plan, meta, workspace, installDir, ad := sp.Plan, sp.meta, sp.workspace, sp.installDir, sp.adapter
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
	if err := sp.installServers(ctx, d, s.Grants.ServersDir); err != nil {
		return nil, err
	}
	// The install can move the plan to one compiled with the commands it
	// found (InstallServers).
	plan, planDir := sp.Plan, sp.Dir
	seg := s.Persistent
	seg.Share, seg.AppLink = s.Grants.Share, s.Grants.AppLink
	var answers *intake.Answers
	if h := s.Grants.DevHost; h != nil {
		answers = &intake.Answers{Hosts: map[string]*targethost.Host{spec.DevHostID: h}}
	}
	sess, err := engine.StartSession(ctx, d, engine.Params{
		Intake: answers,
		Plan:   plan, PlanDir: planDir, WorkspaceSrc: workspace, RunRoot: installDir, RunnerHome: s.Home,
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

// appLinkSet is the app link tool set the segment's plan has: the
// built-in's, when the segment is given a socket for it.
func (s Segment) appLinkSet() string {
	if s.Grants.AppLink == "" {
		return ""
	}
	return s.Builtin.appLink
}

// coderGrant is the Coding agent's grant the segment's plan has: the
// roster's, for the Coding agent alone, with its dev server readied exactly
// when the grant names it.
func (s Segment) coderGrant() (*spec.CoderGrant, error) {
	g := s.Grants.Coder
	switch {
	case g == nil && s.Grants.DevHost == nil:
		return nil, nil
	case !s.Builtin.coder:
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("%s is not given the Coding agent's grants", s.Builtin.Ref()))
	case g == nil || g.DevHost != (s.Grants.DevHost != nil):
		return nil, engine.Classify(engine.ExitCLI, errors.New("the Coding agent's dev server is given exactly when its roster has it on, readied"))
	}
	out := *g
	return &out, nil
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
	// inputs are what Plan was compiled from: the install compiles it
	// again from them when it changes a server's command.
	inputs PlanInputs
}

// installServers installs the segment's package servers into the agent's
// store and moves Plan and Dir to the plan compiled with the commands the
// install found (InstallServers).
func (sp *SegmentPlan) installServers(ctx context.Context, d dockerx.Client, storeDir string) error {
	plan, dir, err := InstallServers(ctx, d, sp.inputs, sp.Plan, sp.Dir, sp.resolved, storeDir)
	if err != nil {
		return err
	}
	sp.Plan, sp.Dir = plan, dir
	return nil
}

// Plan materializes the built-in and compiles the segment's plan from it and
// the roster's grants: the servers the person has on are declared beside the
// built-in's own, and its folders are mounted, the share when it is on. It
// touches no container engine; a package server is resolved from the agent's
// store, or from the registries when the store does not hold its pin.
func (s Segment) Plan(ctx context.Context, logw io.Writer) (*SegmentPlan, error) {
	if !s.Builtin.persistent || !s.Builtin.runnable {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("%s is not a persistent agent Agentry runs", s.Builtin.Ref()))
	}
	coderGrant, err := s.coderGrant()
	if err != nil {
		return nil, err
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
	in := PlanInputs{
		Home: s.Home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspace,
		Policy: s.Policy, PolicySHA: s.PolicySHA, Adapter: adapterName, Mode: manifest.ModePersistent,
		Servers: servers, AgentFiles: s.agentFiles(), AppLink: s.appLinkSet(), Coder: coderGrant, CompiledBy: s.Build, Log: logw,
	}
	plan, planDir, err := EnsurePlan(in)
	if err != nil {
		return nil, err
	}
	if plan.Harness.Host != nil {
		return nil, engine.Classify(engine.ExitPolicy, errors.New("a persistent agent runs its harness in a container, and this plan runs it on the host"))
	}
	return &SegmentPlan{Plan: plan, Dir: planDir, meta: meta, workspace: workspace, installDir: installDir, resolved: resolved, adapter: ad, inputs: in}, nil
}
