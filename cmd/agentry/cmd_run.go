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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/runnerimage"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

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
	mode         string
	intake       []string
	input        string
	out          string
	outputFormat string
	// guideShown is set once the release's guide has been printed on this
	// terminal, so the profile setup and the intake do not print it twice.
	guideShown         *bool
	eventsPath         string
	adapterName        string
	local              string
	pin                string
	policyPath         string
	timeout            int
	maxTurns           int
	maxCost            float64
	yes                bool
	allowUnsigned      bool
	allowUnscanned     bool
	allowVulnerableMCP bool
	prepare            bool
	dryRun             bool
	// plan is the consent-free preview a client shows a person before they
	// consent (agentry-cli#124): resolve, verify, compile, and print what a
	// run would enforce and what it would ask or refuse — asking nothing,
	// recording nothing, and materializing into a temp dir rather than the
	// runner cache. --dry-run keeps its meaning (the plan for a person who
	// consented on the terminal).
	plan               bool
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
			"own exit code: 0 ok, 1 cli (interrupted, unknown adapter, intake), 2 usage,\n" +
			"10 verify, 11 policy/secret, 12 compile, 13 gateway/docker, 14 agent, 15\n" +
			"timeout, 16 budget, 17 output schema.\n\n" +
			"Run modes: --mode autonomous runs from the files and ends; --mode interactive\n" +
			"opens a chat with the agent in this terminal under exactly the same controls\n" +
			"(/add a file, /finish for the final result, /quit, /caps). A release that\n" +
			"declares both modes is asked on a terminal and runs autonomous otherwise.\n\n" +
			"First run of a release verifies it exactly like `agentry install` (digest +\n" +
			"DSSE attestation; unsigned needs --allow-unsigned; a release the registry\n" +
			"admitted without a scan is unscanned and needs --allow-unscanned, its own\n" +
			"decision), then compiles the run plan and installs the gateway, harness\n" +
			"and runner images. Later runs reuse the verified install, the plan and the\n" +
			"images; an unsigned or local release needs --allow-unsigned on every run,\n" +
			"an unscanned one --allow-unscanned. --prepare stops after that install\n" +
			"step; --dry-run prints the\n" +
			"compiled plan and touches no docker. --local DIR runs an unpublished\n" +
			"BriefAgent directory (treated as unsigned).\n\n" +
			"For drivers (a hosted run's worker): AGENTRY_RUN_GRANT is sent as the\n" +
			"X-Agentry-Run-Grant header on every request to the --registry host and\n" +
			"nowhere else, for a registry prefix that answers for one run;\n" +
			"AGENTRY_PIN_REGISTRY names the registry whose pinned signing keys verify\n" +
			"the release, so pointing --registry at such a prefix keeps the pins made\n" +
			"at login.\n\n" +
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
	f.StringVar(&opts.eventsPath, "events", "", "append the harness event stream to FILE as JSON lines while the run is live (the receipt keeps its own copy either way)")
	f.StringVar(&opts.adapterName, "adapter", "", "harness adapter (default "+adapter.DefaultName+"); see --list-adapters")
	f.StringVar(&opts.local, "local", "", "run an unpublished BriefAgent DIRECTORY instead of a registry ref")
	f.StringVar(&opts.pin, "pin", "", "require the resolved archive digest to equal this sha256:… value")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	f.IntVar(&opts.timeout, "timeout", 0, "override the policy timeout (seconds)")
	f.IntVar(&opts.maxTurns, "max-turns", -1, "override the policy tool-call budget (0 = unlimited)")
	f.Float64Var(&opts.maxCost, "max-cost", -1, "override the policy cost budget in USD; the run is killed (exit 16) once the harness reports more (0 = unlimited)")
	f.BoolVar(&opts.yes, "yes", false, "skip the capability confirmation prompt (CI)")
	f.BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "run a release with no registry attestation (required for --local)")
	f.BoolVar(&opts.allowUnscanned, "allow-unscanned", false, "run a release the registry admitted without a scan (no findings, no report, no attestation)")
	f.BoolVar(&opts.allowVulnerableMCP, "allow-vulnerable-mcp", false, "run even when the registry reports a high/critical vulnerability for a declared MCP server (recorded in the receipt)")
	f.BoolVar(&opts.prepare, "prepare", false, "verify, compile, and install images, then stop without executing")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the compiled run plan and exit (no docker required)")
	f.BoolVar(&opts.plan, "plan", false, "preview for a client: compile the run plan and report the attestation, the consent a run would ask for and the refusal it would meet; asks nothing, records nothing but a first-use pin of the registry's keys (with --json, the machine-readable shape)")
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
	_, _ = fmt.Fprintln(out, "planned (RUNNER-PLAN M-R4): goose")
	return nil
}

// runnerHome is ~/.agentry/runner, or $AGENTRY_RUNNER_HOME (tests and odd
// deployments) — policy.Home, so the policy's default path and everything
// else under the home cannot resolve differently (agentry-cli#149).
func runnerHome() (string, error) { return policy.Home() }

// runRun executes the whole §R6 lifecycle.
func runRun(cmd *cobra.Command, ref string, opts runOpts) error {
	if (ref == "") == (opts.local == "") {
		return errors.New("pass exactly one of ns/name[@range] or --local DIR")
	}
	if opts.outputFormat != "text" && opts.outputFormat != "json" {
		return fmt.Errorf("unknown --output-format %q (want text or json)", opts.outputFormat)
	}
	opts.guideShown = new(bool)
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
	// digest (§R5 standalone mode). A --plan materializes into a temp
	// home instead: a preview writes nothing that outlives it, and an
	// unsigned release is reported rather than refused.
	var meta *installdir.Meta
	var installDir string
	var planRefusal string
	// consentHome is where the consent record lives (the real home); home
	// itself moves to a temp dir under --plan so every later write — the
	// install cache, the profile store, the plan dir — outlives nothing.
	consentHome, matOpts := home, opts
	if opts.plan {
		tmp, terr := os.MkdirTemp("", "agentry-plan-*")
		if terr != nil {
			return fmt.Errorf("create plan dir: %w", terr)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		home = tmp
		matOpts.allowUnsigned = true
		matOpts.allowUnscanned = true
	}
	if opts.local != "" {
		if opts.plan && !opts.allowUnsigned {
			planRefusal = "a local workspace has no attestation; a run needs --allow-unsigned"
		}
		meta, installDir, err = materializeLocal(cmd, opts.local, home, matOpts.allowUnsigned)
	} else {
		meta, installDir, err = materializeRegistry(cmd, ref, home, matOpts)
		if err == nil && opts.plan {
			planRefusal = attestationRefusal(meta.Attestation, opts.allowUnsigned, opts.allowUnscanned)
		}
	}
	if err != nil {
		return err
	}
	workspaceDir := filepath.Join(installDir, "workspace")
	m, err := manifestFromWorkspace(workspaceDir)
	if err != nil {
		return engine.Classify(engine.ExitCompile, err)
	}
	// An install cached before the registry path validated (agentry-cli#158)
	// is held to the same rules at run; the compiler refuses the mount
	// shapes on its own besides.
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		msgs := make([]string, 0, len(blocking))
		for _, e := range blocking {
			msgs = append(msgs, e.Error())
		}
		return engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid:\n  %s", meta.Ref, strings.Join(msgs, "\n  ")))
	}

	// The §R5 capability gate: same semantics as install — egress/exec
	// (or a widened contract vs the last accepted run) needs consent.
	mode, err := resolveRunMode(cmd, opts, m)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if mode == manifest.ModeInteractive && !opts.dryRun && !opts.plan {
		if !stdinIsTTY(cmd.InOrStdin()) {
			return engine.Classify(engine.ExitUsage, errors.New("an interactive run needs a terminal on stdin (the chat is the input); pipe a task to --mode autonomous instead"))
		}
		if opts.outputFormat == "json" {
			return engine.Classify(engine.ExitUsage, errors.New("--output-format json is not available for an interactive run: the transcript and receipts are the record"))
		}
		if opts.eventsPath != "" {
			return engine.Classify(engine.ExitUsage, errors.New("--events is not available for an interactive run: the session streams its own events to the transcript"))
		}
	}

	var consent runPlanConsent
	if opts.plan {
		consent = describeRunConsent(m, meta, consentHome)
	} else if err := confirmRunCapabilities(cmd, m, meta, consentHome, opts, mode); err != nil {
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
	profDir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, installdir.DigestSuffix(meta.Digest))
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
			if !opts.plan {
				return engine.Classify(engine.ExitPolicy, refuseErr)
			}
			if planRefusal == "" {
				planRefusal = refuseErr.Error()
			}
		}
		resolvedServers, err = ensureServerResolutions(cmd.Context(), cmd.ErrOrStderr(), serversDir, pol, m)
		if err != nil {
			return engine.Classify(engine.ExitVerify, err)
		}
		serverSpecs = mcpinstall.Specs(resolvedServers, m.Capabilities.MCPServers, pol.Images)
	}

	plan, planDir, err := ensurePlan(cmd.ErrOrStderr(), home, m, meta, installDir, workspaceDir, pol, polSHA, adapterName, serverSpecs, mode)
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

	if opts.plan {
		return printRunPlan(cmd, plan, meta, m, mode, consent, planRefusal)
	}
	if opts.dryRun {
		return printDryRun(cmd, plan, planDir)
	}

	// Gateway + harness + runner images: the RQ2/RQ5 install step.
	docker := dockerx.New()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(cmd.Context(), docker, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr(),
		plan.Harness.Host == nil); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	// The per-BriefAgent exec image the plan names (agentry-cli#9, #100):
	// built with the operator's catalogued packages; the versions it
	// resolved go into the receipt.
	var execPackages []string
	if plan.ExecImage != nil {
		var err error
		execPackages, err = gateway.EnsureExecImage(cmd.Context(), docker, runnerTag, *plan.ExecImage, homeImageDir(home), cmd.ErrOrStderr())
		if err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
	}
	if len(resolvedServers) > 0 {
		// A CLI killed mid-install left its builder running under the
		// same fixed name: sweep what dead runs left before building
		// (agentry-cli#161). engine.Run sweeps again; the second finds
		// nothing.
		engine.SweepAbandoned(cmd.Context(), docker, cmd.ErrOrStderr())
		if err := mcpinstall.EnsureInstalled(cmd.Context(), docker, resolvedServers, serversDir,
			pol.MCPRegistries, pol.Images, pol.Docker, cmd.ErrOrStderr()); err != nil {
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

	// A driver metering a live run reads the harness's usage two ways, both
	// beside the envelope rather than instead of it (agentry-cli#138).
	onUsage := usageReporter(opts.outputFormat, cmd.ErrOrStderr())
	var eventsOut io.Writer
	if opts.eventsPath != "" {
		f, err := os.OpenFile(opts.eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // operator-named path
		if err != nil {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("open the live events file: %w", err))
		}
		defer func() { _ = f.Close() }()
		eventsOut = f
	}

	params := engine.Params{
		Plan:            plan,
		ExecPackages:    execPackages,
		PlanDir:         planDir,
		WorkspaceSrc:    workspaceDir,
		RunRoot:         installDir,
		RunnerHome:      home,
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
		OnUsage:         onUsage,
		Events:          eventsOut,
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

// startSession and runChat are runInteractive's two collaborators, package
// variables so a test can put a fake session behind a chat that panics or
// returns early and see teardown still run (agentry-cli#161).
var (
	startSession = engine.StartSession
	runChat      = tui.Run
)

// runInteractive drives an interactive plan: the engine's session behind
// the terminal chat (plan §6.2, §6.8). The screen is restored before the
// envelope and receipts are written, so what the user sees last is where
// the transcript went.
func runInteractive(cmd *cobra.Command, docker dockerx.Client, params engine.Params, m *manifest.Manifest, plan *spec.Plan, opts runOpts) error {
	errOut := cmd.ErrOrStderr()
	s, err := startSession(cmd.Context(), docker, params)
	if err != nil {
		return err
	}
	// Teardown on every path (agentry-cli#161): a panic in the terminal chat,
	// or any return before the explicit End below, would leave the session's
	// sandbox, the gateway holding the docker socket and the model key running
	// with nothing left to apply a budget. End is idempotent — a second call
	// returns the first one's envelope — so the explicit End below still
	// produces the receipts this function reports. The deferred call gets a
	// context of its own: on the panic path cmd.Context() may already be
	// cancelled, and a teardown that cannot run is no teardown.
	defer func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = s.End(endCtx, false)
	}()
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
	outcome, uiErr := runChat(cmd.Context(), s, topts, cmd.InOrStdin(), cmd.OutOrStdout())
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

// runPlanConsent is what a run's consent gate would say, computed without
// asking or recording (agentry-cli#124).
type runPlanConsent struct {
	Installed         *installPlanPrior `json:"installed,omitempty"`
	Diff              installed.Diff    `json:"diff"`
	NeedsConfirmation bool              `json:"needsConfirmation"`
	Reason            string            `json:"reason,omitempty"`
}

// describeRunConsent mirrors confirmRunCapabilities' decision without its
// prompt and without installed.Save.
func describeRunConsent(m *manifest.Manifest, meta *installdir.Meta, home string) runPlanConsent {
	var c runPlanConsent
	key := installdir.ConsentKey(home, meta.Namespace, meta.Name)
	state, serr := installed.Load()
	if serr != nil {
		state = installed.State{}
	}
	next := installed.Snapshot(m.Capabilities)
	if prev := state.Find(key); prev != nil {
		c.Installed = &installPlanPrior{Version: prev.Version, Dir: prev.Dir}
		c.Diff = installed.DiffCapabilities(prev.Capabilities, next)
		if c.Diff.HasWidened() {
			c.NeedsConfirmation = true
			c.Reason = "this version widens the declared capabilities"
		}
	} else if capabilityNeedsConfirm(m.Capabilities) {
		c.NeedsConfirmation = true
		c.Reason = "a first run of a BriefAgent that declares network egress or exec"
	}
	return c
}

// attestationRefusal is what a run would stop on for this verdict: nothing
// when verified, the missing flag when unsigned, local or unscanned — each
// its own flag — the verdict itself otherwise.
func attestationRefusal(att receipt.Attestation, allowUnsigned, allowUnscanned bool) string {
	switch att.State {
	case "verified":
		return ""
	case trust.StateUnsigned, "local":
		if allowUnsigned {
			return ""
		}
		return "the release is " + att.State + "; a run needs --allow-unsigned"
	case trust.StateUnscanned:
		if allowUnscanned {
			return ""
		}
		return "the release is unscanned — the registry admitted it without a scan; a run needs --allow-unscanned"
	default:
		return "attestation " + att.State + " — a run refuses"
	}
}

// cachedAttestationGate re-applies the consent gate to an install the
// cache already holds: the cache remembers what the verdict was, and the
// flag is asked for again on every run — and the receipt of this run
// carries the consent given now, not the one given at the install.
func cachedAttestationGate(meta *installdir.Meta, allowUnsigned, allowUnscanned bool) error {
	switch meta.Attestation.State {
	case trust.StateUnsigned, "local":
		if !allowUnsigned {
			return fmt.Errorf("%s is %s (cached install); re-run with --allow-unsigned to run it anyway", meta.Ref, meta.Attestation.State)
		}
		meta.Attestation.Consent = receipt.ConsentAllowUnsigned
	case trust.StateUnscanned:
		if !allowUnscanned {
			return fmt.Errorf("%s is unscanned (cached install) — the registry admitted it without a scan; re-run with --allow-unscanned to run it anyway", meta.Ref)
		}
		meta.Attestation.Consent = receipt.ConsentAllowUnscanned
	}
	return nil
}

// runPlanOutput is the `run --plan --json` shape (agentry-cli#124): the
// compiled plan — what a run would enforce — and what a run would ask or
// refuse, with nothing asked or recorded.
type runPlanOutput struct {
	Ref     string   `json:"ref"`
	Version string   `json:"version"`
	Digest  string   `json:"digest"`
	Mode    string   `json:"mode"`
	Modes   []string `json:"modes"`
	Summary string   `json:"summary,omitempty"`
	// Plan is the compiled plan, canonical (§R4); PlanSha256 its hash.
	Plan       json.RawMessage `json:"plan"`
	PlanSha256 string          `json:"planSha256"`
	// Contract is the manifest's capabilities as canonical JSON; the same
	// form install --plan reports.
	Contract       json.RawMessage `json:"contract"`
	ContractDigest string          `json:"contractDigest"`
	// Attestation is verified | unsigned | unscanned | mismatch |
	// unverifiable; Signed is Attestation == verified.
	Attestation string `json:"attestation"`
	KeyID       string `json:"keyId,omitempty"`
	Signed      bool   `json:"signed"`
	runPlanConsent
	// Refusal is what a run would stop on before executing: the
	// attestation, or a declared server with a known vulnerability.
	Refusal string `json:"refusal,omitempty"`
}

func printRunPlan(cmd *cobra.Command, plan *spec.Plan, meta *installdir.Meta, m *manifest.Manifest, mode manifest.Mode, consent runPlanConsent, refusal string) error {
	b, err := plan.MarshalCanonical()
	if err != nil {
		return err
	}
	contract, cdigest, err := contractJSON(m.Capabilities)
	if err != nil {
		return err
	}
	out := runPlanOutput{
		Ref: meta.Ref, Version: meta.Version, Digest: meta.Digest, Mode: string(mode), Modes: modeStrings(m.SupportedModes()),
		Summary: m.Summary, Plan: bytes.TrimSpace(b), PlanSha256: planHash(plan), Contract: contract, ContractDigest: cdigest,
		Attestation: meta.Attestation.State, KeyID: meta.Attestation.KeyID, Signed: meta.Attestation.Verified,
		runPlanConsent: consent, Refusal: refusal,
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), out)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "plan: %s (%s) plan sha256 %s\n", out.Ref, out.Mode, out.PlanSha256)
	_, _ = fmt.Fprintf(w, "attestation: %s\n", out.Attestation)
	if out.NeedsConfirmation {
		_, _ = fmt.Fprintf(w, "would ask: %s\n", out.Reason)
	}
	if out.Refusal != "" {
		_, _ = fmt.Fprintf(w, "would refuse: %s\n", out.Refusal)
	}
	_, _ = fmt.Fprintln(w, "nothing asked, nothing recorded (--plan)")
	return nil
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
		if len(preset) < len(m.Intake) {
			sayGuide(cmd.ErrOrStderr(), m, opts)
		}
	}
	return intake.Collect(p, m.Intake, preset, m.Capabilities.Secrets)
}

// sayGuide prints the release guide's overview and its list of what to
// gather (DEV-141, plan guided-runs.md §6) on the terminal, once, before
// the first question a run asks: the guided profile's keys or the intake.
// A run that asks nothing prints nothing, and so do a --output-format json
// run and a run with no terminal, whose callers never reach here. The
// overview is markdown and is printed as its text.
func sayGuide(w io.Writer, m *manifest.Manifest, opts runOpts) {
	if m.Guide == nil || opts.outputFormat == "json" || (opts.guideShown != nil && *opts.guideShown) {
		return
	}
	if opts.guideShown != nil {
		*opts.guideShown = true
	}
	g := m.Guide
	if ov := termSafe(strings.TrimSpace(g.Overview)); ov != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", ov)
	}
	if len(g.Prepare) > 0 {
		_, _ = fmt.Fprintln(w, "\nHave these ready:")
		for _, item := range g.Prepare {
			_, _ = fmt.Fprintf(w, "  - %s\n", termSafe(item))
		}
	}
	if g.Files != "" {
		_, _ = fmt.Fprintf(w, "Files: %s\n", termSafe(g.Files))
	}
	_, _ = fmt.Fprintln(w)
}

// termSafe is author text as a terminal may show it: every control
// character but a newline or a tab becomes "?", so a guide cannot send
// escape sequences (colours, cursor moves, a window title, a hyperlink) to
// the terminal it is printed on. The core refuses them in the single-line
// parts already; the overview may span lines, so it is cleaned here.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '?'
		}
		return r
	}, s)
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
	if !stdinIsTTY(cmd.InOrStdin()) || opts.yes || opts.dryRun || opts.plan {
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

// planHash is the whole plan hash — the receipt's runPlanSha256, over the
// canonical indented form spec.Plan.Hash defines, not over the compact
// bytes the JSON output carries; a client matches it, it does not re-derive it.
func planHash(plan *spec.Plan) string {
	h, err := plan.Hash()
	if err != nil {
		return "unhashable"
	}
	return h
}

// resolveOutDir picks and creates the output folder (RQ3).
func resolveOutDir(flag string, meta *installdir.Meta, runID string) (string, error) {
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
func materializeRegistry(cmd *cobra.Command, ref, home string, opts runOpts) (*installdir.Meta, string, error) {
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

	installDir := installdir.ReleaseDir(home, res.Namespace, res.Name, res.Version, res.Digest)
	if meta, ok := installdir.Load(installDir, res.Digest); ok {
		// The registry's word today outranks the cache's memory: an
		// install cached as unsigned by a CLI that did not know the
		// unscanned state, against a registry that now says the version
		// was admitted without a scan, is unscanned, and the consent
		// asked for is that one.
		if res.ScanStatus != nil && *res.ScanStatus == apiclient.ResolveResultScanStatusSkipped && meta.Attestation.State != trust.StateUnscanned {
			meta.Attestation = receipt.Attestation{State: trust.StateUnscanned}
		}
		// The cache remembers the verdict, not the consent: an unsigned
		// release still needs --allow-unsigned on every run, or the gate
		// would be one-shot per digest (agentry-cli#124), and an
		// unscanned one --allow-unscanned.
		if err := cachedAttestationGate(meta, opts.allowUnsigned, opts.allowUnscanned); err != nil {
			return nil, "", engine.Classify(engine.ExitVerify, err)
		}
		narrateCachedConsent(cmd.ErrOrStderr(), meta, opts.plan)
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
	att, err := verifyRunAttestation(cmd, reg, token, res, computed, opts.allowUnsigned, opts.allowUnscanned, opts.plan)
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
	// The verified archive's own manifest, held to the publish rules before
	// it is cached as a verified install: the resolve response's copy is
	// not covered by the digest, and the registry's validation is not ours.
	m, err := manifestFromWorkspace(staging)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	if err := requireValidManifest(cmd, m, fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version)); err != nil {
		return nil, "", err
	}

	meta := &installdir.Meta{
		Ref:       fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version),
		Namespace: res.Namespace, Name: res.Name, Version: res.Version,
		Digest: res.Digest, Source: "registry", Attestation: att,
	}
	// The cache holds the verdict and never the consent, which is this
	// run's alone: the next run is asked again.
	cached := *meta
	cached.Attestation.Consent = ""
	if err := installdir.Write(installDir, staging, &cached); err != nil {
		return nil, "", err
	}
	return meta, installDir, nil
}

// narrateCachedConsent says on stderr what a run from the cache is doing
// with a release the gate would have refused, the way a fresh materialize
// does: a cached unsigned or unscanned release is not "verified", and the
// flag that admits it is named on every run.
func narrateCachedConsent(errOut io.Writer, meta *installdir.Meta, plan bool) {
	doing := "running"
	if plan {
		doing = "a run would proceed"
	}
	switch meta.Attestation.State {
	case trust.StateUnscanned:
		_, _ = fmt.Fprintf(errOut, "warning: unscanned — %s %s (cached install), which the registry admitted without a scan, because --allow-unscanned was given\n", doing, meta.Ref)
	case trust.StateUnsigned, "local":
		_, _ = fmt.Fprintf(errOut, "warning: %s — %s %s (cached install) because --allow-unsigned was given\n", meta.Attestation.State, doing, meta.Ref)
	default:
		_, _ = fmt.Fprintf(errOut, "using verified install %s\n", meta.Ref)
	}
}

// verifyRunAttestation mirrors the install gate (cmd_install.go) but also
// returns the attestation facts the result envelope reports.
// plan makes every verdict reportable rather than a refusal: a preview
// names what a run would stop on (attestationRefusal), and materializes
// into a temp home that outlives nothing.
func verifyRunAttestation(cmd *cobra.Command, reg, token string, res *apiclient.ResolveResult, archiveDigest string, allowUnsigned, allowUnscanned, plan bool) (receipt.Attestation, error) {
	att := attestationOf(cmd.Context(), reg, token, res, archiveDigest)
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
		// Under --plan the flag is forced so the preview can go on, and
		// the refusal is named by attestationRefusal from the real one:
		// the line here says what a run would do, not what was given.
		if plan {
			_, _ = fmt.Fprintf(errOut, "attestation:   unsigned — a run of %s/%s@%s proceeds only with --allow-unsigned\n", res.Namespace, res.Name, res.Version)
		} else {
			_, _ = fmt.Fprintf(errOut, "warning: unsigned — running %s/%s@%s because --allow-unsigned was given\n", res.Namespace, res.Name, res.Version)
		}
		return receipt.Attestation{State: trust.StateUnsigned, Consent: receipt.ConsentAllowUnsigned}, nil
	case trust.StateUnscanned:
		// Its own consent: --allow-unsigned says nothing about a version
		// the registry never scanned.
		if !allowUnscanned {
			return receipt.Attestation{}, fmt.Errorf("unscanned — the registry admitted %s/%s@%s without a scan: no findings, no report and no attestation stand behind it; re-run with --allow-unscanned to run it anyway, accepting that nothing about its contents was checked",
				res.Namespace, res.Name, res.Version)
		}
		if plan {
			_, _ = fmt.Fprintf(errOut, "attestation:   unscanned — the registry admitted %s/%s@%s without a scan; a run proceeds only with --allow-unscanned\n", res.Namespace, res.Name, res.Version)
		} else {
			_, _ = fmt.Fprintf(errOut, "warning: unscanned — running %s/%s@%s, which the registry admitted without a scan, because --allow-unscanned was given\n", res.Namespace, res.Name, res.Version)
		}
		return receipt.Attestation{State: trust.StateUnscanned, Consent: receipt.ConsentAllowUnscanned}, nil
	default:
		if plan {
			_, _ = fmt.Fprintf(errOut, "attestation:   %s — a run would refuse (%s)\n", att.State, att.Detail)
			return receipt.Attestation{State: string(att.State), KeyID: att.KeyID}, nil
		}
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
func materializeLocal(cmd *cobra.Command, dir, home string, allowUnsigned bool) (*installdir.Meta, string, error) {
	if !allowUnsigned {
		return nil, "", engine.Classify(engine.ExitVerify,
			errors.New("--local packages are unsigned by definition; pass --allow-unsigned to run one"))
	}
	m, err := manifestFromWorkspace(dir)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	if err := requireValidManifest(cmd, m, "local package"); err != nil {
		return nil, "", err
	}
	digest, err := localTreeDigest(dir)
	if err != nil {
		return nil, "", err
	}
	installDir := installdir.ReleaseDir(home, m.Namespace, m.Name, m.Version, digest)
	meta := &installdir.Meta{
		Ref:       fmt.Sprintf("%s/%s@%s", m.Namespace, m.Name, m.Version),
		Namespace: m.Namespace, Name: m.Name, Version: m.Version,
		Digest: digest, Source: "local",
		Attestation: receipt.Attestation{State: "local", Consent: receipt.ConsentAllowUnsigned},
	}
	if cached, ok := installdir.Load(installDir, digest); ok {
		return cached, installDir, nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "local package %s (%s)\n", meta.Ref, installdir.DigestSuffix(digest))
	if err := installdir.Write(installDir, dir, meta); err != nil {
		return nil, "", err
	}
	return meta, installDir, nil
}

// ensurePlan reuses the installed plan when nothing it depends on changed
// (§R6/§R9), otherwise recompiles. A reused plan.json is first checked
// against the integrity record its compile wrote under the runner home's
// key (agentry-cli#266): a plan with no record this home can check is
// recompiled, and one that does not match its record — changed after it
// was compiled — refuses the run (exit 10).
func ensurePlan(errOut io.Writer, home string, m *manifest.Manifest, meta *installdir.Meta, installDir, workspaceDir string,
	pol *policy.Policy, polSHA, adapterName string, serverSpecs []spec.MCPServerSpec, mode manifest.Mode,
) (*spec.Plan, string, error) {
	key, err := compile.IntegrityKey(home)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCLI, err)
	}
	// One plan per run mode (plan/<mode>/); a plan/plan.json from the
	// single-plan layout is a stale install and is cleared.
	if _, err := os.Stat(filepath.Join(installDir, "plan", "plan.json")); err == nil {
		_ = os.RemoveAll(filepath.Join(installDir, "plan"))
	}
	planDir := filepath.Join(installDir, "plan", string(mode))
	runnerImage := pol.RunnerImageTag(runnerimage.ContentVersion())
	b, rerr := compile.ReadInstalledPlan(key, planDir)
	var tampered *compile.TamperedPlanError
	switch {
	case errors.As(rerr, &tampered):
		return nil, "", engine.Classify(engine.ExitVerify, rerr)
	case errors.Is(rerr, compile.ErrNoIntegrityRecord):
		// Read again for the notice only, never for use: a plan from an
		// older CLI has no record and says which CLI it was.
		raw, _ := os.ReadFile(filepath.Join(planDir, "plan.json")) //nolint:gosec // our install layout
		if v := cachedPlanVersion(raw); v != "" && v != spec.PlanVersion {
			_, _ = fmt.Fprintf(errOut, "installed plan was compiled by an older CLI (plan version %s, want %s) — recompiling\n", v, spec.PlanVersion)
		} else {
			_, _ = fmt.Fprintln(errOut, "installed plan has no integrity record from this runner home — recompiling")
		}
	}
	if rerr == nil {
		// A plan from an older CLI is recompiled in place, and says so: a
		// cached install used to stay stale after an upgrade until its
		// first execution failed (agentry-cli#60).
		if v := cachedPlanVersion(b); v != "" && v != spec.PlanVersion {
			_, _ = fmt.Fprintf(errOut, "installed plan was compiled by an older CLI (plan version %s, want %s) — recompiling\n", v, spec.PlanVersion)
		}
		if plan, perr := spec.ParsePlan(b); perr == nil &&
			plan.PlanVersion == spec.PlanVersion &&
			plan.Mode == string(mode) &&
			plan.Digest == meta.Digest &&
			plan.PolicySHA256 == polSHA &&
			plan.Harness.Adapter == adapterName &&
			reflect.DeepEqual(plan.MCPServers, serverSpecs) {
			switch {
			case plan.CompiledBy != version.Build():
				// Same schema, another build: the compiler's output may
				// differ (agentry-cli#105 changed what a server's catalog
				// entry carries without touching the schema). An unstamped
				// build is told apart by its binary (agentry-cli#144).
				_, _ = fmt.Fprintf(errOut, "installed plan was compiled by another CLI build (%s, this is %s) — recompiling\n", cmp.Or(plan.CompiledBy, "unknown"), version.Build())
			case plan.Proxy.Image != runnerImage:
				_, _ = fmt.Fprintf(errOut, "installed plan names runner image %s, this CLI builds %s — recompiling\n", plan.Proxy.Image, runnerImage)
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
		Manifest:     m,
		Mode:         mode,
		Digest:       meta.Digest,
		Source:       meta.Source,
		WorkspaceDir: workspaceDir,
		Policy:       pol,
		PolicySHA:    polSHA,
		AdapterName:  adapterName,
		RunnerImage:  runnerImage,
		CompiledBy:   version.Build(),
		MCPServers:   serverSpecs,
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

// cachedPlanVersion reads just the planVersion of a stored plan.json, so a
// plan the current schema cannot parse still names the version it has.
func cachedPlanVersion(b []byte) string {
	var v struct {
		PlanVersion string `json:"planVersion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	return v.PlanVersion
}

// confirmRunCapabilities applies the install-time consent semantics to
// runs, keyed on the runner install dir: first run asks about egress/exec,
// upgrades ask again only when the contract widened (§R1).
func confirmRunCapabilities(cmd *cobra.Command, m *manifest.Manifest, meta *installdir.Meta, home string, opts runOpts, mode manifest.Mode) error {
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
	key := installdir.ConsentKey(home, meta.Namespace, meta.Name)
	state, serr := installed.Load()
	if serr != nil {
		_, _ = fmt.Fprintf(out, "warning: %v — treating this as a first run\n", serr)
		state = installed.State{}
	}
	next := installed.Snapshot(m.Capabilities)
	if err := confirmCapabilityChange(cmd, out, &state, key, meta.Version, next, m.Capabilities, opts.yes); err != nil {
		return err
	}
	// An install owns the agent's directory (managed, agentry-cli#190); a
	// run that re-consents keeps that, and records nothing managed of its
	// own — its cache is not an install.
	managed := false
	if prev := state.Find(key); prev != nil {
		managed = prev.Managed
	}
	state.Upsert(key, meta.Version, next, managed)
	if err := installed.Save(state); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — the next run will re-ask about capabilities\n", err)
	}
	return nil
}

// requireValidManifest holds a manifest to the registry's own publish rules
// before anything is compiled from it. Warnings (e.g. the V5 "*" egress
// flag) surface but do not block — same split the registry applies at
// publish; the RUN-time gate for wildcard egress is the policy check in the
// compiler (§R4). A blocking error is a compile refusal naming every rule.
// Both materialize paths call this before caching: a --local tree because
// nobody else has, and a registry release because the registry's
// validation is that registry's — a self-hosted one whose rules predate
// today's, or a compromised one, is exactly the trust the digest check
// refuses to extend elsewhere (security review 2026-09-20 §4.2).
func requireValidManifest(cmd *cobra.Command, m *manifest.Manifest, what string) error {
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
		return engine.Classify(engine.ExitCompile,
			fmt.Errorf("%s manifest invalid:\n  %s", what, strings.Join(msgs, "\n  ")))
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

// ensureProfile loads — or, on a terminal, guides the creation of — the
// BriefAgent's MCP profile for this signature (RUNNER-PLAN-2 §P2/§P3).
// Returns the profile (nil when none applies), its directory, and the
// effective keep choice: flag > stored profile > policy default.
func ensureProfile(cmd *cobra.Command, home string, meta *installdir.Meta, m *manifest.Manifest,
	pol *policy.Policy, opts runOpts,
) (*profile.Profile, string, bool, error) {
	dir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, installdir.DigestSuffix(meta.Digest))
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

	sayGuide(cmd.ErrOrStderr(), m, opts)
	// The key help rides with the guide: printed on a text run, and like
	// the overview left out of a --output-format json one.
	var help map[string]string
	if m.Guide != nil && opts.outputFormat != "json" {
		help = m.Guide.Connect
	}
	prompter := &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	newProf, err := setup.GuidedProfile(prompter, setup.GuidedProfileOpts{
		Ref:             meta.Ref,
		Digest:          meta.Digest,
		CapabilityLines: capabilityLines(m.Capabilities),
		Secrets:         secrets,
		DefaultKeep:     keep,
		KeepDecided:     keepDecided,
		Help:            help,
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

// runnerCLIVersion feeds the runner image tag derivation (§R7: a new CLI
// version is what triggers a runner image rebuild).
