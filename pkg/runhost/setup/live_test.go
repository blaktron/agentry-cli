package setup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// The live test of setup's SSH steps against a throwaway OpenSSH server in a
// container: the host key's check, the admin sign-in by key and by password,
// sudo with and without its password, the read-only preflight, and, with a
// runhost account made as the install makes it and this CLI as its login
// shell and its service, a run key held by the real sshd to the forced
// command. It runs only when asked: AGENTRY_LIVE_RUNHOST=1 on a host with
// docker and network (the build installs the server's packages).

const liveSudoPassword = "Live-Runhost-Sudo-4471-kq"

type liveServer struct {
	name     string
	port     int
	adminKey []byte
	runKeys  *runhost.DeviceKeys
}

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // the test's own docker calls
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func buildCLI(t *testing.T) string {
	t.Helper()
	mod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(strings.TrimSpace(string(mod)))
	bin := filepath.Join(t.TempDir(), "agentry")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/agentry") //nolint:gosec // the test's own build
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the CLI: %v\n%s", err, out)
	}
	return bin
}

func startLiveServer(t *testing.T) *liveServer {
	t.Helper()
	tag := "agentry-live-runhost-sshd:1"
	dockerfile := "FROM debian:trixie-slim\nRUN apt-get update -qq && apt-get install -y -qq openssh-server sudo procps iproute2 >/dev/null && rm -rf /var/lib/apt/lists/*\n"
	b := exec.Command("docker", "build", "-q", "-t", tag, "-")
	b.Stdin = strings.NewReader(dockerfile)
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("build the test server: %v\n%s", err, out)
	}

	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(adminPriv, "admin")
	if err != nil {
		t.Fatal(err)
	}
	adminPub, _ := ssh.NewPublicKey(adminPriv.Public())
	runKeys, err := runhost.NewDeviceKeys()
	if err != nil {
		t.Fatal(err)
	}
	runPub, _ := runKeys.RunPublic()

	cfg, _ := json.Marshal(runhost.ServerConfig{Name: "live", Owner: runhost.Owner{Kind: runhost.OwnerDevices}, GVisor: GVisorRelease})
	devs, _ := json.Marshal(runhost.Devices{Devices: []runhost.Device{{ID: "live-device", Label: "test", RunKey: runPub}}})
	script := strings.Join([]string{
		"set -e",
		"ssh-keygen -A >/dev/null",
		"useradd -m -s /bin/bash tester",
		"echo 'tester:" + liveSudoPassword + "' | chpasswd",
		"echo 'tester ALL=(ALL) ALL' > /etc/sudoers.d/tester",
		"install -d -m 0700 -o tester /home/tester/.ssh",
		"echo '" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(adminPub))) + "' > /home/tester/.ssh/authorized_keys",
		"chown tester /home/tester/.ssh/authorized_keys",
		// The runhost's account, as the install makes it.
		"useradd --system --user-group --create-home --home-dir " + runhost.Home + " --shell " + runhost.ShellPath + " agentry",
		"usermod -p '*' agentry",
		"install -d -m 0700 -o agentry -g agentry " + runhost.Home + " " + runhost.Home + "/.ssh " + runhost.StateDir,
		"install -d -m 0755 -o agentry -g agentry " + runhost.BinDir,
		"while [ ! -x /tmp/agentry ]; do sleep 0.2; done",
		"install -m 0755 -o agentry /tmp/agentry " + runhost.CLIPath,
		"ln -s agentry " + runhost.ShellPath,
		"printf '%s\\n' '" + runhost.AuthorizedKeyLine(runPub, "live-device") + "' > " + runhost.AuthorizedKeys,
		"printf '%s' '" + string(cfg) + "' > " + runhost.ConfigFile,
		"printf '%s' '" + string(devs) + "' > " + runhost.DevicesFile,
		"chown agentry:agentry " + runhost.AuthorizedKeys + " " + runhost.ConfigFile + " " + runhost.DevicesFile,
		"chmod 0600 " + runhost.AuthorizedKeys,
		"uid=$(id -u agentry); install -d -m 0700 -o agentry -g agentry /run/user/$uid",
		"runuser -u agentry -- env HOME=" + runhost.Home + " XDG_RUNTIME_DIR=/run/user/$uid " + runhost.CLIPath + " runhost service >/tmp/service.log 2>&1 &",
		"mkdir -p /run/sshd",
		"exec /usr/sbin/sshd -D -e -o PasswordAuthentication=yes",
	}, "\n")
	name := fmt.Sprintf("agentry-live-runhost-%d", time.Now().UnixNano()%1e6)
	dockerRun(t, "run", "-d", "--name", name, "-p", "127.0.0.1:0:22", "--entrypoint", "/bin/sh", tag, "-c", script)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-fv", name).Run() })
	dockerRun(t, "cp", buildCLI(t), name+":/tmp/agentry.part")
	dockerRun(t, "exec", name, "mv", "/tmp/agentry.part", "/tmp/agentry")
	port := dockerRun(t, "port", name, "22/tcp")
	p, err := strconv.Atoi(port[strings.LastIndexByte(strings.Split(port, "\n")[0], ':')+1:])
	if err != nil {
		t.Fatalf("published port %q", port)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, _ := exec.Command("docker", "exec", name, "sh", "-c", "ls /run/user/*/"+runhost.SocketName+" && ss -H -ltn 'sport = :22' | grep -q LISTEN && echo listening").CombinedOutput()
		if strings.Contains(string(out), "listening") {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			svc, _ := exec.Command("docker", "exec", name, "cat", "/tmp/service.log").CombinedOutput()
			t.Fatalf("the test server did not come up:\n%s\n%s", logs, svc)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return &liveServer{name: name, port: p, adminKey: pem.EncodeToMemory(block), runKeys: runKeys}
}

func (s *liveServer) logs() string {
	out, _ := exec.Command("docker", "logs", "--tail", "30", s.name).CombinedOutput()
	return string(out)
}

type acceptAll struct{}

func (acceptAll) Ask(string, string) (string, error) { return "", nil }
func (acceptAll) AskSecret(string) (string, error)   { return "", nil }
func (acceptAll) Confirm(string, bool) (bool, error) { return true, nil }

func TestLiveSetupStepsAgainstAContainerisedSSHD(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_RUNHOST") != "1" {
		t.Skip("runs setup's SSH steps against a throwaway sshd: set AGENTRY_LIVE_RUNHOST=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	srv := startLiveServer(t)
	addr := netip.MustParseAddr("127.0.0.1")
	answer := targethost.Answer{User: "tester", Host: "127.0.0.1", Port: srv.port}
	files := targethost.KnownHostsFiles{Runners: filepath.Join(t.TempDir(), "known_hosts")}
	say := func(string) {}

	// The host key: unknown with no one to ask is refused; accepted once at
	// a terminal, it is kept; a different key on file is refused.
	var refusal *targethost.Refusal
	if _, err := targethost.CheckHostKey(ctx, "live", answer, addr, files, nil, say); !errors.As(err, &refusal) || refusal.Detail != targethost.RefusedKeyUnknown {
		t.Fatalf("an unknown host key with no one to ask: %v\n%s", err, srv.logs())
	}
	hk, err := targethost.CheckHostKey(ctx, "live", answer, addr, files, acceptAll{}, say)
	if err != nil {
		t.Fatalf("%v\n%s", err, srv.logs())
	}
	if again, err := targethost.CheckHostKey(ctx, "live", answer, addr, files, nil, say); err != nil || again.From != targethost.TrustRunnersFile {
		t.Fatalf("the kept key was not found again: %+v %v", again, err)
	}

	// The admin sign-in by key: sudo asks for its password, a wrong one is
	// refused, the right one makes the root steps root.
	conn, err := Dial(ctx, addr, srv.port, "tester", hk.Key, AdminAuth{Key: srv.adminKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.FindRoot(ctx); !errors.Is(err, ErrSudoPassword) {
		t.Fatalf("sudo's password was not asked for: %v", err)
	}
	if err := conn.SetSudo(ctx, "wrong"); err == nil {
		t.Fatal("a wrong sudo password was accepted")
	}
	if err := conn.SetSudo(ctx, liveSudoPassword); err != nil {
		t.Fatal(err)
	}
	if r, err := conn.RunRoot(ctx, "id -u", nil); err != nil || strings.TrimSpace(string(r.Stdout)) != "0" {
		t.Fatalf("a root step ran as %q (%v)", r.Stdout, err)
	}

	// The preflight reads the server, and this one (no systemd) stops setup.
	stage, err := conn.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := runPreflight(ctx, conn, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	if facts["os_id"] != "debian" || facts["os_version_id"] != "13" || facts["agentry_user"] != "yes" || facts["listening"] == "" {
		t.Fatalf("facts %v", facts)
	}
	if p := Evaluate(facts, nil); p.Supported {
		t.Fatalf("a server without systemd would be set up: %+v", p.Findings)
	}
	conn.Unstage(ctx, stage)
	if r, _ := conn.Run(ctx, "ls -d "+stage, nil, nil); r.Code == 0 {
		t.Fatal("the staging directory was left behind")
	}

	// The account has no shell: su to it runs only the login-shell guard.
	if r, _ := conn.RunRoot(ctx, "su - agentry -c id 2>&1; su agentry 2>&1 </dev/null", nil); strings.Contains(string(r.Stdout), "uid=") || strings.Count(string(r.Stdout), "has no shell") != 2 {
		t.Fatalf("the agentry account ran a command: %s", r.Stdout)
	}
	_ = conn.Close()

	// The admin sign-in by password, which sudo then takes too.
	pconn, err := Dial(ctx, addr, srv.port, "tester", hk.Key, AdminAuth{Password: liveSudoPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := pconn.FindRoot(ctx); err != nil {
		t.Fatalf("sudo did not take the sign-in password: %v", err)
	}
	_ = pconn.Close()

	// The run key, held by the real sshd to the forced command.
	rec := runhost.Record{Name: "live", Host: "127.0.0.1", Port: srv.port, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk.Key)))}
	rc, err := runhost.DialRunhost(ctx, rec, srv.runKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	v, err := rc.Ask(ctx, "version", nil, nil)
	if err != nil || v.Code != 0 || len(v.Result) == 0 {
		t.Fatalf("version: %+v %v", v, err)
	}
	st, err := rc.Ask(ctx, "status", nil, nil)
	if err != nil || st.Code != 0 || !strings.Contains(string(st.Result), `"name":"live"`) {
		t.Fatalf("status: %+v %v", st, err)
	}
	for _, r := range rc.CheckRestrictions(ctx) {
		if !r.Refused {
			t.Errorf("the run key was not refused %s: %s", r.What, r.Detail)
		}
	}

	// A host key other than the pinned one is refused.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _ := ssh.NewPublicKey(otherPriv.Public())
	rec.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherPub)))
	if c, err := runhost.DialRunhost(ctx, rec, srv.runKeys); !errors.Is(err, runhost.ErrHostKeyChanged) {
		if c != nil {
			_ = c.Close()
		}
		t.Fatalf("a changed host key was not refused: %v", err)
	}
}
