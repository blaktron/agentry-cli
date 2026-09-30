// Package localrun holds what a run needs besides the registry (plan
// knowledge.md §4.2, D6; agentry-cli#318): the plan's reuse or compile for
// any run, a release's or not (EnsurePlan, moved here from the CLI's main
// package, D-031's move for runs); the manifest and the tree digest a local
// package is read and keyed by; and the built-in run — a package this
// binary embeds, run with the attestation state builtin, no registry
// resolve and no consent prompt for a contract the application declares
// itself. The CLI's commands, the desktop and the enterprise build share it.
package localrun

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/knowledge/creator"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry-cli/runnerimage"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ManifestFrom parses agent.yaml at a package tree's root.
func ManifestFrom(dir string) (*manifest.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml")) //nolint:gosec // package root
	if err != nil {
		return nil, fmt.Errorf("read agent.yaml: %w", err)
	}
	m, err := manifest.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parse agent.yaml: %w", err)
	}
	return m, nil
}

// TreeDigest hashes a package directory deterministically (sorted relative
// paths, sizes and content, .git and agentry-out left out) so a local or a
// built-in package is cache-keyed and receipted like an archive, with
// "sha256:" kept for shape parity.
func TreeDigest(dir string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "agentry-out" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash local tree: %w", err)
	}
	sort.Strings(files)
	for _, rel := range files {
		b, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // hashing the named tree
		if rerr != nil {
			return "", fmt.Errorf("hash local tree: %w", rerr)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		_, _ = h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// LocalMeta is the install record of a directory package run with --local:
// unsigned by definition, attestation state local. It is computed, never
// read from a cached install, so no record on disk can give a directory
// another state.
func LocalMeta(dir string) (*installdir.Meta, error) {
	m, err := ManifestFrom(dir)
	if err != nil {
		return nil, err
	}
	digest, err := TreeDigest(dir)
	if err != nil {
		return nil, err
	}
	return &installdir.Meta{
		Ref:       fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
		Namespace: m.Namespace, Name: m.Name, Version: m.Version,
		Digest: digest, Source: "local",
		Attestation: receipt.Attestation{State: "local", Consent: receipt.ConsentAllowUnsigned},
	}, nil
}

// Builtin is a package this binary embeds. Its fields are unexported, so a
// Builtin is only ever one of Builtins(): nothing else — a directory, a
// release, a record on disk — can run with the attestation state builtin,
// and nothing else can be granted the web tools, which ride on the Builtin
// value itself (plan builder-rework.md D6, §7.3).
type Builtin struct {
	namespace, name, version string
	fsys                     fs.FS
	// web grants the web tools: the Drafter's alone.
	web bool
	// localTimeoutFloor is the least time, in seconds, a run of the package
	// on this computer is given (LocalBudgets).
	localTimeoutFloor int
}

// KnowledgeCreator is the Knowledge Creator (pkg/knowledge/creator).
func KnowledgeCreator() Builtin {
	return Builtin{namespace: creator.Namespace, name: creator.Name, version: creator.Version, fsys: creator.FS(),
		localTimeoutFloor: creator.LocalTimeoutFloorSeconds}
}

// Drafter is the Drafter (pkg/drafter), the second built-in package (plan
// builder-rework.md D6), and the one granted the web tools.
func Drafter() Builtin {
	return Builtin{namespace: drafter.Namespace, name: drafter.Name, version: drafter.Version, fsys: drafter.FS(), web: true,
		localTimeoutFloor: drafter.LocalTimeoutFloorSeconds}
}

// WithoutWebTools is the same package with no web tools: the Drafter's
// repair turn (plan builder-simplify.md §5.2 step 3), which reads nothing. A
// grant can only be taken away here, never given.
func (b Builtin) WithoutWebTools() Builtin {
	b.web = false
	return b
}

// LocalBudgets are the budgets a run of the package on this computer
// starts from (plan scan-and-run-tweaks.md §6.1, §6.2; agentry-cli#500,
// DR-54): the runner policy's, with a timeout no shorter than the package's
// own floor. This is the one place a run is given longer than the policy's
// run timeout without the person naming it: that timeout is sized for an
// agent answering a task, and a draft or a pack build outruns it. A caller
// lays whatever was named for this run over them. The floor is on the
// timeout alone: the tool-call and cost caps are the policy's, and no
// budget of a run on this computer counts tokens. It is exported so that
// the desktop can start a draft or a build from the same budgets
// (agentry-desktop#458).
func (b Builtin) LocalBudgets(p policy.Budgets) spec.Budgets {
	return spec.Budgets{TimeoutSeconds: max(p.TimeoutSeconds, b.localTimeoutFloor), MaxTurns: p.MaxTurns, MaxCostUSD: p.MaxCostUSD}
}

// Builtins is every package that may run as builtin.
func Builtins() []Builtin { return []Builtin{KnowledgeCreator(), Drafter()} }

// Ref is the package's ns/name@version.
func (b Builtin) Ref() string { return fmt.Sprintf("%s/%s@%s", b.namespace, b.name, b.version) }

// Namespace is the package's namespace, as its manifest declares it.
func (b Builtin) Namespace() string { return b.namespace }

// Name is the package's name.
func (b Builtin) Name() string { return b.name }

// Version is the package's version.
func (b Builtin) Version() string { return b.version }

// Stage copies the embedded tree into dir, which must not hold any of its
// files: the workspace `agentry builtins publish` uploads (agentry-cli#470).
func (b Builtin) Stage(dir string) error {
	if b.fsys == nil {
		return errors.New("not a built-in package")
	}
	if err := os.CopyFS(dir, b.fsys); err != nil {
		return fmt.Errorf("stage %s: %w", b.Ref(), err)
	}
	return nil
}

// ArchiveDigest is the digest of the archive PackWorkspace makes of the
// embedded tree: the release identity the registry holds for this package,
// since the registry publishes every built-in from these bytes and its own
// build must equal the digest the upload claims (agentry-cli#470). It is not
// the tree digest Materialize keys the runner home and the receipt by.
func (b Builtin) ArchiveDigest() (string, error) {
	stage, err := os.MkdirTemp("", "agentry-builtin-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := b.Stage(stage); err != nil {
		return "", err
	}
	m, err := ManifestFrom(stage)
	if err != nil {
		return "", err
	}
	info, err := PackWorkspace(stage, m, io.Discard)
	if err != nil {
		return "", fmt.Errorf("pack %s: %w", b.Ref(), err)
	}
	return info.Digest, nil
}

// PackWorkspace writes the deterministic archive of a package directory to
// w, filtered by the manifest's publish.include (plan §6.5, D-15), and
// returns its identity. `agentry publish` uploads exactly this, and a
// built-in's ArchiveDigest is this over its embedded tree, so the two cannot
// pack differently.
func PackWorkspace(dir string, m *manifest.Manifest, w io.Writer) (pkgarchive.ArchiveInfo, error) {
	files, err := pkgarchive.WalkWorkspace(dir)
	if err != nil {
		return pkgarchive.ArchiveInfo{}, fmt.Errorf("walk workspace %s: %w", dir, err)
	}
	include := manifest.ResolveInclude(m.Publish.Include, files)
	if len(include) == 0 {
		return pkgarchive.ArchiveInfo{}, fmt.Errorf("publish.include matches no files in %s (V4)", dir)
	}
	info, err := pkgarchive.BuildArchive(dir, include, w)
	if err != nil {
		return pkgarchive.ArchiveInfo{}, fmt.Errorf("build archive: %w", err)
	}
	return info, nil
}

// Materialize lays the embedded tree down in the runner home's release
// directory, keyed by its digest, and returns the record of it with the
// attestation state builtin and the build that carries it. The record is
// made in memory on every call and never read back; an installed copy whose
// tree is not the embedded one is replaced.
func (b Builtin) Materialize(home, build string) (*installdir.Meta, string, error) {
	if b.fsys == nil {
		return nil, "", errors.New("not a built-in package")
	}
	stage, err := os.MkdirTemp("", "agentry-builtin-*")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := b.Stage(stage); err != nil {
		return nil, "", err
	}
	digest, err := TreeDigest(stage)
	if err != nil {
		return nil, "", err
	}
	dir := installdir.ReleaseDir(home, b.namespace, b.name, b.version, digest)
	meta := &installdir.Meta{
		Ref:       b.Ref(),
		Namespace: b.namespace, Name: b.name, Version: b.version,
		Digest: digest, Source: "builtin",
		Attestation: receipt.Attestation{State: trust.StateBuiltin, Build: build},
	}
	if _, ok := installdir.Load(dir, digest); ok {
		if got, err := TreeDigest(filepath.Join(dir, "workspace")); err == nil && got == digest {
			return meta, dir, nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, "", fmt.Errorf("replace the changed copy of %s: %w", b.Ref(), err)
		}
	}
	// The embedded tree is the truth, so a copy that changed is replaced,
	// not refused (installdir.Replace).
	if err := installdir.Replace(dir, stage, meta); err != nil {
		return nil, "", err
	}
	return meta, dir, nil
}

// PlanInputs are what EnsurePlan compiles from.
type PlanInputs struct {
	Home         string
	Manifest     *manifest.Manifest
	Meta         *installdir.Meta
	InstallDir   string
	WorkspaceDir string
	Policy       *policy.Policy
	PolicySHA    string
	Adapter      string
	Servers      []spec.MCPServerSpec
	Mode         manifest.Mode
	// CompiledBy is the build identity of the client compiling: a plan
	// another build compiled is recompiled.
	CompiledBy string
	// Web grants the web tools (compile.Inputs.Web): set from a Builtin
	// that carries them, never from anything a caller passes in.
	Web bool
	Log io.Writer
}

// EnsurePlan reuses the installed plan when nothing it depends on changed
// (§R6/§R9), otherwise recompiles. A reused plan.json is first checked
// against the integrity record its compile wrote under the runner home's
// key (agentry-cli#266): a plan with no record this home can check is
// recompiled, and one that does not match its record — changed after it
// was compiled — refuses the run (exit 10).
func EnsurePlan(in PlanInputs) (*spec.Plan, string, error) {
	errOut := in.Log
	if errOut == nil {
		errOut = io.Discard
	}
	key, err := compile.IntegrityKey(in.Home)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCLI, err)
	}
	// One plan per run mode (plan/<mode>/); a plan/plan.json from the
	// single-plan layout is a stale install and is cleared.
	if _, err := os.Stat(filepath.Join(in.InstallDir, "plan", "plan.json")); err == nil {
		_ = os.RemoveAll(filepath.Join(in.InstallDir, "plan"))
	}
	// A plan with the web tools has a directory of its own, so the
	// Drafter's two plans (with them, and without for its repair turn) do
	// not recompile each other.
	planDir := filepath.Join(in.InstallDir, "plan", string(in.Mode))
	if in.Web {
		planDir += "-web"
	}
	runnerImage := in.Policy.RunnerImageTag(runnerimage.ContentVersion())
	b, rerr := compile.ReadInstalledPlan(key, planDir)
	var tampered *compile.TamperedPlanError
	switch {
	case errors.As(rerr, &tampered):
		return nil, "", engine.Classify(engine.ExitVerify, rerr)
	case errors.Is(rerr, compile.ErrNoIntegrityRecord):
		// Read again for the notice only, never for use: a plan from an
		// older CLI has no record and says which CLI it was.
		raw, _ := os.ReadFile(filepath.Join(planDir, "plan.json")) //nolint:gosec // our install layout
		if v := PlanVersionOf(raw); v != "" && v != spec.PlanVersion {
			_, _ = fmt.Fprintf(errOut, "installed plan was compiled by an older CLI (plan version %s, want %s) — recompiling\n", v, spec.PlanVersion)
		} else {
			_, _ = fmt.Fprintln(errOut, "installed plan has no integrity record from this runner home — recompiling")
		}
	}
	if rerr == nil {
		// A plan from an older CLI is recompiled in place, and says so: a
		// cached install used to stay stale after an upgrade until its
		// first execution failed (agentry-cli#60).
		if v := PlanVersionOf(b); v != "" && v != spec.PlanVersion {
			_, _ = fmt.Fprintf(errOut, "installed plan was compiled by an older CLI (plan version %s, want %s) — recompiling\n", v, spec.PlanVersion)
		}
		if plan, perr := spec.ParsePlan(b); perr == nil &&
			plan.PlanVersion == spec.PlanVersion &&
			plan.Mode == string(in.Mode) &&
			plan.Digest == in.Meta.Digest &&
			plan.PolicySHA256 == in.PolicySHA &&
			plan.Harness.Adapter == in.Adapter &&
			plan.Capabilities.WebTools == in.Web &&
			reflect.DeepEqual(plan.MCPServers, in.Servers) {
			switch {
			case plan.CompiledBy != in.CompiledBy:
				// Same schema, another build: the compiler's output may
				// differ (agentry-cli#105 changed what a server's catalog
				// entry carries without touching the schema). An unstamped
				// build is told apart by its binary (agentry-cli#144).
				_, _ = fmt.Fprintf(errOut, "installed plan was compiled by another CLI build (%s, this is %s) — recompiling\n", cmp.Or(plan.CompiledBy, "unknown"), in.CompiledBy)
			case plan.Proxy.Image != runnerImage:
				_, _ = fmt.Fprintf(errOut, "installed plan names runner image %s, this CLI builds %s — recompiling\n", plan.Proxy.Image, runnerImage)
			case plan.ExecImage != nil && plan.ExecImage.Unsafe != nil:
				// This path never compiles Unsafe Mode (plan unsafe-mode.md
				// D5): a plan the desktop compiled with it on, sharing the
				// runner home, is not reused here (invariant 4).
				_, _ = fmt.Fprintln(errOut, "installed plan was compiled in Unsafe Mode — recompiling without it")
			default:
				if verr := compile.VerifyFiles(plan, planDir); verr == nil {
					return plan, planDir, nil
				}
				_, _ = fmt.Fprintln(errOut, "installed plan failed integrity check — recompiling")
			}
		}
	}
	// Compile replaces planDir whole, in one rename, so it is not cleared
	// first: a removal here could run into another process's compile of
	// the same plan (agentry-cli#289).
	plan, err := compile.Compile(compile.Inputs{
		Manifest:     in.Manifest,
		Mode:         in.Mode,
		Digest:       in.Meta.Digest,
		Source:       in.Meta.Source,
		WorkspaceDir: in.WorkspaceDir,
		Policy:       in.Policy,
		PolicySHA:    in.PolicySHA,
		AdapterName:  in.Adapter,
		RunnerImage:  runnerImage,
		CompiledBy:   in.CompiledBy,
		MCPServers:   in.Servers,
		Web:          in.Web,
		IntegrityKey: key,
	}, planDir)
	if err != nil {
		var refusal *compile.PolicyRefusal
		if errors.As(err, &refusal) {
			return nil, "", engine.Classify(engine.ExitPolicy, err)
		}
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	return plan, planDir, nil
}

// PlanVersionOf reads just the planVersion of a stored plan.json, so a
// plan the current schema cannot parse still names the version it has.
func PlanVersionOf(b []byte) string {
	var v struct {
		PlanVersion string `json:"planVersion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	return v.PlanVersion
}

// Run is one run of a built-in package.
type Run struct {
	Builtin    Builtin
	Home       string
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	// Build is the client's build identity, recorded in the receipt's
	// attestation and the plan.
	Build string
	Task  io.Reader
	// Inputs are paths staged read-only under /inputs.
	Inputs    []string
	Knowledge []engine.AttachedPack
	// Decoded is the decode step the caller ran on the inputs, for the
	// receipt.
	Decoded *decode.Result
	OutDir  string
	// Budgets, when set, replace the plan's.
	Budgets *spec.Budgets
	// Adapter names the harness adapter; empty takes the default.
	Adapter    string
	RunID      string
	KeepRunDir bool
	Stderr     io.Writer
	OnUsage    func(receipt.Usage)
	// Web is the web tools' configuration and keys, for a Builtin granted
	// them (the Drafter); the engine refuses to run one without it.
	Web *engine.WebRun
}

// RepairRun is the Drafter's one repair turn after first (plan
// builder-simplify.md §5.2 step 3; drafter.PostStep): the same package
// without its web tools and with no web configuration, so it holds no key
// and has nothing to read, given the turn's task, folder, budgets, run id
// and usage reporter. Both clients run the turn this way.
func RepairRun(first Run, t drafter.RepairTurn) Run {
	budgets := t.Budgets
	return Run{
		Builtin: first.Builtin.WithoutWebTools(), Home: first.Home, Policy: first.Policy, PolicySHA: first.PolicySHA,
		PolicyFile: first.PolicyFile, Build: first.Build, Task: strings.NewReader(t.Task), OutDir: t.OutDir, Budgets: &budgets,
		Adapter: first.Adapter, RunID: t.RunID, Stderr: first.Stderr, OnUsage: t.OnUsage,
	}
}

// Execute runs it: materialize, validate, plan, ensure the images, run.
// There is no consent prompt: the application declares the contract of
// its own package, and the person's choice was to use the feature.
func (r Run) Execute(ctx context.Context, d dockerx.Client) (*receipt.Envelope, error) {
	logw := r.Stderr
	if logw == nil {
		logw = io.Discard
	}
	meta, installDir, err := r.Builtin.Materialize(r.Home, r.Build)
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
	adapterName := cmp.Or(r.Adapter, adapter.DefaultName)
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	plan, planDir, err := EnsurePlan(PlanInputs{
		Home: r.Home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspace,
		Policy: r.Policy, PolicySHA: r.PolicySHA, Adapter: adapterName, Mode: manifest.ModeAutonomous,
		CompiledBy: r.Build, Web: r.Builtin.web, Log: logw,
	})
	if err != nil {
		return nil, err
	}
	runnerTag := r.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(r.Home, "image")
	if err := gateway.EnsureInstalled(ctx, d, r.Policy, runnerTag, ctxDir, logw, plan.Harness.Host == nil); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	var execPackages []string
	if plan.ExecImage != nil {
		if execPackages, err = gateway.EnsureExecImage(ctx, d, runnerTag, *plan.ExecImage, ctxDir, logw); err != nil {
			return nil, engine.Classify(engine.ExitGateway, err)
		}
	}
	budgets := plan.Budgets
	if r.Budgets != nil {
		budgets = *r.Budgets
	}
	return engine.Run(ctx, d, engine.Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: workspace, RunRoot: installDir, RunnerHome: r.Home,
		OutDir: r.OutDir, Task: r.Task, InputPaths: r.Inputs, Knowledge: r.Knowledge, Decoded: r.Decoded,
		Adapter: ad, Policy: r.Policy, PolicyFile: r.PolicyFile, Budgets: budgets,
		Attestation: meta.Attestation, ExecPackages: execPackages, KeepRunDir: r.KeepRunDir,
		RunID: r.RunID, Stderr: logw, OnUsage: r.OnUsage, Web: r.Web,
	})
}
