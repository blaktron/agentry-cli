package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A run on a remote runhost, from the device: the release is resolved and
// verified here exactly as a local run's, consented to here, and sent with
// the job to the runhost, which verifies it again and runs it as a local
// run of its own. The output comes back sealed to this device and is opened
// into the output folder; the envelope is the runhost CLI's own, with the
// runhost it ran on.

// runOnUnsupported names a flag a run on a runhost does not take yet.
func runOnUnsupported(opts runOpts) string {
	switch {
	case opts.local != "":
		return "--local (a runhost runs a published release)"
	case opts.plan, opts.dryRun, opts.prepare:
		return "--plan, --dry-run and --prepare"
	case opts.destinationsFile != "":
		return "--destinations-file (a runhost takes this device's saved destinations)"
	case opts.connectionsFile != "", opts.reviewInputs != "", opts.eventsPath != "":
		return "--connections-file, --review-inputs and --events"
	case opts.keepRunDir:
		return "--keep-run-dir"
	case len(opts.hostFlags.pins) > 0, opts.hostFlags.knownHosts != "":
		return "--host-key-pin and --host-known-hosts"
	}
	return ""
}

func runOnRunhost(cmd *cobra.Command, ref string, opts runOpts) error {
	if ref == "" {
		return engine.Classify(engine.ExitUsage, errors.New("--on runs a published release: name it as ns/name[@range]"))
	}
	if what := runOnUnsupported(opts); what != "" {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a run on a runhost does not take %s yet", what))
	}
	// No harness runs as a host process on a runhost: the installed claude
	// and codex run as their images there.
	runAdapter, mapped, err := job.RunhostAdapter(opts.adapterName)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if opts.outputFormat != "text" && opts.outputFormat != "json" {
		return fmt.Errorf("unknown --output-format %q (want text or json)", opts.outputFormat)
	}
	if flags.jsonOut {
		opts.outputFormat = "json"
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return err
	}
	rec, ok := recs.Find(opts.on)
	if !ok {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost is named %q on this device: agentry runhost list shows them", opts.on))
	}

	policyFile, err := policy.Resolve(opts.policyPath)
	if err != nil {
		return err
	}
	pol, _, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	// A one-off run carries the saved model it names, else the default,
	// with that one's key alone. A schedule names one the runhost holds
	// instead, and carries no model of this computer's.
	var runModel job.Model
	modelKey := ""
	if opts.runhostSchedule == "" {
		chosen, err := localrun.ChooseRunModel(pol, opts.model, "")
		if err != nil {
			return err
		}
		if modelKey, err = remote.ModelKey(chosen.Policy); err != nil {
			return engine.Classify(engine.ExitPolicy, err)
		}
		runModel = job.ModelOf(chosen.Policy.Model)
		if chosen.Saved.ID != "" {
			runModel = job.ModelOfSaved(chosen.Policy.Model, chosen.Saved)
		}
	}

	// The runhost has its own sign-in and its own managed settings: this
	// device's are read now, refuse here what they refuse outright, and
	// travel with the job for the rest.
	syncManaged(cmd, registryURL())
	held := managed.Current()
	if reason := held.RunRefusal(); reason != "" {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	if reason := held.WhereRefusal(managedsettings.WhereRunhost); reason != "" {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}

	opts.progress.at(receipt.StageVerify)
	meta, installDir, err := materializeRegistry(cmd, ref, home, opts)
	if err != nil {
		return err
	}
	if meta.Source != "registry" {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is installed from a local folder; a runhost runs a published release", meta.Ref))
	}
	opts.progress.running(meta)
	opts.progress.at(receipt.StageCompile)
	m, err := localrun.ManifestFrom(filepath.Join(installDir, "workspace"))
	if err != nil {
		return engine.Classify(engine.ExitCompile, err)
	}
	if reason := held.Refusal(managed.Run{Ref: meta.Ref, TargetHosts: len(m.Capabilities.TargetHosts) > 0, AcceptedRisk: meta.Attestation.Consent}); reason != "" {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	// The applications on this computer are answered by Agentry Desktop
	// here, over a link session this device keeps open for the job.
	dcap, err := runOnDesktopCap(m, pol, opts)
	if err != nil {
		return err
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		return engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid: %v", meta.Ref, blocking[0]))
	}
	mode, err := resolveRunMode(cmd, opts, m)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	switch mode {
	case manifest.ModeAutonomous:
		if opts.sessionProtocol {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--session-protocol drives an interactive session; this run's mode is %s", mode))
		}
	case manifest.ModeInteractive:
		if opts.runhostSchedule != "" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("%s runs interactive, and a schedule runs with nobody there to answer it", meta.Ref))
		}
		if opts.detach {
			return engine.Classify(engine.ExitUsage, errors.New("--detach is for an autonomous run: a session is driven from here"))
		}
		if !opts.sessionProtocol && !stdinIsTTY(cmd.InOrStdin()) {
			return engine.Classify(engine.ExitUsage, errors.New("an interactive run needs a terminal on stdin (the chat is the input); drive the session with --session-protocol, or pipe a task to --mode autonomous"))
		}
		if !opts.sessionProtocol && opts.outputFormat == "json" {
			return engine.Classify(engine.ExitUsage, errors.New("--output-format json is not available for an interactive run: the transcript and receipts are the record"))
		}
	default:
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s runs %s; a runhost takes an autonomous run or an interactive session", meta.Ref, mode))
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
	opts.progress.at(receipt.StageConsent)
	if err := confirmRun(cmd, m, meta, home, opts, mode, nil); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	opts.progress.at(receipt.StageVerify)
	rel, archive, err := runOnRelease(cmd, meta)
	if err != nil {
		return engine.Classify(engine.ExitVerify, err)
	}
	opts.progress.at(receipt.StageIntake)
	carried, err := runOnCarry(cmd, home, meta, m, pol, opts)
	if err != nil {
		return err
	}
	defer carried.done()
	files, err := carried.pack()
	if err != nil {
		return err
	}
	taskIn := runTask(cmd, opts)
	if taskIn == nil {
		taskIn = strings.NewReader("")
	}
	task, err := io.ReadAll(io.LimitReader(taskIn, job.MaxDocument/2+1))
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if len(task) > job.MaxDocument/2 {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the task is over %d bytes, more than a run on a runhost takes", job.MaxDocument/2))
	}
	id, err := remote.NewJobID()
	if err != nil {
		return err
	}
	run := carried.run
	run.Task, run.AllowUnsigned, run.AllowUnscanned = string(task), opts.allowUnsigned, opts.allowUnscanned
	run.Timeout, run.MaxTurns, run.MaxCost = opts.timeout, opts.maxTurns, opts.maxCost
	run.Model, run.ModelKey = runModel, modelKey
	if opts.runhostSchedule != "" {
		// The job is valid as it is sent: the schedule names the runhost's
		// own saved model once it has the runhost's list
		// (putRunhostSchedule).
		run.RunhostModel = job.RunhostDefault
	}
	run.Adapter = runAdapter
	run.Session = mode == manifest.ModeInteractive
	run.Managed = held.Narrowing()
	// The run is bound to this computer's Agentry Desktop before anything is
	// sent, or refused here as a local run would be; the binding travels in
	// the job, and this device unbinds once the job is over.
	var dl *engine.DesktopLink
	// ended says the job has ended: only then does this device unbind, since
	// a job this device lost goes on there and a later attach links it again.
	ended := false
	if dcap != nil {
		dl, err = bindDesktopCap(cmd.Context(), dcap, meta.Ref, meta.Digest, home, id, opts.out)
		if err != nil {
			return err
		}
		defer func() {
			if !ended {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = dl.Unbind(ctx)
		}()
		run.Desktop = &job.Desktop{Run: id, Bound: desktop.Bound{Token: dl.Token, Servers: dl.Servers, Computer: dl.Computer}}
		run.AllowScreen = opts.allowScreen
	}
	j := &job.Job{V: job.Version, ID: id, Device: rec.DeviceID, Release: rel, Run: run}
	if err := j.Validate(); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}

	opts.progress.at(receipt.StagePrepare)
	runhostPostUpdate(cmd)
	if recs, err := runhost.LoadRecords(home); err == nil {
		if r, ok := recs.Find(rec.Name); ok {
			rec = r
		}
	}
	keys, err := runhost.LoadKeys(rec.Name)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	client, err := runhost.DialRunhost(cmd.Context(), *rec, keys)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	defer func() { _ = client.Close() }()
	rv, err := remote.SameVersion(cmd.Context(), client, rec.Name)
	var skew *remote.SkewError
	if errors.As(err, &skew) {
		return engine.Classify(engine.ExitUsage, err)
	}
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	// A runhost whose Unsafe Mode is on runs a release that declares
	// commands in it, which this device's managed settings may forbid: the
	// runhost refuses it too, and this device says so before sending it.
	if rv.UnsafeMode && len(m.Capabilities.Exec.Commands) > 0 {
		if err := refuseUnsafeMode(run.Managed); err != nil {
			return err
		}
	}
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "running %s on %s (%s, host key %s)\n", meta.Ref, rec.Name, rec.Host, rec.HostKeyFingerprint)
	if mapped {
		_, _ = fmt.Fprintf(errOut, "on a runhost the installed %s does not run: %s runs in its image, %s, in a container under gVisor\n", opts.adapterName, harnessName(opts.adapterName), runAdapter)
	}
	if rv.UnsafeMode {
		_, _ = fmt.Fprintf(errOut, "Unsafe Mode is on for %s, a setting of the runhost: a release's declared commands come from the Unsafe Mode image there, without the command catalogue (agentry runhost set %s --unsafe-mode off turns it off)\n", rec.Name, rec.Name)
	}
	if runModel.SignIn() {
		_, _ = fmt.Fprintf(errOut, "paying for the model with the %s sign-in %s keeps\n", modelsignin.VendorOf(runModel.APIKeyKind), rec.Name)
	}
	if runModel.SavedModelName != "" {
		_, _ = fmt.Fprintf(errOut, "on this computer's saved model %q (%s/%s), whose credential alone travels with the run\n", runModel.SavedModelName, runModel.ProviderID, runModel.Name)
	}

	if opts.runhostSchedule != "" {
		return putRunhostSchedule(cmd, client, rec, home, meta, m, j, archive, files, opts)
	}
	opts.progress.at(receipt.StageAgent)
	lj := remote.LocalJob{Job: j.ID, Runhost: rec.Name, Ref: meta.Ref, Namespace: meta.Namespace, Name: meta.Name, Version: meta.Version,
		Digest: meta.Digest, Out: opts.out, DeliverTo: carried.deliverTo, Session: j.Run.Session, StartedAt: time.Now().UTC().Format(time.RFC3339),
		Adapter: runAdapter, UnsafeMode: rv.UnsafeMode}
	if mapped {
		lj.AdapterAsked = opts.adapterName
	}
	if dl != nil {
		lj.Desktop = id
	}
	if err := remote.PutJob(home, lj); err != nil {
		return err
	}
	if dl != nil {
		defer keepRunhostLink(cmd, client, rec, keys, home, j.ID, id)()
	}
	say := func(line string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", rec.Name, safeTerminalText(line, 4096)) }
	if j.Run.Session {
		err := runOnSession(cmd, client, rec, keys, j, archive, files, lj, m, pol, opts, carried, say)
		ended = err == nil
		return err
	}
	res, started, next, err := remote.Start(cmd.Context(), client, j, archive, files, opts.detach, say, nil, nil)
	switch {
	case err != nil && !started:
		ended = true
		remote.DropJob(home, j.ID)
		return engine.Classify(remote.SendClass(err), err)
	case opts.detach:
		carried.finishProfile(errOut)
		return runOnDetached(cmd, rec, j.ID, opts.outputFormat)
	case err != nil:
		var c2 *runhost.Client
		res, c2, err = remote.Reattach(cmd.Context(), rec, keys, j.ID, next, errOut, say, nil)
		if err != nil {
			carried.finishProfile(errOut)
			return engine.Classify(engine.ExitGateway, fmt.Errorf("lost %s: %w; the job goes on there: agentry runhost attach %s %s follows it and opens its output here", rec.Name, err, rec.Name, j.ID))
		}
		defer func() { _ = c2.Close() }()
		client = c2
	}
	ended = true
	carried.finishProfile(errOut)
	opts.progress.at(receipt.StageOutput)
	err = runOnFinish(cmd, client, rec, keys, lj, res.ExitCode, opts.outputFormat)
	opts.progress.enveloped()
	return err
}

// harnessName is the vendor harness an adapter runs, for the person.
func harnessName(adapterName string) string {
	switch adapterName {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return adapterName
}

// runOnDetached says where a detached job is, and how to reach it again.
func runOnDetached(cmd *cobra.Command, rec *runhost.Record, id, format string) error {
	if format == "json" {
		return emitJSON(cmd.OutOrStdout(), map[string]string{"job": id, "runhost": rec.Name, "state": "running"})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "job %s runs on %s\n  follow it:     agentry runhost attach %s %s\n  collect it:    agentry runhost collect %s %s (once it has ended)\n  stop it:       agentry runhost cancel %s %s\n",
		id, rec.Name, rec.Name, id, rec.Name, id, rec.Name, id)
	return nil
}

// runOnRelease fetches the verified release's archive and attestation as
// they are, for the runhost to verify again, and holds them to what this
// device verified: the same digest, and an attestation its own pins verify.
func runOnRelease(cmd *cobra.Command, meta *installdir.Meta) (job.Release, []byte, error) {
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil {
		return job.Release{}, nil, err
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return job.Release{}, nil, err
	}
	res, err := resolveRef(cmd.Context(), client, meta.Ref, true)
	if err != nil {
		return job.Release{}, nil, err
	}
	if res.Digest != meta.Digest {
		return job.Release{}, nil, fmt.Errorf("the registry now resolves %s to %s, not the %s verified here", meta.Ref, res.Digest, meta.Digest)
	}
	archive, err := downloadArchive(cmd.Context(), reg, token, res)
	if err != nil {
		return job.Release{}, nil, err
	}
	if got := sha256Digest(archive); got != meta.Digest {
		return job.Release{}, nil, fmt.Errorf("digest mismatch: registry says %s, archive is %s — refusing to run", meta.Digest, got)
	}
	rel := job.Release{Namespace: meta.Namespace, Name: meta.Name, Version: meta.Version, Digest: meta.Digest,
		Size: int64(len(archive)), Registry: reg}
	switch meta.Attestation.State {
	case trust.StateVerified:
		attURL := fmt.Sprintf("/api/v1/agents/%s/%s/versions/%s/attestation", res.Namespace, res.Name, res.Version)
		if res.AttestationUrl != nil && *res.AttestationUrl != "" {
			attURL = *res.AttestationUrl
		}
		env, err := trust.FetchEnvelope(cmd.Context(), httpClient, reg, token, attURL)
		if err != nil {
			return job.Release{}, nil, err
		}
		pins, err := trust.LoadPinned(reg)
		if err != nil {
			return job.Release{}, nil, err
		}
		if v := trust.VerifyEnvelope(env, pins, meta.Digest); v.State != trust.StateVerified {
			return job.Release{}, nil, fmt.Errorf("the attestation sent to the runhost is %s here: %s", v.State, v.Detail)
		}
		rel.State, rel.Attestation, rel.Keys = job.StateVerified, env, pins
	case trust.StateUnsigned:
		rel.State = job.StateUnsigned
	case trust.StateUnscanned:
		rel.State = job.StateUnscanned
	default:
		return job.Release{}, nil, fmt.Errorf("%s is %s; a runhost runs a verified, unsigned or unscanned release", meta.Ref, meta.Attestation.State)
	}
	return rel, archive, nil
}

// runOnDesktopCap is the release's desktop line as this device's policy
// holds it, for a run sent to a runhost; nil for a release that declares
// none. A run that uses this computer stays attached to it: a detached run
// and a schedule are refused, since no one here would answer their calls.
func runOnDesktopCap(m *manifest.Manifest, pol *policy.Policy, opts runOpts) (*spec.DesktopCap, error) {
	if m.Capabilities.Desktop == nil {
		return nil, nil
	}
	switch {
	case opts.runhostSchedule != "":
		return nil, engine.Classify(engine.ExitUsage, errors.New("this QuickAgent uses an application on your computer through Agentry Desktop, and a schedule on a runhost runs while this computer may be away: run it with --on and follow it"))
	case opts.detach:
		return nil, engine.Classify(engine.ExitUsage, errors.New("this QuickAgent uses an application on your computer through Agentry Desktop, which answers it only while this computer follows the run: leave out --detach"))
	}
	d, err := compile.DesktopCapFor(m.Capabilities.Desktop, pol, opts.allowScreen)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	return d, nil
}

// keepRunhostLink keeps job id's desktop link open while the job runs,
// answering its calls from this computer's Agentry Desktop for the run it
// bound, and returns what ends it.
func keepRunhostLink(cmd *cobra.Command, c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, home, id, run string) func() {
	ctx, cancel := context.WithCancel(cmd.Context())
	done := make(chan struct{})
	errOut := cmd.ErrOrStderr()
	go func() {
		defer close(done)
		err := remote.KeepLink(ctx, remote.LinkDialerFrom(c, rec, keys), id, remote.SocketForward(desktop.SocketPath(home), run), nil)
		if err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(errOut, "the run's use of this computer is no longer linked to %s (%v): its later calls are refused there as desktop-unreachable\n", rec.Name, err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
