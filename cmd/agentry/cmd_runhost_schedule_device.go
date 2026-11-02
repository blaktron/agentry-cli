package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// The device's side of a runhost's schedules: `agentry schedule add --on`
// prepares the run exactly as `agentry run --on` does, then, after a consent
// that names each thing the runhost's store will keep and the server, sends
// it as a schedule instead of starting it.

// scheduleOnRunhost is `agentry schedule add --on <runhost>`: the run's
// arguments the schedule's own flags make, sent through `run --on` in its
// schedule mode.
func scheduleOnRunhost(cmd *cobra.Command, ref, name, on string, spec runSpecFlags, when schedule.Recurrence, now time.Time, yes bool) error {
	switch {
	case ref == "":
		return engine.Classify(engine.ExitUsage, errors.New("--on schedules a published release: name it as ns/name[@range]"))
	case spec.signedOut:
		return engine.Classify(engine.ExitUsage, errors.New("--signed-out is for this computer: a runhost runs its schedules with nobody signed in"))
	case spec.deliverTo != "":
		return engine.Classify(engine.ExitUsage, errors.New("--deliver-to copies to this computer, and a runhost's schedule runs while it may be away: collect its runs with agentry runhost collect"))
	case spec.input != "" && spec.task != "":
		return errors.New("give --input FILE or --task TEXT, not both")
	}
	sc, err := scheduleFromFlags(name, ".", spec, when, now)
	if err != nil {
		return err
	}
	sc.Ref = ref
	sc.Version, sc.Digest = "", ""
	args := []string{ref, "--mode", "autonomous", "--on", on}
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
	for _, d := range r.Destinations {
		args = append(args, "--destination", d)
	}
	if len(r.Destinations) == 0 {
		args = append(args, "--no-destination")
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
		args = append(args, "--timeout", fmt.Sprint(r.Timeout))
	}
	if r.MaxTurns >= 0 {
		args = append(args, "--max-turns", fmt.Sprint(r.MaxTurns))
	}
	if r.MaxCost >= 0 {
		args = append(args, "--max-cost", fmt.Sprint(r.MaxCost))
	}
	if r.Adapter != "" {
		args = append(args, "--adapter", r.Adapter)
	}
	if r.AllowUnsigned {
		args = append(args, "--allow-unsigned")
	}
	if r.AllowUnscanned {
		args = append(args, "--allow-unscanned")
	}
	if r.AllowVulnerableMCP {
		args = append(args, "--allow-vulnerable-mcp")
	}
	if yes {
		args = append(args, "--yes")
	}
	// The task travels with the schedule, read once now: the runhost has no
	// file of this computer's to read at each occurrence.
	switch {
	case spec.task != "":
		home, err := runnerHome()
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(home, ".schedule-task-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.WriteString(spec.task); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		args = append(args, "--input", f.Name())
	case spec.input != "":
		args = append(args, "--input", spec.input)
	}
	whenJSON, err := json.Marshal(when)
	if err != nil {
		return err
	}
	header, err := json.Marshal(runhost.ScheduleHeader{Name: sc.Name, When: whenJSON})
	if err != nil {
		return err
	}
	args = append(args, "--runhost-schedule", string(header))
	return runInProcess(cmd, args, cmd.InOrStdin(), cmd.OutOrStdout())
}

// putRunhostSchedule is `run --on` in its schedule mode: the job prepared
// for the runhost, its OAuth connections handed over, and, after the consent
// that names what the store keeps, sent as a schedule.
func putRunhostSchedule(cmd *cobra.Command, client *runhost.Client, rec *runhost.Record, home string, meta *installdir.Meta, j *job.Job, archive []byte, files *payload.Packed, opts runOpts) error {
	var h runhost.ScheduleHeader
	if err := json.Unmarshal([]byte(opts.runhostSchedule), &h); err != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--runhost-schedule: %w", err))
	}
	if h.Name == "" {
		h.Name = meta.Name
	}
	h.Entries = remote.Entries(j)
	if len(j.Run.Connections) > 0 {
		conns := connection.Open(home, connectionKeys())
		clients := connection.OpenClients(home, connectionKeys())
		for server := range j.Run.Connections {
			hd, err := conns.Hand(connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: server}, clients)
			if err != nil {
				return engine.Classify(engine.ExitPolicy, fmt.Errorf("the connection to %s cannot be handed to %s: %w", server, rec.Name, err))
			}
			raw, err := json.Marshal(hd)
			if err != nil {
				return err
			}
			h.Connections = append(h.Connections, raw)
		}
	}
	errOut := cmd.ErrOrStderr()
	what := scheduleEntriesSaid(h.Entries)
	_, _ = fmt.Fprintf(errOut, "schedule %q runs %s on runhost %s (%s, host key %s) with nobody present.\n", h.Name, meta.Ref, rec.Name, rec.Host, rec.HostKeyFingerprint)
	_, _ = fmt.Fprintf(errOut, "Its store keeps, encrypted under a key systemd seals to that server, for every run of this schedule: %s.\n", what)
	if len(h.Connections) > 0 {
		_, _ = fmt.Fprintln(errOut, "Each OAuth connection is handed to the runhost, which refreshes it itself: both it and this computer then hold the grant, and a vendor that rotates its refresh tokens leaves one of them to connect again.")
	}
	if !opts.yes {
		ask := terminalAsker(cmd)
		if ask == nil {
			return engine.Classify(engine.ExitPolicy, errors.New("no one is at a terminal to agree to what the runhost keeps: pass --yes"))
		}
		ok, err := ask.Confirm(fmt.Sprintf("Keep these on %s for the schedule?", rec.Name), false)
		if err != nil {
			return err
		}
		if !ok {
			return engine.Classify(engine.ExitPolicy, errors.New("nothing was kept: the schedule was not made"))
		}
	}
	say := func(line string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", rec.Name, safeTerminalText(line, 4096)) }
	info, err := remote.PutSchedule(cmd.Context(), client, h, j, archive, files, say)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if flags.jsonOut || opts.outputFormat == "json" {
		return emitJSON(cmd.OutOrStdout(), info)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "schedule %s %q is on runhost %s", info.ID, info.Name, rec.Name)
	if info.Next != nil {
		_, _ = fmt.Fprintf(w, ", next run %s", info.Next.Local().Format("Mon 2 Jan 15:04"))
	}
	_, _ = fmt.Fprintln(w)
	printNextSteps(errOut,
		nextStep{command: "agentry runhost schedules " + rec.Name, notes: []string{"its schedules, their next runs and their runs' jobs"}},
		nextStep{command: "agentry runhost collect " + rec.Name + " <job>", notes: []string{"a run's output, sealed to this computer, opened here"}},
		nextStep{command: "agentry runhost store " + rec.Name + " --remove schedule:" + info.ID, notes: []string{"takes the schedule off the runhost with what it keeps"}})
	return nil
}

func newRunhostSchedulesCmd() *cobra.Command {
	var runNow string
	cmd := &cobra.Command{
		Use:   "schedules <name> [--run <id>]",
		Short: "List a runhost's schedules, their next runs and their runs' jobs",
		Long: "Lists the schedules a runhost runs with nobody present (agentry schedule add\n" +
			"--on makes one): each one's next run, what it keeps in the runhost's store,\n" +
			"and its latest runs, each a job that agentry runhost collect opens here.\n" +
			"--run <id> runs one now. agentry runhost store <name> --remove schedule:<id>\n" +
			"takes one off the runhost.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			_, _, c, err := dialRunhostNamed(cmd.Context(), name)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if runNow != "" {
				f, err := remote.RunSchedule(cmd.Context(), c, runNow)
				if err != nil {
					return engine.Classify(engine.ExitGateway, err)
				}
				if flags.jsonOut {
					return emitJSON(cmd.OutOrStdout(), f)
				}
				if f.Job == "" {
					return engine.Classify(engine.ExitCLI, fmt.Errorf("schedule %s did not start: %s", runNow, f.Message))
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s is running as job %s on %s: agentry runhost attach %s %s follows it\n", runNow, f.Job, name, name, f.Job)
				return nil
			}
			list, err := remote.Schedules(cmd.Context(), c)
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "schedules": list})
			}
			if len(list) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s has no schedules: agentry schedule add <ns/name> --on %s makes one\n", name, name)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tNAME\tRELEASE\tNEXT\tKEEPS")
			for _, sc := range list {
				next := "none"
				switch {
				case sc.Ended:
					next = "ended"
				case sc.Paused:
					next = "paused"
				case sc.Next != nil:
					next = sc.Next.Local().Format("Mon 2 Jan 15:04")
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sc.ID, sc.Name, sc.Ref, next, scheduleEntriesSaid(sc.Entries))
			}
			_ = tw.Flush()
			for _, sc := range list {
				for _, f := range sc.Fires {
					line := fmt.Sprintf("  %s %s: %s", sc.ID, f.Occurrence.Local().Format("Mon 2 Jan 15:04"), f.Outcome)
					if f.Job != "" {
						line += ", job " + f.Job
					}
					if f.Outcome == string(schedule.Failed) {
						line += fmt.Sprintf(" (exit %d) %s", f.ExitCode, f.Message)
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(line))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runNow, "run", "", "run schedule ID now")
	return cmd
}
