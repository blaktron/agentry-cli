package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// A job on a remote runhost, from the device, once it is sent: the device
// follows its narration until it ends, or leaves it running (--detach, a
// dropped connection that does not come back), and later attaches to it,
// collects its sealed output and its sealed envelope, or cancels it. What the
// device needs to do that later is kept in the runner home's
// runhost-jobs.json, with no secret in it.

// runhostLocalJob is one job this device sent, until it is collected.
type runhostLocalJob struct {
	Job       string `json:"job"`
	Runhost   string `json:"runhost"`
	Ref       string `json:"ref"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	// Out is the output folder the run was given, empty for the default.
	Out string `json:"out,omitempty"`
	// DeliverTo is the folder the device copies the deliverables to.
	DeliverTo string `json:"deliverTo,omitempty"`
	Session   bool   `json:"session,omitempty"`
	StartedAt string `json:"startedAt"`
}

const runhostLocalJobsFile = "runhost-jobs.json"

func loadLocalJobs(home string) ([]runhostLocalJob, error) {
	var doc struct {
		Jobs []runhostLocalJob `json:"jobs"`
	}
	b, err := os.ReadFile(filepath.Join(home, runhostLocalJobsFile)) //nolint:gosec // the runner home's own file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", runhostLocalJobsFile, err)
	}
	return doc.Jobs, nil
}

func saveLocalJobs(home string, jobs []runhostLocalJob) error {
	b, err := json.MarshalIndent(struct {
		Jobs []runhostLocalJob `json:"jobs"`
	}{jobs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(home, "."+runhostLocalJobsFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(home, runhostLocalJobsFile))
}

func putLocalJob(home string, j runhostLocalJob) error {
	jobs, err := loadLocalJobs(home)
	if err != nil {
		return err
	}
	jobs = slices.DeleteFunc(jobs, func(x runhostLocalJob) bool { return x.Job == j.Job })
	return saveLocalJobs(home, append(jobs, j))
}

func dropLocalJob(home, id string) {
	jobs, err := loadLocalJobs(home)
	if err != nil {
		return
	}
	_ = saveLocalJobs(home, slices.DeleteFunc(jobs, func(x runhostLocalJob) bool { return x.Job == id }))
}

func findLocalJob(home, runhostName, id string) (runhostLocalJob, bool) {
	jobs, _ := loadLocalJobs(home)
	for _, j := range jobs {
		if j.Job == id && j.Runhost == runhostName {
			return j, true
		}
	}
	return runhostLocalJob{}, false
}

// errDetached ends a job's call once the runhost has said it runs the job,
// for --detach.
var errDetached = errors.New("detached")

// runOnStart sends the job, and its release and files when the runhost asks
// for them, over one session, and follows the job's frames. started says
// whether the runhost said it runs the job: an error after that is a
// connection lost while the job runs on, which next and attach resume. With
// detach, it returns as soon as the job runs. each, when set, takes the
// frames a session's driver reads (events), and moreInput, when set, writes
// the session's commands after the files.
func runOnStart(ctx context.Context, c *runhost.Client, j *job.Job, archive []byte, files *payload.Packed, detach bool,
	say func(string), each func(runhost.Frame), moreInput func(io.Writer) error) (res runhostJobResult, started bool, next int, err error) {
	answered := make(chan string, 1)
	stop := make(chan struct{})
	defer close(stop)
	send := func(w io.Writer) error {
		if err := job.Write(w, j); err != nil {
			return err
		}
		select {
		case a := <-answered:
			if a == runhost.ReleaseNeeded {
				if _, err := w.Write(archive); err != nil {
					return err
				}
			}
		case <-stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		// The run's files follow the release, exactly the size the job names.
		if files != nil {
			f, err := files.Open()
			if err != nil {
				return err
			}
			n, err := io.Copy(w, f)
			_ = f.Close()
			if err == nil && n != files.Size {
				err = fmt.Errorf("the run's files are %d bytes, not the %d the job names", n, files.Size)
			}
			if err != nil {
				return err
			}
		}
		if moreInput != nil {
			return moreInput(w)
		}
		// The input stays open while the device follows the job: its end is
		// how the runhost knows the device left.
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}
	frame := func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			next++
			if say != nil {
				say(f.Text)
			}
		case runhost.FrameEvent:
			next++
			if each != nil {
				each(f)
			}
		case runhost.FrameStarted:
			if f.Text != j.ID {
				return fmt.Errorf("the runhost started job %q, not this run's", f.Text)
			}
			started = true
			if detach {
				return errDetached
			}
		case runhost.FrameRelease:
			if f.Text != runhost.ReleaseNeeded && f.Text != runhost.ReleaseHeld {
				return fmt.Errorf("the runhost answered the job with %q", f.Text)
			}
			select {
			case answered <- f.Text:
			default:
				return errors.New("the runhost answered the job twice")
			}
		}
		return nil
	}
	a, err := c.Call(ctx, "job", nil, send, frame)
	if errors.Is(err, errDetached) {
		return runhostJobResult{Job: j.ID}, true, next, nil
	}
	if err != nil {
		return runhostJobResult{}, started, next, err
	}
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &res) != nil || !res.Done {
		if !started {
			return runhostJobResult{}, false, next, &runhostRefusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "no reason given")}
		}
		return runhostJobResult{}, true, next, fmt.Errorf("the runhost ended the job's answer without its result (exit %d): %s", a.Code, nonEmptyOr(a.Error, "no reason given"))
	}
	if res.Job != j.ID {
		return runhostJobResult{}, true, next, fmt.Errorf("the runhost answered for job %q, not this run's", res.Job)
	}
	return res, true, next, nil
}

// runOnFollow attaches to a job from frame index from and follows it to its
// end.
func runOnFollow(ctx context.Context, c *runhost.Client, id string, from int, say func(string), each func(runhost.Frame)) (runhostJobResult, int, error) {
	next := from
	// The input stays open while the device follows: its end is how the
	// runhost knows the device left.
	stop := make(chan struct{})
	defer close(stop)
	hold := func(io.Writer) error {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}
	a, err := c.Call(ctx, "attach", []string{id, fmt.Sprint(from)}, hold, func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			next++
			if say != nil {
				say(f.Text)
			}
		case runhost.FrameEvent:
			next++
			if each != nil {
				each(f)
			}
		}
		return nil
	})
	if err != nil {
		return runhostJobResult{}, next, err
	}
	var res runhostJobResult
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &res) != nil || !res.Done {
		return runhostJobResult{}, next, &runhostRefusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "the runhost gave no result")}
	}
	return res, next, nil
}

// runOnReattachWaits are how long the device waits before each attempt to
// reach a runhost again after losing it mid-job.
var runOnReattachWaits = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

// runOnReattach reaches the runhost again after the connection was lost while
// the job ran, and follows the job from where the device lost it. It answers
// the client it reached the runhost with.
func runOnReattach(ctx context.Context, rec *runhost.Record, keys *runhost.DeviceKeys, id string, from int, errOut io.Writer, say func(string), each func(runhost.Frame)) (runhostJobResult, *runhost.Client, error) {
	var last error
	for _, wait := range runOnReattachWaits {
		_, _ = fmt.Fprintf(errOut, "lost %s mid-run; the job goes on there. Reaching it again in %s…\n", rec.Name, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return runhostJobResult{}, nil, ctx.Err()
		}
		c, err := runhost.DialRunhost(ctx, *rec, keys)
		if err != nil {
			last = err
			continue
		}
		res, n, err := runOnFollow(ctx, c, id, from, say, each)
		from = n
		if err == nil {
			return res, c, nil
		}
		_ = c.Close()
		last = err
		var r *runhostRefusal
		if errors.As(err, &r) {
			return runhostJobResult{}, nil, err
		}
	}
	return runhostJobResult{}, nil, last
}

// runOnFinish collects a finished job: its sealed envelope and its sealed
// output, opened into the output folder; the envelope the runhost's CLI
// printed is this device's record, with the runhost it ran on; the
// deliverables are copied to the device's deliver folder; and the runhost is
// told to delete the job.
func runOnFinish(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, lj runhostLocalJob, code int, outputFormat string) error {
	env, err := runOnFinishEnv(cmd, c, rec, keys, lj, code)
	if env == nil {
		return err
	}
	printEnvelope(cmd, env, outputFormat)
	return err
}

// runOnFinishEnv is runOnFinish short of printing the envelope, for a
// session whose driver prints it.
func runOnFinishEnv(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, lj runhostLocalJob, code int) (*receipt.Envelope, error) {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	raw, err := runOnCollectEnvelope(ctx, c, lj.Job, rec.DeviceID, keys)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("collect the run's envelope from %s: %w (the job stays there, sealed, for %d days: agentry runhost collect %s %s)", rec.Name, err, int(runhostOutputKeep.Hours()/24), rec.Name, lj.Job))
	}
	// The envelope is the runhost CLI's stdout; a run that printed none still
	// has its output collected, and fails as the CLI's exit says.
	var env receipt.Envelope
	if json.Unmarshal(raw, &env) != nil || env.Agentry == "" {
		env = receipt.Envelope{Status: "error", ExitCode: code, Ref: lj.Ref, Digest: lj.Digest,
			Error: fmt.Sprintf("the run on %s gave no envelope", rec.Name)}
	}
	// The CLI's exit status is what the run ended in, whatever its stdout
	// says.
	if env.ExitCode != code {
		env.ExitCode = code
		if code != 0 {
			env.Status = "error"
		}
	}
	runID := env.RunID
	if runID == "" {
		runID = lj.Job[:12]
	}
	outDir, err := resolveOutDir(lj.Out, &installdir.Meta{Namespace: lj.Namespace, Name: lj.Name, Version: lj.Version, Ref: lj.Ref}, runID)
	if err != nil {
		return nil, err
	}
	if err := runOnCollect(ctx, c, lj.Job, rec.DeviceID, keys, outDir); err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("collect the run's output from %s: %w (it stays there, sealed, for %d days: agentry runhost collect %s %s)", rec.Name, err, int(runhostOutputKeep.Hours()/24), rec.Name, lj.Job))
	}
	if _, err := c.Ask(ctx, "collect", []string{lj.Job, "done"}, nil); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %s keeps the sealed output of job %s until it expires: %v\n", rec.Name, lj.Job, err)
	}
	runOnEnvelope(&env, rec, outDir, lj.Job)
	deliverOnDevice(errOut, &env, outDir, lj.DeliverTo)
	if err := env.Write(outDir); err != nil {
		return nil, err
	}
	if home, err := runnerHome(); err == nil {
		dropLocalJob(home, lj.Job)
	}
	if env.ExitCode != 0 {
		return &env, engine.Classify(env.ExitCode, errors.New(nonEmptyOr(env.Error, "the run on "+rec.Name+" failed")))
	}
	return &env, nil
}

// deliverOnDevice copies a successful run's deliverables to the folder the
// device delivers to, as a local run does at its end, recording each copy
// in the envelope and the deliveries ledger.
func deliverOnDevice(errOut io.Writer, env *receipt.Envelope, outDir, deliverTo string) {
	if deliverTo == "" || env.ExitCode != 0 || env.Deliverables == nil {
		return
	}
	ds := deliver.ToFolder(outDir, env.Deliverables.Files, deliver.FolderOptions{
		Root: deliverTo, Agent: deliver.AgentName(env.Ref), Label: deliver.Label(env.Deliverables.Files, env.RunID),
	})
	env.Deliveries = append(env.Deliveries, ds...)
	if err := deliver.WriteLedger(outDir, ds, true); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: the deliveries could not be recorded in receipts/%s: %v\n", deliver.LedgerFile, err)
	}
}

// runOnCollectEnvelope fetches a finished job's sealed envelope and opens it
// with this device's sealing key.
func runOnCollectEnvelope(ctx context.Context, c *runhost.Client, jobID, device string, keys *runhost.DeviceKeys) ([]byte, error) {
	var sealed bytes.Buffer
	a, err := c.Call(ctx, "collect", []string{jobID, "envelope"}, nil, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		if sealed.Len()+len(f.Data) > runhostMaxEnvelope+output.MaxHeader+(runhostMaxEnvelope/output.ChunkSize+1)*20 {
			return errors.New("the sealed envelope is larger than any run's")
		}
		sealed.Write(f.Data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if a.Code != 0 {
		return nil, errors.New(nonEmptyOr(a.Error, "the runhost would not hand the envelope over"))
	}
	var plain bytes.Buffer
	if err := output.Open(&plain, &sealed, device, keys.Seal, sealedEnvelopeJob(jobID)); err != nil {
		return nil, err
	}
	return plain.Bytes(), nil
}

// dialRunhostNamed loads a runhost's record and keys and reaches it over the
// run key.
func dialRunhostNamed(ctx context.Context, name string) (*runhost.Record, *runhost.DeviceKeys, *runhost.Client, error) {
	home, err := runnerHome()
	if err != nil {
		return nil, nil, nil, err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return nil, nil, nil, err
	}
	rec, ok := recs.Find(name)
	if !ok {
		return nil, nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost is named %q on this device: agentry runhost list shows them", name))
	}
	keys, err := runhost.LoadKeys(name)
	if err != nil {
		return rec, nil, nil, engine.Classify(engine.ExitGateway, err)
	}
	c, err := runhost.DialRunhost(ctx, *rec, keys)
	if err != nil {
		return rec, keys, nil, engine.Classify(engine.ExitGateway, err)
	}
	return rec, keys, c, nil
}

// jobOf is the local record of a job this device sent; a job sent from
// another device, or one whose record is gone, gets one from the runhost's
// own answer.
func jobOf(home string, rec *runhost.Record, id string, ref string) runhostLocalJob {
	if lj, ok := findLocalJob(home, rec.Name, id); ok {
		return lj
	}
	lj := runhostLocalJob{Job: id, Runhost: rec.Name, Ref: ref}
	if parsed, err := parseExactRef(ref); err == nil {
		lj.Namespace, lj.Name, lj.Version = parsed[0], parsed[1], parsed[2]
	}
	return lj
}

// parseExactRef splits ns/name@version.
func parseExactRef(ref string) ([3]string, error) {
	var out [3]string
	slash := bytes.IndexByte([]byte(ref), '/')
	at := bytes.LastIndexByte([]byte(ref), '@')
	if slash <= 0 || at <= slash+1 || at == len(ref)-1 {
		return out, fmt.Errorf("%q is not ns/name@version", ref)
	}
	out[0], out[1], out[2] = ref[:slash], ref[slash+1:at], ref[at+1:]
	return out, nil
}

// runhostJobsOf asks a runhost for its jobs (its status's list).
func runhostJobsOf(ctx context.Context, c *runhost.Client) ([]runhostJobInfo, error) {
	a, err := c.Ask(ctx, "status", nil, nil)
	if err != nil {
		return nil, err
	}
	if a.Code != 0 {
		return nil, errors.New(nonEmptyOr(a.Error, "the runhost did not say its state"))
	}
	var st runhostStatus
	if err := json.Unmarshal(a.Result, &st); err != nil {
		return nil, err
	}
	return st.Jobs, nil
}

func newRunhostJobsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs <name>",
		Short: "List the jobs a runhost runs, and the finished ones whose output waits to be collected",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, c, err := dialRunhostNamed(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			jobs, err := runhostJobsOf(cmd.Context(), c)
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"jobs": jobs})
			}
			out := cmd.OutOrStdout()
			if len(jobs) == 0 {
				_, _ = fmt.Fprintf(out, "%s runs no job and holds no output\n", args[0])
				return nil
			}
			for _, j := range jobs {
				kind := "run"
				if j.Session {
					kind = "session"
				}
				when := j.StartedAt
				if j.State == "finished" {
					when = j.FinishedAt
					if j.ExitCode != nil {
						kind += fmt.Sprintf(", exit %d", *j.ExitCode)
					}
				}
				_, _ = fmt.Fprintf(out, "%s  %-8s %s (%s) %s\n", j.Job, j.State, j.Ref, kind, when)
			}
			return nil
		},
	}
}

func newRunhostAttachCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "attach <name> <job>",
		Short: "Follow a job on a runhost to its end, then open its output here",
		Long: "attach follows the narration of a job a runhost runs, from its start, and once\n" +
			"the job ends collects its sealed output and envelope into the output folder, as\n" +
			"the run would have. A finished job is collected at once.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runhostFollowAndCollect(cmd, args[0], args[1], out, true)
		},
	}
	c.Flags().StringVar(&out, "out", "", "output folder (default: the one the run was given, else ./agentry-out/<ns>-<name>-<runid>)")
	return c
}

func newRunhostCollectCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "collect <name> <job>",
		Short: "Open a finished job's sealed output here, and have the runhost delete it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runhostFollowAndCollect(cmd, args[0], args[1], out, false)
		},
	}
	c.Flags().StringVar(&out, "out", "", "output folder (default: the one the run was given, else ./agentry-out/<ns>-<name>-<runid>)")
	return c
}

// runhostFollowAndCollect is attach (follow) and collect (no follow, and a
// job still running is refused).
func runhostFollowAndCollect(cmd *cobra.Command, name, id, out string, follow bool) error {
	if !job.ValidID(id) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not a job's id (agentry runhost jobs %s lists them)", id, name))
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	rec, keys, c, err := dialRunhostNamed(cmd.Context(), name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	jobs, err := runhostJobsOf(cmd.Context(), c)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	var info *runhostJobInfo
	for i := range jobs {
		if jobs[i].Job == id {
			info = &jobs[i]
		}
	}
	if info == nil {
		dropLocalJob(home, id)
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s holds no job %s: it was collected, has expired, or never ran there", name, id))
	}
	if info.State == "running" && !follow {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("job %s is still running on %s: agentry runhost attach %s %s follows it to its end", id, name, name, id))
	}
	errOut := cmd.ErrOrStderr()
	say := func(line string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", rec.Name, safeTerminalText(line, 4096)) }
	if !follow {
		say = nil
	}
	res, _, err := runOnFollow(cmd.Context(), c, id, 0, say, nil)
	if err != nil {
		var r *runhostRefusal
		if errors.As(err, &r) {
			return engine.Classify(runOnSendClass(err), err)
		}
		var c2 *runhost.Client
		res, c2, err = runOnReattach(cmd.Context(), rec, keys, id, 0, errOut, say, nil)
		if err != nil {
			return engine.Classify(engine.ExitGateway, fmt.Errorf("lost %s: %w (the job goes on there: agentry runhost attach %s %s)", rec.Name, err, rec.Name, id))
		}
		defer func() { _ = c2.Close() }()
		c = c2
	}
	return runOnCollectJob(cmd, c, rec, keys, home, id, info.Ref, out, res.ExitCode)
}

func runOnCollectJob(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, home, id, ref, out string, code int) error {
	lj := jobOf(home, rec, id, ref)
	if out != "" {
		lj.Out = out
	}
	format := "text"
	if flags.jsonOut {
		format = "json"
	}
	return runOnFinish(cmd, c, rec, keys, lj, code, format)
}

func newRunhostCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <name> <job>",
		Short: "Stop a job on a runhost; its run tears down and its output so far is sealed for collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !job.ValidID(args[1]) {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not a job's id", args[1]))
			}
			_, _, c, err := dialRunhostNamed(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			a, err := c.Ask(cmd.Context(), "cancel", []string{args[1]}, func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) })
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			if a.Code != 0 {
				return engine.Classify(runOnSendClass(&runhostRefusal{Code: a.Code, Reason: a.Error}), fmt.Errorf("%s: %s", args[0], nonEmptyOr(a.Error, "the job was not cancelled")))
			}
			var res runhostJobResult
			_ = json.Unmarshal(a.Result, &res)
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), res)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "job %s on %s has ended (exit %d); its output so far waits sealed: agentry runhost collect %s %s\n", args[1], args[0], res.ExitCode, args[0], args[1])
			return nil
		},
	}
}
