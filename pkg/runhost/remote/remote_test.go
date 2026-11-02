package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
)

const testDevice = "aaaaaaaaaaaaaaaa"

// lab is a runhost in this process: an SSH server admitting the device's run
// key alone, relaying each session through runhost.Serve to a scripted
// service that takes a job, runs it in name only, and seals its output and
// envelope to the device.
type lab struct {
	rec  runhost.Record
	keys *runhost.DeviceKeys
	// got is the job the service was sent, and archive the release's bytes.
	got     *job.Job
	archive []byte
	done    bool
}

func newLab(t *testing.T) *lab {
	t.Helper()
	keys, err := runhost.NewDeviceKeys()
	if err != nil {
		t.Fatal(err)
	}
	to, err := output.Devices(map[string][]byte{testDevice: keys.Seal.PublicKey().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	l := &lab{keys: keys}
	sealed := func(fw *runhost.FrameWriter, plain io.Reader, jobString string) error {
		var b bytes.Buffer
		if err := output.Seal(&b, plain, to, jobString); err != nil {
			return err
		}
		return fw.Data(&b)
	}
	sockDir, err := os.MkdirTemp("", "rr")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(sockDir, 0o700)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	svc := &runhost.Service{Socket: filepath.Join(sockDir, "s.sock"), Handlers: map[string]runhost.Handler{
		"job": func(_ context.Context, _ runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
			j, err := job.Read(in)
			if err != nil {
				return 2, err
			}
			l.got = j
			if err := fw.Release(runhost.ReleaseNeeded); err != nil {
				return 1, err
			}
			l.archive = make([]byte, j.Release.Size)
			if _, err := io.ReadFull(in, l.archive); err != nil {
				return 1, err
			}
			_ = fw.Started(j.ID)
			_ = fw.Out("harness: working")
			return 0, fw.Result(runhost.JobResult{Job: j.ID, Done: true})
		},
		"collect": func(_ context.Context, req runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			id := req.Args[0]
			switch {
			case len(req.Args) == 2 && req.Args[1] == "envelope":
				env := `{"agentry":"1","status":"ok","exitCode":0,"runId":"run123","ref":"acme/demo@1.0.0","outDir":"/var/lib/agentry/runhost/jobs/x/out","receipts":{"dir":"/var/lib/agentry/runhost/jobs/x/out/receipts"},"result":"done"}`
				return 0, sealed(fw, strings.NewReader(env), runhost.SealedEnvelopeJob(id))
			case len(req.Args) == 2 && req.Args[1] == "done":
				l.done = true
				return 0, fw.Result(map[string]bool{"deleted": true})
			}
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o700); err != nil {
				return 1, err
			}
			_ = os.WriteFile(filepath.Join(dir, "reports", "r.md"), []byte("report"), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"from":"the agent"}`), 0o600)
			var tarball bytes.Buffer
			if err := output.Pack(&tarball, dir); err != nil {
				return 1, err
			}
			return 0, sealed(fw, &tarball, id)
		},
	}}
	ln, err := svc.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = svc.Serve(ctx, ln) }()

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	runSigner, err := keys.Signer()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), runSigner.PublicKey().Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, errors.New("not the run key")
	}}
	cfg.AddHostKey(hostSigner)
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
			go serveSSH(ctx, c, cfg, svc.Socket)
		}
	}()
	l.rec = runhost.Record{Name: "lab", Host: "127.0.0.1", Port: tl.Addr().(*net.TCPAddr).Port,
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))), HostKeyFingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		DeviceID: testDevice, CLIVersion: "test"}
	return l
}

func serveSSH(ctx context.Context, c net.Conn, cfg *ssh.ServerConfig, socket string) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
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

// A client of the package sends a job, follows it, and collects it: the
// release goes when the runhost asks for it, the narration comes back, the
// sealed output opens into the output folder, and the envelope is this
// device's, naming the runhost; the runhost is told to delete the job, and
// the device forgets it.
func TestAClientRunsAJobOnARunhostThroughThePackage(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	c, err := runhost.DialRunhost(ctx, l.rec, l.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	id, err := NewJobID()
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("the release's archive")
	j := &job.Job{V: job.Version, ID: id, Device: testDevice,
		Release: job.Release{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:" + strings.Repeat("a", 64), Size: int64(len(archive)),
			Registry: "https://registry.test", State: job.StateUnsigned},
		Run: job.Run{AllowUnsigned: true, MaxTurns: -1, MaxCost: -1, ModelKey: "k", Model: job.Model{Provider: "anthropic", Name: "m", APIKeyEnv: "ANTHROPIC_API_KEY"}}}
	home := t.TempDir()
	lj := LocalJob{Job: id, Runhost: "lab", Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Adapter: job.AdapterClaudeImage, AdapterAsked: "claude"}
	if err := PutJob(home, lj); err != nil {
		t.Fatal(err)
	}
	var said []string
	res, started, _, err := Start(ctx, c, j, archive, nil, false, func(s string) { said = append(said, s) }, nil, nil)
	if err != nil || !started || !res.Done {
		t.Fatalf("start: %+v %v %v", res, started, err)
	}
	if l.got == nil || l.got.ID != id || !bytes.Equal(l.archive, archive) {
		t.Fatalf("the runhost was sent %+v and %q", l.got, l.archive)
	}
	if len(said) != 1 || said[0] != "harness: working" {
		t.Fatalf("narration %q", said)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	env, err := Finish(ctx, c, &l.rec, l.keys, lj, res.ExitCode, FinishOptions{Home: home, OutDir: func(string) (string, error) { return out, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if env.Runhost == nil || env.Runhost.Name != "lab" || env.Runhost.Job != id || env.Runhost.AdapterAsked != "claude" || env.OutDir != out {
		t.Fatalf("envelope %+v %+v", env, env.Runhost)
	}
	if b, err := os.ReadFile(filepath.Join(out, "reports", "r.md")); err != nil || string(b) != "report" {
		t.Fatalf("the report: %q %v", b, err)
	}
	var written struct {
		Runhost *struct{ Name string } `json:"runhost"`
	}
	if b, err := os.ReadFile(filepath.Join(out, "result.json")); err != nil || json.Unmarshal(b, &written) != nil || written.Runhost == nil || written.Runhost.Name != "lab" {
		t.Fatalf("result.json is not this device's envelope: %s %v", b, err)
	}
	if !l.done {
		t.Fatal("the runhost was not told to delete the job")
	}
	if _, ok := FindJob(home, "lab", id); ok {
		t.Fatal("the device still records the collected job")
	}
}

// An envelope opens only with the sealing key of the device it was sealed
// to.
func TestAnEnvelopeOpensOnlyForItsDevice(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	c, err := runhost.DialRunhost(ctx, l.rec, l.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := CollectEnvelope(ctx, c, "bbbbbbbbbbbbbbbb", testDevice, l.keys); err != nil {
		t.Fatal(err)
	}
	other, _ := runhost.NewDeviceKeys()
	if _, err := CollectEnvelope(ctx, c, "bbbbbbbbbbbbbbbb", testDevice, other); err == nil {
		t.Fatal("an envelope sealed to another device's key opened")
	}
}
