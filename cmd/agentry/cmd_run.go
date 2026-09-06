// `agentry run` (RUNNER-PLAN §R0): resolve → verify → materialize →
// compile → enforce → run → receipt, one agent per invocation. First use
// of a digest IS the install (RQ5): the verified workspace, the compiled
// plan, and the pulled/built images all persist under ~/.agentry/runner
// so later runs execute without re-deriving anything. The verify gate is
// the same one `agentry install` applies — a run is an install that also
// executes.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blaktron/agentry-cli/runnerimage"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry-cli/pkg/tui"
)

// runOpts carries the run command's flags.
type runOpts struct {
	mode               string
	intake             []string
	input              string
	out                string
	outputFormat       string
	adapterName        string
	local              string
	pin                string
	policyPath         string
	timeout            int
	maxTurns           int
	maxCost            float64
	yes                bool
	allowUnsigned      bool
	allowVulnerableMCP bool
	prepare            bool
	dryRun             bool
	keepRunDir         bool
	listAdapters       bool
	files              []string
	keepProfile        string // "": unset; "true"/"false": the flag decided
	reconfigureProfile bool
}

// newRunCmd builds the run command.
func newRunCmd() *cobra.Command {
	var opts runOpts
	cmd := &cobra.Command{
		Use:   "run [ns/name[@range]]",
		Short: "Verify, install, and execute one BriefAgent in an isolated container",
		Long: "Run a single-purpose BriefAgent once (RUNNER-PLAN.md):\n\n" +
			"  agentry run ns/name@^1.2 --input task.json --out ./reports\n\n" +
			"Task input: --input FILE, else piped stdin, else an empty task. The result\n" +
			"goes to stdout (--output-format text|json); reports the BriefAgent writes\n" +
			"and the full receipts land in the output folder. Every failure class has its\n" +
			"own exit code: 0 ok, 2 usage, 10 verify, 11 policy/secret, 12 compile, 13\n" +
			"gateway/docker, 14 agent, 15 timeout, 16 budget, 17 output schema.\n\n" +
			"Run modes: --mode autonomous runs from the files and ends; --mode interactive\n" +
			"opens a chat with the agent in this terminal under exactly the same controls\n" +
			"(/add a file, /finish for the final result, /quit, /caps). A release that\n" +
			"declares both modes is asked on a terminal and runs autonomous otherwise.\n\n" +
			"First run of a release verifies it exactly like `agentry install` (digest +\n" +
			"DSSE attestation; unsigned needs --allow-unsigned), then compiles the run\n" +
			"plan and installs the gateway/harness/runner images. Later runs reuse all\n" +
			"of it. --prepare stops after that install step; --dry-run prints the\n" +
			"compiled plan and touches no docker. --local DIR runs an unpublished\n" +
			"BriefAgent directory (treated as unsigned).\n\n" +
			"Tools: BriefAgents get exactly the tools their manifest capabilities grant,\n" +
			"served through a per-run Docker MCP Gateway (never from inside the agent\n" +
			"container). The operator policy at ~/.agentry/runner/policy.yaml pins\n" +
			"images and the model endpoint.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.listAdapters {
				return printAdapters(cmd)
			}
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runRun(cmd, ref, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.input, "input", "", "task input file (default: piped stdin, else empty)")
	f.StringVar(&opts.out, "out", "", "output folder for reports and receipts (default ./agentry-out/<ns>-<name>-<runid>)")
	f.StringVar(&opts.outputFormat, "output-format", "text", "stdout format: text | json (json = the full result envelope)")
	f.StringVar(&opts.adapterName, "adapter", "", "harness adapter (default "+adapter.DefaultName+"); see --list-adapters")
	f.StringVar(&opts.local, "local", "", "run an unpublished BriefAgent DIRECTORY instead of a registry ref")
	f.StringVar(&opts.pin, "pin", "", "require the resolved archive digest to equal this sha256:… value")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	f.IntVar(&opts.timeout, "timeout", 0, "override the policy timeout (seconds)")
	f.IntVar(&opts.maxTurns, "max-turns", -1, "override the policy tool-call budget (0 = unlimited)")
	f.Float64Var(&opts.maxCost, "max-cost", -1, "override the policy cost budget in USD; the run is killed (exit 16) once the harness reports more (0 = unlimited)")
	f.BoolVar(&opts.yes, "yes", false, "skip the capability confirmation prompt (CI)")
	f.BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "run a release with no registry attestation (required for --local)")
	f.BoolVar(&opts.allowVulnerableMCP, "allow-vulnerable-mcp", false, "run even when the registry reports a high/critical vulnerability for a declared MCP server (recorded in the receipt)")
	f.BoolVar(&opts.prepare, "prepare", false, "verify, compile, and install images, then stop without executing")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the compiled run plan and exit (no docker required)")
	f.StringVar(&opts.mode, "mode", "", "run mode: autonomous | interactive (default: the release's only mode; asked on a terminal when it declares both, autonomous otherwise)")
	f.BoolVar(&opts.keepRunDir, "keep-run-dir", false, "keep the per-run scratch dir for debugging (secrets are still deleted)")
	f.BoolVar(&opts.listAdapters, "list-adapters", false, "list harness adapters and exit")
	f.StringArrayVar(&opts.files, "file", nil, "file or directory to provide to the run read-only under /inputs (repeatable)")
	f.StringArrayVar(&opts.intake, "intake", nil, "answer an intake question non-interactively: id=value (repeatable; unanswered questions are asked on a terminal)")
	f.StringVar(&opts.keepProfile, "keep-profile", "", "keep the BriefAgent's MCP profile after the run: true | false (default: the profile's own setting, else policy)")
	f.BoolVar(&opts.reconfigureProfile, "reconfigure-profile", false, "re-run the guided MCP-profile setup even when a profile exists")
	return cmd
}

// printAdapters lists registered and planned adapters.
func printAdapters(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	for _, n := range adapter.Names() {
		suffix := ""
		if n == adapter.DefaultName {
			suffix = " (default)"
		}
		_, _ = fmt.Fprintf(out, "%s%s\n", n, suffix)
	}
	_, _ = fmt.Fprintln(out, "planned (RUNNER-PLAN M-R4): goose, claude, codex")
	return nil
}

// runnerHome is ~/.agentry/runner, or $AGENTRY_RUNNER_HOME (tests and odd
// deployments).
func runnerHome() (string, error) {
	if h := os.Getenv("AGENTRY_RUNNER_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory (set AGENTRY_RUNNER_HOME): %w", err)
	}
	return filepath.Join(home, ".agentry", "runner"), nil
}

// installMeta is <installDir>/install.json: what was verified, once, so
// repeat runs of the same digest skip the network without skipping the
// truth about how it was admitted.
type installMeta struct {
	Ref         string              `json:"ref"`
	Namespace   string              `json:"namespace"`
	Name        string              `json:"name"`
	Version     string              `json:"version"`
	Digest      string              `json:"digest"`
	Source      string              `json:"source"`
	Attestation receipt.Attestation `json:"attestation"`
}

// runRun executes the whole §R6 lifecycle.
func runRun(cmd *cobra.Command, ref string, opts runOpts) error {
	if (ref == "") == (opts.local == "") {
		return errors.New("pass exactly one of ns/name[@range] or --local DIR")
	}
	if opts.outputFormat != "text" && opts.outputFormat != "json" {
		return fmt.Errorf("unknown --output-format %q (want text or json)", opts.outputFormat)
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}

	policyPath := opts.policyPath
	if policyPath == "" {
		if policyPath, err = policy.DefaultPath(); err != nil {
			return err
		}
	}
	pol, polSHA, err := policy.EnsureDefault(policyPath)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	// Materialize: verified workspace + install metadata, cached per
	// digest (§R5 standalone mode).
	var meta *installMeta
	var installDir string
	if opts.local != "" {
		meta, installDir, err = materializeLocal(cmd, opts.local, home, opts.allowUnsigned)
	} else {
		meta, installDir, err = materializeRegistry(cmd, ref, home, opts)
	}
	if err != nil {
		return err
	}
	workspaceDir := filepath.Join(installDir, "workspace")
	m, err := manifestFromWorkspace(workspaceDir)
	if err != nil {
		return engine.Classify(engine.ExitCompile, err)
	}

	// The §R5 capability gate: same semantics as install — egress/exec
	// (or a widened contract vs the last accepted run) needs consent.
	mode, err := resolveRunMode(cmd, opts, m)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if mode == manifest.ModeInteractive && !opts.dryRun {
		if !stdinIsTTY(cmd.InOrStdin()) {
			return engine.Classify(engine.ExitUsage, errors.New("an interactive run needs a terminal on stdin (the chat is the input); pipe a task to --mode autonomous instead"))
		}
		if opts.outputFormat == "json" {
			return engine.Classify(engine.ExitUsage, errors.New("--output-format json is not available for an interactive run: the transcript and receipts are the record"))
		}
	}

	if err := confirmRunCapabilities(cmd, m, meta, installDir, opts, mode); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	adapterName := opts.adapterName
	if adapterName == "" {
		adapterName = adapter.DefaultName
	}
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}

	// Declared MCP servers (RUNNER-PLAN-3): resolve the manifest pins —
	// offline when the signature's store already matches them — so the
	// compiler can bake the launch specs into the plan. Installation into
	// the store happens with the image install below; --dry-run stops at
	// resolution and touches no docker.
	profDir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, digestSuffix(meta.Digest))
	serversDir := mcpinstall.StoreDir(profDir)
	var resolvedServers []mcpinstall.Resolved
	var serverSpecs []spec.MCPServerSpec
	var vulnLine string
	if len(m.Capabilities.MCPServers) > 0 {
		// The registry's vulnerability verdict on each declared server
		// (agentry#177, DR-16): refused on a known high/critical advisory
		// unless the operator says otherwise; fail-open when the registry
		// cannot answer. The receipt records the outcome either way.
		statuses := mcpinstall.CheckVulnerabilities(cmd.Context(), nil, registryURL(), m.Capabilities.MCPServers)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "declared MCP servers, per the registry: %s\n", mcpinstall.Summary(statuses))
		var refuseErr error
		vulnLine, refuseErr = vulnerabilityVerdict(statuses, registryURL(), opts.allowVulnerableMCP, pol.MCPServers.AllowVulnerable)
		if refuseErr != nil {
			return engine.Classify(engine.ExitPolicy, refuseErr)
		}
		resolvedServers, err = ensureServerResolutions(cmd.Context(), cmd.ErrOrStderr(), serversDir, pol, m)
		if err != nil {
			return engine.Classify(engine.ExitVerify, err)
		}
		serverSpecs = mcpinstall.Specs(resolvedServers, m.Capabilities.MCPServers, pol.Images)
	}

	plan, planDir, err := ensurePlan(cmd.ErrOrStderr(), m, meta, installDir, workspaceDir, pol, polSHA, adapterName, serverSpecs, mode)
	if err != nil {
		return err
	}
	if vulnLine != "" {
		// Run-time evidence on the compiled plan's enforcement record; the
		// cached plan on disk is untouched (the verdict is per run).
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		plan.Enforcement["mcpServers.vulnerabilities"] = vulnLine
	}

	if opts.dryRun {
		return printDryRun(cmd, plan, planDir)
	}

	// Gateway + harness + runner images: the RQ2/RQ5 install step.
	docker := dockerx.New()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(cmd.Context(), docker, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	// The per-BriefAgent exec image the plan names (agentry-cli#9).
	if _, err := gateway.EnsureExecImage(cmd.Context(), docker, runnerTag, plan.Capabilities.ExecCommands, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if len(resolvedServers) > 0 {
		if err := mcpinstall.EnsureInstalled(cmd.Context(), docker, resolvedServers, serversDir,
			pol.MCPRegistries, pol.Images, cmd.ErrOrStderr()); err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
	}

	// The BriefAgent's MCP profile (RUNNER-PLAN-2 §P2/§P3): load or, on a
	// terminal, run guided setup. Part of the install step, so --prepare
	// includes it; --dry-run (above) never prompts.
	prof, profDir, keep, err := ensureProfile(cmd, home, meta, m, pol, opts)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	if opts.prepare {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "prepared %s (plan %s, images installed) — run again without --prepare to execute\n",
			plan.Ref, shortHash(plan))
		return nil
	}

	answers, err := collectIntake(cmd, m, opts)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}

	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir, err := resolveOutDir(opts.out, meta, runID)
	if err != nil {
		return err
	}

	budgets := plan.Budgets
	if opts.timeout > 0 {
		budgets.TimeoutSeconds = opts.timeout
	}
	if opts.maxTurns >= 0 {
		budgets.MaxTurns = opts.maxTurns
	}
	if opts.maxCost >= 0 {
		budgets.MaxCostUSD = opts.maxCost
	}

	var profSecrets map[string]string
	if prof != nil {
		profSecrets = prof.Secrets
	}
	overrides := secretOverrides(profSecrets, answers)
	params := engine.Params{
		Plan:            plan,
		PlanDir:         planDir,
		WorkspaceSrc:    workspaceDir,
		RunRoot:         installDir,
		OutDir:          outDir,
		Task:            taskReader(cmd, opts.input),
		InputPaths:      opts.files,
		Intake:          answers,
		Adapter:         ad,
		Policy:          pol,
		SecretOverrides: overrides,
		ServersDir:      serversDir,
		Budgets:         budgets,
		Attestation:     meta.Attestation,
		KeepRunDir:      opts.keepRunDir,
		RunID:           runID,
		Stderr:          cmd.ErrOrStderr(),
	}
	if mode == manifest.ModeInteractive {
		runErr := runInteractive(cmd, docker, params, m, plan, opts)
		finishProfile(cmd.ErrOrStderr(), prof, profDir, keep)
		return runErr
	}
	env, runErr := engine.Run(cmd.Context(), docker, params)
	if env != nil {
		printEnvelope(cmd, env, opts.outputFormat)
	}
	finishProfile(cmd.ErrOrStderr(), prof, profDir, keep)
	return runErr
}

// runInteractive drives an interactive plan: the engine's session behind
// the terminal chat (plan §6.2, §6.8). The screen is restored before the
// envelope and receipts are written, so what the user sees last is where
// the transcript went.
func runInteractive(cmd *cobra.Command, docker dockerx.Client, params engine.Params, m *manifest.Manifest, plan *spec.Plan, opts runOpts) error {
	errOut := cmd.ErrOrStderr()
	s, err := engine.StartSession(cmd.Context(), docker, params)
	if err != nil {
		return err
	}
	topts := tui.Options{
		Ref:        plan.Ref,
		Model:      plan.Harness.Model,
		Caps:       capabilityLines(m.Capabilities),
		MaxTurns:   params.Budgets.MaxTurns,
		MaxCostUSD: params.Budgets.MaxCostUSD,
		HasTask:    opts.input != "",
	}
	if plan.Harness.Session != nil {
		topts.Budgets = plan.Harness.Session.Budgets
	}
	if m.Interactive != nil {
		topts.Opening = m.Interactive.Opening
	}
	outcome, uiErr := tui.Run(cmd.Context(), s, topts, cmd.InOrStdin(), cmd.OutOrStdout())
	if uiErr != nil {
		_, _ = fmt.Fprintf(errOut, "chat closed with an error: %v\n", uiErr)
	}
	env, runErr := s.End(cmd.Context(), outcome.Finish)
	if env != nil {
		if _, reason := s.Ended(); reason != "" {
			_, _ = fmt.Fprintf(errOut, "session ended: %s\n", reason)
		}
		if outcome.Finish && runErr == nil {
			var asString string
			if err := json.Unmarshal(env.Result, &asString); err == nil {
				if asString != "" {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), asString)
				}
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(env.Result))
			}
		}
		_, _ = fmt.Fprintf(errOut, "transcript, reports and receipts: %s\n", env.OutDir)
	}
	if runErr == nil && uiErr != nil {
		return engine.Classify(engine.ExitCLI, uiErr)
	}
	return runErr
}

// homeImageDir is where the embedded runner-image build context
// materializes under the runner home.
func homeImageDir(home string) string { return filepath.Join(home, "image") }

// taskReader implements the §R2 input precedence: --input file > piped
// stdin > empty. A TTY stdin is never read — an interactive terminal
// sitting silent is not a task.
func taskReader(cmd *cobra.Command, input string) io.Reader {
	if input != "" {
		f, err := os.Open(input) //nolint:gosec // user-named task file
		if err != nil {
			// Surfaced here rather than pre-validated: the engine treats
			// a nil reader as an empty task, but a NAMED file that does
			// not open must fail loudly.
			return &errReader{err: err}
		}
		return f
	}
	if !stdinIsTTY(cmd.InOrStdin()) {
		return cmd.InOrStdin()
	}
	return nil
}

// errReader defers a task-file open error into the engine's read path.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

// printEnvelope writes the run result per --output-format. Text mode
// prints only the agent's result (Unix composition, §R8); the envelope is
// always in the output folder either way.
func printEnvelope(cmd *cobra.Command, env *receipt.Envelope, format string) {
	out := cmd.OutOrStdout()
	if format == "json" {
		b, err := env.Marshal()
		if err == nil {
			_, _ = out.Write(b)
		}
		return
	}
	var asString string
	if err := json.Unmarshal(env.Result, &asString); err == nil {
		if asString != "" {
			_, _ = fmt.Fprintln(out, asString)
		}
	} else {
		_, _ = fmt.Fprintln(out, string(env.Result))
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "reports and receipts: %s\n", env.OutDir)
}

// printDryRun prints the compiled plan (§R4: deterministic, hashable,
// printable) without touching docker.
func printDryRun(cmd *cobra.Command, plan *spec.Plan, planDir string) error {
	b, err := plan.MarshalCanonical()
	if err != nil {
		return err
	}
	_, _ = cmd.OutOrStdout().Write(b)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "run plan sha256: %s\nplan dir: %s\nmode: %s (declared: %s)\n", shortHash(plan), planDir, plan.Mode, strings.Join(plan.Modes, ","))
	return nil
}

// collectIntake answers the manifest's up-front questions (plan §3.5):
// --intake presets first, then the terminal when there is one.
func collectIntake(cmd *cobra.Command, m *manifest.Manifest, opts runOpts) (*intake.Answers, error) {
	preset, err := intake.ParsePreset(opts.intake)
	if err != nil {
		return nil, err
	}
	if len(m.Intake) == 0 {
		if len(preset) > 0 {
			return nil, errors.New("--intake given but the BriefAgent asks no intake questions")
		}
		return nil, nil
	}
	var p setup.Prompter
	if stdinIsTTY(cmd.InOrStdin()) {
		p = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	return intake.Collect(p, m.Intake, preset, m.Capabilities.Secrets)
}

// secretOverrides merges the profile's stored secrets with the intake's
// secret answers (under their SecretName). answers is nil for a BriefAgent
// that asks no intake questions — the common case, which used to panic
// here (agentry-cli#49).
func secretOverrides(profSecrets map[string]string, answers *intake.Answers) map[string]string {
	overrides := make(map[string]string, len(profSecrets))
	for k, v := range profSecrets {
		overrides[k] = v
	}
	if answers != nil {
		for id, v := range answers.Secrets {
			overrides[intake.SecretName(id)] = v
		}
	}
	return overrides
}

// resolveRunMode applies the plan's default rule (§6.2): the flag wins; a
// release with one mode runs in it; one with both is asked on a terminal
// and runs autonomous otherwise (a --yes or --dry-run run never asks).
func resolveRunMode(cmd *cobra.Command, opts runOpts, m *manifest.Manifest) (manifest.Mode, error) {
	if opts.mode != "" {
		mode := manifest.Mode(opts.mode)
		if !mode.Valid() {
			return "", fmt.Errorf("unknown --mode %q (want autonomous or interactive)", opts.mode)
		}
		return mode, nil
	}
	modes := m.SupportedModes()
	if len(modes) == 1 {
		return modes[0], nil
	}
	if len(modes) == 0 {
		return manifest.ModeAutonomous, nil // the compiler refuses it with the reason
	}
	if !stdinIsTTY(cmd.InOrStdin()) || opts.yes || opts.dryRun {
		return manifest.ModeAutonomous, nil
	}
	p := &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	answer, err := p.Ask("This BriefAgent runs autonomous or interactive. Mode", string(manifest.ModeAutonomous))
	if err != nil {
		return "", err
	}
	mode := manifest.Mode(strings.TrimSpace(answer))
	if !mode.Valid() {
		return "", fmt.Errorf("unknown mode %q (want autonomous or interactive)", answer)
	}
	return mode, nil
}

func shortHash(plan *spec.Plan) string {
	h, err := plan.Hash()
	if err != nil {
		return "unhashable"
	}
	return h[:12]
}

// resolveOutDir picks and creates the output folder (RQ3).
func resolveOutDir(flag string, meta *installMeta, runID string) (string, error) {
	dir := flag
	if dir == "" {
		dir = filepath.Join("agentry-out", fmt.Sprintf("%s-%s-%s", meta.Namespace, meta.Name, runID))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve output dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	return abs, nil
}

// materializeRegistry resolves, verifies, and caches a registry release
// (§R1: the same gate as install — digest, DSSE, --allow-unsigned).
func materializeRegistry(cmd *cobra.Command, ref, home string, opts runOpts) (*installMeta, string, error) {
	if _, err := manifest.ParseRef(ref); err != nil {
		return nil, "", err
	}
	reg := registryURL()
	token := credentialFor(reg)
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return nil, "", err
	}
	res, err := resolveRef(cmd.Context(), client, ref, true)
	if err != nil {
		return nil, "", err
	}
	if res.Yanked {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s/%s@%s is YANKED — running only because the version was pinned exactly\n",
			res.Namespace, res.Name, res.Version)
	}
	if opts.pin != "" && res.Digest != opts.pin {
		return nil, "", engine.Classify(engine.ExitVerify,
			fmt.Errorf("--pin mismatch: registry resolved %s, pin requires %s", res.Digest, opts.pin))
	}

	installDir := filepath.Join(home, "agents", res.Namespace, res.Name,
		fmt.Sprintf("%s-%s", res.Version, digestSuffix(res.Digest)))
	if meta, ok := loadInstallMeta(installDir, res.Digest); ok {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "using verified install %s\n", installDir)
		return meta, installDir, nil
	}

	archive, err := downloadArchive(cmd.Context(), reg, token, res)
	if err != nil {
		return nil, "", err
	}
	computed := sha256Digest(archive)
	if computed != res.Digest {
		return nil, "", engine.Classify(engine.ExitVerify,
			fmt.Errorf("digest mismatch: registry says %s, archive is %s — refusing to run", res.Digest, computed))
	}
	att, err := verifyRunAttestation(cmd, reg, token, res, computed, opts.allowUnsigned)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitVerify, err)
	}

	staging, err := os.MkdirTemp("", "agentry-run-*")
	if err != nil {
		return nil, "", fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := pkgarchive.Extract(bytes.NewReader(archive), staging, int64(len(archive)), installExtractLimits()); err != nil {
		return nil, "", engine.Classify(engine.ExitVerify, fmt.Errorf("extract archive: %w", err))
	}

	meta := &installMeta{
		Ref:       fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version),
		Namespace: res.Namespace, Name: res.Name, Version: res.Version,
		Digest: res.Digest, Source: "registry", Attestation: att,
	}
	if err := writeInstall(installDir, staging, meta); err != nil {
		return nil, "", err
	}
	return meta, installDir, nil
}

// verifyRunAttestation mirrors the install gate (cmd_install.go) but also
// returns the attestation facts the result envelope reports.
func verifyRunAttestation(cmd *cobra.Command, reg, token string, res *apiclient.ResolveResult, archiveDigest string, allowUnsigned bool) (receipt.Attestation, error) {
	attURL := ""
	if res.AttestationUrl != nil {
		attURL = *res.AttestationUrl
	}
	if attURL == "" {
		attURL = fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/attestation", res.Namespace, res.Name, res.Version)
	}
	att := trust.VerifyAttestation(cmd.Context(), httpClient, reg, token, attURL, archiveDigest)
	// Narration to stderr: run's stdout carries only the result (§R8).
	errOut := cmd.ErrOrStderr()
	switch att.State {
	case trust.StateVerified:
		_, _ = fmt.Fprintf(errOut, "attestation:   verified (key %s)\n", att.KeyID)
		if att.FirstUse {
			_, _ = fmt.Fprintln(errOut, "note: signing keys pinned on first use (TOFU) — run `agentry login` to pin deliberately")
		}
		return receipt.Attestation{State: "verified", KeyID: att.KeyID, Verified: true}, nil
	case trust.StateUnsigned:
		if !allowUnsigned {
			return receipt.Attestation{}, fmt.Errorf("unsigned — registry offers no attestation for %s/%s@%s (%s); re-run with --allow-unsigned to run it anyway",
				res.Namespace, res.Name, res.Version, att.Detail)
		}
		_, _ = fmt.Fprintf(errOut, "warning: unsigned — running %s/%s@%s because --allow-unsigned was given\n", res.Namespace, res.Name, res.Version)
		return receipt.Attestation{State: "unsigned"}, nil
	default:
		hint := ""
		if att.KeyRotated {
			hint = " (the registry advertises signing key ids you have not pinned — re-pin deliberately if this rotation is expected)"
		}
		return receipt.Attestation{}, fmt.Errorf("attestation %s for %s/%s@%s: %s%s — refusing to run",
			att.State, res.Namespace, res.Name, res.Version, att.Detail, hint)
	}
}

// materializeLocal admits an unpublished package directory (§R5): parse +
// validate the manifest, hash the tree, and cache like a registry install
// keyed by that hash. Local trees are unsigned by definition, so the same
// --allow-unsigned decision applies.
func materializeLocal(cmd *cobra.Command, dir, home string, allowUnsigned bool) (*installMeta, string, error) {
	if !allowUnsigned {
		return nil, "", engine.Classify(engine.ExitVerify,
			errors.New("--local packages are unsigned by definition; pass --allow-unsigned to run one"))
	}
	m, err := manifestFromWorkspace(dir)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	// Warnings (e.g. the V5 "*" egress flag) surface but do not block —
	// same split the registry applies at publish; the RUN-time gate for
	// wildcard egress is the policy check in the compiler (§R4).
	errs := manifest.Validate(m)
	for _, e := range errs {
		if manifest.IsWarning(e) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", e)
		}
	}
	if blocking := manifest.Blocking(errs); len(blocking) > 0 {
		msgs := make([]string, 0, len(blocking))
		for _, e := range blocking {
			msgs = append(msgs, e.Error())
		}
		return nil, "", engine.Classify(engine.ExitCompile,
			fmt.Errorf("local package manifest invalid:\n  %s", strings.Join(msgs, "\n  ")))
	}
	digest, err := localTreeDigest(dir)
	if err != nil {
		return nil, "", err
	}
	installDir := filepath.Join(home, "agents", m.Namespace, m.Name,
		fmt.Sprintf("%s-%s", m.Version, digestSuffix(digest)))
	meta := &installMeta{
		Ref:       fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
		Namespace: m.Namespace, Name: m.Name, Version: m.Version,
		Digest: digest, Source: "local",
		Attestation: receipt.Attestation{State: "local"},
	}
	if cached, ok := loadInstallMeta(installDir, digest); ok {
		return cached, installDir, nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "local package %s (%s)\n", meta.Ref, digestSuffix(digest))
	if err := writeInstall(installDir, dir, meta); err != nil {
		return nil, "", err
	}
	return meta, installDir, nil
}

// writeInstall lands workspace + install.json under installDir.
func writeInstall(installDir, srcTree string, meta *installMeta) error {
	// A half-written install must not look installed: build aside, then
	// rename into place.
	if err := os.RemoveAll(installDir); err != nil {
		return fmt.Errorf("clear install dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(installDir), 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}
	tmp := installDir + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear partial install: %w", err)
	}
	if err := copyWorkspace(srcTree, filepath.Join(tmp, "workspace")); err != nil {
		return err
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal install metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "install.json"), append(b, '\n'), 0o644); err != nil { //nolint:gosec // metadata, not secret
		return fmt.Errorf("write install metadata: %w", err)
	}
	if err := os.Rename(tmp, installDir); err != nil {
		return fmt.Errorf("finalize install dir: %w", err)
	}
	return nil
}

// loadInstallMeta reuses a cached verified install iff its recorded digest
// matches what the registry (or tree hash) says now.
func loadInstallMeta(installDir, wantDigest string) (*installMeta, bool) {
	b, err := os.ReadFile(filepath.Join(installDir, "install.json")) //nolint:gosec // our install layout
	if err != nil {
		return nil, false
	}
	var meta installMeta
	if err := json.Unmarshal(b, &meta); err != nil || meta.Digest != wantDigest {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(installDir, "workspace")); err != nil {
		return nil, false
	}
	return &meta, true
}

// ensurePlan reuses the installed plan when nothing it depends on changed
// (§R6/§R9), otherwise recompiles.
func ensurePlan(errOut io.Writer, m *manifest.Manifest, meta *installMeta, installDir, workspaceDir string,
	pol *policy.Policy, polSHA, adapterName string, serverSpecs []spec.MCPServerSpec, mode manifest.Mode,
) (*spec.Plan, string, error) {
	// One plan per run mode (plan/<mode>/); a plan/plan.json from the
	// single-plan layout is a stale install and is cleared.
	if _, err := os.Stat(filepath.Join(installDir, "plan", "plan.json")); err == nil {
		_ = os.RemoveAll(filepath.Join(installDir, "plan"))
	}
	planDir := filepath.Join(installDir, "plan", string(mode))
	if b, err := os.ReadFile(filepath.Join(planDir, "plan.json")); err == nil { //nolint:gosec // our install layout
		if plan, perr := spec.ParsePlan(b); perr == nil &&
			plan.PlanVersion == spec.PlanVersion &&
			plan.Mode == string(mode) &&
			plan.Digest == meta.Digest &&
			plan.PolicySHA256 == polSHA &&
			plan.Harness.Adapter == adapterName &&
			reflect.DeepEqual(plan.MCPServers, serverSpecs) {
			if verr := compile.VerifyFiles(plan, planDir); verr == nil {
				return plan, planDir, nil
			}
			_, _ = fmt.Fprintln(errOut, "installed plan failed integrity check — recompiling")
		}
	}
	if err := os.RemoveAll(planDir); err != nil {
		return nil, "", fmt.Errorf("clear stale plan: %w", err)
	}
	plan, err := compile.Compile(compile.Inputs{
		Manifest:     m,
		Mode:         mode,
		Digest:       meta.Digest,
		Source:       meta.Source,
		WorkspaceDir: workspaceDir,
		Policy:       pol,
		PolicySHA:    polSHA,
		AdapterName:  adapterName,
		RunnerImage:  pol.RunnerImageTag(runnerimage.ContentVersion()),
		MCPServers:   serverSpecs,
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

// confirmRunCapabilities applies the install-time consent semantics to
// runs, keyed on the runner install dir: first run asks about egress/exec,
// upgrades ask again only when the contract widened (§R1).
func confirmRunCapabilities(cmd *cobra.Command, m *manifest.Manifest, meta *installMeta, installDir string, opts runOpts, mode manifest.Mode) error {
	// Everything here narrates to stderr: stdout is the run's result
	// channel (§R8) and must stay pipeline-clean.
	out := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(out, "%s (%s)\n", meta.Ref, modesLabel(m.SupportedModes()))
	if m.Summary != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", m.Summary)
	}
	_, _ = fmt.Fprintln(out, "capabilities:")
	for _, line := range capabilityLines(m.Capabilities) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	if mode == manifest.ModeInteractive {
		// Plan §8: the one line the consent screen gains for a session.
		_, _ = fmt.Fprintln(out, "  interactive: you can talk to this agent and give it files; you cannot give it more access than listed above")
	}
	key := installed.Key{
		Registry: "runner", Namespace: meta.Namespace, Name: meta.Name,
		Target: "runner", Dir: filepath.Dir(installDir),
	}
	state, serr := installed.Load()
	if serr != nil {
		_, _ = fmt.Fprintf(out, "warning: %v — treating this as a first run\n", serr)
		state = installed.State{}
	}
	next := installed.Snapshot(m.Capabilities)
	if err := confirmCapabilityChange(cmd, out, &state, key, meta.Version, next, m.Capabilities, opts.yes); err != nil {
		return err
	}
	state.Upsert(key, meta.Version, next, false)
	if err := installed.Save(state); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — the next run will re-ask about capabilities\n", err)
	}
	return nil
}

// manifestFromWorkspace parses agent.yaml at a tree root.
func manifestFromWorkspace(dir string) (*manifest.Manifest, error) {
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

// localTreeDigest hashes a package directory deterministically (sorted
// relative paths + sizes + content) so --local runs are cache-keyed and
// receipted just like archives, with "sha256:" kept for shape parity.
func localTreeDigest(dir string) (string, error) {
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
		b, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // hashing the user's named tree
		if rerr != nil {
			return "", fmt.Errorf("hash local tree: %w", rerr)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		_, _ = h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// copyWorkspace copies a local package tree, skipping VCS and output dirs.
func copyWorkspace(src, dest string) error {
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == ".git" || info.Name() == "agentry-out") {
			return filepath.SkipDir
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p) //nolint:gosec // copying the user's named tree
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		return fmt.Errorf("copy package tree: %w", err)
	}
	return nil
}

// ensureProfile loads — or, on a terminal, guides the creation of — the
// BriefAgent's MCP profile for this signature (RUNNER-PLAN-2 §P2/§P3).
// Returns the profile (nil when none applies), its directory, and the
// effective keep choice: flag > stored profile > policy default.
func ensureProfile(cmd *cobra.Command, home string, meta *installMeta, m *manifest.Manifest,
	pol *policy.Policy, opts runOpts,
) (*profile.Profile, string, bool, error) {
	dir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, digestSuffix(meta.Digest))
	prof, err := profile.Load(dir)
	if err != nil {
		return nil, dir, false, err
	}

	keep := pol.KeepProfilesByDefault()
	keepDecided := false
	switch opts.keepProfile {
	case "":
		if prof != nil {
			keep = prof.Keep
		}
	case "true":
		keep, keepDecided = true, true
	case "false":
		keep, keepDecided = false, true
	default:
		return nil, dir, false, fmt.Errorf("--keep-profile %q must be true or false", opts.keepProfile)
	}

	// The union of agent-level and per-server secret names (RUNNER-PLAN-3):
	// guided setup collects every key the release's gateway will need.
	secrets := allSecretNames(m)
	needGuided := opts.reconfigureProfile || (prof == nil && len(secrets) > 0)
	if !needGuided {
		if prof == nil && keepDecided && keep {
			// Nothing to collect, but the operator explicitly asked to
			// keep a profile: record the choice for future schema growth
			// (P2 §P3).
			prof = profile.New(meta.Ref, meta.Digest, true, nil)
			if err := prof.Save(dir); err != nil {
				return nil, dir, false, err
			}
		}
		return prof, dir, keep, nil
	}

	if !stdinIsTTY(cmd.InOrStdin()) {
		if opts.reconfigureProfile {
			return nil, dir, false, errors.New("--reconfigure-profile needs an interactive terminal (the desktop app configures profiles as a form)")
		}
		// Non-interactive first run: v1 semantics — secrets resolve from
		// the policy source; no profile is created and nothing degrades
		// silently (a missing secret still refuses with exit 11).
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"note: no MCP profile for %s and no terminal for guided setup — secrets resolve from the %s source\n",
			meta.Ref, pol.SecretSource.Mode)
		return nil, dir, keep, nil
	}

	prompter := &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	newProf, err := setup.GuidedProfile(prompter, setup.GuidedProfileOpts{
		Ref:             meta.Ref,
		Digest:          meta.Digest,
		CapabilityLines: capabilityLines(m.Capabilities),
		Secrets:         secrets,
		DefaultKeep:     keep,
		KeepDecided:     keepDecided,
	})
	if err != nil {
		return nil, dir, false, err
	}
	if prof != nil {
		newProf.CreatedAt = prof.CreatedAt
	}
	if err := newProf.Save(dir); err != nil {
		return nil, dir, false, err
	}
	return newProf, dir, newProf.Keep, nil
}

// finishProfile applies the completion choice after a run's lifecycle
// finished, success or failure (P2 §P2): keep → persist + stamp; remove →
// the profile dies with the run.
func finishProfile(errOut io.Writer, prof *profile.Profile, dir string, keep bool) {
	if prof == nil {
		return
	}
	if keep {
		prof.Keep = true
		if err := prof.Touch(dir); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: %v\n", err)
		}
		return
	}
	if err := profile.Remove(dir); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %v\n", err)
		return
	}
	_, _ = fmt.Fprintln(errOut, "MCP profile removed (run completed with keep-profile=false)")
}

// digestSuffix shortens sha256:… for directory names.
func digestSuffix(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// runnerCLIVersion feeds the runner image tag derivation (§R7: a new CLI
// version is what triggers a runner image rebuild).
