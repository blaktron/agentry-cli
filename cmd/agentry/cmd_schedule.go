// `agentry schedule`: runs on this computer for later and on repeat (plan
// quickagent-scheduling.md, agentry-cli#747). The store under the runner
// home is the truth about every schedule; the operating system's own
// scheduler only wakes `agentry schedule fire <id>`, which re-reads the
// store and decides (D4). A fire runs the exact version consented to when
// the schedule was made, autonomous, never with a blanket --yes (D3), and
// everything it needs is checked when the schedule is made (D8).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// scheduleAlarm is the system's alarm; replaced in tests.
var scheduleAlarm = schedule.ForThisSystem

// scheduleNow is the clock fire reads; replaced in tests.
var scheduleNow = time.Now

// scheduleExecute runs one scheduled run through `agentry run` in this
// process and returns what became of it; replaced in tests.
var scheduleExecute = executeScheduledRun

func newScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Run a QuickAgent on this computer later, or on repeat like a calendar entry",
		Long: "Schedules run an installed QuickAgent on this computer at a later time, or on\n" +
			"repeat: every day, weekdays, chosen days of the week, or a day of the month,\n" +
			"each at a time in a time zone. No sign-in is needed for a public release.\n\n" +
			"A schedule runs the exact version it was made for, autonomous, with the\n" +
			"consent given when it was made; a newer version needs a new schedule.\n" +
			"Everything a run needs (the model key in the keychain, the secrets, the\n" +
			"connections, Docker, the output folder) is checked when it is made.\n\n" +
			"The schedule is kept under the runner home; the system's own scheduler\n" +
			"(on Linux a systemd user timer) wakes `agentry schedule fire`. Schedules\n" +
			"run while you are signed in to this computer. An occurrence missed while\n" +
			"the computer was off runs once when it is back, if that is before the next\n" +
			"one; otherwise it is recorded as missed.\n\n" +
			"  agentry schedule add acme/digest --daily 09:00 --out ~/reports\n" +
			"  agentry schedule add acme/digest --weekly tue --at 22:00 --out ~/reports\n" +
			"  agentry schedule add acme/digest --once \"2026-10-20 08:00\" --out ~/reports",
	}
	cmd.AddCommand(
		newScheduleAddCmd(),
		newScheduleListCmd(),
		newScheduleShowCmd(),
		newScheduleEditCmd(),
		newSchedulePauseCmd(true),
		newSchedulePauseCmd(false),
		newScheduleRemoveCmd(),
		newScheduleRunCmd(),
		newScheduleFireCmd(),
		newScheduleSyncCmd(),
	)
	return cmd
}

func scheduleStore() (schedule.Store, error) {
	home, err := runnerHome()
	if err != nil {
		return schedule.Store{}, err
	}
	return schedule.StoreIn(home), nil
}

// whenFlags are the repeat flags add and edit share.
type whenFlags struct {
	once, daily, weekdays, weekly, at, zone, until string
	monthly, times                                 int
}

func (w *whenFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&w.once, "once", "", `run once at "YYYY-MM-DD HH:MM"`)
	f.StringVar(&w.daily, "daily", "", "run every day at HH:MM")
	f.StringVar(&w.weekdays, "weekdays", "", "run Monday to Friday at HH:MM")
	f.StringVar(&w.weekly, "weekly", "", "run every week on DAYS (mon,tue,…) at --at")
	f.IntVar(&w.monthly, "monthly", 0, "run every month on day N (1–31; a month without it runs on its last day) at --at")
	f.StringVar(&w.at, "at", "", "the time (HH:MM, 24-hour) for --weekly and --monthly")
	f.StringVar(&w.zone, "zone", "", "IANA time zone of the times (default: this computer's)")
	f.StringVar(&w.until, "until", "", "repeat until this date (YYYY-MM-DD), its occurrence included")
	f.IntVar(&w.times, "times", 0, "repeat this many times, then stop")
}

func (w *whenFlags) given() bool {
	return w.once != "" || w.daily != "" || w.weekdays != "" || w.weekly != "" || w.monthly != 0
}

// recurrence builds the repeat the flags describe, starting at start.
func (w *whenFlags) recurrence(start time.Time) (schedule.Recurrence, error) {
	r := schedule.Recurrence{Zone: w.zone, Start: start}
	if r.Zone == "" {
		r.Zone = schedule.LocalZone()
	}
	n := 0
	for _, set := range []bool{w.once != "", w.daily != "", w.weekdays != "", w.weekly != "", w.monthly != 0} {
		if set {
			n++
		}
	}
	if n != 1 {
		return r, errors.New("give exactly one of --once, --daily, --weekdays, --weekly or --monthly")
	}
	needAt := w.weekly != "" || w.monthly != 0
	if needAt && w.at == "" {
		return r, errors.New("--weekly and --monthly need --at HH:MM")
	}
	if !needAt && w.at != "" {
		return r, errors.New("--at is for --weekly and --monthly; the others carry their own time")
	}
	switch {
	case w.once != "":
		date, clock, ok := strings.Cut(strings.TrimSpace(w.once), " ")
		if !ok {
			return r, fmt.Errorf(`--once %q is not "YYYY-MM-DD HH:MM"`, w.once)
		}
		r.Kind, r.Date, r.Time = schedule.Once, date, strings.TrimSpace(clock)
	case w.daily != "":
		r.Kind, r.Time = schedule.Daily, w.daily
	case w.weekdays != "":
		r.Kind, r.Time = schedule.Weekdays, w.weekdays
	case w.weekly != "":
		days, err := schedule.ParseDays(w.weekly)
		if err != nil {
			return r, fmt.Errorf("--weekly: %w", err)
		}
		r.Kind, r.Days, r.Time = schedule.Weekly, days, w.at
	default:
		r.Kind, r.MonthDay, r.Time = schedule.Monthly, w.monthly, w.at
	}
	switch {
	case w.until != "" && w.times != 0:
		return r, errors.New("give --until or --times, not both")
	case w.until != "":
		r.End = schedule.End{Kind: schedule.EndOn, On: w.until}
	case w.times != 0:
		r.End = schedule.End{Kind: schedule.EndAfter, After: w.times}
	default:
		r.End = schedule.End{Kind: schedule.EndNever}
	}
	if r.Kind == schedule.Once {
		r.End = schedule.End{}
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	if _, ok := r.Next(start); !ok {
		return r, errors.New("that schedule has no occurrence after now")
	}
	return r, nil
}

// runSpecFlags are the run setup's flags add takes.
type runSpecFlags struct {
	input, task, local                          string
	files, knowledge, intake                    []string
	voice, noVoice                              bool
	timeout, maxTurns                           int
	maxCost                                     float64
	adapter, policyPath                         string
	allowUnsigned, allowUnscanned, allowVulnMCP bool
}

func newScheduleAddCmd() *cobra.Command {
	var (
		when    whenFlags
		spec    runSpecFlags
		name    string
		out     string
		yes     bool
		noCheck bool
	)
	cmd := &cobra.Command{
		Use:   "add [ns/name[@range]]",
		Short: "Schedule a QuickAgent to run on this computer",
		Long: "Schedule a QuickAgent to run later or on repeat. The release is resolved now\n" +
			"and its exact version pinned; its capabilities are shown and consented to\n" +
			"now (on a terminal, or with --yes), its MCP profile and connections set up,\n" +
			"and its images installed, the same install step as `agentry run --prepare`.\n" +
			"Then every input a scheduled run will need is checked: a run fired by the\n" +
			"system does not see this shell, so the model key must be in the keychain\n" +
			"and every secret in the MCP profile or the policy's envFile source.\n\n" +
			"Only a release that can run autonomous can be scheduled: nobody is there\n" +
			"to answer an interactive one.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			if (ref == "") == (spec.local == "") {
				return errors.New("pass exactly one of ns/name[@range] or --local DIR")
			}
			if out == "" {
				return errors.New("--out DIR is required: each scheduled run gets its own folder in it")
			}
			if spec.input != "" && spec.task != "" {
				return errors.New("give --input FILE or --task TEXT, not both")
			}
			now := scheduleNow()
			when, err := when.recurrence(now)
			if err != nil {
				return err
			}
			return scheduleAdd(cmd, ref, name, out, spec, when, now, yes, noCheck)
		},
	}
	when.bind(cmd)
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "a name for the schedule (default: the QuickAgent's)")
	f.StringVar(&out, "out", "", "the folder each scheduled run's own output folder is made in (required)")
	f.StringVar(&spec.local, "local", "", "schedule an unpublished QuickAgent DIRECTORY (needs --allow-unsigned)")
	f.StringVar(&spec.input, "input", "", "task file, read at each run (default: the agent's own instruction)")
	f.StringVar(&spec.task, "task", "", "task text, kept with the schedule")
	f.StringArrayVar(&spec.files, "file", nil, "file or directory each run is given read-only under /inputs (repeatable; read at each run)")
	f.StringArrayVar(&spec.knowledge, "knowledge", nil, "knowledge pack to attach: a kept pack's NAME or a pack DIRECTORY (repeatable)")
	f.StringArrayVar(&spec.intake, "intake", nil, "answer an intake question: id=value (repeatable; never a secret question's)")
	f.BoolVar(&spec.voice, "voice", false, "attach the Writing Voice kept on this computer")
	f.BoolVar(&spec.noVoice, "no-voice", false, "do not attach the Writing Voice")
	f.IntVar(&spec.timeout, "timeout", 0, "override the policy timeout (seconds)")
	f.IntVar(&spec.maxTurns, "max-turns", -1, "override the policy tool-call budget (0 = unlimited)")
	f.Float64Var(&spec.maxCost, "max-cost", -1, "override the policy cost budget in USD (0 = unlimited)")
	f.StringVar(&spec.adapter, "adapter", "", "harness adapter (default: the run's)")
	f.StringVar(&spec.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&spec.allowUnsigned, "allow-unsigned", false, "allow a release with no registry attestation, at every run of this schedule")
	f.BoolVar(&spec.allowUnscanned, "allow-unscanned", false, "allow a release the registry admitted without a scan, at every run of this schedule")
	f.BoolVar(&spec.allowVulnMCP, "allow-vulnerable-mcp", false, "allow a declared MCP server the gate rates high or critical, at every run of this schedule")
	f.BoolVar(&yes, "yes", false, "consent to the release's capabilities without the prompt (no terminal)")
	f.BoolVar(&noCheck, "no-check", false, "skip the install step and the input checks (for tests only; the first run may fail)")
	_ = f.MarkHidden("no-check")
	return cmd
}

// scheduleAdd makes one schedule: resolve and pin (D3), consent and
// install through `run --prepare`, check every input a fire needs (D8),
// keep it, and write its alarm.
func scheduleAdd(cmd *cobra.Command, ref, name, out string, spec runSpecFlags, when schedule.Recurrence, now time.Time, yes, noCheck bool) error {
	outAbs, err := filepath.Abs(expandHome(out))
	if err != nil {
		return err
	}
	if err := writableDir(outAbs); err != nil {
		return fmt.Errorf("--out %s: %w", out, err)
	}
	sc := schedule.Schedule{
		Name: name, Registry: registryURL(), Created: now, When: when,
		Run: schedule.RunSpec{
			OutDir: outAbs, Files: absAll(spec.files), Knowledge: knowledgeRefs(spec.knowledge), Intake: spec.intake,
			Voice: spec.voice, NoVoice: spec.noVoice, Timeout: spec.timeout, MaxTurns: spec.maxTurns, MaxCost: spec.maxCost,
			Adapter: spec.adapter, AllowUnsigned: spec.allowUnsigned, AllowUnscanned: spec.allowUnscanned,
			AllowVulnerableMCP: spec.allowVulnMCP,
		},
	}
	if spec.policyPath != "" {
		if sc.Run.Policy, err = filepath.Abs(expandHome(spec.policyPath)); err != nil {
			return err
		}
	}
	if spec.input != "" {
		if sc.Run.Input, err = filepath.Abs(expandHome(spec.input)); err != nil {
			return err
		}
		if _, err := os.Stat(sc.Run.Input); err != nil {
			return fmt.Errorf("--input: %w", err)
		}
	}
	if spec.local != "" {
		if sc.Local, err = filepath.Abs(expandHome(spec.local)); err != nil {
			return err
		}
	}

	errOut := cmd.ErrOrStderr()
	preview, err := schedulePreview(cmd, sc, ref)
	if err != nil {
		return err
	}
	if !slices.Contains(preview.Modes, string(manifest.ModeAutonomous)) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s runs interactive only: a schedule runs with nobody there to answer, so it can only run an autonomous release", preview.Ref))
	}
	for _, r := range preview.Refusals {
		if r.Lift == "" || !liftGiven(r.Lift, sc.Run) {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("a run would refuse: %s", r.Message))
		}
	}
	if sc.Local == "" {
		parsed, perr := manifest.ParseRef(preview.Ref)
		if perr != nil {
			return perr
		}
		sc.Ref = parsed.Namespace + "/" + parsed.Name
	} else {
		sc.Ref = preview.Ref
	}
	sc.Version, sc.Digest = preview.Version, preview.Digest
	if sc.Name == "" {
		sc.Name = sc.Ref
	}

	if !noCheck {
		// The install step a run makes, with consent asked now on a terminal
		// (or given by --yes) and recorded for this exact version.
		if err := schedulePrepare(cmd, sc, yes); err != nil {
			return err
		}
		if gaps := fireGaps(sc); len(gaps) > 0 {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("a scheduled run of %s would not have what it needs:\n  - %s", sc.Ref, strings.Join(gaps, "\n  - ")))
		}
	}

	store, err := scheduleStore()
	if err != nil {
		return err
	}
	var task []byte
	if spec.task != "" {
		task = []byte(spec.task)
	}
	if err := store.Create(&sc, task); err != nil {
		return err
	}
	if err := installAlarm(sc); err != nil {
		_ = store.Remove(sc.ID)
		return fmt.Errorf("the schedule was not kept: %w", err)
	}
	next, _ := sc.When.Next(now)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), scheduleView(store, sc, now))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "scheduled %s (%s@%s): %s\nnext run %s\nschedule id %s\n",
		sc.Name, sc.Ref, sc.Version, sc.When.Describe(), fmtWhen(next, sc.When.Zone), sc.ID)
	_, _ = fmt.Fprintln(errOut, "it runs while you are signed in to this computer; each run's folder is made in "+sc.Run.OutDir)
	return nil
}

// liftGiven reports whether the person gave, at add, the flag that lifts a
// refusal.
func liftGiven(lift string, r schedule.RunSpec) bool {
	switch lift {
	case receipt.ConsentAllowUnsigned:
		return r.AllowUnsigned
	case receipt.ConsentAllowUnscanned:
		return r.AllowUnscanned
	case "--allow-vulnerable-mcp":
		return r.AllowVulnerableMCP
	}
	return false
}

// schedulePreview is `run --plan --json` in this process: the version and
// digest a schedule pins, its modes, and what a run would refuse.
func schedulePreview(cmd *cobra.Command, sc schedule.Schedule, ref string) (*runPlanOutput, error) {
	args := []string{"--plan"}
	if sc.Local != "" {
		args = append(args, "--local", sc.Local)
	} else {
		args = append([]string{ref}, args...)
	}
	if sc.Run.Policy != "" {
		args = append(args, "--policy", sc.Run.Policy)
	}
	args = append(args, allowArgs(sc.Run)...)
	var stdout bytes.Buffer
	saved := flags.jsonOut
	flags.jsonOut = true
	defer func() { flags.jsonOut = saved }()
	if err := runInProcess(cmd, args, bytes.NewReader(nil), &stdout); err != nil {
		return nil, err
	}
	var p runPlanOutput
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		return nil, fmt.Errorf("read the run plan: %w", err)
	}
	if p.Digest == "" || p.Version == "" {
		return nil, errors.New("the run plan names no version and digest to pin")
	}
	return &p, nil
}

// schedulePrepare is `run --prepare` for the pinned version: verify,
// consent, compile, images, MCP profile and connections, with the person's
// terminal for the questions it asks.
func schedulePrepare(cmd *cobra.Command, sc schedule.Schedule, yes bool) error {
	args := append(baseRunArgs(sc), "--prepare")
	if yes {
		args = append(args, "--yes")
	}
	return runInProcess(cmd, args, cmd.InOrStdin(), cmd.ErrOrStderr())
}

// runInProcess runs `agentry run args…` in this process against the
// schedule's registry, with in as its stdin and its stdout to out.
func runInProcess(cmd *cobra.Command, args []string, in interface{ Read([]byte) (int, error) }, out interface{ Write([]byte) (int, error) }) error {
	rc := newRunCmd()
	rc.SetArgs(args)
	rc.SetIn(in)
	rc.SetOut(out)
	rc.SetErr(cmd.ErrOrStderr())
	rc.SilenceUsage, rc.SilenceErrors = true, true
	return rc.ExecuteContext(cmd.Context())
}

// baseRunArgs is what every run of sc is given: the pinned release, the
// mode, the policy, the run setup's answers and the consents given at add.
func baseRunArgs(sc schedule.Schedule) []string {
	var args []string
	if sc.Local != "" {
		args = append(args, "--local", sc.Local)
	} else {
		args = append(args, sc.Ref+"@"+sc.Version, "--pin", sc.Digest)
	}
	args = append(args, "--mode", string(manifest.ModeAutonomous))
	r := sc.Run
	if r.Policy != "" {
		args = append(args, "--policy", r.Policy)
	}
	for _, f := range r.Files {
		args = append(args, "--file", f)
	}
	for _, k := range r.Knowledge {
		args = append(args, "--knowledge", k)
	}
	for _, i := range r.Intake {
		args = append(args, "--intake", i)
	}
	if r.Voice {
		args = append(args, "--voice")
	}
	if r.NoVoice {
		args = append(args, "--no-voice")
	}
	if r.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(r.Timeout))
	}
	if r.MaxTurns >= 0 {
		args = append(args, "--max-turns", strconv.Itoa(r.MaxTurns))
	}
	if r.MaxCost >= 0 {
		args = append(args, "--max-cost", strconv.FormatFloat(r.MaxCost, 'f', -1, 64))
	}
	if r.Adapter != "" {
		args = append(args, "--adapter", r.Adapter)
	}
	return append(args, allowArgs(r)...)
}

func allowArgs(r schedule.RunSpec) []string {
	var args []string
	if r.AllowUnsigned {
		args = append(args, "--allow-unsigned")
	}
	if r.AllowUnscanned {
		args = append(args, "--allow-unscanned")
	}
	if r.AllowVulnerableMCP {
		args = append(args, "--allow-vulnerable-mcp")
	}
	return args
}

// fireGaps is every input a fire would lack (D8). A fire runs under the
// system's scheduler, not this shell: an environment variable set here is
// not there, so a key or a secret only in the environment is a gap.
func fireGaps(sc schedule.Schedule) []string {
	home, err := runnerHome()
	if err != nil {
		return []string{err.Error()}
	}
	pf, err := policy.Resolve(sc.Run.Policy)
	if err != nil {
		return []string{err.Error()}
	}
	pol, _, err := policy.EnsureDefault(pf.Path)
	if err != nil {
		return []string{err.Error()}
	}
	m, ns, name, err := scheduledManifest(home, sc)
	if err != nil {
		return []string{err.Error()}
	}
	var prof *profile.Profile
	if ns != "" {
		prof, _ = profile.Load(profile.Dir(home, ns, name, sc.Version, installdir.DigestSuffix(sc.Digest)))
	}
	var profSecrets map[string]string
	if prof != nil {
		profSecrets = prof.Secrets
	}
	return checkFireInputs(m, pol, profSecrets, sc.Run.Intake, modelkey.Present, os.Getenv)
}

// checkFireInputs is fireGaps' decision, with the keychain and the
// environment passed in so it can be tested.
func checkFireInputs(m *manifest.Manifest, pol *policy.Policy, profSecrets map[string]string, presetIntake []string,
	keyPresent func(provider string) (bool, error), getenv func(string) string,
) []string {
	var gaps []string
	// The model key.
	if env := pol.Model.APIKeyEnv; env != "" {
		inEnv := strings.TrimSpace(getenv(env)) != ""
		if pol.Model.APIKeySource == modelkey.SourceKeyring {
			ok, err := keyPresent(pol.Model.Provider)
			switch {
			case err != nil:
				gaps = append(gaps, fmt.Sprintf("the model key: %v", err))
			case !ok && inEnv:
				gaps = append(gaps, fmt.Sprintf("the model key is only in $%s in this shell, which a scheduled run does not see: keep it in the keychain with `agentry setup`", env))
			case !ok:
				gaps = append(gaps, "no model key is kept in the keychain: run `agentry setup`")
			}
		} else {
			gaps = append(gaps, fmt.Sprintf("the model key comes from $%s, which a scheduled run does not see: keep it in the keychain with `agentry setup`", env))
		}
	}
	// The declared secrets a run must have.
	optional := map[string]bool{}
	for _, n := range m.Capabilities.OptionalSecrets {
		optional[n] = true
	}
	held := func(name string) bool {
		if v, ok := profSecrets[name]; ok && v != "" {
			return true
		}
		return pol.SecretSource.Mode == "envFile" && engine.SecretResolvable(name, nil, pol.SecretSource)
	}
	for _, n := range allSecretNames(m) {
		if optional[n] || held(n) {
			continue
		}
		if pol.SecretSource.Mode != "envFile" && getenv(n) != "" {
			gaps = append(gaps, fmt.Sprintf("the secret %s is only in this shell's environment, which a scheduled run does not see: keep it in the QuickAgent's MCP profile (run it once on a terminal) or in the policy's envFile", n))
			continue
		}
		gaps = append(gaps, fmt.Sprintf("the secret %s is not kept: set it in the QuickAgent's MCP profile (run it once on a terminal) or in the policy's envFile", n))
	}
	// The intake: answered at add, never a secret question by flag (it
	// would be kept in the clear), a secret one from the profile or source.
	preset, err := intake.ParsePreset(presetIntake)
	if err != nil {
		return append(gaps, err.Error())
	}
	for _, q := range m.Intake {
		if _, given := preset[q.ID]; given && q.Type == manifest.IntakeSecret {
			gaps = append(gaps, fmt.Sprintf("intake %s is a secret: a schedule keeps no secret answer; keep %s in the MCP profile or the policy's envFile instead", q.ID, intake.SecretName(q.ID)))
		}
	}
	if len(m.Intake) > 0 || len(preset) > 0 {
		if _, err := intake.CollectWithSource(nil, m.Intake, preset, m.Capabilities.Secrets, held); err != nil {
			gaps = append(gaps, err.Error())
		}
	}
	return gaps
}

// scheduledManifest reads the pinned release's manifest from its install
// (the prepare step laid it down), or a local directory's.
func scheduledManifest(home string, sc schedule.Schedule) (*manifest.Manifest, string, string, error) {
	if sc.Local != "" {
		m, err := localrun.ManifestFrom(sc.Local)
		return m, "", "", err
	}
	ns, name, ok := strings.Cut(sc.Ref, "/")
	if !ok {
		return nil, "", "", fmt.Errorf("schedule ref %q is not ns/name", sc.Ref)
	}
	dir := installdir.ReleaseDir(home, ns, name, sc.Version, sc.Digest)
	m, err := localrun.ManifestFrom(filepath.Join(dir, "workspace"))
	if err != nil {
		return nil, "", "", fmt.Errorf("read the installed %s@%s: %w", sc.Ref, sc.Version, err)
	}
	return m, ns, name, nil
}

// installAlarm writes sc's entry in the system's scheduler.
func installAlarm(sc schedule.Schedule) error {
	alarm, err := scheduleAlarm()
	if err != nil {
		return err
	}
	e, err := fireEntry(sc.ID)
	if err != nil {
		return err
	}
	return alarm.Install(sc, e)
}

func removeAlarm(id string) error {
	alarm, err := scheduleAlarm()
	if err != nil {
		return err
	}
	return alarm.Remove(id)
}

// fireEntry is the command the alarm runs: this binary's absolute path,
// and the paths a fire needs resolved now, never a credential
// (invariant 7).
func fireEntry(id string) (schedule.Entry, error) {
	bin, err := os.Executable()
	if err != nil {
		return schedule.Entry{}, fmt.Errorf("find this agentry binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	e := schedule.Entry{Binary: bin, Args: []string{"schedule", "fire", id}}
	if p := os.Getenv("PATH"); p != "" {
		e.Env = append(e.Env, "PATH="+p)
	}
	docker := os.Getenv("AGENTRY_DOCKER")
	if docker == "" {
		if p, err := exec.LookPath("docker"); err == nil {
			docker, _ = filepath.Abs(p)
		}
	}
	if docker != "" {
		e.Env = append(e.Env, "AGENTRY_DOCKER="+docker)
	}
	// Where the runner home, the CLI's config and the docker daemon are,
	// when this shell names them: paths and names, never values of keys.
	for _, k := range []string{"AGENTRY_RUNNER_HOME", "AGENTRY_CONFIG_DIR", "DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v := os.Getenv(k); v != "" {
			e.Env = append(e.Env, k+"="+v)
		}
	}
	return e, nil
}

func newScheduleFireCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "fire <id>",
		Short:  "Run a schedule's occurrence if one is due (what the system's scheduler calls)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return scheduleFire(cmd, args[0])
		},
	}
}

// scheduleFire is one alarm ring: re-read the store, record what was
// missed, run what is due once, and end the schedule when nothing is left.
func scheduleFire(cmd *cobra.Command, id string) error {
	store, err := scheduleStore()
	if err != nil {
		return err
	}
	errOut := cmd.ErrOrStderr()
	sc, err := store.Get(id)
	if errors.Is(err, schedule.ErrNotFound) {
		// An alarm that outlived its schedule: remove it.
		_, _ = fmt.Fprintf(errOut, "schedule %s is gone; removing its alarm\n", id)
		return removeAlarm(id)
	}
	if err != nil {
		return err
	}
	st, err := store.State(id)
	if err != nil {
		return err
	}
	if st.Ended {
		_, _ = fmt.Fprintf(errOut, "schedule %s has ended; removing its alarm\n", id)
		return removeAlarm(id)
	}
	if sc.Paused {
		_, _ = fmt.Fprintf(errOut, "schedule %s is paused\n", id)
		return nil
	}
	now := scheduleNow()
	run, missed := schedule.Due(sc.When, st, now)
	for _, m := range missed {
		if ok, err := store.Claim(id, m); err != nil || !ok {
			continue
		}
		_ = store.AppendFire(id, schedule.Fire{Occurrence: m, Outcome: schedule.Missed,
			Message: "the computer was off, asleep or signed out, and the next chance came after the following occurrence"})
		st.Last = m
	}
	if run.IsZero() {
		if len(missed) > 0 {
			_ = store.SaveState(id, st)
		}
		_, _ = fmt.Fprintf(errOut, "schedule %s: nothing due\n", id)
		return endIfDone(store, sc, st, now)
	}
	won, err := store.Claim(id, run)
	if err != nil {
		return err
	}
	if !won {
		_, _ = fmt.Fprintf(errOut, "schedule %s: the occurrence of %s is already being run\n", id, fmtWhen(run, sc.When.Zone))
		return nil
	}
	st.Last = run
	if err := store.SaveState(id, st); err != nil {
		return err
	}
	store.Prune(id, now.AddDate(0, 0, -30))
	rec := scheduleExecute(cmd, store, sc, run, false)
	if err := store.AppendFire(id, rec); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: record the run: %v\n", err)
	}
	_ = endIfDone(store, sc, st, run)
	if rec.Outcome != schedule.Ran {
		return &exitError{code: max(rec.ExitCode, 1), err: fmt.Errorf("scheduled run of %s failed: %s", sc.Ref, rec.Message)}
	}
	return nil
}

// endIfDone marks a schedule with no occurrence after t as ended and
// removes its alarm.
func endIfDone(store schedule.Store, sc schedule.Schedule, st schedule.State, t time.Time) error {
	if _, ok := sc.When.Next(t); ok {
		return nil
	}
	st.Ended = true
	if err := store.SaveState(sc.ID, st); err != nil {
		return err
	}
	return removeAlarm(sc.ID)
}

// executeScheduledRun runs sc once, for occurrence (or now, manual), with
// no terminal: stdin is empty, so nothing is asked and the task is the
// schedule's own or the agent's instruction.
func executeScheduledRun(cmd *cobra.Command, store schedule.Store, sc schedule.Schedule, occurrence time.Time, manual bool) schedule.Fire {
	rec := schedule.Fire{Occurrence: occurrence, Manual: manual, Started: scheduleNow()}
	outDir, err := runFolder(sc, occurrence, manual)
	if err != nil {
		rec.Outcome, rec.ExitCode, rec.Message, rec.Ended = schedule.Failed, engine.ExitCLI, err.Error(), scheduleNow()
		return rec
	}
	rec.OutDir = outDir
	args := append(baseRunArgs(sc), "--out", outDir, "--output-format", "json")
	switch {
	case sc.Run.Task:
		args = append(args, "--input", store.TaskPath(sc.ID))
	case sc.Run.Input != "":
		args = append(args, "--input", sc.Run.Input)
	}
	savedRegistry := flags.registry
	flags.registry = sc.Registry
	defer func() { flags.registry = savedRegistry }()
	var stdout bytes.Buffer
	err = runInProcess(cmd, args, bytes.NewReader(nil), &stdout)
	rec.Ended = scheduleNow()
	var env receipt.Envelope
	if json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env) == nil {
		rec.RunID, rec.ExitCode, rec.ExitClass = env.RunID, env.ExitCode, env.ExitClass
		rec.Message = env.Error
	}
	if err == nil {
		rec.Outcome = schedule.Ran
		return rec
	}
	rec.Outcome = schedule.Failed
	if rec.ExitCode == 0 {
		rec.ExitCode = engine.ExitCLI
		var ce *engine.ClassifiedError
		if errors.As(err, &ce) {
			rec.ExitCode = ce.Code
		}
	}
	if rec.Message == "" {
		rec.Message = err.Error()
	}
	return rec
}

// runFolder is a new folder for one run in the schedule's output folder,
// named for the QuickAgent and the occurrence's wall-clock time.
func runFolder(sc schedule.Schedule, occurrence time.Time, manual bool) (string, error) {
	loc, err := time.LoadLocation(sc.When.Zone)
	if err != nil {
		loc = time.UTC
	}
	base := strings.ReplaceAll(sc.Ref, "/", "-")
	if sc.Local != "" {
		base = filepath.Base(sc.Local)
	}
	stamp := occurrence.In(loc).Format("20060102-1504")
	if manual {
		stamp += "-now"
	}
	if err := os.MkdirAll(sc.Run.OutDir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 100; i++ {
		name := base + "-" + stamp
		if i > 0 {
			name += "-" + strconv.Itoa(i+1)
		}
		dir := filepath.Join(sc.Run.OutDir, name)
		if err := os.Mkdir(dir, 0o755); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free folder name for %s in %s", base+"-"+stamp, sc.Run.OutDir)
}

func newScheduleRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <id>",
		Short: "Run a schedule now (Run now: an ordinary run, no occurrence is used up)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			sc, err := store.Get(args[0])
			if err != nil {
				return err
			}
			now := scheduleNow()
			rec := scheduleExecute(cmd, store, sc, now, true)
			if err := store.AppendFire(sc.ID, rec); err != nil {
				return err
			}
			if rec.Outcome != schedule.Ran {
				return &exitError{code: max(rec.ExitCode, 1), err: fmt.Errorf("run of %s failed: %s", sc.Ref, rec.Message)}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ran %s: %s\n", sc.Name, rec.OutDir)
			return nil
		},
	}
}

// scheduleViewJSON is one schedule as list and show report it.
type scheduleViewJSON struct {
	schedule.Schedule
	Describe string          `json:"describe"`
	Next     *time.Time      `json:"next,omitempty"`
	Ended    bool            `json:"ended,omitempty"`
	Alarm    bool            `json:"alarm"`
	Last     *schedule.Fire  `json:"lastFire,omitempty"`
	Upcoming []time.Time     `json:"upcoming,omitempty"`
	Fires    []schedule.Fire `json:"fires,omitempty"`
}

func scheduleView(store schedule.Store, sc schedule.Schedule, now time.Time) scheduleViewJSON {
	v := scheduleViewJSON{Schedule: sc, Describe: sc.When.Describe()}
	st, _ := store.State(sc.ID)
	v.Ended = st.Ended
	if !st.Ended {
		if n, ok := sc.When.Next(now); ok {
			v.Next = &n
		}
	}
	if fires, _ := store.Fires(sc.ID, 1); len(fires) == 1 {
		v.Last = &fires[0]
	}
	if alarm, err := scheduleAlarm(); err == nil {
		ids, _ := alarm.Installed()
		v.Alarm = slices.Contains(ids, sc.ID)
	}
	return v
}

func newScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the schedules on this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			list, bad, err := store.List()
			if err != nil {
				return err
			}
			for _, b := range bad {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", b)
			}
			now := scheduleNow()
			views := make([]scheduleViewJSON, 0, len(list))
			for _, sc := range list {
				views = append(views, scheduleView(store, sc, now))
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), views)
			}
			if len(views) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no schedules on this computer")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tNAME\tQUICKAGENT\tWHEN\tNEXT\tLAST")
			for _, v := range views {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, v.Ref+"@"+v.Version, v.Describe, nextLabel(v), lastLabel(v.Last))
			}
			return tw.Flush()
		},
	}
}

func nextLabel(v scheduleViewJSON) string {
	switch {
	case v.Paused:
		return "paused"
	case v.Ended || v.Next == nil:
		return "ended"
	case !v.Alarm:
		return fmtWhen(*v.Next, v.When.Zone) + " (no alarm: run `agentry schedule sync`)"
	}
	return fmtWhen(*v.Next, v.When.Zone)
}

func lastLabel(f *schedule.Fire) string {
	if f == nil {
		return "-"
	}
	s := string(f.Outcome)
	if f.Outcome == schedule.Failed {
		s += " (exit " + strconv.Itoa(f.ExitCode) + ")"
	}
	return s + " " + f.Occurrence.Local().Format("2006-01-02 15:04")
}

func newScheduleShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one schedule, its next occurrences and what its runs did",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			sc, err := store.Get(args[0])
			if err != nil {
				return err
			}
			now := scheduleNow()
			v := scheduleView(store, sc, now)
			if !v.Ended && !sc.Paused {
				v.Upcoming = sc.When.Between(now, now.AddDate(1, 0, 0), 5)
			}
			v.Fires, _ = store.Fires(sc.ID, 10)
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), v)
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "%s (%s)\nquickagent: %s@%s %s\nwhen: %s\nnext: %s\noutput folder: %s\n",
				sc.Name, sc.ID, sc.Ref, sc.Version, sc.Digest, v.Describe, nextLabel(v), sc.Run.OutDir)
			for _, u := range v.Upcoming {
				_, _ = fmt.Fprintf(w, "  upcoming %s\n", fmtWhen(u, sc.When.Zone))
			}
			for _, f := range v.Fires {
				line := fmt.Sprintf("  %s %s", fmtWhen(f.Occurrence, sc.When.Zone), f.Outcome)
				if f.Manual {
					line += " (run now)"
				}
				if f.Outcome == schedule.Failed {
					line += fmt.Sprintf(" exit %d: %s", f.ExitCode, f.Message)
				}
				if f.OutDir != "" {
					line += " → " + f.OutDir
				}
				_, _ = fmt.Fprintln(w, line)
			}
			return nil
		},
	}
}

func newScheduleEditCmd() *cobra.Command {
	var (
		when     whenFlags
		name     string
		out      string
		timeout  int
		maxTurns int
		maxCost  float64
	)
	cmd := &cobra.Command{
		Use:   "edit <id>",
		Short: "Change a schedule's repeat, name, output folder or limits",
		Long: "Change when a schedule runs, its name, its output folder or its limits. What\n" +
			"it runs and with which inputs is fixed when it is made: for a new version or\n" +
			"other inputs, remove it and add a new one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			sc, err := store.Get(args[0])
			if err != nil {
				return err
			}
			f := cmd.Flags()
			now := scheduleNow()
			if when.given() {
				r, err := when.recurrence(now)
				if err != nil {
					return err
				}
				sc.When = r
				// A new repeat counts from now; what is past stays handled.
				st, _ := store.State(sc.ID)
				st.Ended, st.Last = false, now
				if err := store.SaveState(sc.ID, st); err != nil {
					return err
				}
			} else if f.Changed("zone") || f.Changed("until") || f.Changed("times") || f.Changed("at") {
				return errors.New("give the whole repeat again (--daily, --weekly … with --zone, --until or --times)")
			}
			if f.Changed("name") {
				sc.Name = name
			}
			if f.Changed("out") {
				abs, err := filepath.Abs(expandHome(out))
				if err != nil {
					return err
				}
				if err := writableDir(abs); err != nil {
					return fmt.Errorf("--out %s: %w", out, err)
				}
				sc.Run.OutDir = abs
			}
			if f.Changed("timeout") {
				sc.Run.Timeout = timeout
			}
			if f.Changed("max-turns") {
				sc.Run.MaxTurns = maxTurns
			}
			if f.Changed("max-cost") {
				sc.Run.MaxCost = maxCost
			}
			if err := store.Save(sc); err != nil {
				return err
			}
			if !sc.Paused {
				if err := installAlarm(sc); err != nil {
					return err
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", sc.Name, sc.When.Describe())
			return nil
		},
	}
	when.bind(cmd)
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the schedule's name")
	f.StringVar(&out, "out", "", "the folder each run's own folder is made in")
	f.IntVar(&timeout, "timeout", 0, "the policy timeout override (seconds; 0 = the policy's)")
	f.IntVar(&maxTurns, "max-turns", -1, "the tool-call budget override (-1 = the policy's, 0 = unlimited)")
	f.Float64Var(&maxCost, "max-cost", -1, "the cost budget override in USD (-1 = the policy's, 0 = unlimited)")
	return cmd
}

func newSchedulePauseCmd(pause bool) *cobra.Command {
	use, short := "resume <id>", "Resume a paused schedule (what fell due while it was paused does not run)"
	if pause {
		use, short = "pause <id>", "Pause a schedule: its alarm is removed until it is resumed"
	}
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			sc, err := store.Get(args[0])
			if err != nil {
				return err
			}
			sc.Paused = pause
			if !pause {
				// Occurrences that fell while paused are neither run nor missed.
				st, _ := store.State(sc.ID)
				st.Last = scheduleNow()
				if err := store.SaveState(sc.ID, st); err != nil {
					return err
				}
			}
			if err := store.Save(sc); err != nil {
				return err
			}
			if pause {
				err = removeAlarm(sc.ID)
			} else {
				err = installAlarm(sc)
			}
			if err != nil {
				return err
			}
			state := "resumed"
			if pause {
				state = "paused"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", state, sc.Name)
			return nil
		},
	}
}

func newScheduleRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm"},
		Short:   "Remove a schedule and its alarm (its runs' folders are kept)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := scheduleStore()
			if err != nil {
				return err
			}
			sc, err := store.Get(args[0])
			if err != nil {
				return err
			}
			if err := removeAlarm(sc.ID); err != nil {
				return err
			}
			if err := store.Remove(sc.ID); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", sc.Name)
			return nil
		},
	}
}

func newScheduleSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Rewrite every schedule's alarm from the store and remove alarms with no schedule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, removed, err := scheduleSync()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d alarm(s) written, %d removed\n", n, removed)
			return nil
		},
	}
}

// scheduleSync makes the system's alarms match the store: one per live
// schedule, written by this binary, and none for a paused, ended or
// removed one.
func scheduleSync() (written, removed int, err error) {
	store, err := scheduleStore()
	if err != nil {
		return 0, 0, err
	}
	alarm, err := scheduleAlarm()
	if err != nil {
		return 0, 0, err
	}
	list, _, err := store.List()
	if err != nil {
		return 0, 0, err
	}
	live := map[string]bool{}
	var errs []error
	for _, sc := range list {
		st, _ := store.State(sc.ID)
		if sc.Paused || st.Ended {
			continue
		}
		live[sc.ID] = true
		e, ferr := fireEntry(sc.ID)
		if ferr != nil {
			return written, removed, ferr
		}
		if ierr := alarm.Install(sc, e); ierr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sc.ID, ierr))
			continue
		}
		written++
	}
	ids, err := alarm.Installed()
	if err != nil {
		return written, removed, err
	}
	for _, id := range ids {
		if live[id] {
			continue
		}
		if rerr := alarm.Remove(id); rerr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, rerr))
			continue
		}
		removed++
	}
	return written, removed, errors.Join(errs...)
}

func fmtWhen(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("Mon 2006-01-02 15:04 MST")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func absAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if abs, err := filepath.Abs(expandHome(p)); err == nil {
			out = append(out, abs)
		} else {
			out = append(out, p)
		}
	}
	return out
}

// knowledgeRefs keeps a kept pack's NAME as given and makes a pack
// DIRECTORY absolute, so a fire from another working folder finds it.
func knowledgeRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if fi, err := os.Stat(expandHome(r)); err == nil && fi.IsDir() {
			if abs, err := filepath.Abs(expandHome(r)); err == nil {
				out = append(out, abs)
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// writableDir makes dir if needed and proves a file can be written in it.
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".agentry-schedule-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
