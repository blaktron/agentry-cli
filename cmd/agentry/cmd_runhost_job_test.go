package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry/pkg/attest"
)

const testDevice = "aaaaaaaaaaaaaaaa"

// fakeRunhostCLI stands in for the CLI a job runs: it records its argv and
// environment, writes an output folder with members and a file that is not
// one, narrates the model key (which must not reach the device), and prints
// an envelope.
const fakeRunhostCLI = `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  prev="$a"
done
printf '%s\n' "$@" > "$RUNHOST_TEST_DIR/argv"
env > "$RUNHOST_TEST_DIR/env"
mkdir -p "$out/reports" "$out/receipts" "$out/scratch"
echo '{"from":"the agent"}' > "$out/result.json"
echo report > "$out/reports/r.md"
echo receipt > "$out/receipts/events.ndjson"
echo leftover > "$out/scratch/x"
[ -f secrets.env ] && cp secrets.env "$RUNHOST_TEST_DIR/secrets.seen"
[ -f intake.json ] && cp intake.json "$RUNHOST_TEST_DIR/intake.seen"
[ -d files ] && (cd files && find . -type f | sort) > "$RUNHOST_TEST_DIR/files.seen"
[ -f known_hosts ] && cp known_hosts "$RUNHOST_TEST_DIR/known.seen"
[ -f hostkeys/server ] && cp hostkeys/server "$RUNHOST_TEST_DIR/hostkey.seen"
if [ -s secrets.env ]; then
  gw="$AGENTRY_RUNNER_HOME/agents/acme/demo/1.0.0-test/runs/r1/cfg/gateway"
  mkdir -p "$gw" && cp secrets.env "$gw/secrets.env"
  echo "the secrets are $(cat secrets.env)" >&2
fi
echo "the model key is $TEST_MODEL_API_KEY" >&2
[ -n "$RUNHOST_TEST_HANG" ] && { trap '' INT; sleep 30; }
echo "harness: working" >&2
printf '{"agentry":"1","status":"ok","exitCode":0,"runId":"run123","ref":"acme/demo@1.0.0","outDir":"%s","receipts":{"dir":"%s/receipts"},"result":"done"}\n' "$out" "$out"
exit "${RUNHOST_TEST_EXIT:-0}"
`

type runhostServer struct {
	dir  string
	seal *ecdh.PrivateKey
}

// newRunhostServer points the service's state at a temp directory, with one
// paired device and the fake CLI.
func newRunhostServer(t *testing.T) *runhostServer {
	t.Helper()
	isolateEnv(t)
	dir := t.TempDir()
	saved := []*string{&runhostJobsDir, &runhostStoreDir, &runhostStoreKey, &runhostDevicesFile}
	old := make([]string, len(saved))
	for i, p := range saved {
		old[i] = *p
	}
	oldCLI := runhostCLI
	t.Cleanup(func() {
		for i, p := range saved {
			*p = old[i]
		}
		runhostCLI = oldCLI
	})
	runhostJobsDir = filepath.Join(dir, "jobs")
	runhostStoreDir = filepath.Join(dir, "store", "releases")
	runhostStoreKey = filepath.Join(dir, "store", "store.key")
	runhostDevicesFile = filepath.Join(dir, "devices.json")
	t.Setenv(policy.HomeEnv, filepath.Join(dir, "runner"))
	path, err := policy.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	pol.Docker.Runtime, pol.Docker.GatewayRuntime = runhost.RunscRuntime, runhost.RunscUDSRuntime
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	seal, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devs := runhost.Devices{Devices: []runhost.Device{{ID: testDevice, SealKey: base64.StdEncoding.EncodeToString(seal.PublicKey().Bytes())}}}
	b, _ := json.Marshal(devs)
	if err := os.WriteFile(runhostDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "fake-agentry")
	if err := os.WriteFile(cli, []byte(fakeRunhostCLI), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	runhostCLI = func() (string, error) { return cli, nil }
	t.Setenv("RUNHOST_TEST_DIR", dir)
	return &runhostServer{dir: dir, seal: seal}
}

func unsignedJob(t *testing.T, id string) (*job.Job, []byte) {
	t.Helper()
	_, archive, digest := buildTestArchive(t, skillFiles("demo", ""))
	return &job.Job{V: job.Version, ID: id, Device: testDevice,
		Release: job.Release{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: digest, Size: int64(len(archive)),
			Registry: "https://registry.test", State: job.StateUnsigned},
		Run: job.Run{AllowUnsigned: true, MaxTurns: -1, MaxCost: -1, ModelKey: "sk-test-model-key",
			Model: job.Model{Provider: "anthropic", Name: "claude-x", APIKeyEnv: "TEST_MODEL_API_KEY", ProxyAllow: []string{"api.anthropic.com:443"}}},
	}, archive
}

type jobAnswer struct {
	release string
	out     []string
	result  runhostJobResult
	code    int
	err     error
}

func sendJob(t *testing.T, j *job.Job, archive []byte) jobAnswer {
	t.Helper()
	return sendJobCtx(t, context.Background(), j, archive)
}

func sendJobCtx(t *testing.T, ctx context.Context, j *job.Job, archive []byte) jobAnswer {
	t.Helper()
	var in bytes.Buffer
	if err := job.Write(&in, j); err != nil {
		t.Fatal(err)
	}
	in.Write(archive)
	var out bytes.Buffer
	code, err := runhostJobVerb(ctx, runhost.Request{Verb: "job"}, &in, runhost.NewFrameWriter(&out))
	a := jobAnswer{code: code, err: err}
	if _, rerr := runhost.ReadFrames(io.MultiReader(&out, strings.NewReader(`{"type":"exit"}`+"\n")), func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameRelease:
			a.release = f.Text
		case runhost.FrameOut:
			a.out = append(a.out, f.Text)
		case runhost.FrameResult:
			_ = json.Unmarshal(f.Result, &a.result)
		}
		return nil
	}); rerr != nil {
		t.Fatal(rerr)
	}
	return a
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			r, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(got)
	return got
}

// A job's secrets leave no file on the runhost however its run ends: when it
// succeeds, fails, or is cut off by a dropped session and killed past the
// grace, the job's secrets file and the run's own copy go with the run; and a
// copy a killed service left is swept at its next start (I3).
func TestARunhostJobsSecretsLeaveNoFileBehind(t *testing.T) {
	srv := newRunhostServer(t)
	home, _ := policy.Home()
	oldGrace := runhostCancelGrace
	runhostCancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { runhostCancelGrace = oldGrace })
	gateway := filepath.Join(home, "agents", "acme", "demo", "1.0.0-test", "runs", "r1", "cfg", "gateway", "secrets.env")
	leftover := func() []string {
		var found []string
		for _, root := range []string{runhostJobsDir, filepath.Join(home, "agents")} {
			_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					if b, _ := os.ReadFile(p); strings.Contains(string(b), "tok-secret-value-2") {
						found = append(found, p)
					}
				}
				return nil
			})
		}
		return found
	}
	for i, end := range []struct {
		name string
		exit string
		hang bool
	}{{"success", "0", false}, {"failure", "12", false}, {"a dropped session", "0", true}} {
		t.Setenv("RUNHOST_TEST_EXIT", end.exit)
		hang := ""
		if end.hang {
			hang = "1"
		}
		t.Setenv("RUNHOST_TEST_HANG", hang)
		j, archive := unsignedJob(t, fmt.Sprintf("%016x", 0xabc0+i))
		j.Run.Secrets = map[string]string{"API_TOKEN": "tok-secret-value-2"}
		ctx, cancel := context.WithCancel(context.Background())
		if end.hang {
			go func() { time.Sleep(time.Second); cancel() }()
		}
		a := sendJobCtx(t, ctx, j, archive)
		cancel()
		if !end.hang && a.err != nil {
			t.Fatalf("%s: %v", end.name, a.err)
		}
		if seen, _ := os.ReadFile(filepath.Join(srv.dir, "secrets.seen")); string(seen) != "API_TOKEN=tok-secret-value-2\n" {
			t.Errorf("%s: the run read %q", end.name, seen)
		}
		for _, l := range a.out {
			if strings.Contains(l, "tok-secret-value-2") {
				t.Errorf("%s: narration carried the secret: %q", end.name, l)
			}
		}
		if found := leftover(); len(found) > 0 {
			t.Errorf("%s: the secret is still on the runhost in %v", end.name, found)
		}
		_ = os.Remove(filepath.Join(srv.dir, "secrets.seen"))
	}
	// A killed service's leftovers: the run's copy is swept at the start.
	if err := os.MkdirAll(filepath.Dir(gateway), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gateway, []byte("API_TOKEN=tok-secret-value-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sweepRunCredentials(home)
	if found := leftover(); len(found) > 0 {
		t.Errorf("the start's sweep left %v", found)
	}
}

// A job runs its release as a local run of the runhost's CLI, never a
// hosted one; its output comes back sealed to the device with only its
// members; and nothing but the sealed output and the job's record is left.
func TestARunhostJobRunsAndLeavesOnlyItsSealedOutput(t *testing.T) {
	srv := newRunhostServer(t)
	t.Setenv("AGENTRY_HOSTED", "1")
	t.Setenv("AGENTRY_RUN_GRANT", "grant")
	t.Setenv("AGENTRY_JOB_TOKEN", "token")
	t.Setenv("TEST_MODEL_API_KEY", "the service's own, never the run's")
	j, archive := unsignedJob(t, "0123456789abcdef")
	a := sendJob(t, j, archive)
	if a.err != nil || a.code != 0 {
		t.Fatalf("job: %d %v (%q)", a.code, a.err, a.out)
	}
	if a.release != runhost.ReleaseNeeded || a.result.Job != j.ID || a.result.ExitCode != 0 || !strings.Contains(string(a.result.Envelope), `"runId":"run123"`) {
		t.Fatalf("answer: %+v", a)
	}
	for _, l := range a.out {
		if strings.Contains(l, "sk-test-model-key") {
			t.Errorf("the model key reached the device: %q", l)
		}
	}

	env, _ := os.ReadFile(filepath.Join(srv.dir, "env"))
	for _, k := range []string{"AGENTRY_HOSTED=", "AGENTRY_RUN_GRANT=", "AGENTRY_JOB_TOKEN="} {
		if strings.Contains(string(env), "\n"+k) || strings.HasPrefix(string(env), k) {
			t.Errorf("the run's environment carries %s", k)
		}
	}
	if !strings.Contains(string(env), "TEST_MODEL_API_KEY=sk-test-model-key\n") || strings.Contains(string(env), "service's own") {
		t.Errorf("the run's model key: %s", env)
	}
	argv, _ := os.ReadFile(filepath.Join(srv.dir, "argv"))
	for _, want := range []string{"run\nacme/demo@1.0.0\n--installed\n--pin\n" + j.Release.Digest + "\n", "--allow-unsigned\n", "--yes\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	for _, never := range []string{"--review-inputs", "--connections-file", "--destinations-file"} {
		if strings.Contains(string(argv), never) {
			t.Errorf("argv carries %s", never)
		}
	}

	jobDir := filepath.Join(runhostJobsDir, j.ID)
	if got := listFiles(t, jobDir); strings.Join(got, " ") != "job.json output.sealed" {
		t.Errorf("the job left %v", got)
	}
	sealed, _ := os.ReadFile(filepath.Join(jobDir, "output.sealed"))
	if bytes.Contains(sealed, []byte("report")) || bytes.Contains(sealed, []byte("the agent")) {
		t.Error("the sealed output holds plaintext")
	}

	// Collected, opened with the device's key: the members, and nothing else.
	var col bytes.Buffer
	if code, err := runhostCollectVerb(context.Background(), runhost.Request{Args: []string{j.ID}}, nil, runhost.NewFrameWriter(&col)); code != 0 || err != nil {
		t.Fatalf("collect: %d %v", code, err)
	}
	var box bytes.Buffer
	_, _ = runhost.ReadFrames(io.MultiReader(&col, strings.NewReader(`{"type":"exit"}`+"\n")), func(f runhost.Frame) error {
		box.Write(f.Data)
		return nil
	})
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(output.Open(pw, &box, testDevice, srv.seal, j.ID)) }()
	dst := filepath.Join(t.TempDir(), "out")
	if err := output.Unpack(pr, dst); err != nil {
		t.Fatal(err)
	}
	if got := listFiles(t, dst); strings.Join(got, " ") != "receipts/events.ndjson reports/r.md result.json" {
		t.Errorf("collected %v", got)
	}

	// The same release again is not sent: the store holds it.
	j2, _ := unsignedJob(t, "fedcba9876543210")
	if a := sendJob(t, j2, nil); a.err != nil || a.release != runhost.ReleaseHeld || a.result.Job != j2.ID {
		t.Fatalf("second job: %+v", a)
	}

	if code, err := runhostCollectVerb(context.Background(), runhost.Request{Args: []string{j.ID, "done"}}, nil, runhost.NewFrameWriter(io.Discard)); code != 0 || err != nil {
		t.Fatalf("collect done: %d %v", code, err)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Error("collect done left the job")
	}
	if code, _ := runhostCollectVerb(context.Background(), runhost.Request{Args: []string{"../../etc"}}, nil, runhost.NewFrameWriter(io.Discard)); code != 2 {
		t.Error("collect took a path for a job id")
	}
}

// The names the job gives the release are held to the archive's own, and a
// first release from a registry pins its keys only once it verifies.
func TestTheRunhostHoldsTheJobsNamesToTheArchive(t *testing.T) {
	newRunhostServer(t)
	j, archive := unsignedJob(t, "0123456789abcdef")
	j.Release.Name = "other"
	if a := sendJob(t, j, archive); a.code != 10 || a.err == nil || !strings.Contains(a.err.Error(), "the archive is acme/demo@1.0.0") {
		t.Errorf("a renamed release: %+v", a)
	}
	s := newTestSigner(t)
	_, archive2, digest := buildTestArchive(t, skillFiles("demo", ""))
	rel := job.Release{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: digest, Size: int64(len(archive2)),
		Registry: "https://pins.test", State: job.StateVerified, Attestation: s.envelope(t, "sha256:"+strings.Repeat("0", 64)), Keys: s.keys}
	if _, err := runhostVerifyRelease(rel, archive2); err == nil {
		t.Fatal("an envelope over another digest verified")
	}
	if pinned, _ := trust.LoadPinned(rel.Registry); len(pinned.Keys) != 0 {
		t.Error("a release that did not verify pinned its keys")
	}
}

// A policy changed since setup to run containers outside gVisor runs nothing.
func TestARunhostJobNeedsTheGVisorPolicy(t *testing.T) {
	srv := newRunhostServer(t)
	path, _ := policy.DefaultPath()
	pol, _, _ := policy.Load(path)
	pol.Docker.Runtime, pol.Docker.GatewayRuntime = "", ""
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	j, archive := unsignedJob(t, "0123456789abcdef")
	if a := sendJob(t, j, archive); a.err == nil || !strings.Contains(a.err.Error(), "runsc") {
		t.Errorf("a policy without gVisor: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(srv.dir, "argv")); err == nil {
		t.Error("a run started")
	}
}

// A run that fails still has its output sealed and its plaintext removed,
// and its exit code is the job's.
func TestAFailedRunhostRunIsSealedToo(t *testing.T) {
	newRunhostServer(t)
	t.Setenv("RUNHOST_TEST_EXIT", "14")
	j, archive := unsignedJob(t, "0123456789abcdef")
	a := sendJob(t, j, archive)
	if a.code != 14 || a.result.ExitCode != 14 {
		t.Fatalf("answer: %+v", a)
	}
	if got := listFiles(t, filepath.Join(runhostJobsDir, j.ID)); strings.Join(got, " ") != "job.json output.sealed" {
		t.Errorf("the job left %v", got)
	}
}

// An archive that is not the job's digest, a stored release changed at rest,
// and a device that is not paired are each refused before anything runs.
func TestARunhostJobRefusesWhatItCannotVerify(t *testing.T) {
	srv := newRunhostServer(t)
	j, archive := unsignedJob(t, "0123456789abcdef")
	bad := append([]byte{}, archive...)
	bad[len(bad)/2] ^= 1
	if a := sendJob(t, j, bad); a.code != 10 || a.err == nil || !strings.Contains(a.err.Error(), "digest mismatch") {
		t.Errorf("a changed archive: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(srv.dir, "argv")); err == nil {
		t.Fatal("a refused job ran")
	}
	if a := sendJob(t, j, archive); a.code != 0 {
		t.Fatalf("job: %+v", a)
	}
	entry := filepath.Join(runhostStoreDir, strings.TrimPrefix(j.Release.Digest, "sha256:")+".release")
	b, _ := os.ReadFile(entry)
	b[len(b)-1] ^= 1
	if err := os.WriteFile(entry, b, 0o600); err != nil {
		t.Fatal(err)
	}
	j2, _ := unsignedJob(t, "fedcba9876543210")
	if a := sendJob(t, j2, nil); a.code != 10 || a.err == nil {
		t.Errorf("a changed stored release: %+v", a)
	}
	j3, _ := unsignedJob(t, "1111111111111111")
	j3.Device = "bbbbbbbbbbbbbbbb"
	if a := sendJob(t, j3, archive); a.code != 2 {
		t.Errorf("an unpaired device: %+v", a)
	}
}

type testSigner struct {
	key  *ecdsa.PrivateKey
	keys trust.KeySet
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return &testSigner{key: k, keys: trust.KeySet{Keys: []trust.SigningKey{{KeyID: attest.KeyIDFor(&k.PublicKey), Algorithm: "ecdsa-p256", PublicKey: pub}}}}
}

func (s *testSigner) envelope(t *testing.T, digest string) []byte {
	t.Helper()
	stmt := attest.BuildStatement("https://registry.test", "acme/demo", "1.0.0", digest, "cafe", "src", attest.ScanRunRef{ID: "s"}, 1)
	payload, _ := json.Marshal(stmt)
	sum := sha256.Sum256(attest.PAE(attest.PayloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(attest.DSSEEnvelope{PayloadType: attest.PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []attest.DSSESignature{{KeyID: s.keys.Keys[0].KeyID, Sig: base64.StdEncoding.EncodeToString(sig)}}})
	return b
}

// A verified release is verified again on the runhost against its own pins:
// the device's keys at the first release from a registry, the runhost's own
// after, which another key set cannot replace.
func TestTheRunhostVerifiesAgainstItsOwnPins(t *testing.T) {
	isolateEnv(t)
	s, other := newTestSigner(t), newTestSigner(t)
	_, archive, digest := buildTestArchive(t, skillFiles("demo", ""))
	rel := job.Release{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: digest, Size: int64(len(archive)),
		Registry: "https://registry.test", State: job.StateVerified, Attestation: s.envelope(t, digest), Keys: s.keys}
	meta, err := runhostVerifyRelease(rel, archive)
	if err != nil || !meta.Attestation.Verified || meta.Attestation.KeyID != s.keys.Keys[0].KeyID {
		t.Fatalf("verified: %+v %v", meta, err)
	}
	if pinned, _ := trust.LoadPinned(rel.Registry); !trust.SameKeyIDs(pinned, s.keys) {
		t.Fatal("the device's keys were not pinned")
	}
	// Another key set, even with an envelope it signed, is not taken.
	swapped := rel
	swapped.Keys, swapped.Attestation = other.keys, other.envelope(t, digest)
	if _, err := runhostVerifyRelease(swapped, archive); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("another key set: %v", err)
	}
	// An envelope for another archive, with the pinned keys.
	wrong := rel
	wrong.Attestation = s.envelope(t, "sha256:"+strings.Repeat("0", 64))
	if _, err := runhostVerifyRelease(wrong, archive); err == nil {
		t.Error("an envelope over another digest was taken")
	}
}

// --installed runs only an exact version laid down at --pin, and asks no
// registry.
func TestRunInstalledTakesOnlyWhatIsLaidDown(t *testing.T) {
	isolateEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	t.Setenv("AGENTRY_REGISTRY", "http://127.0.0.1:1")
	home, _ := policy.Home()
	_, archive, digest := buildTestArchive(t, skillFiles("demo", ""))
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: digest, Source: "registry"}
	meta.Attestation.State = trust.StateUnsigned
	if err := runhostLayDown(home, meta, archive); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--installed", "--pin", digest, "--allow-unsigned", "--dry-run"); err != nil {
		t.Fatalf("the laid-down release: %v\n%s", err, stderr)
	}
	for name, args := range map[string][]string{
		"no pin":         {"run", "acme/demo@1.0.0", "--installed", "--allow-unsigned", "--dry-run"},
		"a range":        {"run", "acme/demo@^1.0.0", "--installed", "--pin", digest, "--allow-unsigned", "--dry-run"},
		"another digest": {"run", "acme/demo@1.0.0", "--installed", "--pin", "sha256:" + strings.Repeat("1", 64), "--allow-unsigned", "--dry-run"},
		"no consent":     {"run", "acme/demo@1.0.0", "--installed", "--pin", digest, "--dry-run"},
	} {
		if _, _, err := runCLI(t, nil, args...); err == nil {
			t.Errorf("%s: ran", name)
		}
	}
}
