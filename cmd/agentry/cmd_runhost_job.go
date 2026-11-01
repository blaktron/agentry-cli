package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runhost/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The runhost's job and collect verbs, on the server. A job's release is
// verified again here against this runhost's own pinned keys, kept in the
// store, laid down in this account's runner home, and run by this CLI as a
// local run. The run's output is sealed to every paired device, and its
// plaintext, its policy and its task are deleted, whatever way the run ends.

// The server's job layout, under the service's state directory.
var (
	runhostJobsDir     = filepath.Join(runhost.StateDir, "jobs")
	runhostDevicesFile = runhost.DevicesFile
	runhostStoreDir    = filepath.Join(runhost.StateDir, "store", "releases")
	runhostStoreKey    = filepath.Join(runhost.StateDir, "store", store.KeyFile)
	runhostOutputKeep  = 7 * 24 * time.Hour
)

const (
	runhostSealedName  = "output.sealed"
	runhostJobRecord   = "job.json"
	runhostMaxEnvelope = 16 << 20
	// runhostExitNoOutput is collect's answer for a job whose output the
	// runhost does not hold.
	runhostExitNoOutput = 4
)

// runhostJobResult is the job verb's answer.
type runhostJobResult struct {
	Job      string          `json:"job"`
	ExitCode int             `json:"exitCode"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
}

// runhostJobDone is what the server keeps of a finished job beside its
// sealed output, for collection and expiry.
type runhostJobDone struct {
	Job        string `json:"job"`
	Device     string `json:"device"`
	Ref        string `json:"ref"`
	Digest     string `json:"digest"`
	ExitCode   int    `json:"exitCode"`
	FinishedAt string `json:"finishedAt"`
}

// runhostCLI is the CLI a job runs: the service's own. A variable for tests.
var runhostCLI = os.Executable

func runhostJobVerb(ctx context.Context, _ runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	j, err := job.Read(in)
	if err != nil {
		return engine.ExitUsage, err
	}
	devices, err := pairedSealKeys()
	if err != nil {
		return engine.ExitCLI, err
	}
	if _, ok := devices[j.Device]; !ok {
		return engine.ExitUsage, fmt.Errorf("device %s is not paired with this runhost", j.Device)
	}
	st, err := store.Open(runhostStoreDir, runhostStoreKey)
	if err != nil {
		return engine.ExitCLI, fmt.Errorf("the runhost's store: %w", err)
	}
	archive, err := runhostTakeRelease(st, j, in, fw)
	if err != nil {
		return engine.ExitVerify, err
	}
	meta, err := runhostVerifyRelease(j.Release, archive)
	if err != nil {
		return engine.ExitVerify, err
	}
	if !st.Has(j.Release.Digest) {
		if err := st.Put(j.Release, archive); err != nil {
			return engine.ExitCLI, fmt.Errorf("keep the release in the runhost's store: %w", err)
		}
	}
	home, err := runnerHome()
	if err != nil {
		return engine.ExitCLI, err
	}
	if err := runhostLayDown(home, meta, archive); err != nil {
		return engine.ExitVerify, err
	}
	_ = fw.Out("%s", runhostVerifiedLine(meta))
	code, envelope, err := runhostRun(ctx, j, devices, fw)
	if err != nil {
		return engine.ExitCLI, err
	}
	return code, fw.Result(runhostJobResult{Job: j.ID, ExitCode: code, Envelope: envelope})
}

// runhostVerifiedLine says what the runhost checked of the release, and
// claims no more: an unsigned or unscanned release is held to its digest
// alone, and runs on the person's own flag.
func runhostVerifiedLine(meta *installdir.Meta) string {
	switch meta.Attestation.State {
	case trust.StateVerified:
		return fmt.Sprintf("%s: the runhost checked its digest and its attestation (key %s); running", meta.Ref, meta.Attestation.KeyID)
	case trust.StateUnscanned:
		return fmt.Sprintf("%s: the runhost checked its digest; it is unscanned and runs on --allow-unscanned", meta.Ref)
	default:
		return fmt.Sprintf("%s: the runhost checked its digest; it is unsigned and runs on --allow-unsigned", meta.Ref)
	}
}

// runhostTakeRelease answers whether the release must follow the job's
// document, and reads it: from the device, or from the store when the store
// holds that digest.
func runhostTakeRelease(st *store.Store, j *job.Job, in io.Reader, fw *runhost.FrameWriter) ([]byte, error) {
	if st.Has(j.Release.Digest) {
		if err := fw.Release(runhost.ReleaseHeld); err != nil {
			return nil, err
		}
		_, archive, err := st.Get(j.Release.Digest)
		if errors.Is(err, store.ErrDamaged) {
			// Dropped, so the next run sends the release again rather than
			// meeting the same entry.
			_ = st.Remove(j.Release.Digest)
			return nil, fmt.Errorf("%w; it was removed, and the next run sends the release again", err)
		}
		return archive, err
	}
	if err := fw.Release(runhost.ReleaseNeeded); err != nil {
		return nil, err
	}
	archive := make([]byte, j.Release.Size)
	if _, err := io.ReadFull(in, archive); err != nil {
		return nil, fmt.Errorf("the release's archive was cut short: %w", err)
	}
	return archive, nil
}

// runhostVerifyRelease holds the archive to the job's digest, and a verified
// release's attestation to this runhost's own pinned keys for its registry,
// pinned from the device at the first release from that registry.
func runhostVerifyRelease(rel job.Release, archive []byte) (*installdir.Meta, error) {
	if got := sha256Digest(archive); got != rel.Digest {
		return nil, fmt.Errorf("digest mismatch: the job names %s, the archive is %s; refusing to run", rel.Digest, got)
	}
	meta := &installdir.Meta{
		Ref: rel.Ref(), Namespace: rel.Namespace, Name: rel.Name, Version: rel.Version,
		Digest: rel.Digest, Source: "registry",
	}
	switch rel.State {
	case job.StateVerified:
		pins, first, err := runhostPins(rel.Registry, rel.Keys)
		if err != nil {
			return nil, err
		}
		res := trust.VerifyEnvelope(rel.Attestation, pins, rel.Digest)
		if res.State != trust.StateVerified {
			return nil, fmt.Errorf("attestation %s for %s on the runhost: %s; refusing to run", res.State, rel.Ref(), res.Detail)
		}
		if first {
			if err := trust.SavePinned(rel.Registry, pins); err != nil {
				return nil, fmt.Errorf("pin %s's signing keys on the runhost: %w", rel.Registry, err)
			}
		}
		meta.Attestation = receipt.Attestation{State: trust.StateVerified, KeyID: res.KeyID, Verified: true}
	case job.StateUnsigned:
		meta.Attestation = receipt.Attestation{State: trust.StateUnsigned}
	case job.StateUnscanned:
		meta.Attestation = receipt.Attestation{State: trust.StateUnscanned}
	}
	return meta, nil
}

// runhostPins is the key set the runhost verifies a release from registry
// with: its own pins, or, before its first release from that registry, the
// device's set, which first says whether it is pinned (it is, once a release
// verifies with it). A device whose pins differ cannot replace the runhost's.
func runhostPins(registry string, sent trust.KeySet) (keys trust.KeySet, first bool, err error) {
	pinned, err := trust.LoadPinned(registry)
	if err != nil {
		return trust.KeySet{}, false, err
	}
	if len(pinned.Keys) == 0 {
		return sent, true, nil
	}
	if !trust.SameKeyIDs(pinned, sent) {
		change := trust.DiffKeys(pinned, sent)
		return trust.KeySet{}, false, fmt.Errorf("this device's signing keys for %s are not the ones the runhost pinned at its first run from it (%s): the runhost verifies with its own pins and keeps them", registry, change.String())
	}
	return pinned, false, nil
}

// runhostLayDown extracts the verified archive and lays it down in the
// runner home as a verified install; an install of that digest already
// there, which its record vouches for, is kept.
func runhostLayDown(home string, meta *installdir.Meta, archive []byte) error {
	staging, err := os.MkdirTemp("", "agentry-runhost-release-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := pkgarchive.Extract(bytes.NewReader(archive), staging, int64(len(archive)), installExtractLimits()); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	m, err := localrun.ManifestFrom(staging)
	if err != nil {
		return err
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		return fmt.Errorf("%s manifest invalid: %v", meta.Ref, blocking[0])
	}
	// The attestation binds the archive's digest; the name and version the
	// job gives it are the device's, so they are held to the archive's own.
	if m.Namespace != meta.Namespace || m.Name != meta.Name || m.Version != meta.Version {
		return fmt.Errorf("the job names %s, but the archive is %s/%s@%s; refusing to run", meta.Ref, m.Namespace, m.Name, m.Version)
	}
	return installdir.Write(installdir.ReleaseDir(home, meta.Namespace, meta.Name, meta.Version, meta.Digest), staging, meta)
}

// runhostChildEnv is the run's environment: the service's own, less every
// variable that would make the run a hosted one or hand it a hosted
// credential, plus the model's key under the policy's variable.
func runhostChildEnv(base []string, j *job.Job) []string {
	drop := map[string]bool{envHosted: true, envRunGrant: true, envJobToken: true, trust.EnvPinRegistry: true}
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] || strings.HasPrefix(k, "AGENTRY_HOSTED") || (j.Run.Model.APIKeyEnv != "" && k == j.Run.Model.APIKeyEnv) {
			continue
		}
		out = append(out, kv)
	}
	if j.Run.Model.APIKeyEnv != "" && j.Run.ModelKey != "" {
		out = append(out, j.Run.Model.APIKeyEnv+"="+j.Run.ModelKey)
	}
	return out
}

// runhostRunArgs is the run's argv after the CLI's own path: a typed list,
// never a shell line.
func runhostRunArgs(j *job.Job, policyFile, taskFile, outDir string) []string {
	args := []string{"run", j.Release.Ref(), "--installed", "--pin", j.Release.Digest,
		"--policy", policyFile, "--input", taskFile, "--out", outDir,
		"--output-format", "json", "--yes", "--mode", "autonomous"}
	if j.Run.AllowUnsigned {
		args = append(args, "--allow-unsigned")
	}
	if j.Run.AllowUnscanned {
		args = append(args, "--allow-unscanned")
	}
	if j.Run.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(j.Run.Timeout))
	}
	if j.Run.MaxTurns >= 0 {
		args = append(args, "--max-turns", strconv.Itoa(j.Run.MaxTurns))
	}
	if j.Run.MaxCost >= 0 {
		args = append(args, "--max-cost", strconv.FormatFloat(j.Run.MaxCost, 'f', -1, 64))
	}
	return args
}

// runhostRun runs the job's release in a job directory of its own, streams
// the run's narration, seals its output and leaves nothing else behind.
func runhostRun(ctx context.Context, j *job.Job, devices map[string][]byte, fw *runhost.FrameWriter) (int, json.RawMessage, error) {
	if err := os.MkdirAll(runhostJobsDir, 0o700); err != nil {
		return 0, nil, err
	}
	dir := filepath.Join(runhostJobsDir, j.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 0, nil, fmt.Errorf("job %s: %w", j.ID, err)
	}
	outDir := filepath.Join(dir, "out")
	policyFile := filepath.Join(dir, "policy.yaml")
	taskFile := filepath.Join(dir, "task.txt")
	// Everything but the sealed output and the job's record goes, on every
	// path out of here; a CLI killed meanwhile is swept at the next start.
	defer func() {
		for _, p := range []string{outDir, policyFile, taskFile} {
			_ = os.RemoveAll(p)
		}
	}()
	if err := runhostJobPolicy(policyFile, j); err != nil {
		return 0, nil, err
	}
	if err := os.WriteFile(taskFile, []byte(j.Run.Task), 0o600); err != nil {
		return 0, nil, err
	}
	self, err := runhostCLI()
	if err != nil {
		return 0, nil, err
	}
	cmd := exec.CommandContext(ctx, self, runhostRunArgs(j, policyFile, taskFile, outDir)...) //nolint:gosec // the service's own CLI, a typed argv
	cmd.Env = runhostChildEnv(os.Environ(), j)
	cmd.Dir = dir
	var stdout limitedBuffer
	stdout.max = runhostMaxEnvelope
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, nil, err
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	// The narration is read to its end before Wait, which closes the pipe.
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		_ = fw.Out("%s", maskKey(sc.Text(), j.Run.ModelKey))
	}
	werr := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(werr, &exitErr):
		code = exitErr.ExitCode()
	case werr != nil:
		return 0, nil, werr
	}
	if err := runhostSeal(dir, outDir, j, devices); err != nil {
		return 0, nil, fmt.Errorf("seal the run's output: %w", err)
	}
	done := runhostJobDone{Job: j.ID, Device: j.Device, Ref: j.Release.Ref(), Digest: j.Release.Digest, ExitCode: code, FinishedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(done)
	if err := os.WriteFile(filepath.Join(dir, runhostJobRecord), b, 0o600); err != nil {
		return 0, nil, err
	}
	envelope := []byte(maskKey(string(bytes.TrimSpace(stdout.Bytes())), j.Run.ModelKey))
	if !json.Valid(envelope) {
		envelope = nil
	}
	return code, envelope, nil
}

// maskKey is s with every occurrence of key masked.
func maskKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// runhostJobPolicy writes the job's policy: the runhost's own, with the
// device's model, whose key the run reads from its environment.
func runhostJobPolicy(file string, j *job.Job) error {
	path, err := policy.DefaultPath()
	if err != nil {
		return err
	}
	pol, _, err := policy.Load(path)
	if err != nil {
		return fmt.Errorf("the runhost's policy: %w", err)
	}
	// Every container class runs under gVisor on a runhost; a policy changed
	// since setup that says otherwise runs nothing.
	if pol.Docker.Runtime != runhost.RunscRuntime || pol.Docker.GatewayRuntime != runhost.RunscUDSRuntime {
		return fmt.Errorf("the runhost's policy runs containers under %q and the gateway under %q, not %s and %s: run agentry runhost add again, which repairs it", pol.Docker.Runtime, pol.Docker.GatewayRuntime, runhost.RunscRuntime, runhost.RunscUDSRuntime)
	}
	pol.Model = j.Run.Model.Policy()
	if err := pol.Validate(); err != nil {
		return fmt.Errorf("the run's model on this runhost: %w", err)
	}
	return policy.Save(file, pol)
}

// runhostSeal packs the run's output and seals it to every paired device,
// written aside and renamed, so a sealed output is whole or absent.
func runhostSeal(dir, outDir string, j *job.Job, devices map[string][]byte) error {
	to, err := output.Devices(devices)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sealing-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := os.Stat(outDir); err != nil {
		// A run that stopped before its output folder existed packs an
		// empty tar.
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := output.Pack(pw, outDir)
		pw.CloseWithError(err)
		packed <- err
	}()
	serr := output.Seal(tmp, pr, to, j.ID)
	_ = pr.CloseWithError(errors.New("sealing stopped"))
	if perr := <-packed; serr == nil {
		serr = perr
	}
	if serr != nil {
		_ = tmp.Close()
		return serr
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, runhostSealedName))
}

// pairedSealKeys are the paired devices' sealing keys by device id.
func pairedSealKeys() (map[string][]byte, error) {
	var devs runhost.Devices
	if err := readJSONFile(runhostDevicesFile, &devs); err != nil {
		return nil, fmt.Errorf("the runhost's devices: %w", err)
	}
	out := map[string][]byte{}
	for _, d := range devs.Devices {
		raw, err := base64.StdEncoding.DecodeString(d.SealKey)
		if err != nil {
			return nil, fmt.Errorf("device %s's sealing key is not readable: %w", d.ID, err)
		}
		out[d.ID] = raw
	}
	if len(out) == 0 {
		return nil, errors.New("no device is paired with this runhost")
	}
	return out, nil
}

// runhostCollectVerb answers `collect <job>` with the job's sealed output as
// data frames, and `collect <job> done` by deleting the job.
func runhostCollectVerb(_ context.Context, req runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
	if len(req.Args) == 0 || len(req.Args) > 2 || !job.ValidID(req.Args[0]) || (len(req.Args) == 2 && req.Args[1] != "done") {
		return engine.ExitUsage, errors.New("collect takes a job's id, and then done to delete it")
	}
	dir := filepath.Join(runhostJobsDir, req.Args[0])
	if len(req.Args) == 2 {
		return 0, os.RemoveAll(dir)
	}
	f, err := os.Open(filepath.Join(dir, runhostSealedName)) //nolint:gosec // a job directory named by a checked id
	if errors.Is(err, os.ErrNotExist) {
		return runhostExitNoOutput, fmt.Errorf("the runhost holds no output of job %s: it was collected, has expired, or never finished", req.Args[0])
	}
	if err != nil {
		return engine.ExitCLI, err
	}
	defer func() { _ = f.Close() }()
	return 0, fw.Data(f)
}

// sweepRunhostJobs removes what jobs left behind: a job with no sealed
// output (its CLI was killed, or the service was) and a sealed output older
// than the keep. It runs at the service's start, when no job is running,
// and hourly after, when it touches only finished jobs.
func sweepRunhostJobs(now time.Time, atStart bool) {
	entries, err := os.ReadDir(runhostJobsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(runhostJobsDir, e.Name())
		st, err := os.Stat(filepath.Join(dir, runhostSealedName))
		switch {
		case err != nil && atStart:
			_ = os.RemoveAll(dir)
		case err == nil && now.Sub(st.ModTime()) > runhostOutputKeep:
			_ = os.RemoveAll(dir)
		}
	}
}

// limitedBuffer keeps the first max bytes written to it and discards the
// rest.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
