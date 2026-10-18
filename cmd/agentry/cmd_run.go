// `agentry run`: resolve → verify → materialize → compile → enforce →
// run → receipt, one agent per invocation. First use of a digest IS the
// install: the verified workspace, the compiled plan, and the
// pulled/built images all persist under ~/.agentry/runner so later runs
// execute without re-deriving anything. The verify gate is the same one
// `agentry install` applies — a run is an install that also executes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/pkg/termtext"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry-cli/pkg/tui"
)

// runOpts carries the run command's flags.
type runOpts struct {
	// consentHome is the real runner home, where a local install
	// lives; --plan materializes into a temp home, and a local install
	// is looked up here all the same. Empty: the home.
	consentHome string
	mode        string
	intake      []string
	intakeToken []string
	// hostFlags are the target hosts' sign-in flags.
	hostFlags    targetHostFlags
	input        string
	out          string
	outputFormat string
	// guideShown is set once the release's guide has been printed on this
	// terminal, so the profile setup and the intake do not print it twice.
	guideShown *bool
	// progress is where the run has got to, for the envelope of a run that
	// fails before the engine writes one.
	progress           *runProgress
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
	// consent: resolve, verify, compile, and print what a run would
	// enforce and what it would ask or refuse — asking nothing, recording
	// nothing, and materializing into a temp dir rather than the runner
	// cache. --dry-run keeps its meaning (the plan for a person who
	// consented on the terminal).
	plan         bool
	keepRunDir   bool
	listAdapters bool
	files        []string
	knowledge    []string
	// voice and noVoice override the Writing Voice's always-attach flag
	// for one run; voiceFile attaches a VOICE.md a driver names.
	voice, noVoice bool
	voiceFile      string
	// reportTheme is --report-theme, reportDesign --report-design.
	reportTheme string
	// deliverTo is --deliver-to, the folder the run copies its deliverables to.
	deliverTo string
	// destinations is --destination: the saved destinations the run sends its
	// result and deliverables to.
	destinations       []string
	reportDesign       string
	keepProfile        string // "": unset; "true"/"false": the flag decided
	reconfigureProfile bool
	// sessionProtocol drives an interactive session from JSON-line commands
	// on stdin instead of the terminal chat, with its events on descriptor 3.
	sessionProtocol bool
	// protocolEvents is descriptor 3, opened once the flag is checked.
	protocolEvents io.Writer
	// sessionInputs is the directory the protocol's driver stages the files
	// it adds in; an add names a file directly in it.
	sessionInputs string
	// connectionsFile hands the run access tokens a host already
	// fetched: the store is not opened and nothing is refreshed.
	connectionsFile string
	// reviewInputs is the registry whose AI Review reads the inputs' text
	// before the agent starts, with the job's credential in $envJobToken
	// (a hosted run's host).
	reviewInputs string
}

// newRunCmd builds the run command.
func newRunCmd() *cobra.Command {
	var opts runOpts
	cmd := &cobra.Command{
		Use:   "run [ns/name[@range]]",
		Short: "Verify, install, and execute one QuickAgent in an isolated container",
		Long: "Run a single-purpose QuickAgent once:\n\n" +
			"  agentry run ns/name@^1.2 --input task.json --out ./reports\n\n" +
			"Task input: --input FILE, else piped stdin; with no task (or an empty one) the\n" +
			"agent is sent its own instruction and runs that. The result goes to stdout\n" +
			"(--output-format text|json); reports the QuickAgent writes and the full\n" +
			"receipts land in the output folder. Every failure class has its own exit\n" +
			"code: 0 ok, 1 cli (interrupted, unknown adapter, intake), 2 usage,\n" +
			"10 verify, 11 policy/secret, 12 compile, 13 gateway/docker, 14 agent, 15\n" +
			"timeout, 16 budget, 17 output schema.\n\n" +
			"Run modes: --mode autonomous runs from the files and ends; --mode interactive\n" +
			"opens a chat with the agent in this terminal under exactly the same controls\n" +
			"(/add a file, /finish for the final result, /quit, /caps). A release that\n" +
			"declares both modes is asked on a terminal and runs autonomous otherwise.\n" +
			"--session-protocol drives an interactive session from a program instead,\n" +
			"with no terminal: JSON-line commands on stdin ({\"send\":{\"text\":…}},\n" +
			"{\"add\":{\"path\":…}}, {\"end\":{\"finish\":true|false}}; end of input ends\n" +
			"without a result), one JSON line per session event on descriptor 3, the\n" +
			"result on stdout as for any run (the envelope under --output-format json),\n" +
			"usage narrated on stderr. The task comes only from --input; an add names a\n" +
			"file directly in --session-inputs DIR.\n\n" +
			"First run of a release verifies it exactly like `agentry install` (digest +\n" +
			"DSSE attestation; unsigned needs --allow-unsigned; a release the registry\n" +
			"admitted without a scan is unscanned and needs --allow-unscanned, its own\n" +
			"decision), then compiles the run plan and installs the gateway, harness\n" +
			"and runner images. Later runs reuse the verified install, the plan and the\n" +
			"images; an unsigned or local release needs --allow-unsigned on every run,\n" +
			"an unscanned one --allow-unscanned. --prepare stops after that install\n" +
			"step; --dry-run prints the\n" +
			"compiled plan and touches no docker. --local DIR runs an unpublished\n" +
			"QuickAgent directory (treated as unsigned).\n\n" +
			"For drivers (a hosted run's worker): AGENTRY_RUN_GRANT is sent as the\n" +
			"X-Agentry-Run-Grant header on every request to the --registry host and\n" +
			"nowhere else, for a registry prefix that answers for one run;\n" +
			"AGENTRY_PIN_REGISTRY names the registry whose pinned signing keys verify\n" +
			"the release, so pointing --registry at such a prefix keeps the pins made\n" +
			"at login.\n\n" +
			"Tools: QuickAgents get exactly the tools their manifest capabilities grant,\n" +
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
			opts.progress = &runProgress{stage: receipt.StagePolicy, ref: ref}
			err := runRun(cmd, ref, opts)
			if err != nil && opts.outputFormat == "json" && !opts.progress.printed {
				printFailure(cmd.Context(), cmd, opts.progress, err)
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.input, "input", "", "task input file (default: piped stdin, else the agent's own instruction)")
	f.StringVar(&opts.out, "out", "", "output folder for reports and receipts (default ./agentry-out/<ns>-<name>-<runid>)")
	f.StringVar(&opts.outputFormat, "output-format", "text", "stdout format: text | json (json = the full result envelope)")
	f.StringVar(&opts.eventsPath, "events", "", "append the harness event stream to FILE as JSON lines while the run is live (the receipt keeps its own copy either way)")
	f.StringVar(&opts.adapterName, "adapter", "", "harness adapter (default "+adapter.DefaultName+", or "+adapter.CodexImageAdapterName+" when the model is paid by your ChatGPT plan); see --list-adapters")
	f.StringVar(&opts.local, "local", "", "run an unpublished QuickAgent DIRECTORY instead of a registry ref")
	f.StringVar(&opts.pin, "pin", "", "require the resolved archive digest to equal this sha256:… value")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	f.IntVar(&opts.timeout, "timeout", 0, "override the policy timeout (seconds)")
	f.IntVar(&opts.maxTurns, "max-turns", -1, "override the policy tool-call budget (0 = unlimited)")
	f.Float64Var(&opts.maxCost, "max-cost", -1, "override the policy cost budget in USD; the run is killed (exit 16) once the harness reports more (0 = unlimited)")
	f.BoolVar(&opts.yes, "yes", false, "skip the capability confirmation prompt (CI)")
	f.BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "run a release with no registry attestation (required for --local)")
	f.BoolVar(&opts.allowUnscanned, "allow-unscanned", false, "run a release the registry admitted without a scan (no findings, no report, no attestation)")
	f.BoolVar(&opts.allowVulnerableMCP, "allow-vulnerable-mcp", false, "run even when the registry's gate rates a declared MCP server's advisory high or critical (recorded in the receipt)")
	f.BoolVar(&opts.prepare, "prepare", false, "verify, compile, and install images, then stop without executing")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the compiled run plan and exit (no docker required)")
	f.BoolVar(&opts.plan, "plan", false, "preview for a client: compile the run plan and report the attestation, the consent a run would ask for and the refusal it would meet; asks nothing, records nothing but a first-use pin of the registry's keys (with --json, the machine-readable shape)")
	f.StringVar(&opts.mode, "mode", "", "run mode: autonomous | interactive (default: the release's only mode; asked on a terminal when it declares both, autonomous otherwise)")
	f.BoolVar(&opts.keepRunDir, "keep-run-dir", false, "keep the per-run scratch dir for debugging (secrets are still deleted)")
	f.StringVar(&opts.reviewInputs, "review-inputs", "", "have AI Review read every input's text through this REGISTRY's model-input scan, with the job's credential in $"+envJobToken+", before the agent starts; an input it flags, or cannot read, refuses the run (a hosted run's host)")
	f.StringVar(&opts.connectionsFile, "connections-file", "", "take each OAuth-signed-in MCP server's access token from this 0600 JSON FILE instead of this computer's connections, refreshing nothing (a hosted run's host)")
	f.BoolVar(&opts.listAdapters, "list-adapters", false, "list harness adapters and exit")
	f.StringArrayVar(&opts.files, "file", nil, "file or directory to provide to the run read-only under /inputs (repeatable)")
	f.StringArrayVar(&opts.knowledge, "knowledge", nil, "knowledge pack to attach read-only at /knowledge/<name>, searched with the knowledge tools: a pack kept on this computer by NAME, or a pack DIRECTORY (repeatable)")
	f.BoolVar(&opts.voice, "voice", false, "attach the Writing Voice kept on this computer even when it is not set to attach to every run")
	f.BoolVar(&opts.noVoice, "no-voice", false, "do not attach the Writing Voice to this run")
	f.StringVar(&opts.reportTheme, "report-theme", "", "the built-in report theme this run's PDFs take: "+strings.Join(toolsrv.BuiltinThemes(), " | ")+" (default: your account's report design when signed in, else report.design or report.theme in the config, else plain)")
	f.StringVar(&opts.reportDesign, "report-design", "", "the report design this run's PDFs take: a FILE holding a report design (theme document version 2); instead of --report-theme")
	f.StringVar(&opts.deliverTo, "deliver-to", "", "copy the run's deliverables, the files it was for, to FOLDER/<QuickAgent>/<date time> <title>/ once it succeeds (default: deliver.folder in the config, else nowhere); the output folder is left as it is")
	f.StringArrayVar(&opts.destinations, "destination", nil, "send the run's result and deliverables to this saved destination once it succeeds (repeatable; default: the QuickAgent's default set, else nowhere); see agentry destinations")
	f.StringVar(&opts.voiceFile, "voice-file", "", "attach the VOICE.md in FILE as the run's Writing Voice (for a driver such as a sandbox host)")
	f.StringArrayVar(&opts.intake, "intake", nil, "answer an intake question non-interactively: id=value (repeatable; unanswered questions are asked on a terminal); a repository question takes its https address, with #branch, #tag or #commit for another ref; a url question takes one file's https address")
	f.StringArrayVar(&opts.intakeToken, "intake-token", nil, "the access token for an input source: id=ENV_NAME, the name of an environment variable holding it (repeatable; otherwise the key saved for the source's host in the system keychain, then a prompt on a terminal, then none)")
	addTargetHostFlags(cmd, &opts.hostFlags)
	addHostKeyPinFlag(cmd, &opts.hostFlags)
	f.StringVar(&opts.keepProfile, "keep-profile", "", "keep the QuickAgent's MCP profile after the run: true | false (default: the profile's own setting, else policy)")
	f.BoolVar(&opts.reconfigureProfile, "reconfigure-profile", false, "re-run the guided MCP-profile setup even when a profile exists")
	f.BoolVar(&opts.sessionProtocol, "session-protocol", false, "drive an interactive session from JSON-line commands on stdin, with its events as JSON lines on descriptor 3 and no terminal (for a program such as a hosted run's host)")
	f.StringVar(&opts.sessionInputs, "session-inputs", "", "with --session-protocol: the DIRECTORY the driver stages files in; an add names a regular file directly in it (without it, the session takes no files)")
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
	_, _ = fmt.Fprintln(out, "planned: goose")
	return nil
}

// runnerHome is ~/.agentry/runner, or $AGENTRY_RUNNER_HOME (tests and odd
// deployments) — policy.Home, so the policy's default path and everything
// else under the home cannot resolve differently.
func runnerHome() (string, error) { return policy.Home() }

// runRun executes the whole run lifecycle.
func runRun(cmd *cobra.Command, ref string, opts runOpts) error {
	if (ref == "") == (opts.local == "") {
		return errors.New("pass exactly one of ns/name[@range] or --local DIR")
	}
	if opts.outputFormat != "text" && opts.outputFormat != "json" {
		return fmt.Errorf("unknown --output-format %q (want text or json)", opts.outputFormat)
	}
	opts.guideShown = new(bool)
	// AI Review of the inputs is checked before anything is pulled: a
	// host that asks for it and gives no credential refuses here.
	review, err := inputReview(opts.reviewInputs)
	if err != nil {
		return err
	}
	// The report theme is chosen before anything starts, so a name that is
	// not a theme's refuses here. A preview asks no account.
	cfg, cerr := loadCLIConfig()
	if cerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; its report settings are not read\n", cerr)
	}
	var account accountTheme
	if !opts.plan && !opts.dryRun && !opts.prepare {
		account = readAccountTheme(registryURL())
	}
	reportTheme, err := chooseReportTheme(cmd.Context(), reportFlags{theme: opts.reportTheme, design: opts.reportDesign}, cfg, account, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	// Where the run delivers is chosen before anything starts too, so a
	// relative deliver.folder refuses here.
	deliverTo, err := chooseDeliverFolder(opts.deliverTo, cfg)
	if err != nil {
		return err
	}
	attached, detach, err := attachKnowledge(cmd, opts.knowledge)
	if err != nil {
		return err
	}
	defer detach()
	var attachedVoice *engine.AttachedVoice
	if !opts.plan && !opts.dryRun && !opts.prepare {
		if attachedVoice, err = attachVoice(cmd, opts); err != nil {
			return err
		}
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}

	// The receipt names the policy file and how it was chosen, beside its
	// digest.
	policyFile, err := policy.Resolve(opts.policyPath)
	if err != nil {
		return err
	}
	pol, polSHA, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	opts.progress.at(receipt.StageVerify)
	// Materialize: verified workspace + install metadata, cached per
	// digest. A --plan materializes into a temp home instead: a
	// preview writes nothing that outlives it, and an unsigned
	// release is reported rather than refused.
	var meta *installdir.Meta
	var installDir string
	// planRefusals is every refusal the gates before compile would meet —
	// the attestation and the declared servers' vulnerabilities — for
	// --plan: the preview exists so the person sees them all before
	// consenting, not only the first a run stops on.
	var planRefusals []runPlanRefusal
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
		matOpts.consentHome = consentHome
		matOpts.allowUnsigned = true
		matOpts.allowUnscanned = true
	}
	// integrity is what --plan says of the installed tree in the real
	// home: the preview materializes into a temp home, so the check a run
	// makes first is made here against the install a run would use.
	var integrity installIntegrity
	if opts.local != "" {
		if opts.plan && !opts.allowUnsigned {
			planRefusals = append(planRefusals, runPlanRefusal{Kind: refusalAttestation,
				Message: "a local workspace has no attestation; a run needs --allow-unsigned", Lift: receipt.ConsentAllowUnsigned})
		}
		integrity = integrityLocal
		meta, installDir, err = materializeLocal(cmd, opts.local, home, matOpts.allowUnsigned)
		// --pin holds a local directory to the digest it had when it was
		// pinned (a schedule's): an edited directory is
		// another release and does not run in its place.
		if err == nil && opts.pin != "" && meta.Digest != opts.pin {
			err = engine.Classify(engine.ExitVerify,
				fmt.Errorf("--pin mismatch: the directory %s is now %s, pin requires %s", opts.local, meta.Digest, opts.pin))
		}
	} else {
		meta, installDir, err = materializeRegistry(cmd, ref, home, matOpts)
		if err == nil && opts.plan {
			var r *runPlanRefusal
			integrity, r = installedIntegrity(consentHome, meta)
			if r != nil {
				// A run meets it before the attestation gate.
				planRefusals = append(planRefusals, *r)
			}
			if r := attestationRefusal(meta.Attestation, opts.allowUnsigned, opts.allowUnscanned); r != "" {
				planRefusals = append(planRefusals, runPlanRefusal{Kind: refusalAttestation, Message: r, Lift: attestationLift(meta.Attestation)})
			}
		}
	}
	if err != nil {
		return err
	}
	opts.progress.running(meta)
	opts.progress.at(receipt.StageCompile)
	workspaceDir := filepath.Join(installDir, "workspace")
	m, err := localrun.ManifestFrom(workspaceDir)
	if err != nil {
		return engine.Classify(engine.ExitCompile, err)
	}
	// An install cached without validation is held to the same rules
	// at run; the compiler refuses the mount shapes on its own
	// besides.
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		msgs := make([]string, 0, len(blocking))
		for _, e := range blocking {
			msgs = append(msgs, e.Error())
		}
		return engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid:\n  %s", meta.Ref, strings.Join(msgs, "\n  ")))
	}
	// The release's shipped packs, loaded once its manifest is valid: the
	// pack names are its knowledge.ships, held to the name rule by V11.
	shipped, err := shippedKnowledge(workspaceDir, m, meta)
	if err != nil {
		return err
	}
	if attached, err = withShipped(shipped, attached); err != nil {
		return err
	}
	shippedSizes := packSizes(shipped)
	sayKnowledge(cmd.ErrOrStderr(), shipped)

	// The capability gate: same semantics as install — egress/exec
	// (or a widened contract vs the last accepted run) needs consent.
	mode, err := resolveRunMode(cmd, opts, m)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if opts.sessionInputs != "" && !opts.sessionProtocol {
		return engine.Classify(engine.ExitUsage, errors.New("--session-inputs is for --session-protocol"))
	}
	if opts.sessionProtocol && mode != manifest.ModeInteractive {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--session-protocol drives an interactive session; this run's mode is %s", mode))
	}
	if mode == manifest.ModeInteractive && !opts.dryRun && !opts.plan {
		// The terminal rule stands for everything but the session protocol,
		// whose input is its commands and whose stdout is the envelope's.
		if !opts.sessionProtocol {
			if !stdinIsTTY(cmd.InOrStdin()) {
				return engine.Classify(engine.ExitUsage, errors.New("an interactive run needs a terminal on stdin (the chat is the input); pipe a task to --mode autonomous instead, or drive the session with --session-protocol"))
			}
			if opts.outputFormat == "json" {
				return engine.Classify(engine.ExitUsage, errors.New("--output-format json is not available for an interactive run: the transcript and receipts are the record"))
			}
		}
		if opts.eventsPath != "" {
			return engine.Classify(engine.ExitUsage, errors.New("--events is not available for an interactive run: the session streams its own events to the transcript"))
		}
		if opts.sessionProtocol {
			if opts.sessionInputs != "" {
				if fi, err := os.Stat(opts.sessionInputs); err != nil || !fi.IsDir() {
					return engine.Classify(engine.ExitUsage, fmt.Errorf("--session-inputs %s is not a directory", opts.sessionInputs))
				}
			}
			w, err := protocolEventsOut()
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			opts.protocolEvents = w
			ignoreBrokenPipes()
		}
	}

	var consent runPlanConsent
	opts.progress.at(receipt.StageConsent)
	if opts.plan {
		consent = describeRunConsent(m, meta, consentHome)
	} else if err := confirmRunCapabilities(cmd, m, meta, consentHome, opts, mode, shippedSizes); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	opts.progress.at(receipt.StageCompile)
	adapterName := opts.adapterName
	if adapterName == "" {
		adapterName = adapter.DefaultFor(pol)
	}
	ad, err := adapter.Get(adapterName)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}

	// Declared MCP servers: resolve the manifest pins — offline when the
	// signature's store already matches them — so the compiler can bake
	// the launch specs into the plan. Installation into the store happens
	// with the image install below; --dry-run stops at resolution and
	// touches no docker.
	profDir := profile.Dir(home, meta.Namespace, meta.Name, meta.Version, installdir.DigestSuffix(meta.Digest))
	serversDir := mcpinstall.StoreDir(profDir)
	var resolvedServers []mcpinstall.Resolved
	var serverSpecs []spec.MCPServerSpec
	var vulnLine string
	if len(m.Capabilities.MCPServers) > 0 {
		opts.progress.at(receipt.StageMCPServers)
		// The registry's vulnerability verdict on each declared server: refused
		// on an advisory the gate rates high or critical unless the operator
		// says otherwise; fail-open when the registry cannot answer. The
		// receipt records the outcome either way.
		statuses := mcpinstall.CheckVulnerabilities(cmd.Context(), nil, registryURL(), m.Capabilities.MCPServers)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "declared MCP servers, per the registry: %s\n", mcpinstall.Summary(statuses))
		var refuseErr error
		vulnLine, refuseErr = vulnerabilityVerdict(statuses, registryURL(), opts.allowVulnerableMCP, pol.MCPServers.AllowVulnerable)
		if refuseErr != nil {
			if !opts.plan {
				return engine.Classify(engine.ExitPolicy, refuseErr)
			}
			planRefusals = append(planRefusals, runPlanRefusal{Kind: refusalVulnerableServer, Message: refuseErr.Error(), Lift: "--allow-vulnerable-mcp"})
		}
		resolvedServers, err = serverResolutions(cmd.Context(), cmd.ErrOrStderr(), serversDir, pol, m)
		if err != nil {
			return engine.Classify(engine.ExitVerify, err)
		}
		serverSpecs = mcpinstall.Specs(resolvedServers, m.Capabilities.MCPServers, pol.Images)
	}

	// The install is checked again before the plan is compiled from it: a
	// person can sit at the consent prompt for minutes, and a plan compiled
	// from files edited meanwhile would be kept and reused. The run's own
	// copy is checked once more by the engine (CheckWorkspace).
	if opts.local == "" && !opts.plan {
		if err := recheckInstall(installDir, meta); err != nil {
			return err
		}
	}
	opts.progress.at(receipt.StageCompile)
	plan, planDir, err := localrun.EnsurePlan(localrun.PlanInputs{
		Home: home, Manifest: m, Meta: meta, InstallDir: installDir, WorkspaceDir: workspaceDir,
		Policy: pol, PolicySHA: polSHA, Adapter: adapterName, Servers: serverSpecs, Mode: mode,
		CompiledBy: version.Build(), Log: cmd.ErrOrStderr(),
	})
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
		return printRunPlan(cmd, plan, meta, m, mode, consent, integrity, planRefusals)
	}
	if opts.dryRun {
		return printDryRun(cmd, plan, planDir)
	}

	// Where the run sends its result is chosen before anything is installed
	// too: a name that is not saved, or has no signing secret, refuses here,
	// and a hosted run's host takes none.
	var destinations []destination.Resolved
	if opts.reviewInputs != "" {
		if len(opts.destinations) > 0 {
			return engine.Classify(engine.ExitUsage, errors.New("--destination is for a run on this computer: a hosted run's host delivers nowhere"))
		}
	} else {
		var fromDefault bool
		if destinations, fromDefault, err = chooseDestinations(opts.destinations, plan.Ref, opts.local != ""); err != nil {
			return err
		}
		if fromDefault {
			names := make([]string, len(destinations))
			for i, d := range destinations {
				names[i] = d.Name
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "delivering to %s when the run succeeds (the default set you chose for %s)\n", strings.Join(names, ", "), strings.SplitN(plan.Ref, "@", 2)[0])
		}
	}
	// Gateway + harness + runner images: the install step.
	opts.progress.at(receipt.StageImages)
	docker := newDockerClient()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	// A plan run is refused for a hosted run and with a cost budget
	// before anything is installed for it.
	if err := refuseSignInRun(plan, opts); err != nil {
		return err
	}
	// And so is a target host.
	if err := refuseHostedTargetHosts(plan, opts); err != nil {
		return err
	}
	if err := gateway.EnsureInstalledFor(cmd.Context(), docker, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr(), plan); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	// The per-QuickAgent exec image the plan names: built with the
	// operator's catalogued packages; the versions it resolved go
	// into the receipt.
	var execPackages []string
	if plan.ExecImage != nil {
		var err error
		execPackages, err = gateway.EnsureExecImage(cmd.Context(), docker, runnerTag, *plan.ExecImage, homeImageDir(home), cmd.ErrOrStderr())
		if err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
	}
	// The host link's image, so a --prepare leaves nothing to build at
	// the run; the engine builds it otherwise.
	if plan.HostLink != nil {
		hl := targethost.HostLinkImage(runnerTag, plan.HostLink.Base)
		if _, err := targethost.EnsureImage(cmd.Context(), docker, hl, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
	}
	if len(resolvedServers) > 0 {
		// A CLI killed mid-install left its builder running under the
		// same fixed name: sweep what dead runs left before building.
		// engine.Run sweeps again; the second finds nothing.
		engine.SweepAbandoned(cmd.Context(), docker, cmd.ErrOrStderr())
		opts.progress.at(receipt.StageMCPServers)
		if err := mcpinstall.EnsureInstalled(cmd.Context(), docker, resolvedServers, serversDir,
			pol.MCPRegistries, pol.Images, pol.Docker, cmd.ErrOrStderr()); err != nil {
			return engine.Classify(engine.ExitGateway, err)
		}
	}

	// The QuickAgent's MCP profile: load or, on a terminal, run guided
	// setup. Part of the install step, so --prepare includes it;
	// --dry-run (above) never prompts.
	opts.progress.at(receipt.StageProfile)
	prof, profDir, keep, err := ensureProfile(cmd, home, meta, m, pol, opts)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	// The connections of the remote servers that sign in by OAuth: asked
	// for at Start on a terminal, refreshed under their locks before the
	// sandbox starts. --prepare makes them too, as it does the profile.
	var conns *runConnections
	if opts.connectionsFile != "" {
		conns, err = connectionsFromFile(opts.connectionsFile, m)
	} else {
		conns, err = ensureConnections(cmd, home, meta, m)
	}
	if err != nil {
		return err
	}
	if conns.line != "" {
		if plan.Enforcement == nil {
			plan.Enforcement = map[string]string{}
		}
		plan.Enforcement["mcpServers.connections"] = conns.line
	}

	if opts.prepare {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "prepared %s (plan %s, images installed) — run again without --prepare to execute\n",
			plan.Ref, shortHash(plan))
		return nil
	}

	var profSecrets map[string]string
	if prof != nil {
		profSecrets = prof.Secrets
	}

	opts.progress.at(receipt.StageIntake)
	answers, err := collectIntake(cmd, m, opts, func(name string) bool {
		return engine.SecretResolvable(name, profSecrets, pol.SecretSource)
	})
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := resolveSourceTokens(cmd, answers, opts); err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if err := readyTargetHosts(cmd, answers, plan, pol, home, opts); err != nil {
		return err
	}

	opts.progress.at(receipt.StagePrepare)
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

	overrides := secretOverrides(profSecrets, answers)
	signIn, renewSignIn, err := runModelSignIn(cmd.Context(), home, plan, hostedRun(opts))
	if err != nil {
		return err
	}
	// A connected account's key is readied in the keychain the run reads.
	if err := refreshConnectedKey(cmd.Context(), home, plan, hostedRun(opts)); err != nil {
		return err
	}

	// A driver metering a live run reads the harness's usage two ways, both
	// beside the envelope rather than instead of it.
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
		InputReview:      review,
		Hosted:           hostedFor(opts),
		Plan:             plan,
		ExecPackages:     execPackages,
		PlanDir:          planDir,
		WorkspaceSrc:     workspaceDir,
		CheckWorkspace:   installedCopyCheck(opts, installDir, meta),
		RunRoot:          installDir,
		RunnerHome:       home,
		OutDir:           outDir,
		Task:             runTask(cmd, opts),
		InputPaths:       opts.files,
		Knowledge:        attached,
		Voice:            attachedVoice,
		ReportTheme:      reportTheme,
		DeliverTo:        deliverTo,
		Destinations:     destinations,
		Intake:           answers,
		Adapter:          ad,
		Policy:           pol,
		PolicyFile:       &policyFile,
		SecretOverrides:  overrides,
		Connections:      conns.tokens,
		RenewConnection:  conns.renew,
		ModelSignIn:      signIn,
		RenewModelSignIn: renewSignIn,
		ServersDir:       serversDir,
		Budgets:          budgets,
		Attestation:      meta.Attestation,
		KeepRunDir:       opts.keepRunDir,
		RunID:            runID,
		Stderr:           cmd.ErrOrStderr(),
		OnUsage:          onUsage,
		Events:           eventsOut,
	}
	if mode == manifest.ModeInteractive {
		runErr := runInteractive(cmd, docker, params, m, plan, opts)
		finishProfile(cmd.ErrOrStderr(), prof, profDir, keep)
		return runErr
	}
	// A one-shot run of a release ends blocked, not ok, when its agent
	// could not do the task.
	params.EndBlocked = true
	env, runErr := engine.Run(cmd.Context(), docker, params)
	if env != nil {
		printEnvelope(cmd, env, opts.outputFormat)
		opts.progress.enveloped()
	}
	finishProfile(cmd.ErrOrStderr(), prof, profDir, keep)
	return runErr
}

// startSession and runChat are runInteractive's two collaborators, package
// variables so a test can put a fake session behind a chat that panics or
// returns early and see teardown still run.
var (
	startSession = engine.StartSession
	runChat      = tui.Run
)

// runInteractive drives an interactive plan: the engine's session behind
// the terminal chat. The screen is restored before the envelope and
// receipts are written, so what the user sees last is where the
// transcript went.
func runInteractive(cmd *cobra.Command, docker dockerx.Client, params engine.Params, m *manifest.Manifest, plan *spec.Plan, opts runOpts) error {
	errOut := cmd.ErrOrStderr()
	s, err := startSession(cmd.Context(), docker, params)
	if err != nil {
		return err
	}
	// Teardown on every path: a panic in the terminal chat, or any return
	// before the explicit End below, would leave the session's sandbox, the
	// gateway holding the docker socket and the model key running with nothing
	// left to apply a budget. End is idempotent — a second call returns the
	// first one's envelope — so the explicit End below still produces the
	// receipts this function reports. The deferred call gets a context of its
	// own: on the panic path cmd.Context() may already be cancelled, and a
	// teardown that cannot run is no teardown.
	defer func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = s.End(endCtx, false)
	}()
	if opts.sessionProtocol {
		return runProtocol(cmd, s, opts)
	}
	topts := tui.Options{
		Ref:        plan.Ref,
		Model:      plan.Harness.Model,
		Caps:       capabilityLines(m.Contract()),
		MaxTurns:   params.Budgets.MaxTurns,
		MaxCostUSD: params.Budgets.MaxCostUSD,
		PlanRun:    plan.Harness.ModelSignIn != nil,
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
		printDeliverySummary(errOut, env)
	}
	if runErr == nil && uiErr != nil {
		return engine.Classify(engine.ExitCLI, uiErr)
	}
	return runErr
}

// serveProtocol is runProtocol's collaborator, a package variable as
// runChat is.
var serveProtocol = protocol.Serve

// runProtocol drives the session from the commands on stdin, its events on
// descriptor 3. stdout gets what any run prints there — the envelope under
// --output-format json, the result otherwise — and stderr the usage
// narration a driver meters by, so the host that parses a run's output
// parses a session's the same way.
func runProtocol(cmd *cobra.Command, s session.Session, opts runOpts) error {
	errOut := cmd.ErrOrStderr()
	env, runErr := serveProtocol(cmd.Context(), s, cmd.InOrStdin(), opts.protocolEvents, protocol.Options{
		OpeningTurn: sendsOpeningTurn(opts.input),
		InputsDir:   opts.sessionInputs,
		OnUsage:     usageReporter(opts.outputFormat, errOut),
		OnDropped: func() {
			_, _ = fmt.Fprintln(errOut, "session protocol: the events reader stopped taking events; the rest are in the receipts only")
		},
	})
	if env != nil {
		printEnvelope(cmd, env, opts.outputFormat)
		opts.progress.enveloped()
	}
	return runErr
}

// protocolEventsOut is descriptor 3, where the session protocol writes its
// events; a driver that did not open it gets a usage error before anything
// runs. A variable so a test can hand the protocol a buffer.
var protocolEventsOut = openProtocolEvents

// sendsOpeningTurn says whether the session will send the task as its first
// turn: the engine sends a task only when it is not blank. A task file that
// cannot be read fails the session's start, so false is never acted on.
func sendsOpeningTurn(input string) bool {
	if input == "" {
		return false
	}
	b, err := os.ReadFile(input) //nolint:gosec // the user-named task file the run reads too
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// runTask is the run's task reader. Under the session protocol stdin is the
// command stream, never the task: the task comes from --input alone.
func runTask(cmd *cobra.Command, opts runOpts) io.Reader {
	if opts.sessionProtocol && opts.input == "" {
		return nil
	}
	return taskReader(cmd, opts.input)
}

// homeImageDir is where the embedded runner-image build context
// materializes under the runner home.
func homeImageDir(home string) string { return filepath.Join(home, "image") }

// taskReader implements the input precedence: --input file > piped
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

// runProgress is where a run has got to: the step it is in (a
// receipt.Stage word), the release once verified, and whether an envelope
// has been printed. Its methods take a nil receiver, as runOpts built
// without one (a test's) leave it.
type runProgress struct {
	stage   string
	meta    *installdir.Meta
	ref     string
	printed bool
}

// at moves the run into a step.
func (p *runProgress) at(stage string) {
	if p != nil {
		p.stage = stage
	}
}

// running records the release the run verified and installed.
func (p *runProgress) running(meta *installdir.Meta) {
	if p != nil {
		p.meta = meta
	}
}

// enveloped records that the run's own envelope was printed.
func (p *runProgress) enveloped() {
	if p != nil {
		p.printed = true
	}
}

// printFailure prints the envelope of a run that failed before the engine
// wrote one, under --output-format json: a driver reading stdout gets the
// exit class and the step it stopped in, not silence. There is no run, so
// no run id, receipts or output folder; the release and its attestation
// are there once it was verified. A run interrupted here says so under
// the cli class, as the engine's does, so a driver records an
// interruption and not a refusal.
func printFailure(ctx context.Context, cmd *cobra.Command, p *runProgress, err error) {
	code := engine.ExitCLI
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		code = ce.Code
	}
	msg, reason := err.Error(), receipt.ReasonOf(err)
	if ctx.Err() != nil {
		code, msg, reason = engine.ExitCLI, "interrupted", nil
	}
	env := &receipt.Envelope{
		Agentry:              receipt.EnvelopeVersion,
		Status:               "error",
		ExitCode:             code,
		Error:                msg,
		Stage:                p.stage,
		Reason:               reason,
		Ref:                  p.ref,
		CapabilitiesEnforced: map[string]string{},
		Result:               receipt.TextResult(""),
	}
	if m := p.meta; m != nil {
		env.Ref, env.Digest, env.Attestation = m.Ref, m.Digest, m.Attestation
	}
	var changed *installdir.ChangedError
	if errors.As(err, &changed) {
		env.InstalledChanges = installedChanges(changed)
		if env.Ref == "" {
			env.Ref = changed.Ref
		}
	}
	printEnvelope(cmd, env, "json")
}

// recheckInstall holds a registry install to its record again, after the
// consent prompt and before anything is compiled from it.
func recheckInstall(installDir string, meta *installdir.Meta) error {
	_, err := installdir.Inspect(installDir, meta.Digest)
	var changed *installdir.ChangedError
	if errors.As(err, &changed) {
		changed.Ref = meta.Ref
		return engine.Classify(engine.ExitVerify, changed)
	}
	if err != nil {
		return engine.Classify(engine.ExitVerify, fmt.Errorf("installed %s: %w", meta.Ref, err))
	}
	return nil
}

// installedCopyCheck is the engine's check of a registry release's per-run
// copy against the install's record; a --local directory has none (it is
// its own source, the one exemption).
func installedCopyCheck(opts runOpts, installDir string, meta *installdir.Meta) func(string) error {
	if opts.local != "" {
		return nil
	}
	return func(copyDir string) error {
		err := installdir.CheckCopy(installDir, copyDir)
		var changed *installdir.ChangedError
		if errors.As(err, &changed) {
			changed.Ref = meta.Ref
			return changed
		}
		return err
	}
}

// installedChanges is a changed install's refusal in the envelope's shape.
func installedChanges(e *installdir.ChangedError) *receipt.InstalledChanges {
	return &receipt.InstalledChanges{
		Changed: e.Changed, Added: e.Added, Removed: e.Removed, Record: string(e.Record),
		Repair: e.RepairCommand(),
	}
}

// printEnvelope writes the run result per --output-format. Text mode
// prints only the agent's result (Unix composition); the envelope is
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
	printDeliverySummary(cmd.ErrOrStderr(), env)
}

// runPlanConsent is what a run's consent gate would say, computed without
// asking or recording.
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
	next := installed.Snapshot(m.Contract())
	if prev := state.Find(key); prev != nil {
		c.Installed = &installPlanPrior{Version: prev.Version, Dir: prev.Dir}
		c.Diff = installed.DiffCapabilities(prev.Capabilities, next)
		if c.Diff.HasWidened() {
			c.NeedsConfirmation = true
			c.Reason = "this version widens the declared capabilities"
		}
	} else if capabilityNeedsConfirm(m.Capabilities) {
		c.NeedsConfirmation = true
		c.Reason = "a first run of a QuickAgent that declares " + confirmedCapabilitiesWords(m.Capabilities)
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
	case trust.StateUnsigned, trust.StateLocal:
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
	case trust.StateBuiltin:
		// Only the application's own packages are built in, and their
		// record is made in memory on each run (localrun.Builtin): a
		// cached install that claims it is not one of them.
		return fmt.Errorf("%s is recorded as built in (cached install), which only the application's own packages are; remove %s and install it again", meta.Ref, installdir.AgentDir("<runner home>", meta.Namespace, meta.Name))
	case trust.StateUnsigned, trust.StateLocal:
		if !allowUnsigned {
			return fmt.Errorf("%s is %s (cached install); re-run with --allow-unsigned to run it anyway", meta.Ref, meta.Attestation.State)
		}
		meta.Attestation.Consent = receipt.ConsentAllowUnsigned
	case trust.StateUnscanned:
		if !allowUnscanned {
			return fmt.Errorf("%s is unscanned (cached install) — the registry admitted it without a scan; re-run with --allow-unscanned to run it anyway", meta.Ref)
		}
		meta.Attestation.Consent = receipt.ConsentAllowUnscanned
	case trust.StateVerified:
	default:
		// A state this gate does not know is refused, not waved through:
		// only the four above are ever cached (install refuses the rest).
		return fmt.Errorf("%s is recorded with the attestation state %q (cached install), which a run does not admit; remove it and install it again", meta.Ref, meta.Attestation.State)
	}
	return nil
}

// runPlanOutput is the `run --plan --json` shape: the compiled plan —
// what a run would enforce — and what a run would ask or refuse, with
// nothing asked or recorded.
type runPlanOutput struct {
	Ref     string   `json:"ref"`
	Version string   `json:"version"`
	Digest  string   `json:"digest"`
	Mode    string   `json:"mode"`
	Modes   []string `json:"modes"`
	Summary string   `json:"summary,omitempty"`
	// Plan is the compiled plan, canonical; PlanSha256 its hash.
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
	// Integrity is what a run would find of the release installed in the
	// runner home: installIntegrity's values. A changed install is also
	// the first of Refusals.
	Integrity installIntegrity `json:"integrity"`
	runPlanConsent
	// Refusals is every refusal the gates before compile would meet, in
	// the order a run meets them: the attestation, then a declared server
	// with a known vulnerability. A client that lifts one with the
	// person's acceptance still meets the others.
	//
	// Deprecated: Refusal is Refusals[0].Message, kept for clients that
	// read one refusal.
	Refusal  string           `json:"refusal,omitempty"`
	Refusals []runPlanRefusal `json:"refusals,omitempty"`
}

// runPlanRefusal is one refusal a run would meet: Kind is the gate,
// Message what it says, and Lift the flag that lifts it — empty when no
// flag does (a signature no pinned key verifies), so a client lifts only
// what the person accepted, by name.
type runPlanRefusal struct {
	Kind    refusalKind `json:"kind"`
	Message string      `json:"message"`
	Lift    string      `json:"lift,omitempty"`
}

// refusalKind names the gate a --plan refusal comes from.
type refusalKind string

const (
	// refusalInstalledChanged: the installed release changed after it was
	// laid down. No flag lifts it; the message names `agentry install
	// --repair`. refusalInstalledUnreadable: it could not be read to check.
	refusalInstalledChanged    refusalKind = "installed_changed"
	refusalInstalledUnreadable refusalKind = "installed_unreadable"
	refusalAttestation         refusalKind = "attestation"
	refusalVulnerableServer    refusalKind = "vulnerable_server"
)

// installIntegrity is what a run would find of the release installed in
// the runner home, checked against its integrity record.
type installIntegrity string

const (
	// integrityUnchanged: installed, and its files are what was laid down.
	integrityUnchanged installIntegrity = "unchanged"
	// integrityChanged: installed and changed since; a run refuses it.
	integrityChanged installIntegrity = "changed"
	// integrityNoRecord: installed with no integrity record from this
	// runner home (laid down by an older CLI); a run fetches the release
	// again, checks it as a first fetch is, and lays it down in its place.
	integrityNoRecord installIntegrity = "no-record"
	// integrityNotInstalled: a run fetches, verifies and lays it down.
	integrityNotInstalled installIntegrity = "not-installed"
	// integrityLocal: a --local directory is not checked. It is its own
	// source, with no signed release to be faithful to; its cached copy is
	// replaced from it whenever they differ. The only exemption.
	integrityLocal installIntegrity = "not-checked-local"
	// integrityUnreadable: the installed tree could not be read to check
	// it; a run fails.
	integrityUnreadable installIntegrity = "unreadable"
)

// integrityLines is what the text preview says of each.
var integrityLines = map[installIntegrity]string{
	integrityUnchanged:    "unchanged since it was installed",
	integrityChanged:      "changed since it was installed — a run refuses it",
	integrityNoRecord:     "installed without an integrity record from this runner home — a run fetches it again first",
	integrityNotInstalled: "not installed — a run fetches it",
	integrityLocal:        "not checked — a local directory is its own source",
	integrityUnreadable:   "could not be read to check it — a run fails",
}

// installedIntegrity checks the release a run would use in home and says
// what a run would find, with the refusal a run would meet.
func installedIntegrity(home string, meta *installdir.Meta) (installIntegrity, *runPlanRefusal) {
	dir := installdir.ReleaseDir(home, meta.Namespace, meta.Name, meta.Version, meta.Digest)
	_, err := installdir.Inspect(dir, meta.Digest)
	var changed *installdir.ChangedError
	switch {
	case err == nil:
		return integrityUnchanged, nil
	case errors.As(err, &changed):
		changed.Ref = meta.Ref
		return integrityChanged, &runPlanRefusal{Kind: refusalInstalledChanged, Message: changed.Error()}
	case errors.Is(err, installdir.ErrNoRecord):
		return integrityNoRecord, nil
	case errors.Is(err, installdir.ErrNotInstalled):
		return integrityNotInstalled, nil
	default:
		return integrityUnreadable, &runPlanRefusal{Kind: refusalInstalledUnreadable, Message: err.Error()}
	}
}

// attestationLift is the flag that lifts an attestation refusal: the
// consent a run would record for it (receipt.Consent*), or none.
func attestationLift(att receipt.Attestation) string {
	switch att.State {
	case trust.StateUnsigned, trust.StateLocal:
		return receipt.ConsentAllowUnsigned
	case trust.StateUnscanned:
		return receipt.ConsentAllowUnscanned
	}
	return ""
}

func printRunPlan(cmd *cobra.Command, plan *spec.Plan, meta *installdir.Meta, m *manifest.Manifest, mode manifest.Mode, consent runPlanConsent, integrity installIntegrity, refusals []runPlanRefusal) error {
	b, err := plan.MarshalCanonical()
	if err != nil {
		return err
	}
	contract, cdigest, err := contractJSON(m.Contract())
	if err != nil {
		return err
	}
	out := runPlanOutput{
		Ref: meta.Ref, Version: meta.Version, Digest: meta.Digest, Mode: string(mode), Modes: modeStrings(m.SupportedModes()),
		Summary: m.Summary, Plan: bytes.TrimSpace(b), PlanSha256: planHash(plan), Contract: contract, ContractDigest: cdigest,
		Attestation: meta.Attestation.State, KeyID: meta.Attestation.KeyID, Signed: meta.Attestation.Verified,
		Integrity: integrity, runPlanConsent: consent, Refusals: refusals,
	}
	if len(refusals) > 0 {
		out.Refusal = refusals[0].Message
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), out)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "plan: %s (%s) plan sha256 %s\n", out.Ref, out.Mode, out.PlanSha256)
	_, _ = fmt.Fprintf(w, "attestation: %s\n", out.Attestation)
	if line := integrityLines[out.Integrity]; line != "" {
		_, _ = fmt.Fprintf(w, "installed: %s\n", line)
	}
	if out.NeedsConfirmation {
		_, _ = fmt.Fprintf(w, "would ask: %s\n", out.Reason)
	}
	for _, r := range out.Refusals {
		_, _ = fmt.Fprintf(w, "would refuse: %s\n", r.Message)
	}
	_, _ = fmt.Fprintln(w, "nothing asked, nothing recorded (--plan)")
	return nil
}

// printDryRun prints the compiled plan (deterministic, hashable,
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

// collectIntake answers the manifest's up-front questions:
// --intake presets first, then the terminal when there is one; with no
// terminal, a secret question the run's profile or secret source already
// holds is answered from there (fromSource).
func collectIntake(cmd *cobra.Command, m *manifest.Manifest, opts runOpts, fromSource func(string) bool) (*intake.Answers, error) {
	preset, err := intake.ParsePreset(opts.intake)
	if err != nil {
		return nil, err
	}
	if len(m.Intake) == 0 {
		if len(preset) > 0 {
			return nil, errors.New("--intake given but the QuickAgent asks no intake questions")
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
	return intake.CollectRun(p, m, preset, fromSource)
}

// resolveSourceTokens gives each input source the run was given its
// access token: the variable --intake-token names, the key saved for
// its host, a prompt on a terminal, or none.
func resolveSourceTokens(cmd *cobra.Command, answers *intake.Answers, opts runOpts) error {
	flags, err := sources.ParseTokenFlags(opts.intakeToken)
	if err != nil {
		return err
	}
	srcs := answers.SourceList()
	if len(srcs) == 0 {
		if len(flags) > 0 {
			return errors.New("--intake-token given but the run was given no input source")
		}
		return nil
	}
	var ask sources.TokenAsker
	if stdinIsTTY(cmd.InOrStdin()) {
		ask = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	return sources.ResolveTokens(srcs, flags, os.Getenv, ask, func(msg string) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", msg)
	})
}

// sayGuide prints the release guide's overview and its list of what to
// gather on the terminal, once, before the first question a run asks: the
// guided profile's keys or the intake. A run that asks nothing prints
// nothing, and so do a --output-format json run and a run with no terminal,
// whose callers never reach here. The overview is markdown and is printed
// as its text behind termtext's gutter, so no line of it can read as the
// CLI's own.
func sayGuide(w io.Writer, m *manifest.Manifest, opts runOpts) {
	if (m.Guide == nil && m.Inputs == nil) || opts.outputFormat == "json" || (opts.guideShown != nil && *opts.guideShown) {
		return
	}
	if opts.guideShown != nil {
		*opts.guideShown = true
	}
	g := m.Guide
	if g == nil {
		g = &manifest.Guide{}
	}
	if ov := strings.TrimSpace(g.Overview); ov != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", termtext.Quote(ov))
	}
	if len(g.Prepare) > 0 {
		_, _ = fmt.Fprintln(w, "\nHave these ready:")
		for _, item := range g.Prepare {
			_, _ = fmt.Fprintf(w, "  - %s\n", termtext.Safe(item))
		}
	}
	if g.Files != "" {
		_, _ = fmt.Fprintf(w, "Files: %s\n", termtext.Safe(g.Files))
	}
	if line := inputsLine(m); line != "" {
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintln(w)
}

// inputsLine says what files a release takes and how its agent reads them
// (the manifest's inputs block), or nothing for a release without the
// block. Its words are the CLI's own: the block holds only the closed
// kinds and views, never the author's text.
func inputsLine(m *manifest.Manifest) string {
	if m.Inputs == nil {
		return ""
	}
	var kinds []string
	for _, k := range m.TakesKinds() {
		if k.Valid() {
			kinds = append(kinds, string(k))
		}
	}
	how := "as text or as uploaded"
	switch m.DefaultInputView() {
	case manifest.InputViewText:
		how = "as text"
	case manifest.InputViewOriginal:
		how = "as uploaded"
	}
	if len(kinds) == 0 {
		kinds = []string{string(manifest.InputKindAny)}
	}
	return "Files it takes: " + strings.Join(kinds, ", ") + "; read " + how + "."
}

// secretOverrides merges the profile's stored secrets with the intake's
// secret answers (under their SecretName). answers is nil for a QuickAgent
// that asks no intake questions — the common case, which must not panic
// here.
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

// resolveRunMode applies the default rule: the flag wins; a
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
	answer, err := p.Ask("This QuickAgent runs autonomous or interactive. Mode", string(manifest.ModeAutonomous))
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

// resolveOutDir picks and creates the output folder.
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
// (the same gate as install — digest, DSSE, --allow-unsigned).
func materializeRegistry(cmd *cobra.Command, ref, home string, opts runOpts) (*installdir.Meta, string, error) {
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		return nil, "", err
	}
	// A QuickAgent installed from a local folder runs from the runner home,
	// never resolved: no registry holds it. It is held to its integrity
	// record like an installed release, and it is unsigned, so every run
	// needs --allow-unsigned. A version range, or a version the local
	// install is not, resolves through the registry as before.
	localHome := home
	if opts.consentHome != "" {
		localHome = opts.consentHome
	}
	// A channel other than stable asks the registry for its releases.
	onChannel := parsed.Channel == "" || parsed.Channel == "stable"
	if meta, dir, ok, lerr := localinstall.Materialize(localHome, parsed.Namespace, parsed.Name, parsed.Range); onChannel && ok {
		if lerr != nil {
			return nil, "", engine.Classify(engine.ExitVerify, lerr)
		}
		if opts.pin != "" && opts.pin != meta.Digest {
			return nil, "", engine.Classify(engine.ExitVerify,
				fmt.Errorf("--pin mismatch: the local install is %s, pin requires %s", meta.Digest, opts.pin))
		}
		if opts.consentHome != "" && opts.consentHome != home {
			// --plan: every later write (the compiled plan inside the
			// release) stays in the temp home, so the verified local
			// release is copied there and previewed from the copy.
			planDir := installdir.ReleaseDir(home, meta.Namespace, meta.Name, meta.Version, meta.Digest)
			if err := installdir.Write(planDir, filepath.Join(dir, "workspace"), meta); err != nil {
				return nil, "", err
			}
			dir = planDir
		}
		if err := cachedAttestationGate(meta, opts.allowUnsigned, opts.allowUnscanned); err != nil {
			return nil, "", engine.Classify(engine.ExitVerify, err)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: installed from a local folder, unsigned (%s)\n", meta.Ref, installdir.DigestSuffix(meta.Digest))
		return meta, dir, nil
	} else if lerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — a local install of %s/%s could not be looked for; resolving it through the registry\n", lerr, parsed.Namespace, parsed.Name)
	}
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil {
		return nil, "", err
	}
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
	// The installed tree is held to its integrity record before anything
	// trusts it: a release that changed after it was laid down is refused
	// here, before anything is fetched; one no record of this home
	// vouches for (laid down before records existed) is fetched and
	// verified again below, and replaces what is there.
	meta, ierr := installdir.Inspect(installDir, res.Digest)
	var changed *installdir.ChangedError
	reExport := ""
	switch {
	case errors.As(ierr, &changed):
		// Named for what was resolved, not for the install.json that
		// failed its record.
		changed.Ref = fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version)
		return nil, "", engine.Classify(engine.ExitVerify, changed)
	case errors.Is(ierr, installdir.ErrNoRecord):
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "installed %s/%s@%s has no integrity record from this runner home (laid down by an older CLI, or under another key) — fetching it again and checking it as a first fetch is\n", res.Namespace, res.Name, res.Version)
		reExport = meta.Export
	case ierr != nil && !errors.Is(ierr, installdir.ErrNotInstalled):
		return nil, "", engine.Classify(engine.ExitCLI, ierr)
	}
	if ierr == nil {
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
		// would be one-shot per digest, and an unscanned one
		// --allow-unscanned.
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
	m, err := localrun.ManifestFrom(staging)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	if err := requireValidManifest(cmd, m, fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version)); err != nil {
		return nil, "", err
	}

	meta = &installdir.Meta{
		Ref:       fmt.Sprintf("%s/%s@%s", res.Namespace, res.Name, res.Version),
		Namespace: res.Namespace, Name: res.Name, Version: res.Version,
		Digest: res.Digest, Source: "registry", Attestation: att,
	}
	// The cache holds the verdict and never the consent, which is this
	// run's alone: the next run is asked again.
	cached := *meta
	cached.Attestation.Consent = ""
	if reExport != "" {
		// An install laid down before records existed keeps the export it
		// had when it is laid down again.
		if err := installdir.Install(installDir, staging, m, &cached, reExport); err != nil {
			return nil, "", err
		}
		return meta, installDir, nil
	}
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
	case trust.StateUnsigned, trust.StateLocal:
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
	// Narration to stderr: run's stdout carries only the result.
	errOut := cmd.ErrOrStderr()
	switch att.State {
	case trust.StateVerified:
		_, _ = fmt.Fprintf(errOut, "attestation:   verified (key %s)\n", att.KeyID)
		if note := att.RetiredNote(); note != "" {
			_, _ = fmt.Fprintf(errOut, "  %s\n", note)
		}
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

// materializeLocal admits an unpublished package directory: parse +
// validate the manifest, hash the tree, and cache like a registry install
// keyed by that hash. Local trees are unsigned by definition, so the same
// --allow-unsigned decision applies.
func materializeLocal(cmd *cobra.Command, dir, home string, allowUnsigned bool) (*installdir.Meta, string, error) {
	if !allowUnsigned {
		return nil, "", engine.Classify(engine.ExitVerify,
			errors.New("--local packages are unsigned by definition; pass --allow-unsigned to run one"))
	}
	m, err := localrun.ManifestFrom(dir)
	if err != nil {
		return nil, "", engine.Classify(engine.ExitCompile, err)
	}
	if err := requireValidManifest(cmd, m, "local package"); err != nil {
		return nil, "", err
	}
	// The record is computed, never taken from a cached install: nothing
	// on disk can give a directory another attestation state (the
	// built-in's above all).
	meta, err := localrun.LocalMeta(dir)
	if err != nil {
		return nil, "", err
	}
	installDir := installdir.ReleaseDir(home, meta.Namespace, meta.Name, meta.Version, meta.Digest)
	if _, ok := installdir.Load(installDir, meta.Digest); ok {
		return meta, installDir, nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "local package %s (%s)\n", meta.Ref, installdir.DigestSuffix(meta.Digest))
	// The directory is the truth and there is no signed release to be
	// faithful to: a cached copy that changed is replaced from it, never
	// refused (the one exemption to the integrity check).
	if err := installdir.Replace(installDir, dir, meta); err != nil {
		return nil, "", err
	}
	return meta, installDir, nil
}

// confirmRunCapabilities applies the install-time consent semantics to
// runs, keyed on the runner install dir: first run asks about egress/exec,
// upgrades ask again only when the contract widened.
func confirmRunCapabilities(cmd *cobra.Command, m *manifest.Manifest, meta *installdir.Meta, home string, opts runOpts, mode manifest.Mode, shippedSizes map[string]int64) error {
	// Everything here narrates to stderr: stdout is the run's result
	// channel and must stay pipeline-clean.
	out := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(out, "%s (%s)\n", meta.Ref, modesLabel(m.SupportedModes()))
	if m.Summary != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", m.Summary)
	}
	_, _ = fmt.Fprintln(out, "capabilities:")
	for _, line := range capabilityLinesSized(m.Contract(), shippedSizes) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	if mode == manifest.ModeInteractive {
		// The one line the consent screen gains for a session.
		_, _ = fmt.Fprintln(out, "  interactive: you can talk to this agent and give it files; you cannot give it more access than listed above")
	}
	key := installdir.ConsentKey(home, meta.Namespace, meta.Name)
	state, serr := installed.Load()
	if serr != nil {
		_, _ = fmt.Fprintf(out, "warning: %v — treating this as a first run\n", serr)
		state = installed.State{}
	}
	next := installed.Snapshot(m.Contract())
	if err := confirmCapabilityChange(cmd, out, &state, key, meta.Version, next, m.Contract(), opts.yes); err != nil {
		return err
	}
	// An install owns the agent's directory (managed) and a local
	// install its record; Reconsent keeps both.
	if !state.Reconsent(key, meta.Version, next, meta.Digest) {
		_, _ = fmt.Fprintf(out, "note: %s/%s's record is its local install; this run's consent is not kept\n", meta.Namespace, meta.Name)
		return nil
	}
	if err := installed.Save(state); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — the next run will re-ask about capabilities\n", err)
	}
	return nil
}

// requireValidManifest holds a manifest to the registry's own publish rules
// before anything is compiled from it. Warnings (e.g. the V5 "*" egress
// flag) surface but do not block — same split the registry applies at
// publish; the RUN-time gate for wildcard egress is the policy check in the
// compiler. A blocking error is a compile refusal naming every rule. Both
// materialize paths call this before caching: a --local tree because nobody
// else has, and a registry release because the registry's validation is
// that registry's — a self-hosted one whose rules predate today's, or a
// compromised one, is exactly the trust the digest check refuses to extend
// elsewhere.
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

// ensureProfile loads — or, on a terminal, guides the creation of — the
// QuickAgent's MCP profile for this signature. Returns the profile (nil
// when none applies), its directory, and the effective keep choice:
// flag > stored profile > policy default.
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

	// The union of agent-level and per-server secret names: guided
	// setup collects every key the release's gateway will need.
	secrets := allSecretNames(m)
	needGuided := opts.reconfigureProfile || (prof == nil && len(secrets) > 0)
	if !needGuided {
		if prof == nil && keepDecided && keep {
			// Nothing to collect, but the operator explicitly asked to
			// keep a profile: record the choice for future schema
			// growth.
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
		// Non-interactive first run: secrets resolve from the policy
		// source; no profile is created and nothing degrades silently (a
		// missing required secret still refuses with exit 11; an optional
		// one the run goes without is named).
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
		CapabilityLines: capabilityLines(m.Contract()),
		Secrets:         secrets,
		Optional:        m.Capabilities.OptionalSecrets,
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
// finished, success or failure: keep → persist + stamp; remove → the
// profile dies with the run.
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

// runnerCLIVersion feeds the runner image tag derivation (a new CLI
// version is what triggers a runner image rebuild).

// inputReview is AI Review of the inputs for --review-inputs: the named
// registry's model-input scan with the job's credential. Nil without the
// flag: a local run reviews nothing. The registry is named apart from
// --registry because a private release's run reaches the registry through
// a prefix, and the scan route is at the registry's root.
func inputReview(registry string) (webscan.Scanner, error) {
	if registry == "" {
		return nil, nil
	}
	// The credential rides on every call: https, or http to this machine
	// alone (a test registry). The value is not echoed back: it is the
	// host's, and could carry what it should not.
	u, err := url.Parse(registry)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !loopbackHost(u.Hostname()))) {
		return nil, engine.Classify(engine.ExitUsage, errors.New("--review-inputs takes the registry's address, https://host[/path] (http only to this machine)"))
	}
	tok := strings.TrimSpace(os.Getenv(envJobToken))
	if tok == "" {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("--review-inputs reviews with the job's credential in $%s, and it is not set", envJobToken))
	}
	return &webscan.Registry{Base: strings.TrimRight(registry, "/"), JobToken: tok}, nil
}

// loopbackHost says whether host names this machine.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
