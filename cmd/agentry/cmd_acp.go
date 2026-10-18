// `agentry acp`: one QuickAgent run for an editor or an orchestrating agent
// that speaks the Agent Client Protocol, with this process as the agent side
// (pkg/runner/acpagent) and the same run underneath as `agentry run`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/acpagent"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// acpRun is what a run driven over ACP changes in runRun: its task is the
// client's first prompt, its consent is asked of the client, and what it
// reports goes to the client.
type acpRun struct {
	host  *acpagent.Host
	first string
}

// task is the reader of the run's task: an autonomous run takes the first
// prompt as its task; an interactive session sends it as its first turn
// itself, so the engine is given none.
func (a *acpRun) task(mode manifest.Mode) io.Reader {
	if mode == manifest.ModeAutonomous {
		return strings.NewReader(a.first)
	}
	return nil
}

func newACPCmd() *cobra.Command {
	opts := runOpts{outputFormat: "text", maxTurns: -1, maxCost: -1}
	cmd := &cobra.Command{
		Use:   "acp [ns/name[@range]]",
		Short: "Run a QuickAgent as an Agent Client Protocol agent on stdin and stdout",
		Long: "Serve one QuickAgent to an editor or another agent over the Agent Client Protocol,\n" +
			"on stdin and stdout (JSON-RPC, one message per line); the client launches this\n" +
			"command. Nothing is verified, installed or started until the client's first\n" +
			"prompt; the run's plan and consent are then put to the client as a permission\n" +
			"request, and a client that cannot show it, declines it or does not answer\n" +
			"refuses: the run does not start. --yes is the person's standing consent, as for\n" +
			"`agentry run`.\n\n" +
			"An interactive QuickAgent is one session, and each prompt a turn of its chat; the\n" +
			"deliverables its teardown lists come back when the client closes the session. An\n" +
			"autonomous one takes the first prompt as its task, runs it once and ends: its\n" +
			"result and its deliverables (small text files embedded, the rest as links to the\n" +
			"verified file) are the last updates of that prompt. A release that declares both\n" +
			"modes runs interactive unless --mode says otherwise.\n\n" +
			"The run is the one `agentry run` makes, with the same controls: the QuickAgent\n" +
			"gets only the tools its release declares, never the client's file system,\n" +
			"terminal or MCP servers; a session/cancel or session/close ends the run;\n" +
			"--deliver-to and --destination apply. Anything that would ask a person (an\n" +
			"intake question, an account sign-in) is answered by a flag or set up beforehand\n" +
			"with `agentry run`; narration goes to stderr.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			if (ref == "") == (opts.local == "") {
				return engine.Classify(engine.ExitUsage, errors.New("pass exactly one of ns/name[@range] or --local DIR"))
			}
			if opts.noDestination && len(opts.destinations) > 0 {
				return engine.Classify(engine.ExitUsage, errors.New("--no-destination and --destination exclude each other"))
			}
			if opts.mode != "" && !manifest.Mode(opts.mode).Valid() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("unknown --mode %q (want autonomous or interactive)", opts.mode))
			}
			// stdin and stdout are the protocol's: the run narrates to stderr
			// and has no one to ask at a terminal.
			in, out := cmd.InOrStdin(), cmd.OutOrStdout()
			cmd.SetOut(cmd.ErrOrStderr())
			cmd.SetIn(strings.NewReader(""))
			b := &acpBackend{cmd: cmd, ref: ref, opts: opts}
			return acpagent.Serve(cmd.Context(), in, out, acpagent.Info{Name: "agentry", Version: version.Build()}, b)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.local, "local", "", "run an unpublished QuickAgent DIRECTORY instead of a registry ref")
	f.StringVar(&opts.pin, "pin", "", "require the resolved archive digest to equal this sha256:… value")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	f.StringVar(&opts.adapterName, "adapter", "", "harness adapter; see agentry run --list-adapters")
	f.StringVar(&opts.mode, "mode", "", "run mode: autonomous | interactive (default: the release's only mode; interactive when it declares both)")
	f.StringVar(&opts.out, "out", "", "output folder for reports and receipts (default ./agentry-out/<ns>-<name>-<runid>)")
	f.IntVar(&opts.timeout, "timeout", 0, "override the policy timeout (seconds)")
	f.IntVar(&opts.maxTurns, "max-turns", -1, "override the policy tool-call budget (0 = unlimited)")
	f.Float64Var(&opts.maxCost, "max-cost", -1, "override the policy cost budget in USD (0 = unlimited)")
	f.BoolVar(&opts.yes, "yes", false, "consent to the run's capabilities on the person's behalf: the client is not asked")
	f.BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "run a release with no registry attestation (required for --local)")
	f.BoolVar(&opts.allowUnscanned, "allow-unscanned", false, "run a release the registry admitted without a scan")
	f.BoolVar(&opts.allowVulnerableMCP, "allow-vulnerable-mcp", false, "run even when the registry's gate rates a declared MCP server's advisory high or critical")
	f.BoolVar(&opts.keepRunDir, "keep-run-dir", false, "keep the per-run scratch dir for debugging (secrets are still deleted)")
	f.StringArrayVar(&opts.files, "file", nil, "file or directory to provide to the run read-only under /inputs (repeatable)")
	f.StringArrayVar(&opts.knowledge, "knowledge", nil, "knowledge pack to attach read-only: a pack kept on this computer by NAME, or a pack DIRECTORY (repeatable)")
	f.BoolVar(&opts.voice, "voice", false, "attach the Writing Voice kept on this computer even when it is not set to attach to every run")
	f.BoolVar(&opts.noVoice, "no-voice", false, "do not attach the Writing Voice to this run")
	f.StringVar(&opts.reportTheme, "report-theme", "", "the built-in report theme this run's PDFs take")
	f.StringVar(&opts.reportDesign, "report-design", "", "the report design this run's PDFs take: a FILE holding a report design")
	f.StringVar(&opts.deliverTo, "deliver-to", "", "copy the run's deliverables to FOLDER/<QuickAgent>/<date time> <title>/ once it succeeds (default: deliver.folder in the config, else nowhere)")
	f.BoolVar(&opts.noDestination, "no-destination", false, "send the run's result to no destination, not even the QuickAgent's default set")
	f.StringArrayVar(&opts.destinations, "destination", nil, "send the run's result and deliverables to this saved destination once it succeeds (repeatable); see agentry destinations")
	f.StringArrayVar(&opts.intake, "intake", nil, "answer an intake question: id=value (repeatable); there is no terminal to ask on")
	f.StringArrayVar(&opts.intakeToken, "intake-token", nil, "the access token for an input source: id=ENV_NAME, the name of an environment variable holding it (repeatable)")
	return cmd
}

// acpBackend is the connection's one run: runRun, with the client in the
// places a terminal and its person would be.
type acpBackend struct {
	cmd  *cobra.Command
	ref  string
	opts runOpts
}

// Run implements acpagent.Backend.
func (b *acpBackend) Run(ctx context.Context, h *acpagent.Host, first string) error {
	opts := b.opts
	opts.acp = &acpRun{host: h, first: first}
	opts.progress = &runProgress{stage: receipt.StagePolicy, ref: b.ref}
	b.cmd.SetContext(ctx)
	return runRun(b.cmd, b.ref, opts)
}

// confirmRun is the run's consent gate: the terminal's confirmation for
// `agentry run`, the client's permission request for a run over ACP. What the
// client is shown is what the terminal would be, and what it records on
// consent is the same record.
func confirmRun(cmd *cobra.Command, m *manifest.Manifest, meta *installdir.Meta, home string, opts runOpts, mode manifest.Mode, shippedSizes map[string]int64) error {
	if opts.acp == nil {
		return confirmRunCapabilities(cmd, m, meta, home, opts, mode, shippedSizes)
	}
	text := acpConsentText(m, meta, home, opts, mode, shippedSizes)
	if opts.yes {
		opts.acp.host.Say(text)
	} else if err := opts.acp.host.Consent(cmd.Context(), acpagent.Consent{Title: "Run " + meta.Ref + "?", Text: text}); err != nil {
		return err
	}
	opts.yes = true // asked of the client already: nothing is asked on a terminal
	return confirmRunCapabilities(cmd, m, meta, home, opts, mode, shippedSizes)
}

// acpConsentText is the plan the client puts to the person: the release, how
// it was checked, the mode, what it may do, what changed since the person last
// accepted it and where its output goes.
func acpConsentText(m *manifest.Manifest, meta *installdir.Meta, home string, opts runOpts, mode manifest.Mode, shippedSizes map[string]int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%s)\n", meta.Ref, meta.Version, mode)
	if m.Summary != "" {
		fmt.Fprintf(&b, "%s\n", oneLineText(m.Summary))
	}
	fmt.Fprintf(&b, "\nrelease %s, %s\n", meta.Digest, attestationWords(meta))
	b.WriteString("\ncapabilities:\n")
	for _, line := range capabilityLinesSized(m.Contract(), shippedSizes) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if mode == manifest.ModeInteractive {
		b.WriteString("  interactive: you talk to this agent in this session; you cannot give it more access than listed above\n")
	}
	c := describeRunConsent(m, meta, home)
	if c.Installed != nil {
		fmt.Fprintf(&b, "\nlast accepted at %s", c.Installed.Version)
		if c.Diff.HasWidened() {
			b.WriteString(", and this version WIDENS it:\n")
			for _, l := range c.Diff.Widened {
				fmt.Fprintf(&b, "  + %s\n", l)
			}
		} else {
			b.WriteString("; nothing widened\n")
		}
	} else if c.NeedsConfirmation {
		fmt.Fprintf(&b, "\nfirst run: %s\n", c.Reason)
	}
	if opts.deliverTo != "" {
		fmt.Fprintf(&b, "\ndeliverables are copied to %s\n", opts.deliverTo)
	}
	if len(opts.destinations) > 0 {
		fmt.Fprintf(&b, "\nresult and deliverables are sent to: %s\n", strings.Join(opts.destinations, ", "))
	}
	b.WriteString("\nThe agent has only the tools its release declares: none of this client's files, terminal or MCP servers.\n")
	// Author text (a summary, an egress host) reaches the client cleaned of
	// what a terminal-drawing client would act on.
	return termtext.Safe(b.String())
}

func attestationWords(meta *installdir.Meta) string {
	if meta.Attestation.State == "" {
		return "attestation not checked"
	}
	return "attestation: " + meta.Attestation.State
}

func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// runACPSession serves an interactive session's prompts, then ends it and
// reports what its teardown left (the deliverables) to the client.
func runACPSession(cmd *cobra.Command, s session.Session, opts runOpts) error {
	errOut := cmd.ErrOrStderr()
	_ = opts.acp.host.Converse(cmd.Context(), s)
	endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env, err := s.End(endCtx, false)
	if env != nil {
		if _, reason := s.Ended(); reason != "" {
			_, _ = fmt.Fprintf(errOut, "session ended: %s\n", reason)
		}
		_, _ = fmt.Fprintf(errOut, "transcript, reports and receipts: %s\n", env.OutDir)
		printDeliverySummary(errOut, env)
		opts.progress.enveloped()
	}
	opts.acp.host.Finish(env, err)
	return err
}
