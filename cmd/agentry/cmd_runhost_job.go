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
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runhost/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
	// runhostUnsafeModeFile is the runhost's Unsafe Mode setting.
	runhostUnsafeModeFile = runhost.UnsafeModeFile
	runhostStoreDir       = filepath.Join(runhost.StateDir, "store", "releases")
	runhostStoreKey       = filepath.Join(runhost.StateDir, "store", store.KeyFile)
	runhostOutputKeep     = 7 * 24 * time.Hour
	// runhostCancelGrace is how long an interrupted run has to tear down
	// before it is killed.
	runhostCancelGrace = 30 * time.Second
)

const (
	// runhostFilesDir is where a job's files are laid down in its directory.
	runhostFilesDir   = "files"
	runhostSealedName = "output.sealed"
	// runhostEnvelopeName is the run's envelope, sealed beside its output.
	runhostEnvelopeName = "envelope.sealed"
	runhostJobRecord    = "job.json"
	runhostMaxEnvelope  = 16 << 20
	// runhostExitNoOutput is collect's answer for a job whose output the
	// runhost does not hold.
	runhostExitNoOutput = 4
	// runhostExitRunning is collect's answer for a job still running.
	runhostExitRunning = 5
)

// runhostJobResult is the job verb's answer.
type runhostJobResult struct {
	Job      string `json:"job"`
	ExitCode int    `json:"exitCode"`
	// Done says the job has ended: its sealed output and envelope wait for
	// collect. Next is the index after the last frame the answer carried.
	Done bool `json:"done"`
	Next int  `json:"next,omitempty"`
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
	Session    bool   `json:"session,omitempty"`
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
	// A model paid with Agentry credits needs the runhost's credits token
	// for its registry, before anything of the job is taken.
	if j.Run.Model.PaysWithCredits() {
		if _, err := runhostJobCredits(j.Run.Model.Policy()); err != nil {
			return engine.ExitPolicy, err
		}
	}
	// So does a model paid by a plan's sign-in, the runhost's own.
	if j.Run.Model.SignIn() {
		if err := runhostJobSignIn(j.Run.Model.APIKeyKind); err != nil {
			return engine.ExitPolicy, err
		}
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
	dir, err := runhostJobDir(j)
	if err != nil {
		return engine.ExitCLI, err
	}
	// The run's files follow the release on the stream; they are laid down
	// in the job's own directory, which keeps nothing but the sealed output
	// once the run ends.
	if j.Run.Payload != nil {
		if err := payload.Unpack(in, j.Run.Payload.Size, j.Run.Payload.Digest, filepath.Join(dir, runhostFilesDir)); err != nil {
			_ = os.RemoveAll(dir)
			return engine.ExitUsage, err
		}
	}
	// The job runs under the service from here, apart from this session: a
	// device that leaves finds it again with attach.
	jobCtx, cancel := context.WithCancel(ctx)
	live := newRunhostLive(j, cancel)
	live.out("%s", runhostVerifiedLine(meta))
	ready := make(chan struct{})
	go func() {
		defer cancel()
		code, err := runhostRun(jobCtx, j, dir, home, devices, live, ready)
		if err != nil {
			// A job that sealed nothing keeps nothing.
			if _, serr := os.Stat(filepath.Join(dir, runhostSealedName)); serr != nil {
				_ = os.RemoveAll(dir)
			}
			if code == 0 {
				code = engine.ExitCLI
			}
		}
		live.finish(code, err)
		// An attach that arrives after this reads the job's record.
		live.forget()
	}()
	<-ready
	if err := fw.Started(j.ID); err != nil {
		return 0, nil
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		if j.Run.Session {
			live.relaySession(in)
			return
		}
		_, _ = io.Copy(io.Discard, in)
	}()
	return live.follow(ctx, fw, 0, gone)
}

// runhostJobDir makes the job's own directory, owner-only, refusing a job
// whose id is already taken.
func runhostJobDir(j *job.Job) (string, error) {
	if err := os.MkdirAll(runhostJobsDir, 0o700); err != nil {
		return "", err
	}
	dir := filepath.Join(runhostJobsDir, j.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("job %s: %w", j.ID, err)
	}
	return dir, nil
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
// credential, and less any the job's own credentials travel in, plus the
// model's key under the policy's variable, or the runhost's credits token
// for a model paid with Agentry credits, and the job's credentials under
// theirs (runhostCredentialEnv).
func runhostChildEnv(base []string, j *job.Job, creditsToken string) []string {
	drop := map[string]bool{envHosted: true, envRunGrant: true, envJobToken: true, trust.EnvPinRegistry: true, envRunhostCreditsToken: true}
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] || strings.HasPrefix(k, "AGENTRY_HOSTED") || strings.HasPrefix(k, "AGENTRY_DEST_") || strings.HasPrefix(k, runhostSourceTokenPrefix) || strings.HasPrefix(k, runhostHostEnvPrefix) ||
			(j.Run.Model.APIKeyEnv != "" && k == j.Run.Model.APIKeyEnv) {
			continue
		}
		out = append(out, kv)
	}
	if j.Run.Model.APIKeyEnv != "" && j.Run.ModelKey != "" {
		out = append(out, j.Run.Model.APIKeyEnv+"="+j.Run.ModelKey)
	}
	if creditsToken != "" {
		out = append(out, envRunhostCreditsToken+"="+creditsToken)
	}
	return append(out, runhostCredentialEnv(j)...)
}

// runhostSourceTokenPrefix names the variables an input source's token
// reaches the run in; the run hands each to its source's fetch step alone.
const runhostSourceTokenPrefix = "AGENTRY_SOURCE_TOKEN_"

// runhostSourceTokenEnv is the variable the token of the source answering
// question id travels in.
func runhostSourceTokenEnv(id string) string { return runhostSourceTokenPrefix + strings.ToUpper(id) }

// runhostHostEnvPrefix names the variables a target host's password,
// passphrase and sudo password reach the run in.
const runhostHostEnvPrefix = "AGENTRY_HOST_"

// runhostHostEnv names the variables of target host id's password, key
// passphrase and sudo password.
func runhostHostEnv(id string) (password, passphrase, sudo string) {
	u := strings.ToUpper(id)
	return runhostHostEnvPrefix + "PASSWORD_" + u, runhostHostEnvPrefix + "PASSPHRASE_" + u, runhostHostEnvPrefix + "SUDO_" + u
}

// runhostCredentialEnv is the job's credentials as the run's environment
// takes them: each input source's token, and each destination's credentials
// under the names its destinations file gives (destinationEnv). The run
// takes the destinations' out of its own environment as it reads them.
func runhostCredentialEnv(j *job.Job) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(j.Run.SourceTokens)) {
		out = append(out, runhostSourceTokenEnv(id)+"="+j.Run.SourceTokens[id])
	}
	for _, id := range slices.Sorted(maps.Keys(j.Run.TargetHosts)) {
		h := j.Run.TargetHosts[id]
		password, passphrase, sudo := runhostHostEnv(id)
		for _, kv := range [][2]string{{password, h.Password}, {passphrase, h.Passphrase}, {sudo, h.Sudo}} {
			if kv[1] != "" {
				out = append(out, kv[0]+"="+kv[1])
			}
		}
	}
	for i, d := range j.Run.Destinations {
		secret, auth, key := destinationEnv(i)
		out = append(out, secret+"="+d.Secret)
		if d.Authorization != "" {
			out = append(out, auth+"="+d.Authorization)
		}
		if d.AccessKeyID != "" {
			out = append(out, key+"="+d.AccessKeyID)
		}
	}
	return out
}

// destinationEnv names the variables of the i-th destination's signing
// secret (a bucket's secret access key), Authorization value and access key
// id.
func destinationEnv(i int) (secret, auth, key string) {
	return fmt.Sprintf("AGENTRY_DEST_SECRET_%d", i), fmt.Sprintf("AGENTRY_DEST_AUTH_%d", i), fmt.Sprintf("AGENTRY_DEST_KEY_%d", i)
}

// runhostStaged is what a job's directory holds for its run, each path
// empty when the job carries none of it.
type runhostStaged struct {
	dir, files                        string
	policy, task, out, secrets        string
	intake, connections, destinations string
	// inputs is a session's inputs folder.
	inputs string
	// knownHosts is the device's lines for the target hosts, and hostKeys
	// each target host's key file, by id.
	knownHosts string
	hostKeys   map[string]string
}

// runhostRunArgs is the run's argv after the CLI's own path: a typed list,
// never a shell line. The registry is the release's, which the run asks of
// its declared servers' advisories and nothing else: the release itself is
// the one laid down at --pin.
func runhostRunArgs(j *job.Job, st runhostStaged) []string {
	args := []string{"--registry", j.Release.Registry, "run", j.Release.Ref(), "--installed", "--pin", j.Release.Digest,
		"--policy", st.policy, "--input", st.task, "--out", st.out,
		"--output-format", "json", "--yes"}
	if j.Run.Session {
		args = append(args, "--mode", "interactive", "--session-protocol", "--session-inputs", st.inputs)
	} else {
		args = append(args, "--mode", "autonomous")
	}
	if j.Run.Adapter != "" {
		args = append(args, "--adapter", j.Run.Adapter)
	}
	file := func(member string) string { return filepath.Join(st.files, filepath.FromSlash(member)) }
	for _, name := range j.Run.Inputs {
		args = append(args, "--file", file(name))
	}
	for _, name := range j.Run.Knowledge {
		args = append(args, "--knowledge", file(name))
	}
	if j.Run.Voice != "" {
		args = append(args, "--voice-file", file(j.Run.Voice))
	} else {
		args = append(args, "--no-voice")
	}
	switch {
	case j.Run.ReportDesign != "":
		args = append(args, "--report-design", file(j.Run.ReportDesign))
	case j.Run.ReportTheme != "":
		args = append(args, "--report-theme", j.Run.ReportTheme)
	}
	if st.intake != "" {
		args = append(args, "--intake-file", st.intake)
	}
	for _, id := range slices.Sorted(maps.Keys(j.Run.SourceTokens)) {
		args = append(args, "--intake-token", id+"="+runhostSourceTokenEnv(id))
	}
	if st.connections != "" {
		args = append(args, "--connections-file", st.connections)
	}
	if st.destinations != "" {
		args = append(args, "--destinations-file", st.destinations)
	} else {
		args = append(args, "--no-destination")
	}
	if st.knownHosts != "" {
		args = append(args, "--host-known-hosts", st.knownHosts)
	}
	for _, id := range slices.Sorted(maps.Keys(j.Run.TargetHosts)) {
		h := j.Run.TargetHosts[id]
		password, passphrase, sudo := runhostHostEnv(id)
		if f := st.hostKeys[id]; f != "" {
			args = append(args, "--host-key", id+"="+f)
		}
		if h.Password != "" {
			args = append(args, "--host-password", id+"="+password)
		}
		if h.Passphrase != "" {
			args = append(args, "--host-passphrase", id+"="+passphrase)
		}
		if h.Sudo != "" {
			args = append(args, "--host-sudo-password", id+"="+sudo)
		}
	}
	if j.Run.AllowVulnerableMCP {
		args = append(args, "--allow-vulnerable-mcp")
	}
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

// runhostRun runs the job's release in its job directory, streams the run's
// narration, seals its output and leaves nothing else behind: the secrets
// file, the intake, connections and destinations files, the run's files and
// any file the CLI left holding one of the job's credentials go on every
// path, and what a killed service left is swept at its next start.
func runhostRun(ctx context.Context, j *job.Job, dir, home string, devices map[string][]byte, live *runhostLive, ready chan<- struct{}) (code int, err error) {
	readied := false
	defer func() {
		if !readied {
			close(ready)
		}
	}()
	st := runhostStaged{dir: dir, files: filepath.Join(dir, runhostFilesDir),
		policy: filepath.Join(dir, "policy.yaml"), task: filepath.Join(dir, "task.txt"),
		out: filepath.Join(dir, "out"), secrets: filepath.Join(dir, "secrets.env")}
	if j.Run.Session {
		st.inputs = filepath.Join(dir, "session-inputs")
	}
	// A model paid with Agentry credits opens its session with the
	// runhost's own credits token, kept in its store.
	var creditsToken string
	if j.Run.Model.PaysWithCredits() {
		tok, err := runhostJobCredits(j.Run.Model.Policy())
		if err != nil {
			return engine.ExitPolicy, err
		}
		creditsToken = tok
	}
	// A model paid by a plan's sign-in runs on the one the runhost keeps.
	if j.Run.Model.SignIn() {
		if err := runhostJobSignIn(j.Run.Model.APIKeyKind); err != nil {
			return engine.ExitPolicy, err
		}
	}
	values := runhostJobValues(j)
	if creditsToken != "" {
		values = append([]string{creditsToken}, values...)
	}
	defer func() {
		for _, p := range []string{st.secrets, st.intake, st.connections, st.destinations, st.knownHosts, filepath.Join(dir, "hostkeys"), st.files, st.inputs, st.out, st.policy, policy.NewestPathFor(st.policy), st.task} {
			if p != "" {
				_ = os.RemoveAll(p)
			}
		}
		dropRunCredentials(home, values)
	}()
	if err := runhostStage(j, &st); err != nil {
		return engine.ExitUsage, err
	}
	self, err := runhostCLI()
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, self, runhostRunArgs(j, st)...) //nolint:gosec // the service's own CLI, a typed argv
	cmd.Env = runhostChildEnv(os.Environ(), j, creditsToken)
	cmd.Dir = dir
	// A cancel, or the service stopping, interrupts the run, which tears its
	// containers down as a local run's interrupt does, and is killed if it
	// has not after the grace. A device that leaves does neither: the job
	// goes on, and a session to its idle timeout.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = runhostCancelGrace
	var stdout limitedBuffer
	stdout.max = runhostMaxEnvelope
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	var events *os.File
	if j.Run.Session {
		// The session's commands arrive on its stdin, and its events leave on
		// descriptor 3, as the session protocol takes them.
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		live.stdin, live.inputs = stdin, st.inputs
		r, w, err := os.Pipe()
		if err != nil {
			return 0, err
		}
		cmd.ExtraFiles = []*os.File{w}
		events = r
		defer func() { _ = r.Close() }()
		defer func() { _ = w.Close() }()
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	readied = true
	close(ready)
	var pumps sync.WaitGroup
	if events != nil {
		_ = cmd.ExtraFiles[0].Close()
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			sc := bufio.NewScanner(events)
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				if f, ok := sessionEventFrame(sc.Bytes(), values); ok {
					live.emit(f)
				}
			}
		}()
	}
	// The narration is read to its end before Wait, which closes the pipe.
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		live.out("%s", maskValues(sc.Text(), values))
	}
	pumps.Wait()
	werr := cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case errors.As(werr, &exitErr):
		code = exitErr.ExitCode()
		if code < 0 {
			// Killed by a signal past the grace: no class of the run's own.
			code = engine.ExitCLI
		}
	case werr != nil:
		return 0, werr
	}
	if err := runhostSeal(dir, st.out, j, devices); err != nil {
		return 0, fmt.Errorf("seal the run's output: %w", err)
	}
	// The envelope is the CLI's stdout, which names the run's result: it is
	// sealed to the devices too, so nothing of the run's outcome is kept in
	// the clear.
	envelope := []byte(maskValues(string(bytes.TrimSpace(stdout.Bytes())), values))
	if !json.Valid(envelope) {
		envelope = []byte("null")
	}
	if err := runhostSealBytes(dir, runhostEnvelopeName, envelope, sealedEnvelopeJob(j.ID), devices); err != nil {
		return 0, fmt.Errorf("seal the run's envelope: %w", err)
	}
	done := runhostJobDone{Job: j.ID, Device: j.Device, Ref: j.Release.Ref(), Digest: j.Release.Digest, ExitCode: code,
		FinishedAt: time.Now().UTC().Format(time.RFC3339), Session: j.Run.Session}
	b, _ := json.Marshal(done)
	if err := os.WriteFile(filepath.Join(dir, runhostJobRecord), b, 0o600); err != nil {
		return 0, err
	}
	return code, nil
}

// runhostSealBytes seals b to every paired device under the job string
// sealJob, written aside and renamed into dir as name.
func runhostSealBytes(dir, name string, b []byte, sealJob string, devices map[string][]byte) error {
	to, err := output.Devices(devices)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sealing-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := output.Seal(tmp, bytes.NewReader(b), to, sealJob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// maskValues is s with every occurrence of each value masked, the longest
// first, so a value that holds another is masked whole.
func maskValues(s string, values []string) string {
	for _, v := range values {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// runhostJobValues are the job's credentials, every value the run's
// narration and envelope are masked of: the model key, the secrets, the
// sources' tokens, the connections' tokens and the destinations'
// credentials, longest first.
func runhostJobValues(j *job.Job) []string {
	r := j.Run
	values := []string{r.ModelKey}
	for _, v := range r.Secrets {
		values = append(values, v)
	}
	for _, v := range r.SourceTokens {
		values = append(values, v)
	}
	for _, c := range r.Connections {
		values = append(values, c.AccessToken)
	}
	for _, d := range r.Destinations {
		values = append(values, d.Secret, d.Authorization, d.AccessKeyID)
	}
	for _, h := range r.TargetHosts {
		values = append(values, h.Key, h.Password, h.Passphrase, h.Sudo)
	}
	values = slices.DeleteFunc(values, func(v string) bool { return len(v) < 4 })
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(values)
}

// runhostStage writes what the job's run reads, each file owner-only and
// created new: the task, the secrets file the job's policy names as its
// secret source (written even when empty, so no declared secret resolves
// from the service's own environment), the intake answers, the connections
// and the destinations, and the policy.
func runhostStage(j *job.Job, st *runhostStaged) error {
	if err := writeNewFile(st.task, []byte(j.Run.Task)); err != nil {
		return err
	}
	if st.inputs != "" {
		if err := os.Mkdir(st.inputs, 0o700); err != nil {
			return err
		}
	}
	var env strings.Builder
	for _, name := range slices.Sorted(maps.Keys(j.Run.Secrets)) {
		env.WriteString(name + "=" + j.Run.Secrets[name] + "\n")
	}
	if err := writeNewFile(st.secrets, []byte(env.String())); err != nil {
		return err
	}
	if len(j.Run.Intake) > 0 || len(j.Run.IntakeFiles) > 0 {
		answers := maps.Clone(j.Run.Intake)
		if answers == nil {
			answers = map[string]string{}
		}
		for id, member := range j.Run.IntakeFiles {
			answers[id] = filepath.Join(st.files, filepath.FromSlash(member))
		}
		st.intake = filepath.Join(st.dir, "intake.json")
		if err := writeNewJSON(st.intake, answers); err != nil {
			return err
		}
	}
	if len(j.Run.Connections) > 0 {
		st.connections = filepath.Join(st.dir, "connections.json")
		if err := writeNewJSON(st.connections, j.Run.Connections); err != nil {
			return err
		}
	}
	if len(j.Run.Destinations) > 0 {
		type entry struct {
			Name             string `json:"name"`
			Kind             string `json:"kind"`
			URL              string `json:"url,omitempty"`
			Bucket           string `json:"bucket,omitempty"`
			Prefix           string `json:"prefix,omitempty"`
			Region           string `json:"region,omitempty"`
			Endpoint         string `json:"endpoint,omitempty"`
			SecretEnv        string `json:"secretEnv"`
			AccessKeyIDEnv   string `json:"accessKeyIdEnv,omitempty"`
			AuthorizationEnv string `json:"authorizationEnv,omitempty"`
		}
		var doc struct {
			Destinations []entry `json:"destinations"`
		}
		for i, d := range j.Run.Destinations {
			secret, auth, key := destinationEnv(i)
			e := entry{Name: d.Name, Kind: d.Kind, URL: d.URL, Bucket: d.Bucket, Prefix: d.Prefix, Region: d.Region, Endpoint: d.Endpoint, SecretEnv: secret}
			if d.Authorization != "" {
				e.AuthorizationEnv = auth
			}
			if d.AccessKeyID != "" {
				e.AccessKeyIDEnv = key
			}
			doc.Destinations = append(doc.Destinations, e)
		}
		st.destinations = filepath.Join(st.dir, "destinations.json")
		if err := writeNewJSON(st.destinations, doc); err != nil {
			return err
		}
	}
	if len(j.Run.TargetHosts) > 0 {
		var known strings.Builder
		for _, id := range slices.Sorted(maps.Keys(j.Run.TargetHosts)) {
			for _, l := range j.Run.TargetHosts[id].Known {
				known.WriteString(l + "\n")
			}
		}
		st.knownHosts = filepath.Join(st.dir, "known_hosts")
		if err := writeNewFile(st.knownHosts, []byte(known.String())); err != nil {
			return err
		}
		keys := filepath.Join(st.dir, "hostkeys")
		if err := os.Mkdir(keys, 0o700); err != nil {
			return err
		}
		st.hostKeys = map[string]string{}
		for id, h := range j.Run.TargetHosts {
			if h.Key == "" {
				continue
			}
			f := filepath.Join(keys, id)
			if err := writeNewFile(f, []byte(h.Key)); err != nil {
				return err
			}
			st.hostKeys[id] = f
		}
	}
	return runhostJobPolicy(st.policy, st.secrets, j)
}

// writeNewFile writes data to a new file only its owner can read, never
// through a link and never over a file already there.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|openNoFollow, 0o600) //nolint:gosec // the job's own directory
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func writeNewJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeNewFile(path, b)
}

// runCredentialFiles are the files under a run's configuration a local run
// writes a credential into for its containers, and deletes at its teardown:
// the gateway's secrets file, the remote servers' connection folders and the
// target hosts' sign-in folders.
// A CLI killed past its grace never reaches its teardown.
func runCredentialFiles(home string) []string {
	cfgs, _ := filepath.Glob(filepath.Join(installdir.AgentsRoot(home), "*", "*", "*", "runs", "*", "cfg"))
	var out []string
	for _, cfg := range cfgs {
		out = append(out, filepath.Join(cfg, "gateway", "secrets.env"), filepath.Join(cfg, spec.ConnectionsDir), filepath.Join(cfg, spec.HostsDir))
	}
	return out
}

// dropRunCredentials deletes each credential file a run left under the
// runner home that holds one of values: the CLI deletes them at its
// teardown, which a killed CLI never reaches. Another job's are left alone.
func dropRunCredentials(home string, values []string) {
	if len(values) == 0 {
		return
	}
	holds := func(p string) bool {
		f, err := os.OpenFile(p, os.O_RDONLY|openNoFollow, 0) //nolint:gosec // under the runner home, the CLI's own layout
		if err != nil {
			return false
		}
		raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
		_ = f.Close()
		if err != nil {
			return false
		}
		for _, v := range values {
			if bytes.Contains(raw, []byte(v)) {
				return true
			}
		}
		return false
	}
	for _, p := range runCredentialFiles(home) {
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
		case fi.Mode().IsRegular():
			if holds(p) {
				_ = os.Remove(p)
			}
		case fi.IsDir():
			found := false
			_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() && holds(q) {
					found = true
					return fs.SkipAll
				}
				return nil
			})
			if found {
				_ = os.RemoveAll(p)
			}
		}
	}
}

// sweepRunCredentials deletes every credential file a run left under the
// runner home: the service's start, when no job is running.
func sweepRunCredentials(home string) {
	for _, p := range runCredentialFiles(home) {
		_ = os.RemoveAll(p)
	}
}

// runhostJobPolicy writes the job's policy: the runhost's own, with the
// device's model, whose key the run reads from its environment, and the
// job's secrets file as its secret source.
func runhostJobPolicy(file, secrets string, j *job.Job) error {
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
	pol.SecretSource = policy.SecretSource{Mode: "envFile", EnvFile: secrets}
	// A target host's address is held to the person's own lists.
	pol.Hosts = policy.HostsPolicy{Allow: j.Run.HostsAllow, Deny: j.Run.HostsDeny}
	if err := pol.Validate(); err != nil {
		return fmt.Errorf("the run's model on this runhost: %w", err)
	}
	if err := policy.Save(file, pol); err != nil {
		return err
	}
	// The vendors' newest harness images the runhost built (its sign-in or
	// an earlier run) are recorded beside its own policy: the job's policy
	// takes the same record, so a run uses the image the sign-in was made in.
	if b, err := os.ReadFile(policy.NewestPathFor(path)); err == nil { //nolint:gosec // the runhost's own record
		return os.WriteFile(policy.NewestPathFor(file), b, 0o600)
	}
	return nil
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
	if len(req.Args) == 0 || len(req.Args) > 2 || !job.ValidID(req.Args[0]) || (len(req.Args) == 2 && req.Args[1] != "done" && req.Args[1] != "envelope") {
		return engine.ExitUsage, errors.New("collect takes a job's id, and then envelope for its envelope or done to delete it")
	}
	if runningJob(req.Args[0]) != nil {
		return runhostExitRunning, fmt.Errorf("job %s is still running: attach to it to follow it to its end", req.Args[0])
	}
	dir := filepath.Join(runhostJobsDir, req.Args[0])
	name := runhostSealedName
	if len(req.Args) == 2 {
		if req.Args[1] == "done" {
			return 0, os.RemoveAll(dir)
		}
		name = runhostEnvelopeName
	}
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // a job directory named by a checked id
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
