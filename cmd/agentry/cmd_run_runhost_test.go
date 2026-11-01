package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry/pkg/manifest"
)

// startTestRunhost is a runhost in this process: an SSH server that admits
// only the device's run key and relays each session through runhost.Serve
// to a service answering version, job and collect with the real verbs.
func startTestRunhost(t *testing.T, runKey ssh.PublicKey) (hostKey ssh.PublicKey, port int) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(sockDir, 0o700)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	svc := &runhost.Service{Socket: filepath.Join(sockDir, "s.sock"), Handlers: map[string]runhost.Handler{
		"version": func(_ context.Context, _ runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			return 0, fw.Result(map[string]string{"version": version.Version})
		},
		"job":     runhostJobVerb,
		"collect": runhostCollectVerb,
	}}
	l, err := svc.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = svc.Serve(ctx, l) }()

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(runKey.Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, errors.New("not the run key")
	}}
	cfg.AddHostKey(signer)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tl.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go serveTestSSH(ctx, c, cfg, svc.Socket)
		}
	}()
	return signer.PublicKey(), tl.Addr().(*net.TCPAddr).Port
}

func serveTestSSH(ctx context.Context, c net.Conn, cfg *ssh.ServerConfig, socket string) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				if r.Type != "exec" || len(r.Payload) < 4 {
					_ = r.Reply(false, nil)
					continue
				}
				_ = r.Reply(true, nil)
				command := string(r.Payload[4 : 4+binary.BigEndian.Uint32(r.Payload)])
				go func() {
					code := runhost.Serve(ctx, command, ch, ch, socket)
					_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(code))) //nolint:gosec // an exit status
					_ = ch.Close()
				}()
			}
		}()
	}
}

// agentry run --on runs a published release on the runhost and opens its
// sealed output here: the result, the reports and the receipts, the
// envelope naming the runhost, and nothing left on the runhost after.
func TestRunOnARunhostEndToEnd(t *testing.T) {
	srv := newRunhostServer(t)
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	home, _ := policy.Home()
	path, _ := policy.DefaultPath()
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model = policy.Model{Provider: "anthropic", Name: "claude-x", APIKeyEnv: "TEST_MODEL_API_KEY", ProxyAllow: []string{"api.anthropic.com:443"}}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MODEL_API_KEY", "sk-test-model-key")

	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	dk, err := runhost.NewDeviceKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := runhost.SaveKeys("lab", dk); err != nil {
		t.Fatal(err)
	}
	devs := runhost.Devices{Devices: []runhost.Device{{ID: testDevice, SealKey: dk.SealPublic()}}}
	b, _ := json.Marshal(devs)
	if err := os.WriteFile(runhostDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := dk.Signer()
	hostKey, port := startTestRunhost(t, signer.PublicKey())
	recs := &runhost.Records{Runhosts: []runhost.Record{{Name: "lab", Host: "127.0.0.1", Port: port,
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))), HostKeyFingerprint: ssh.FingerprintSHA256(hostKey),
		DeviceID: testDevice, CLIVersion: version.Version}}}
	if err := recs.Save(home); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out")
	stdout, stderr, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", out, "--output-format", "json")
	if err != nil {
		t.Fatalf("run --on: %v\n%s", err, stderr)
	}
	var env receipt.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%s", err, stdout)
	}
	if env.Runhost == nil || env.Runhost.Name != "lab" || env.Runhost.HostKeyFingerprint != ssh.FingerprintSHA256(hostKey) || env.OutDir != out || env.RunID != "run123" {
		t.Fatalf("envelope: %+v", env)
	}
	if !strings.Contains(stderr, "lab: harness: working") || strings.Contains(stderr+stdout, "sk-test-model-key") {
		t.Errorf("narration: %s", stderr)
	}
	if got := listFiles(t, out); strings.Join(got, " ") != "receipts/events.ndjson reports/r.md result.json" {
		t.Errorf("the output folder holds %v", got)
	}
	rj, _ := os.ReadFile(filepath.Join(out, "result.json"))
	if !strings.Contains(string(rj), `"runhost"`) || strings.Contains(string(rj), "the agent") {
		t.Errorf("result.json is not this device's envelope: %s", rj)
	}
	task, _ := os.ReadFile(filepath.Join(srv.dir, "argv"))
	if !strings.Contains(string(task), "--input\n") {
		t.Errorf("argv: %s", task)
	}
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs after collection", len(entries))
	}
	// A run that fails on the runhost fails here with its exit, its output
	// collected all the same.
	t.Setenv("RUNHOST_TEST_EXIT", "14")
	failOut := filepath.Join(t.TempDir(), "fail")
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", failOut)
	wantClassified(t, err, 14)
	if got := listFiles(t, failOut); strings.Join(got, " ") != "receipts/events.ndjson reports/r.md result.json" {
		t.Errorf("the failed run's folder holds %v", got)
	}
	t.Setenv("RUNHOST_TEST_EXIT", "0")
	// A job the runhost refuses keeps the runhost's class: a device it has
	// not paired is a usage refusal, not a gateway failure.
	unpaired, _ := json.Marshal(runhost.Devices{Devices: []runhost.Device{{ID: "bbbbbbbbbbbbbbbb", SealKey: dk.SealPublic()}}})
	if err := os.WriteFile(runhostDevicesFile, unpaired, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", filepath.Join(t.TempDir(), "refused"))
	wantClassified(t, err, 2)
	if !strings.Contains(err.Error(), "refused the run (exit 2)") {
		t.Errorf("refusal: %v\n%s", err, stderr)
	}
	if err := os.WriteFile(runhostDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The release now in the runhost's store is not sent again.
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", filepath.Join(t.TempDir(), "out2")); err != nil {
		t.Fatalf("second run: %v\n%s", err, stderr)
	}
}

// A refusal keeps the class the runhost gave it when that is a usage,
// verify or policy refusal; any other answer, and a session that ended
// without one, is the gateway's.
func TestRunOnSendClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&runhostRefusal{Code: 2, Reason: "x"}, 2},
		{&runhostRefusal{Code: 10, Reason: "x"}, 10},
		{&runhostRefusal{Code: 11, Reason: "x"}, 11},
		{&runhostRefusal{Code: 1, Reason: "x"}, 13},
		{&runhostRefusal{Code: 14, Reason: "x"}, 13},
		{fmt.Errorf("wrapped: %w", &runhostRefusal{Code: 10, Reason: "x"}), 10},
		{io.ErrUnexpectedEOF, 13},
		{errors.New("the session ended without an answer"), 13},
	} {
		if got := runOnSendClass(tc.err); got != tc.want {
			t.Errorf("%v: class %d, want %d", tc.err, got, tc.want)
		}
	}
}

// A runhost the device does not know, a flag the job does not carry, and a
// model a runhost cannot be handed are refused before anything is sent.
func TestRunOnRefusesWhatItDoesNotCarry(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "nowhere"); err == nil || !strings.Contains(err.Error(), "no runhost is named") {
		t.Errorf("an unknown runhost: %v", err)
	}
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "lab", "--file", "x"); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Errorf("--file: %v", err)
	}
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "lab", "--adapter", "claude"); err == nil || !strings.Contains(err.Error(), "--adapter") {
		t.Errorf("--adapter: %v", err)
	}
	if what := runOnRefusedRelease(&manifest.Manifest{Capabilities: manifest.Capabilities{Secrets: []string{"TOKEN"}}}); what != "secrets" {
		t.Errorf("secrets: %q", what)
	}
	for name, m := range map[string]policy.Model{
		"credits": {Provider: policy.ProviderAgentry, Name: "x"},
		"sign-in": {Provider: "anthropic", Name: "x", APIKeyKind: policy.KeyKindSubscription},
		"no key":  {Provider: "anthropic", Name: "x", APIKeyEnv: "UNSET_TEST_KEY_VAR"},
	} {
		if _, err := runOnModelKey(&policy.Policy{Model: m}); err == nil {
			t.Errorf("%s: a key was taken", name)
		}
	}
}
