package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/voice"
	"github.com/blaktron/agentry-cli/pkg/voice/build"
)

// voiceBuild is the build itself (pkg/voice/build); a seam for the
// commands' tests, which check what is kept and said around it.
var voiceBuild = build.Build

// voiceNow is the clock the voice's record is dated by; a seam for tests.
var voiceNow = time.Now

// voiceOpts are the Voice Writer commands' flags.
type voiceOpts struct {
	files      []string
	brief      string
	current    string
	write      string
	out        string
	policyPath string
	budgetFlags
	maxTextBytes int64
	outputFormat string
	stage        string
	onUsage      func(receipt.Usage)
}

// newVoiceCmd is `agentry voice` (plan writing-voice.md; agentry-cli#531):
// the Writing Voice this computer keeps, which runs here write in.
func newVoiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "voice",
		Short: "Make and keep the Writing Voice your runs write in",
		Long: "A Writing Voice is one file, VOICE.md, of instructions an LLM follows to write the way you do. With one kept\n" +
			"on this computer, every run here attaches it (`agentry run` reads it through the voice_read tool) unless\n" +
			"you turn that off (`agentry voice attach off`) or say so for one run (--no-voice). It governs how a run\n" +
			"writes, never what it does.\n\n" +
			"`create`, `improve` and `edit` run the built-in Voice Writer on this computer, against the model your runner\n" +
			"policy names: it costs no credits, and nothing it reads or writes is reviewed. `set` keeps your own text.\n" +
			"The voice is kept sealed on this computer, under a key the registry you are signed in to wraps for your\n" +
			"account (signed out, this computer's key). It stays here: a voice in your Agentry account is reviewed\n" +
			"before it is used there.",
	}
	cmd.AddCommand(newVoiceCreateCmd(), newVoiceImproveCmd(), newVoiceEditCmd(), newVoiceShowCmd(),
		newVoiceSetCmd(), newVoiceAttachCmd(), newVoiceDeleteCmd())
	return cmd
}

func voiceRunFlags(cmd *cobra.Command, o *voiceOpts) {
	f := cmd.Flags()
	f.StringVar(&o.write, "write", "", "write the new VOICE.md to FILE instead of keeping it on this computer (for a driver such as a sandbox host)")
	f.StringVar(&o.out, "out", "", "output folder for the Voice Writer run's reports and receipts (default: private, removed after)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.IntVar(&o.timeout, "timeout", 0, "the run's wall-clock budget in seconds (default: the policy's or 1800, whichever is longer)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the run once the harness reports this much cost in USD (default: the policy's)")
	f.StringVar(&o.outputFormat, "output-format", "text", "stdout format: text | json (json = the run's result envelope with the build's report under voice)")
}

func voiceSampleFlags(cmd *cobra.Command, o *voiceOpts) {
	f := cmd.Flags()
	f.StringArrayVar(&o.files, "file", nil, "a sample of your writing, or a directory of them (repeatable; required)")
	f.StringVar(&o.brief, "brief", "", "how you want to sound, in a sentence or two (optional)")
	f.Int64Var(&o.maxTextBytes, "max-text-bytes", 0, "cap the samples' decoded text the Voice Writer reads, in bytes (0 = no cap)")
}

func voiceCurrentFlag(cmd *cobra.Command, o *voiceOpts) {
	cmd.Flags().StringVar(&o.current, "current", "", "the current voice as a VOICE.md FILE, instead of the one kept on this computer")
}

func newVoiceCreateCmd() *cobra.Command {
	var o voiceOpts
	cmd := &cobra.Command{
		Use:   "create --file PATH [--file PATH…] [--brief TEXT]",
		Short: "Make your Writing Voice from samples of your writing, with the built-in Voice Writer",
		Long: "Make a Writing Voice from samples of your own writing: Markdown, text, HTML and the like as they are; PDF,\n" +
			"DOCX, ODT, EPUB, RTF, PPTX, XLSX and images as the text decoded from them before the run, 10 MB in all. The\n" +
			"more varied the samples (emails, posts, reports), the better the voice. It replaces the voice kept here.",
		Args: cobra.NoArgs,
		RunE: voiceRunE(&o, func(cmd *cobra.Command, _ []string) error { return runVoiceBuild(cmd, build.Create, &o) }),
	}
	voiceSampleFlags(cmd, &o)
	voiceRunFlags(cmd, &o)
	return cmd
}

func newVoiceImproveCmd() *cobra.Command {
	var o voiceOpts
	cmd := &cobra.Command{
		Use:   "improve --file PATH [--file PATH…] [--brief TEXT]",
		Short: "Improve your Writing Voice from more samples, with the built-in Voice Writer",
		Long:  "Run the Voice Writer again over your voice as it stands, more samples of your writing and an optional brief.",
		Args:  cobra.NoArgs,
		RunE:  voiceRunE(&o, func(cmd *cobra.Command, _ []string) error { return runVoiceBuild(cmd, build.Improve, &o) }),
	}
	voiceSampleFlags(cmd, &o)
	voiceCurrentFlag(cmd, &o)
	voiceRunFlags(cmd, &o)
	return cmd
}

func newVoiceEditCmd() *cobra.Command {
	var o voiceOpts
	cmd := &cobra.Command{
		Use:   "edit --note TEXT",
		Short: "Change your Writing Voice by a note, with the built-in Voice Writer (an AI Edit)",
		Long: "Run the Voice Writer over your voice as it stands with a note saying what should change (\"fewer\n" +
			"exclamation marks\", \"more formal in reports\"). To write the change yourself, use `agentry voice set`.",
		Args: cobra.NoArgs,
		RunE: voiceRunE(&o, func(cmd *cobra.Command, _ []string) error { return runVoiceBuild(cmd, build.Edit, &o) }),
	}
	cmd.Flags().StringVar(&o.brief, "note", "", "what should change (required)")
	voiceCurrentFlag(cmd, &o)
	voiceRunFlags(cmd, &o)
	return cmd
}

// voiceRunE wraps a Voice Writer command: the output format is checked
// first, and in its json form a build that failed before the run wrote an
// envelope still writes one, as `agentry knowledge create` does.
func voiceRunE(o *voiceOpts, work func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if o.outputFormat != "text" && o.outputFormat != "json" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("unknown --output-format %q (want text or json)", o.outputFormat))
		}
		o.stage = receipt.StagePolicy
		o.onUsage = usageReporter(o.outputFormat, cmd.ErrOrStderr())
		err := work(cmd, args)
		var printed *envelopePrinted
		if err != nil && o.outputFormat == "json" && !errors.As(err, &printed) {
			printBuiltinFailure(cmd, localrun.VoiceWriter().Ref(), o.stage, err)
		}
		if errors.As(err, &printed) {
			return printed.err
		}
		return err
	}
}

// voiceStore is the store the voice is kept in: the knowledge store's,
// whose key wrapping the voice shares (plan writing-voice.md §4).
func voiceStore(cmd *cobra.Command) (*store.Store, error) { return knowledgeStore(cmd) }

// voiceReport is the json report of a Voice Writer command.
type voiceReport struct {
	Job      string   `json:"job"`
	SHA256   string   `json:"sha256"`
	Bytes    int      `json:"bytes"`
	Kept     bool     `json:"kept,omitempty"`
	Written  string   `json:"written,omitempty"`
	Samples  []string `json:"samples,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
	Cut      []string `json:"cut,omitempty"`
	Receipts string   `json:"receipts,omitempty"`
}

// runVoiceBuild runs the Voice Writer for job and keeps what it wrote on
// this computer, or writes it to --write.
func runVoiceBuild(cmd *cobra.Command, job build.Job, o *voiceOpts) error {
	errOut := cmd.ErrOrStderr()
	var s *store.Store
	var current []byte
	if o.write == "" {
		var err error
		if s, err = voiceStore(cmd); err != nil {
			return err
		}
	}
	switch {
	case o.current != "":
		b, err := readVoiceFile(o.current)
		if err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--current: %w", err))
		}
		current = b
	case job != build.Create && s != nil:
		b, _, err := s.OpenVoice(cmd.Context())
		if err != nil {
			return voiceErr(err)
		}
		current = b
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	policyFile, err := policy.Resolve(o.policyPath)
	if err != nil {
		return err
	}
	pol, polSHA, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	o.stage = receipt.StagePrepare
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The Voice Writer runs on this machine against the model your runner policy names (%s).\n"+
		"It costs no credits, and nothing it reads or writes is reviewed.\n", model)

	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir, private := o.out, o.out == ""
	if private {
		// The run's reports hold the voice in the clear: a private folder
		// in the runner home, removed with the build, unless --out named
		// one.
		if outDir, err = os.MkdirTemp(home, ".voice-run-"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(outDir) }()
	} else {
		_, _ = fmt.Fprintf(errOut, "the Voice Writer run's output in %s keeps the voice it writes in the clear, under reports/\n", o.out)
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	budgets := o.over(localrun.VoiceWriter().LocalBudgets(pol.Budgets))
	res, err := voiceBuild(cmd.Context(), knowledgeDocker(), build.Options{
		Job: job, Samples: o.files, Brief: o.brief, Current: current, MaxTextBytes: o.maxTextBytes,
		Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile, Build: version.Build(),
		OutDir: outDir, Budgets: &budgets, RunID: runID, Stderr: errOut, OnUsage: o.onUsage,
	})
	report := voiceReport{Job: string(job)}
	if !private {
		report.Receipts = filepath.Join(outDir, "receipts")
	}
	if res != nil {
		report.Samples, report.Skipped, report.Cut = res.Samples, res.Skipped, res.Cut
		if len(res.Skipped) > 0 {
			_, _ = fmt.Fprintf(errOut, "not read (no decoder reads them, or the decoder failed): %s\n", strings.Join(res.Skipped, ", "))
		}
		if len(res.Cut) > 0 {
			_, _ = fmt.Fprintf(errOut, "cut short at --max-text-bytes: %s\n", strings.Join(res.Cut, ", "))
		}
	}
	if err != nil {
		if o.outputFormat == "json" && res != nil && res.Envelope != nil {
			printBuiltinEnvelope(cmd.Context(), cmd.OutOrStdout(), *res.Envelope, "voice", report, err)
			return &envelopePrinted{err: err}
		}
		return err
	}
	report.SHA256, report.Bytes = voice.Digest(res.Text), len(res.Text)
	created := false
	if o.write != "" {
		if err := os.WriteFile(o.write, res.Text, 0o600); err != nil {
			err = fmt.Errorf("write the Writing Voice to %s: %w", o.write, err)
			return voiceAfterRun(cmd, o, res, report, err)
		}
		report.Written = o.write
	} else {
		by := &voice.Creator{CLI: version.Build(), Model: pol.Model.Name}
		var kerr error
		if _, created, kerr = s.KeepVoice(cmd.Context(), res.Text, job.Source(), by, voiceNow()); kerr != nil {
			return voiceAfterRun(cmd, o, res, report, fmt.Errorf("the Voice Writer wrote a voice that could not be kept: %w", kerr))
		}
		report.Kept = true
	}
	if o.outputFormat == "json" {
		printBuiltinEnvelope(cmd.Context(), cmd.OutOrStdout(), *res.Envelope, "voice", report, nil)
		return nil
	}
	out := cmd.OutOrStdout()
	if report.Written != "" {
		_, _ = fmt.Fprintf(out, "Wrote the Writing Voice to %s (%d bytes, %s).\n", report.Written, report.Bytes, report.SHA256)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Kept your Writing Voice on this computer (%d bytes, %s). See it with `agentry voice show`.\n", report.Bytes, report.SHA256)
	if created {
		sayVoiceOn(out)
	}
	return nil
}

// voiceAfterRun reports a failure after the Voice Writer ran.
func voiceAfterRun(cmd *cobra.Command, o *voiceOpts, res *build.Result, report voiceReport, err error) error {
	if o.outputFormat == "json" && res.Envelope != nil {
		printBuiltinEnvelope(cmd.Context(), cmd.OutOrStdout(), *res.Envelope, "voice", report, err)
		return &envelopePrinted{err: err}
	}
	return err
}

// sayVoiceOn is what a person hears when their first voice is made (D11).
func sayVoiceOn(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Your voice is on for every run. Turn it off with `agentry voice attach off`, or for one run with --no-voice.")
}

// voiceErr classes a store's refusal: no voice kept is a usage error.
func voiceErr(err error) error {
	if errors.Is(err, voice.ErrNoVoice) {
		return engine.Classify(engine.ExitUsage, err)
	}
	return err
}

func newVoiceShowCmd() *cobra.Command {
	var meta bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the Writing Voice kept on this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			if meta {
				v, err := s.Voice()
				if err != nil {
					return voiceErr(err)
				}
				printJSON(cmd.OutOrStdout(), v.Meta)
				return nil
			}
			text, v, err := s.OpenVoice(cmd.Context())
			if err != nil {
				return voiceErr(err)
			}
			_, _ = cmd.OutOrStdout().Write(text)
			attach := "on"
			if !v.Meta.AlwaysAttach {
				attach = "off"
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\n(%s; attached to every run: %s)\n", v.Meta.Describe(), attach)
			return nil
		},
	}
	cmd.Flags().BoolVar(&meta, "json", false, "print the voice's record (digest, size, source, attach) as JSON instead of its text")
	return cmd
}

func newVoiceSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set FILE|-",
		Short: "Keep your own text as the Writing Voice (a hand edit)",
		Long:  "Keep the VOICE.md in FILE (or on stdin, with -) as the voice on this computer, in place of the one kept here.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := cmd.InOrStdin()
			if args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
				defer func() { _ = f.Close() }()
				r = f
			}
			text, err := io.ReadAll(io.LimitReader(r, voice.MaxBytes+1))
			if err != nil {
				return err
			}
			if err := voice.Check(text); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			v, created, err := s.KeepVoice(cmd.Context(), text, voice.SourceEdit, nil, voiceNow())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Kept your Writing Voice on this computer (%d bytes, %s).\n", v.Meta.Bytes, v.Meta.SHA256)
			if created {
				sayVoiceOn(cmd.OutOrStdout())
			}
			return nil
		},
	}
}

func newVoiceAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "attach on|off",
		Short:     "Turn attaching the Writing Voice to every run on or off",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var on bool
			switch args[0] {
			case "on":
				on = true
			case "off":
			default:
				return engine.Classify(engine.ExitUsage, fmt.Errorf("attach takes on or off, not %q", args[0]))
			}
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			if _, err := s.SetVoiceAttach(on); err != nil {
				return voiceErr(err)
			}
			if on {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Your voice is on for every run on this computer (--no-voice skips it for one).")
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Your voice is off: a run attaches it only with --voice.")
			}
			return nil
		},
	}
}

func newVoiceDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Delete the Writing Voice kept on this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := voiceStore(cmd)
			if err != nil {
				return err
			}
			if err := s.DeleteVoice(); err != nil {
				return voiceErr(err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Deleted the Writing Voice kept on this computer.")
			return nil
		},
	}
}

// attachVoice is the voice a run attaches (plan writing-voice.md D5, D11):
// --voice-file's, else this computer's when --voice says so or its
// always-attach flag is on and --no-voice does not say otherwise. None is
// nil. It says on stderr what it attached.
func attachVoice(cmd *cobra.Command, opts runOpts) (*engine.AttachedVoice, error) {
	if opts.voice && opts.noVoice {
		return nil, engine.Classify(engine.ExitUsage, errors.New("--voice and --no-voice contradict each other"))
	}
	if opts.noVoice {
		return nil, nil
	}
	if opts.voiceFile != "" {
		text, err := readVoiceFile(opts.voiceFile)
		if err != nil {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--voice-file: %w", err))
		}
		v, err := engine.AttachVoice(text, "given with --voice-file")
		if err != nil {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--voice-file: %w", err))
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Writing Voice from %s attached (%d bytes)\n", opts.voiceFile, len(text))
		return v, nil
	}
	s, err := voiceStore(cmd)
	if err != nil {
		return nil, err
	}
	kept, err := s.Voice()
	switch {
	case errors.Is(err, voice.ErrNoVoice):
		if opts.voice {
			return nil, engine.Classify(engine.ExitUsage, err)
		}
		return nil, nil
	case err != nil:
		return nil, err
	}
	if !opts.voice && !kept.Meta.AlwaysAttach {
		return nil, nil
	}
	// A kept voice that will not open (signed out of the registry that
	// wraps its key, altered) is a plan-time refusal, as a kept pack's is.
	text, kept, err := s.OpenVoice(cmd.Context())
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	v, err := engine.AttachVoice(text, "kept on this computer, "+kept.Meta.Source.Phrase()+", not reviewed")
	if err != nil {
		return nil, engine.Classify(engine.ExitVerify, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Writing Voice attached (%d bytes, %s; --no-voice skips it)\n", len(text), kept.Meta.Source.Phrase())
	return v, nil
}

// readVoiceFile reads a VOICE.md a person or a driver named: a regular
// file, never followed through a link, read no further than one byte past
// the format's cap, so the format's own check is what refuses it.
func readVoiceFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // a file the person or the driver named
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, voice.MaxBytes+1))
}
