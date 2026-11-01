package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
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
	case opts.sessionProtocol, opts.mode == string(manifest.ModeInteractive):
		return "an interactive run"
	case len(opts.files) > 0, len(opts.knowledge) > 0, opts.voice, opts.voiceFile != "":
		return "--file, --knowledge and --voice"
	case len(opts.intake) > 0, len(opts.intakeToken) > 0:
		return "--intake"
	case len(opts.destinations) > 0, opts.destinationsFile != "", opts.deliverTo != "":
		return "--destination and --deliver-to"
	case opts.connectionsFile != "", opts.reviewInputs != "", opts.eventsPath != "":
		return "--connections-file, --review-inputs and --events"
	case opts.reportDesign != "", opts.reportTheme != "":
		return "--report-design and --report-theme"
	case opts.keepProfile != "", opts.reconfigureProfile:
		return "the MCP profile flags"
	}
	return ""
}

// runOnRefusedRelease says what a release declares that a run on a runhost
// does not carry yet.
func runOnRefusedRelease(m *manifest.Manifest) string {
	switch {
	case len(m.Capabilities.Secrets) > 0 || len(m.Capabilities.OptionalSecrets) > 0:
		return "secrets"
	case len(m.Intake) > 0:
		return "intake questions"
	case len(m.Capabilities.TargetHosts) > 0:
		return "target hosts"
	case len(m.Capabilities.MCPServers) > 0:
		return "MCP servers"
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
	modelKey, err := runOnModelKey(pol)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
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
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		return engine.Classify(engine.ExitCompile, fmt.Errorf("%s manifest invalid: %v", meta.Ref, blocking[0]))
	}
	if what := runOnRefusedRelease(m); what != "" {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s declares %s, which a run on a runhost does not carry yet", meta.Ref, what))
	}
	mode, err := resolveRunMode(cmd, opts, m)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if mode != manifest.ModeAutonomous {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s runs %s; a run on a runhost is autonomous", meta.Ref, mode))
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
	task, err := io.ReadAll(io.LimitReader(runTask(cmd, opts), job.MaxDocument/2+1))
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if len(task) > job.MaxDocument/2 {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the task is over %d bytes, more than a run on a runhost takes", job.MaxDocument/2))
	}
	id, err := newRunhostJobID()
	if err != nil {
		return err
	}
	j := &job.Job{V: job.Version, ID: id, Device: rec.DeviceID, Release: rel, Run: job.Run{
		Task: string(task), AllowUnsigned: opts.allowUnsigned, AllowUnscanned: opts.allowUnscanned,
		Adapter: opts.adapterName, Timeout: opts.timeout, MaxTurns: opts.maxTurns, MaxCost: opts.maxCost,
		Model: job.ModelOf(pol.Model), ModelKey: modelKey,
	}}
	if err := j.Validate(); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}

	opts.progress.at(receipt.StagePrepare)
	keys, err := runhost.LoadKeys(rec.Name)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	client, err := runhost.DialRunhost(cmd.Context(), *rec, keys)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	defer func() { _ = client.Close() }()
	if err := runOnSameVersion(cmd.Context(), client, rec.Name); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "running %s on %s (%s, host key %s)\n", meta.Ref, rec.Name, rec.Host, rec.HostKeyFingerprint)

	opts.progress.at(receipt.StageAgent)
	res, err := runOnSend(cmd.Context(), client, j, archive, func(line string) { _, _ = fmt.Fprintf(errOut, "%s: %s\n", rec.Name, line) })
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if res.Job != j.ID {
		return engine.Classify(engine.ExitGateway, fmt.Errorf("%s answered for job %q, not this run's", rec.Name, res.Job))
	}

	opts.progress.at(receipt.StageOutput)
	var env receipt.Envelope
	if err := json.Unmarshal(res.Envelope, &env); err != nil {
		return engine.Classify(engine.ExitCLI, fmt.Errorf("%s's run gave no envelope (exit %d)", rec.Name, res.ExitCode))
	}
	runID := env.RunID
	if runID == "" {
		runID = j.ID[:12]
	}
	outDir, err := resolveOutDir(opts.out, meta, runID)
	if err != nil {
		return err
	}
	opts.progress.wrote(outDir)
	if err := runOnCollect(cmd.Context(), client, j.ID, rec.DeviceID, keys, outDir); err != nil {
		return engine.Classify(engine.ExitCLI, fmt.Errorf("collect the run's output from %s: %w (it stays there, sealed, for 7 days)", rec.Name, err))
	}
	if _, err := client.Ask(cmd.Context(), "collect", []string{j.ID, "done"}, nil); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %s keeps the sealed output of job %s until it expires: %v\n", rec.Name, j.ID, err)
	}

	runOnEnvelope(&env, rec, outDir, j.ID)
	if err := env.Write(outDir); err != nil {
		return err
	}
	printEnvelope(cmd, &env, opts.outputFormat)
	opts.progress.enveloped()
	if env.ExitCode != 0 {
		return engine.Classify(env.ExitCode, errors.New(nonEmptyOr(env.Error, "the run on "+rec.Name+" failed")))
	}
	return nil
}

// runOnModelKey is the device policy's model key, which the run carries in
// flight: the variable the policy names, else its keychain entry. A model a
// runhost cannot be handed this way yet is refused here.
func runOnModelKey(pol *policy.Policy) (string, error) {
	switch {
	case pol.Model.Provider == policy.ProviderAgentry:
		return "", errors.New("a run on a runhost cannot use Agentry credits yet: set a model of your own with agentry setup")
	case policy.IsModelSignIn(pol.Model.APIKeyKind) || pol.Model.APIKeyKind == policy.KeyKindSubscription:
		return "", errors.New("a run on a runhost cannot use a model sign-in yet: set an API key with agentry setup")
	}
	key, err := modelkey.Personal(pol.Model.APIKeyEnv, pol.Model.APIKeySource, pol.Model.Provider)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("no key for the model %s/%s: set $%s, or keep it in the keychain with agentry setup", pol.Model.Provider, pol.Model.Name, pol.Model.APIKeyEnv)
	}
	return key, nil
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

// runOnSameVersion refuses a runhost whose CLI is not this device's
// version: the run's plan, policy and envelope are this version's.
func runOnSameVersion(ctx context.Context, c *runhost.Client, name string) error {
	a, err := c.Ask(ctx, "version", nil, nil)
	if err != nil {
		return fmt.Errorf("ask %s its version: %w", name, err)
	}
	var v struct {
		Version string `json:"version"`
	}
	if a.Code != 0 || json.Unmarshal(a.Result, &v) != nil {
		return fmt.Errorf("%s did not say its version: %s", name, a.Error)
	}
	if v.Version != version.Version {
		return fmt.Errorf("%s runs CLI %s and this device %s; a run needs the same version on both: run agentry runhost add %s again from this device, which updates it", name, v.Version, version.Version, name)
	}
	return nil
}

// runOnSend sends the job, and its release when the runhost asks for it,
// over one session, and reads the run's narration and its result.
func runOnSend(ctx context.Context, c *runhost.Client, j *job.Job, archive []byte, say func(string)) (runhostJobResult, error) {
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
				_, err := w.Write(archive)
				return err
			}
			return nil
		case <-stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	each := func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			if say != nil {
				say(f.Text)
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
	a, err := c.Call(ctx, "job", nil, send, each)
	if err != nil {
		return runhostJobResult{}, err
	}
	var res runhostJobResult
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &res) != nil {
		return runhostJobResult{}, fmt.Errorf("the runhost refused the run (exit %d): %s", a.Code, nonEmptyOr(a.Error, "no reason given"))
	}
	return res, nil
}

// runOnCollect fetches the job's sealed output and opens it with this
// device's sealing key into outDir. An output that does not open, or holds
// anything a run's output does not, leaves nothing in outDir.
func runOnCollect(ctx context.Context, c *runhost.Client, jobID, device string, keys *runhost.DeviceKeys, outDir string) error {
	sealed, err := os.CreateTemp(filepath.Dir(outDir), ".runhost-output-*")
	if err != nil {
		return err
	}
	defer func() { _ = sealed.Close(); _ = os.Remove(sealed.Name()) }()
	var written int64
	a, err := c.Call(ctx, "collect", []string{jobID}, nil, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		if written += int64(len(f.Data)); written > output.MaxOutput+output.MaxHeader+(output.MaxOutput/output.ChunkSize+1)*20 {
			return errors.New("the sealed output is larger than any run's")
		}
		_, err := sealed.Write(f.Data)
		return err
	})
	if err != nil {
		return err
	}
	if a.Code != 0 {
		return errors.New(nonEmptyOr(a.Error, "the runhost would not hand the output over"))
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(outDir), ".runhost-open-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(output.Open(pw, sealed, device, keys.Seal, jobID)) }()
	if err := output.Unpack(pr, filepath.Join(staging, "out")); err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	return moveInto(filepath.Join(staging, "out"), outDir)
}

// moveInto moves every entry of src into dst, which exists.
func moveInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		to := filepath.Join(dst, e.Name())
		if err := os.RemoveAll(to); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}

// runOnEnvelope takes the runhost CLI's envelope as this device's record:
// its folders are this device's, and it names the runhost it ran on.
func runOnEnvelope(env *receipt.Envelope, rec *runhost.Record, outDir, jobID string) {
	if env.OutDir != "" {
		if env.Receipts.Dir != "" {
			if rel, err := filepath.Rel(env.OutDir, env.Receipts.Dir); err == nil && !strings.HasPrefix(rel, "..") {
				env.Receipts.Dir = filepath.Join(outDir, rel)
			}
		}
	}
	env.OutDir = outDir
	if env.Receipts.Dir == "" {
		env.Receipts.Dir = filepath.Join(outDir, "receipts")
	}
	env.Runhost = &receipt.Runhost{Name: rec.Name, Host: rec.Host, HostKeyFingerprint: rec.HostKeyFingerprint, CLIVersion: rec.CLIVersion, Job: jobID}
}

// newRunhostJobID is a fresh job id.
func newRunhostJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
