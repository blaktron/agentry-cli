package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
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
		"attach":  runhostAttachVerb,
		"cancel":  runhostCancelVerb,
		"collect": runhostCollectVerb,
		"status": func(_ context.Context, _ runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			return 0, fw.Result(runhostStatus{Name: "lab", Jobs: runhostJobs()})
		},
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
	var connsMu sync.Mutex
	var conns []net.Conn
	dropTestRunhost = func() {
		connsMu.Lock()
		defer connsMu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
	}
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, c)
			connsMu.Unlock()
			go serveTestSSH(ctx, c, cfg, svc.Socket)
		}
	}()
	return signer.PublicKey(), tl.Addr().(*net.TCPAddr).Port
}

// dropTestRunhost cuts every connection the test runhost holds, as a laptop
// going to sleep does.
var dropTestRunhost func()

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

// newRunOnLab is a runhost named lab in this process, paired with this
// device, whose policy names a model with its key in the environment.
func newRunOnLab(t *testing.T) (*runhostServer, string) {
	t.Helper()
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

	dk, err := runhost.NewDeviceKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := runhost.SaveKeys("lab", dk); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(runhost.Devices{Devices: []runhost.Device{{ID: testDevice, SealKey: dk.SealPublic()}}})
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
	return srv, home
}

// A run on a runhost carries its secrets, its intake and its files: the
// runhost's run reads them from its job directory, the narration and the
// envelope are masked of every secret, and once the run ends nothing of them
// is left on the runhost, the run's own copy of the secrets included (I3).
func TestRunOnCarriesTheWholeJob(t *testing.T) {
	srv, home := newRunOnLab(t)
	t.Setenv("API_TOKEN", "tok-secret-value-1")

	fr := newFakeRegistry(t)
	caps := "capabilities:\n  secrets: [API_TOKEN, SPARE_TOKEN]\n  optionalSecrets: [SPARE_TOKEN]\n" +
		"intake:\n  - id: topic\n    prompt: The topic\n    type: text\n    required: true\n  - id: doc\n    prompt: The document\n    type: file\n    required: true\n"
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", caps))

	in := t.TempDir()
	input := filepath.Join(in, "notes.txt")
	doc := filepath.Join(in, "brief.md")
	_ = os.WriteFile(input, []byte("input text"), 0o600)
	_ = os.WriteFile(doc, []byte("brief text"), 0o600)
	_ = os.MkdirAll(filepath.Join(in, "folder", "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(in, "folder", "sub", "a.txt"), []byte("a"), 0o600)

	out := filepath.Join(t.TempDir(), "out")
	stdout, stderr, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", out, "--output-format", "json",
		"--file", input, "--file", filepath.Join(in, "folder"), "--intake", "topic=birds", "--intake", "doc="+doc)
	if err != nil {
		t.Fatalf("run --on: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout+stderr, "tok-secret-value-1") || strings.Contains(stdout+stderr, "sk-test-model-key") {
		t.Errorf("a secret reached the device's narration or envelope:\n%s\n%s", stderr, stdout)
	}
	if seen, _ := os.ReadFile(filepath.Join(srv.dir, "secrets.seen")); string(seen) != "API_TOKEN=tok-secret-value-1\n" {
		t.Errorf("the run's secrets file held %q", seen)
	}
	var answers map[string]string
	raw, _ := os.ReadFile(filepath.Join(srv.dir, "intake.seen"))
	if err := json.Unmarshal(raw, &answers); err != nil || answers["topic"] != "birds" || !strings.HasSuffix(answers["doc"], "/files/intake/doc/brief.md") {
		t.Errorf("the run's intake answers: %s", raw)
	}
	if files, _ := os.ReadFile(filepath.Join(srv.dir, "files.seen")); string(files) != "./inputs/folder/sub/a.txt\n./inputs/notes.txt\n./intake/doc/brief.md\n" {
		t.Errorf("the run's files: %q", files)
	}
	argv, _ := os.ReadFile(filepath.Join(srv.dir, "argv"))
	for _, want := range []string{"--registry\n" + fr.url() + "\n", "--intake-file\n", "/files/inputs/notes.txt\n", "/files/inputs/folder\n", "--no-voice\n", "--no-destination\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "birds") || strings.Contains(string(argv), "tok-secret") {
		t.Errorf("an answer or a secret is on the run's command line:\n%s", argv)
	}
	// Nothing of the job is left on the runhost: the job directory went at
	// collection, and the run's own copy of the secrets with the run.
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs", len(entries))
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "acme", "demo", "1.0.0-test", "runs", "r1", "cfg", "gateway", "secrets.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run's copy of the secrets is still there: %v", err)
	}

	// A required secret this device has no value for is refused before
	// anything is sent.
	t.Setenv("API_TOKEN", "")
	_ = os.Remove(filepath.Join(srv.dir, "argv"))
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--intake", "topic=birds", "--intake", "doc="+doc)
	wantClassified(t, err, 11)
	if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.dir, "argv")); err == nil {
		t.Error("a run started on the runhost without its secret")
	}
}

// A release with a target host runs on a runhost with the host's sign-in and
// the keys this device has accepted for it: the runhost resolves the address
// and checks the key against those lines alone. A host this device has
// never accepted a key for is refused before anything is sent.
func TestRunOnCarriesATargetHost(t *testing.T) {
	srv, home := newRunOnLab(t)
	fr := newFakeRegistry(t)
	caps := "capabilities:\n  exec:\n    commands: [git]\n  targetHosts: [server]\nintake:\n  - id: server\n    prompt: The server\n    type: host\n"
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", caps))

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	hostPub, _, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewPublicKey(hostPub)
	line := "box.test " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk)))

	run := func(answer string) (string, error) {
		_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0",
			"--on", "lab", "--allow-unsigned", "--yes", "--out", filepath.Join(t.TempDir(), "out"),
			"--intake", "server="+answer, "--host-key", "server="+keyFile)
		return stderr, err
	}
	// Not yet accepted here: refused, and nothing reaches the runhost.
	if _, err := run("root@box.test"); err == nil || !strings.Contains(err.Error(), "has not accepted a host key for box.test") {
		t.Fatalf("an unknown host: %v", err)
	}
	wantClassified(t, func() error { _, err := run("root@box.test"); return err }(), 11)
	if _, err := os.Stat(filepath.Join(srv.dir, "argv")); err == nil {
		t.Fatal("a run started on the runhost for a host this device does not know")
	}

	if err := os.WriteFile(filepath.Join(home, "known_hosts"), []byte("other.test "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk)))+"\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, err := run("root@box.test")
	if err != nil {
		t.Fatalf("run --on: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "the runhost resolves its address") {
		t.Errorf("the device did not say where the address is resolved:\n%s", stderr)
	}
	if known, _ := os.ReadFile(filepath.Join(srv.dir, "known.seen")); string(known) != line+"\n" {
		t.Errorf("the run's known_hosts: %q", known)
	}
	if key, _ := os.ReadFile(filepath.Join(srv.dir, "hostkey.seen")); string(key) != string(pem.EncodeToMemory(block)) {
		t.Error("the run's key file is not the device's key")
	}
	argv, _ := os.ReadFile(filepath.Join(srv.dir, "argv"))
	for _, want := range []string{"--host-known-hosts\n", "--host-key\nserver=", "/hostkeys/server\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	var answers map[string]string
	raw, _ := os.ReadFile(filepath.Join(srv.dir, "intake.seen"))
	if json.Unmarshal(raw, &answers) != nil || answers["server"] != "root@box.test" {
		t.Errorf("the run's intake: %s", raw)
	}
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs", len(entries))
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
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "lab", "--keep-run-dir"); err == nil || !strings.Contains(err.Error(), "--keep-run-dir") {
		t.Errorf("--keep-run-dir: %v", err)
	}
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "lab", "--host-key-pin", "h=SHA256:x"); err == nil || !strings.Contains(err.Error(), "--host-key-pin") {
		t.Errorf("--host-key-pin: %v", err)
	}
	if _, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--on", "lab", "--adapter", "claude"); err == nil || !strings.Contains(err.Error(), "--adapter") {
		t.Errorf("--adapter: %v", err)
	}
	// A model paid with Agentry credits carries no key: the runhost pays
	// with its own credits token.
	if key, err := runOnModelKey(&policy.Policy{Model: policy.Model{Provider: policy.ProviderAgentry, Name: "x"}}); err != nil || key != "" {
		t.Errorf("credits: %q, %v", key, err)
	}
	for name, m := range map[string]policy.Model{
		"sign-in": {Provider: "anthropic", Name: "x", APIKeyKind: policy.KeyKindSubscription},
		"no key":  {Provider: "anthropic", Name: "x", APIKeyEnv: "UNSET_TEST_KEY_VAR"},
	} {
		if _, err := runOnModelKey(&policy.Policy{Model: m}); err == nil {
			t.Errorf("%s: a key was taken", name)
		}
	}
}

// A run detached on a runhost goes on there: jobs lists it, collect refuses
// it while it runs, attach follows it to its end and opens its output here,
// and nothing of it is left on either side after. A cancelled job tears down
// and its output so far is collected all the same.
func TestRunOnDetachAttachCancelCollect(t *testing.T) {
	newRunOnLab(t)
	home, _ := policy.Home()
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	oldGrace := runhostCancelGrace
	runhostCancelGrace = 200 * time.Millisecond
	t.Cleanup(func() { runhostCancelGrace = oldGrace })

	detach := func() string {
		t.Helper()
		stdout, stderr, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
			"--on", "lab", "--allow-unsigned", "--yes", "--detach", "--output-format", "json")
		if err != nil {
			t.Fatalf("run --detach: %v\n%s", err, stderr)
		}
		var d struct{ Job, Runhost, State string }
		if err := json.Unmarshal([]byte(stdout), &d); err != nil || d.Runhost != "lab" || d.State != "running" || d.Job == "" {
			t.Fatalf("detached: %s (%v)", stdout, err)
		}
		return d.Job
	}

	t.Setenv("RUNHOST_TEST_SLEEP", "3")
	id := detach()
	stdout, _, err := runCLI(t, nil, "--json", "runhost", "jobs", "lab")
	if err != nil || !strings.Contains(stdout, id) {
		t.Fatalf("jobs: %v\n%s", err, stdout)
	}
	if _, _, err := runCLI(t, nil, "runhost", "collect", "lab", id); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("collect while running: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out")
	_, stderr, err := runCLI(t, nil, "runhost", "attach", "lab", id, "--out", out)
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "lab: harness: working") {
		t.Errorf("attach did not follow the narration:\n%s", stderr)
	}
	if got := listFiles(t, out); strings.Join(got, " ") != "receipts/events.ndjson reports/r.md result.json" {
		t.Errorf("the output folder holds %v", got)
	}
	rj, _ := os.ReadFile(filepath.Join(out, "result.json"))
	if !strings.Contains(string(rj), `"runhost"`) || !strings.Contains(string(rj), id) {
		t.Errorf("result.json: %s", rj)
	}
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs after collection", len(entries))
	}
	if jobs, _ := loadLocalJobs(home); len(jobs) != 0 {
		t.Errorf("the device kept %v", jobs)
	}

	// Cancelled mid-run: it ends, and what it wrote is collected.
	t.Setenv("RUNHOST_TEST_SLEEP", "30")
	id = detach()
	stdout, stderr, err = runCLI(t, nil, "runhost", "cancel", "lab", id)
	if err != nil || !strings.Contains(stdout, "has ended") {
		t.Fatalf("cancel: %v\n%s%s", err, stdout, stderr)
	}
	t.Setenv("RUNHOST_TEST_SLEEP", "")
	if _, _, err := runCLI(t, nil, "runhost", "collect", "lab", id, "--out", filepath.Join(t.TempDir(), "cancelled")); err == nil {
		t.Error("a cancelled run collected as a success")
	}
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs after the cancelled one was collected", len(entries))
	}
	if _, _, err := runCLI(t, nil, "runhost", "attach", "lab", "0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "holds no job") {
		t.Errorf("attach to no job: %v", err)
	}
}

// A session on a runhost is driven over the job's channel by the session
// protocol on this device: three turns and a file added reach the runhost's
// session, its events come back on this device's descriptor 3, and the
// session's output and envelope are collected when it ends.
func TestRunOnSessionOverTheChannel(t *testing.T) {
	srv, _ := newRunOnLab(t)
	fr := newFakeRegistry(t)
	files := skillFiles("demo", "")
	files["agent.yaml"] = strings.Replace(files["agent.yaml"], "modes: [autonomous]", "modes: [interactive]", 1)
	fr.addAgent("acme", "demo", "interactive", "s").addVersion(t, "1.0.0", files)
	events := &turnWatcher{answered: make(chan struct{}, 8), added: make(chan struct{}, 8)}
	withProtocolEvents(t, events, nil)
	inputs := t.TempDir()
	if err := os.WriteFile(filepath.Join(inputs, "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A driver waits for each answer before the next turn, as a person at
	// a chat does.
	cmdsR, cmdsW := io.Pipe()
	go func() {
		defer func() { _ = cmdsW.Close() }()
		wait := func(ch chan struct{}) {
			select {
			case <-ch:
			case <-time.After(20 * time.Second):
			}
		}
		_, _ = io.WriteString(cmdsW, `{"send":{"text":"one"}}`+"\n")
		wait(events.answered)
		_, _ = io.WriteString(cmdsW, `{"add":{"path":"notes.txt"}}`+"\n")
		wait(events.added)
		_, _ = io.WriteString(cmdsW, `{"send":{"text":"two"}}`+"\n")
		wait(events.answered)
		_, _ = io.WriteString(cmdsW, `{"send":{"text":"three"}}`+"\n")
		wait(events.answered)
		_, _ = io.WriteString(cmdsW, `{"end":{"finish":false}}`+"\n")
	}()
	out := filepath.Join(t.TempDir(), "out")
	stdout, stderr, err := runCLI(t, cmdsR, "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--mode", "interactive", "--session-protocol", "--session-inputs", inputs,
		"--out", out, "--output-format", "json")
	if err != nil {
		t.Fatalf("session --on: %v\n%s", err, stderr)
	}
	if turns, _ := os.ReadFile(filepath.Join(srv.dir, "turns")); strings.TrimSpace(string(turns)) != "3" {
		t.Errorf("the runhost's session took %q turns", turns)
	}
	if seen, _ := os.ReadFile(filepath.Join(srv.dir, "session-inputs.seen")); strings.TrimSpace(string(seen)) != "notes.txt" {
		t.Errorf("the runhost's session inputs: %q", seen)
	}
	for _, want := range []string{`"answer 1"`, `"answer 3"`, `"input_added"`, `"session_end"`} {
		if !strings.Contains(events.String(), want) {
			t.Errorf("the device's events lack %s:\n%s", want, events.String())
		}
	}
	var env receipt.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.Runhost == nil || env.OutDir != out {
		t.Errorf("envelope: %v\n%s", err, stdout)
	}
	if entries, _ := os.ReadDir(runhostJobsDir); len(entries) != 0 {
		t.Errorf("the runhost kept %d jobs after the session", len(entries))
	}
}

// turnWatcher is descriptor 3 for a session test: it keeps every event and
// says when an answer or an added file arrives.
type turnWatcher struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	answered chan struct{}
	added    chan struct{}
}

func (w *turnWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for _, l := range strings.Split(string(p), "\n") {
		switch {
		case strings.Contains(l, `"assistant_turn"`):
			w.answered <- struct{}{}
		case strings.Contains(l, `"input_added"`):
			w.added <- struct{}{}
		}
	}
	return len(p), nil
}

func (w *turnWatcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A connection lost while a run goes on is reached again: the device waits,
// attaches to the job where it lost it, and opens its output when it ends.
func TestRunOnReattachesAfterALostConnection(t *testing.T) {
	newRunOnLab(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	old := runOnReattachWaits
	runOnReattachWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { runOnReattachWaits = old })
	t.Setenv("RUNHOST_TEST_SLEEP", "2")
	go func() {
		time.Sleep(700 * time.Millisecond)
		dropTestRunhost()
	}()
	out := filepath.Join(t.TempDir(), "out")
	_, stderr, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", out)
	if err != nil {
		t.Fatalf("run --on: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "lost lab mid-run; the job goes on there") || !strings.Contains(stderr, "lab: harness: working") {
		t.Errorf("the device did not reach the job again:\n%s", stderr)
	}
	if got := listFiles(t, out); strings.Join(got, " ") != "receipts/events.ndjson reports/r.md result.json" {
		t.Errorf("the output folder holds %v", got)
	}
}
