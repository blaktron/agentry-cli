package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runhost/signin"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/schedule"
	"github.com/blaktron/agentry/pkg/manifest"
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
	if spec.model != "" {
		args = append(args, "--model", spec.model)
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
// for the runhost and, after the consent that names what the store keeps,
// a connection of the runhost's own made for each OAuth server the release
// signs in to, then the job sent as a schedule.
func putRunhostSchedule(cmd *cobra.Command, client *runhost.Client, rec *runhost.Record, home string, meta *installdir.Meta, m *manifest.Manifest, j *job.Job, archive []byte, files *payload.Packed, opts runOpts) error {
	var h runhost.ScheduleHeader
	if err := json.Unmarshal([]byte(opts.runhostSchedule), &h); err != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--runhost-schedule: %w", err))
	}
	if h.Name == "" {
		h.Name = meta.Name
	}
	// A runhost that cannot keep a schedule says so before anything is
	// asked of the person.
	takes, err := remote.TakesSchedules(cmd.Context(), client)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if !takes {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("runhost %s takes no schedule: its systemd cannot seal its store's key to it (schedules need systemd %d or newer; agentry runhost store %s says so)", rec.Name, schedule.MinSystemd, rec.Name))
	}
	// The schedule runs on one of the runhost's own saved models, read from
	// its list at each run: the one --model names, else its default.
	held, err := runhostScheduleSavedModel(cmd, client, rec.Name, opts.model)
	if err != nil {
		return err
	}
	if opts.model == "" {
		j.Run.RunhostModel = job.RunhostDefault
	} else {
		j.Run.RunhostModel = held.ID
	}
	if err := j.Validate(); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	h.Entries = remote.Entries(j)
	var servers []manifest.MCPServer
	for _, srv := range oauthServers(m) {
		if _, ok := j.Run.Connections[connectionServer(srv)]; ok {
			servers = append(servers, srv)
		}
	}
	errOut := cmd.ErrOrStderr()
	what := scheduleEntriesSaid(h.Entries)
	_, _ = fmt.Fprintf(errOut, "schedule %q runs %s on runhost %s (%s, host key %s) with nobody present.\n", h.Name, meta.Ref, rec.Name, rec.Host, rec.HostKeyFingerprint)
	if opts.model == "" {
		_, _ = fmt.Fprintf(errOut, "Each run is on %s's default saved model, whichever it is then (now %q, %s/%s).\n", rec.Name, held.Name, held.Provider, held.Model)
	} else {
		_, _ = fmt.Fprintf(errOut, "Each run is on %s's saved model %q (%s/%s), whose credential the runhost holds.\n", rec.Name, held.Name, held.Provider, held.Model)
	}
	_, _ = fmt.Fprintf(errOut, "Its store keeps, encrypted under a key systemd seals to that server, for every run of this schedule: %s.\n", what)
	if len(servers) > 0 {
		_, _ = fmt.Fprintf(errOut, "Each OAuth connection it signs in to is one of %s's own: you sign in for the runhost once, in your browser here, and it keeps and refreshes that grant itself. This computer's connections are not shared with it.\n", rec.Name)
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
	for _, srv := range servers {
		if err := runhostConnect(cmd, client, rec, home, meta, srv, m.Guide.ServerConnect(srv)); err != nil {
			return err
		}
	}
	info, err := remote.PutSchedule(cmd.Context(), client, h, j, archive, files, say)
	if err != nil {
		return engine.Classify(remote.SendClass(err), err)
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

// runhostConnect makes the runhost's own connection to srv for the release
// meta names, unless it keeps a working one: the consent opens in the
// person's browser here, at the authorization endpoint this device
// discovers for the server itself, and the runhost keeps the grant. A
// vendor that takes only the person's own OAuth client is given the one
// this device keeps for its issuer, or asked for one at the terminal.
func runhostConnect(cmd *cobra.Command, client *runhost.Client, rec *runhost.Record, home string, meta *installdir.Meta, srv manifest.MCPServer, help string) error {
	ctx := cmd.Context()
	httpc := vendorHTTP()
	var md *mcpauth.ServerMetadata
	if srv.IsRemote() {
		d, err := mcpauth.DiscoverRemote(ctx, httpc, srv.URL)
		if err != nil {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("discover how %s signs in: %w", connectionServer(srv), err))
		}
		md = &d.Server
	} else {
		d, err := mcpauth.DiscoverIssuer(ctx, httpc, srv.OAuth.Issuer)
		if err != nil {
			return engine.Classify(engine.ExitCLI, fmt.Errorf("discover how %s signs in: %w", connectionServer(srv), err))
		}
		md = d
	}
	req := signin.ConnectRequest{
		Ref: meta.Ref, Digest: meta.Digest, Server: connectionServer(srv), Scopes: connectionScopes(srv),
		AgentryClient: connection.OpenPublicClients(home, registryURL(), httpClient).ForServer(ctx, srv, md.Issuer),
	}
	if srv.IsRemote() {
		req.URL = srv.URL
	} else {
		req.Issuer = srv.OAuth.Issuer
	}
	clients := connection.OpenClients(home, connectionKeys())
	errOut := narration(cmd)
	p := signin.Person{
		Say: func(s string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", rec.Name, safeTerminalText(s, 4096)) },
		Open: func(u string) {
			if os.Getenv("AGENTRY_NO_BROWSER") != "" {
				return
			}
			if err := openBrowser(u); err != nil {
				_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
			}
		},
	}
	_, _ = fmt.Fprintf(errOut, "\n%s needs runhost %s to sign in to %s.\n", meta.Ref, rec.Name, connectionWhat(srv))
	for attempt := 0; ; attempt++ {
		if own, err := clients.Get(md.Issuer); err != nil {
			return engine.Classify(engine.ExitCLI, err)
		} else if own != nil {
			req.OwnClientID, req.OwnClientSecret = own.ID, own.Secret
		}
		kept, err := signin.Connect(ctx, client, req, md.AuthorizationEndpoint, p)
		if err == nil {
			_, _ = fmt.Fprintf(errOut, "runhost %s is connected to %s for %s\n", rec.Name, kept.Kind, meta.Ref)
			return nil
		}
		needs := (&mcpauth.NeedsOwnClientError{Issuer: md.Issuer}).Error()
		if attempt == 0 && req.OwnClientID == "" && strings.Contains(err.Error(), needs) && consentTTY(cmd.InOrStdin()) {
			if aerr := askOwnClient(cmd, bufio.NewReader(cmd.InOrStdin()), clients, md.Issuer, connectionServer(srv), help); aerr != nil {
				return engine.Classify(engine.ExitPolicy, aerr)
			}
			continue
		}
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("connect runhost %s to %s: %w", rec.Name, connectionServer(srv), err))
	}
}

// runhostScheduleSavedModel is the runhost's saved model a schedule there runs
// on: the one ref names, else its default. One the runhost does not hold,
// or cannot use now, is refused by name.
func runhostScheduleSavedModel(cmd *cobra.Command, c remote.Caller, name, ref string) (remote.SavedModel, error) {
	list, err := remote.ListModels(cmd.Context(), c)
	if err != nil {
		return remote.SavedModel{}, err
	}
	rememberRunhostModels(name, list)
	var held remote.SavedModel
	var ok bool
	if ref == "" || strings.EqualFold(ref, job.RunhostDefault) {
		if held, ok = list.DefaultModel(); !ok {
			return held, engine.Classify(engine.ExitPolicy, fmt.Errorf("runhost %s holds no saved model for the schedule to run on: make one of this computer's available there (agentry runhost model %s --add <saved model>)", name, name))
		}
	} else if held, ok = list.Find(ref); !ok {
		return held, engine.Classify(engine.ExitPolicy, fmt.Errorf("--model: runhost %s holds no saved model %q: agentry runhost model %s lists those it holds, and agentry runhost model %s --add <saved model> makes one of this computer's available there", name, ref, name, name))
	}
	if held.Refusal != "" {
		return held, engine.Classify(engine.ExitPolicy, fmt.Errorf("runhost %s cannot run on its saved model %q now: %s: make it available again (agentry runhost model %s --add %q)", name, held.Name, held.Refusal, name, held.Name))
	}
	return held, nil
}

