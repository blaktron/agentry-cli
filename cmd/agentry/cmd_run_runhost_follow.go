package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
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

// runOnFinish collects a finished job: its sealed envelope and its sealed
// output, opened into the output folder; the envelope the runhost's CLI
// printed is this device's record, with the runhost it ran on; the
// deliverables are copied to the device's deliver folder; and the runhost is
// told to delete the job.
func runOnFinish(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, lj remote.LocalJob, code int, outputFormat string) error {
	env, err := runOnFinishEnv(cmd, c, rec, keys, lj, code)
	if env == nil {
		return err
	}
	printEnvelope(cmd, env, outputFormat)
	return err
}

// runOnFinishEnv is runOnFinish short of printing the envelope, for a
// session whose driver prints it: the output folder is the one the job was
// sent with, or agentry-out's default for the run's id.
func runOnFinishEnv(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, lj remote.LocalJob, code int) (*receipt.Envelope, error) {
	home, _ := runnerHome()
	errOut := cmd.ErrOrStderr()
	return remote.Finish(cmd.Context(), c, rec, keys, lj, code, remote.FinishOptions{
		Home: home,
		OutDir: func(runID string) (string, error) {
			return resolveOutDir(lj.Out, &installdir.Meta{Namespace: lj.Namespace, Name: lj.Name, Version: lj.Version, Ref: lj.Ref}, runID)
		},
		Warn:        func(w string) { _, _ = fmt.Fprintln(errOut, w) },
		CollectHint: fmt.Sprintf(": agentry runhost collect %s %s", rec.Name, lj.Job),
	})
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
		remote.DropJob(home, id)
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
	res, _, err := remote.Follow(cmd.Context(), c, id, 0, say, nil)
	if err != nil {
		var r *remote.Refusal
		if errors.As(err, &r) {
			return engine.Classify(remote.SendClass(err), err)
		}
		var c2 *runhost.Client
		res, c2, err = remote.Reattach(cmd.Context(), rec, keys, id, 0, errOut, say, nil)
		if err != nil {
			return engine.Classify(engine.ExitGateway, fmt.Errorf("lost %s: %w (the job goes on there: agentry runhost attach %s %s)", rec.Name, err, rec.Name, id))
		}
		defer func() { _ = c2.Close() }()
		c = c2
	}
	return runOnCollectJob(cmd, c, rec, keys, home, id, info.Ref, out, res.ExitCode)
}

func runOnCollectJob(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, home, id, ref, out string, code int) error {
	lj := remote.JobOf(home, rec, id, ref)
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
				return engine.Classify(remote.SendClass(&remote.Refusal{Code: a.Code, Reason: a.Error}), fmt.Errorf("%s: %s", args[0], nonEmptyOr(a.Error, "the job was not cancelled")))
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
