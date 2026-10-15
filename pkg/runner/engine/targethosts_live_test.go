package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// The live target-host test. Everything is real but the
// model: the runner and host link images built here, the pinned gateway
// and its docker filter, the relay, and a throwaway OpenSSH server on
// this machine, reached at this machine's own LAN address as a person's
// server is. In the agent container's place, on its network with its
// token, runs hostclient: an MCP client that calls the host tools through
// the gateway as an agent would. It runs only when asked:
// AGENTRY_LIVE_HOSTLINK=1 on a host with docker and network (the build
// installs the server's packages).

const (
	livePassword = "Live-Host-Pass-7731-qz"
	liveSudo     = livePassword
	// What the host holds that the agent must get unchanged: keys the
	// gateway's secret block would refuse in any other tool result.
	liveAWSKey   = "AKIAQ3EGRQ5WJ2LZ7HXN"
	liveAWSValue = "wJalrXUtnFEMIK7MDENGbPxRfiCYq3x8Nz2LkVb1"
	liveGitHub   = "ghp_R8mZq2LkVb1x9Nw4Ty7Pc3Ha6Sd0Fg5Jk2Lp"
)

// agentSwap is the real daemon, with the agent container's image and
// command swapped for hostclient's; what was asked of the agent is kept,
// and during is told of the run's containers while the agent runs.
type agentSwap struct {
	dockerx.Client
	client string
	env    map[string]string
	during func(agent dockerx.RunSpec)
	after  func()
	out    bytes.Buffer
}

func (a *agentSwap) RunForeground(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if !strings.HasSuffix(r.Name, "-agent") {
		return a.Client.RunForeground(ctx, r, stdin, stdout, stderr)
	}
	if a.during != nil {
		a.during(r)
	}
	r.Image = "alpine:3.22"
	r.Entrypoint = "/hostclient"
	r.Argv = nil
	r.Mounts = []dockerx.Mount{{Source: a.client, Target: "/hostclient", ReadOnly: true}}
	env := map[string]string{}
	for k, v := range r.Env {
		env[k] = v
	}
	for k, v := range a.env {
		env[k] = v
	}
	r.Env = env
	code, err := a.Client.RunForeground(ctx, r, nil, io.MultiWriter(stdout, &a.out), stderr)
	// The gateway starts its servers on first use and keeps them: after
	// the calls, and before teardown, they are all up to inspect.
	if a.after != nil {
		a.after()
	}
	return code, err
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // the test's own docker calls
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// lanAddress is this machine's own outward address: a target host is
// another machine, never loopback, so the server is reached here.
func lanAddress(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		t.Skipf("no outward address: %v", err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// startSSHD builds and starts the throwaway server: an account with the
// password, sudo for it, the planted keys and the password itself in
// files the agent will print, and port 22 published on this machine.
func startSSHD(t *testing.T) int {
	t.Helper()
	tag := "agentry-live-sshd:861"
	dockerfile := "FROM alpine:3.22\nRUN apk add --no-cache openssh-server openssh-sftp-server sudo\n"
	cmd := exec.Command("docker", "build", "-q", "-t", tag, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the test server: %v\n%s", err, out)
	}
	name := fmt.Sprintf("agentry-live-sshd-%d", time.Now().UnixNano()%1e6)
	setup := strings.Join([]string{
		"set -e",
		"ssh-keygen -A >/dev/null",
		"adduser -D -s /bin/sh agent",
		"echo 'agent:" + livePassword + "' | chpasswd >/dev/null",
		"echo 'agent ALL=(ALL) ALL' > /etc/sudoers.d/agent",
		"mkdir -p /home/agent/.aws",
		"printf '[default]\\naws_access_key_id = " + liveAWSKey + "\\naws_secret_access_key = " + liveAWSValue + "\\ngithub = " + liveGitHub + "\\n' > /home/agent/.aws/credentials",
		"printf '%s\\n' '" + livePassword + "' > /home/agent/pw.txt",
		"chown -R agent /home/agent",
		"exec /usr/sbin/sshd -D -e -o PasswordAuthentication=yes -o KbdInteractiveAuthentication=no",
	}, "\n")
	dockerOut(t, "run", "-d", "--name", name, "-p", "0:22", "--entrypoint", "/bin/sh", tag, "-c", setup)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-fv", name).Run() })
	port := dockerOut(t, "port", name, "22/tcp")
	port = strings.Split(port, "\n")[0]
	var p int
	if _, err := fmt.Sscanf(port[strings.LastIndexByte(port, ':')+1:], "%d", &p); err != nil {
		t.Fatalf("published port %q", port)
	}
	return p
}

type trustAll struct{}

func (trustAll) Ask(string, string) (string, error) { return "", nil }
func (trustAll) AskSecret(string) (string, error)   { return "", nil }
func (trustAll) Confirm(string, bool) (bool, error) { return true, nil }

func TestLiveATargetHostThroughTheHostLink(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_HOSTLINK") != "1" {
		t.Skip("runs the real host link against a throwaway sshd: set AGENTRY_LIVE_HOSTLINK=1")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	work := t.TempDir()

	// hostclient, built static for the container.
	client := filepath.Join(work, "hostclient")
	build := exec.Command("go", "build", "-o", client, "./testdata/hostclient")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hostclient: %v\n%s", err, out)
	}

	addr := lanAddress(t)
	port := startSSHD(t)

	// The images: the runner, built from this binary's context, and the
	// gateway; the host link's is built by the run itself.
	pol := policy.Default()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(ctx, d, pol, runnerTag, filepath.Join(work, "img"), os.Stderr, false); err != nil {
		t.Fatal(err)
	}

	// The release.
	doc, err := os.ReadFile("../../../testdata/runner/target-host/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(work, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Check the server.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(work, "plan")
	plan, err := compile.Compile(compile.Inputs{Manifest: m, Digest: "sha256:live", Source: "local", WorkspaceDir: ws,
		Policy: pol, PolicySHA: "live", RunnerImage: runnerTag}, planDir)
	if err != nil {
		t.Fatal(err)
	}

	// The host, readied as agentry run readies it: its key read from the
	// server and trusted, the password and sudo password from variables.
	t.Setenv("LIVE_HOST_PW", livePassword)
	t.Setenv("LIVE_HOST_SUDO", liveSudo)
	t.Setenv("ANTHROPIC_API_KEY", "sk-live-test-not-a-key")
	ans, err := targethost.ParseAnswer(fmt.Sprintf("agent@%s:%d", addr, port))
	if err != nil {
		t.Fatal(err)
	}
	flags, err := targethost.ParseFlags(nil, nil, []string{"server=LIVE_HOST_PW"}, []string{"server=LIVE_HOST_SUDO"})
	if err != nil {
		t.Fatal(err)
	}
	var h *targethost.Host
	for i := 0; i < 20; i++ { // the server takes a moment to listen
		h, err = targethost.Prepare(ctx, "server", ans, targethost.Options{
			KnownHosts: targethost.KnownHostsFiles{Runners: filepath.Join(work, "known_hosts")},
			Flags:      flags, Getenv: os.Getenv, Ask: trustAll{}, Say: func(s string) { t.Log(s) },
		})
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ready the host: %v", err)
	}

	steps := []map[string]any{
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "cat ~/.aws/credentials"}},
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "cat ~/pw.txt; base64 ~/pw.txt"}},
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "whoami", "sudo": true}},
		{"tool": hostlink.ToolWrite, "args": map[string]any{"path": "/tmp/agentry-written.txt", "content": "written by the host link\n", "mode": "0640"}},
		{"tool": hostlink.ToolRead, "args": map[string]any{"path": "/tmp/agentry-written.txt"}},
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "stat -c %a /tmp/agentry-written.txt"}},
		{"tool": hostlink.ToolGet, "args": map[string]any{"from": "/etc/os-release", "to": "got/os-release"}},
		{"tool": "scratch_read", "args": map[string]any{"path": "hosts/got/os-release"}},
		{"tool": "scratch_write", "args": map[string]any{"path": "hosts/up.txt", "content": "put from the scratch folder\n"}},
		{"tool": hostlink.ToolPut, "args": map[string]any{"from": "/scratch/hosts/up.txt", "to": "/tmp/up.txt", "mode": "0600"}},
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "cat /tmp/up.txt; stat -c %a /tmp/up.txt"}},
		{"tool": hostlink.ToolRun, "args": map[string]any{"command": "sleep 30", "timeout": 2}},
		// The shell: a sudo prompt answered with the run's sudo
		// password, a package installed, ctrl-c, paging, the cap of
		// four.
		{"tool": hostlink.ToolShellOpen, "args": map[string]any{}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "cat ~/pw.txt", "key": "enter"}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "sudo apk add --no-cache jq", "key": "enter", "wait": 15}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "key": hostlink.KeySudoPassword, "wait": 180}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "jq --version", "key": "enter", "wait": 60}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "sleep 600", "key": "enter", "wait": 2}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "key": "ctrl-c"}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "echo after-$((6*7))", "key": "enter"}},
		{"tool": hostlink.ToolShellSend, "args": map[string]any{"session": 1, "text": "seq 1 3000", "key": "enter", "wait": 10}},
		{"tool": hostlink.ToolShellRead, "args": map[string]any{"session": 1}},
		{"tool": hostlink.ToolShellOpen, "args": map[string]any{"wait": 1}},
		{"tool": hostlink.ToolShellOpen, "args": map[string]any{"wait": 1}},
		{"tool": hostlink.ToolShellOpen, "args": map[string]any{"wait": 1}},
		{"tool": hostlink.ToolShellOpen, "args": map[string]any{"wait": 1}},
		{"tool": hostlink.ToolShellClose, "args": map[string]any{"session": 1}},
	}
	stepsJSON, _ := json.Marshal(steps)
	var inspected []string
	runRoot := filepath.Join(work, "home")
	swap := &agentSwap{
		Client: d, client: client,
		env: map[string]string{
			"HOSTCLIENT_URL": spec.GatewayURL(), "HOSTCLIENT_TOKEN_ENV": spec.GatewayTokenEnv,
			"HOSTCLIENT_STEPS": string(stepsJSON), "HOSTCLIENT_DIAL": fmt.Sprintf("%s:%d", addr, port),
		},
		during: func(agent dockerx.RunSpec) {
			if len(agent.ExtraNetworks) != 0 {
				t.Errorf("the agent container has networks beside its own: %v", agent.ExtraNetworks)
			}
		},
		after: func() {
			t.Logf("containers after the calls:\n%s", dockerOut(t, "ps", "--filter", "label="+runLabel, "--format", "{{.Names}} {{.Image}}"))
			ids := strings.Fields(dockerOut(t, "ps", "-aq", "--filter", "label="+runLabel))
			for _, id := range ids {
				inspected = append(inspected, dockerOut(t, "inspect", id))
			}
		},
	}
	ad, err := adapter.Get(adapter.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(work, "out")
	env, err := Run(ctx, swap, Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: runRoot, RunnerHome: runRoot, OutDir: outDir,
		Task: strings.NewReader("check it"), Adapter: ad, Policy: pol,
		Budgets: spec.Budgets{TimeoutSeconds: 600, MaxTurns: 50}, Attestation: receipt.Attestation{State: "local"},
		Intake: &intake.Answers{Hosts: map[string]*targethost.Host{"server": h}},
		Stderr: os.Stderr, RunID: fmt.Sprintf("live%04d", time.Now().UnixNano()%10000),
	})
	if err != nil {
		t.Fatalf("Run: %v\nagent said: %s", err, swap.out.String())
	}

	// What the agent got.
	var answer struct {
		Results []struct {
			Tool   string          `json:"tool"`
			Error  string          `json:"error"`
			Result json.RawMessage `json:"result"`
		} `json:"results"`
		Dial string `json:"dial"`
	}
	if err := json.Unmarshal([]byte(resultText(t, env.Result)), &answer); err != nil {
		t.Fatalf("the answer: %v\n%s", err, env.Result)
	}
	text := func(i int) (string, bool, string) {
		r := answer.Results[i]
		if r.Error != "" {
			return r.Error, true, "rpc"
		}
		var c struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Resource struct {
					Text string `json:"text"`
				} `json:"resource"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(r.Result, &c)
		if len(c.Content) == 0 {
			return string(r.Result), c.IsError, ""
		}
		if c.Content[0].Type == "resource" {
			return c.Content[0].Resource.Text, c.IsError, "resource"
		}
		return c.Content[0].Text, c.IsError, c.Content[0].Type
	}
	for i := range answer.Results {
		got, isErr, typ := text(i)
		t.Logf("step %d %s (%s, error %v): %.1200s", i, answer.Results[i].Tool, typ, isErr, got)
	}
	expect := func(i int, wants ...string) {
		t.Helper()
		got, isErr, _ := text(i)
		if isErr {
			t.Errorf("step %d (%s) failed: %s", i, answer.Results[i].Tool, got)
		}
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("step %d (%s): want %q in %q", i, answer.Results[i].Tool, w, got)
			}
		}
	}
	// The host's keys reach the agent unchanged, past --block-secrets.
	expect(0, liveAWSKey, liveAWSValue, liveGitHub)
	if _, _, typ := text(0); typ != "resource" {
		t.Errorf("a host result travels as %q, not an embedded resource", typ)
	}
	// The run's own password does not, raw or base64.
	expect(1, hostlink.Redacted+"\n"+hostlink.Redacted+"\nstderr")
	if got, _, _ := text(1); strings.Contains(got, livePassword) || strings.Contains(got, base64.StdEncoding.EncodeToString([]byte(livePassword+"\n"))) {
		t.Errorf("the run's own password reached the agent: %q", got)
	}
	expect(2, "root")
	expect(3, "wrote")
	expect(4, "written by the host link")
	expect(5, "640")
	expect(6, "copied /etc/os-release")
	expect(7, "Alpine")
	expect(9, "copied /scratch/hosts/up.txt")
	expect(10, "put from the scratch folder", "600")
	expect(11, "ended after 2 seconds")
	// The shell.
	expect(12, "[session 1: a login shell on server as agent, 1 of 4 open]")
	expect(13, "cat ~/pw.txt", hostlink.Redacted)
	expect(14, "password for agent")
	expect(15, "Installing jq", "OK:")
	expect(16, "jq-1.")
	expect(17, "sleep 600", "running")
	expect(18, "^C")
	expect(19, "after-42")
	expect(20, "more output is kept")
	expect(21, "\n3000")
	expect(22, "[session 2:")
	expect(23, "[session 3:")
	expect(24, "[session 4:")
	// Refused before any SSH session, as an argument is: not a call.
	if got, isErr, _ := text(25); !isErr || !strings.Contains(got, "has 4 shell sessions open") {
		t.Errorf("a fifth session: %v %q", isErr, got)
	}
	expect(26, "[session 1 on server is closed]")
	for i := 12; i <= 26; i++ {
		if got, _, _ := text(i); strings.Contains(got, liveSudo) || strings.Contains(got, "\x1b") {
			t.Errorf("step %d: the sudo password or a control sequence reached the agent: %q", i, got)
		}
	}
	transcript, err := os.ReadFile(filepath.Join(outDir, "receipts", hostlink.ShellLogFile("server", 1)))
	if err != nil {
		t.Fatalf("the shell's transcript: %v", err)
	}
	t.Logf("receipts/%s:\n%.3000s", hostlink.ShellLogFile("server", 1), transcript)
	for _, want := range []string{`sent "sudo apk add --no-cache jq" enter`, "sent sudo-password", "jq-1.", "after-42", "closed by the agent"} {
		if !strings.Contains(string(transcript), want) {
			t.Errorf("the transcript lacks %q", want)
		}
	}

	if answer.Dial == "connected" || answer.Dial == "" {
		t.Errorf("the agent container reached the target host itself: %q", answer.Dial)
	}

	// The sign-in is in no container's argv, environment or
	// mounts but the host link's folder, which only it mounts.
	hostLinks := 0
	for _, raw := range inspected {
		var list []struct {
			Name   string
			Args   []string
			State  struct{ Status string }
			Config struct {
				Image string
				Cmd   []string
				Env   []string
				User  string
			}
			Mounts []struct{ Source, Destination string }
		}
		if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) != 1 {
			t.Fatalf("inspect: %v", err)
		}
		c := list[0]
		for _, v := range []string{livePassword, base64.StdEncoding.EncodeToString([]byte(livePassword))} {
			if strings.Contains(raw, v) {
				t.Errorf("%s: its inspect holds the password", c.Name)
			}
		}
		isHostLink := len(c.Config.Cmd) == 1 && c.Config.Cmd[0] == "hostlink"
		if isHostLink {
			hostLinks++
			t.Logf("host link container %s (%s), user %q, mounts %v", c.Name, c.State.Status, c.Config.User, c.Mounts)
		}
		for _, m := range c.Mounts {
			if strings.HasSuffix(m.Source, "/cfg/"+spec.HostsDir) || strings.Contains(m.Source, "/cfg/"+spec.HostsDir+"/") {
				if !isHostLink {
					t.Errorf("%s (%v) mounts the host folder %s: only the host link may", c.Name, c.Config.Cmd, m.Source)
				}
			}
		}
	}
	if hostLinks == 0 {
		t.Error("no host link container was inspected")
	}

	// No file left holds the sign-in; the log names every call.
	for _, dir := range []string{outDir, runRoot} {
		_ = filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || !de.Type().IsRegular() {
				return nil
			}
			b, _ := os.ReadFile(p) //nolint:gosec // the test's own folders
			if bytes.Contains(b, []byte(livePassword)) || bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString([]byte(livePassword)))) {
				t.Errorf("%s holds the password", p)
			}
			return nil
		})
	}
	log, err := os.ReadFile(filepath.Join(outDir, "receipts", hostlink.LogFile("server")))
	if err != nil || strings.Count(string(log), "\n") != 24 || !strings.Contains(string(log), `"tool":"host_get"`) || !strings.Contains(string(log), `"keys":["sudo-password"]`) {
		t.Errorf("receipts/host-server.log (%v):\n%s", err, log)
	}
	t.Logf("host.server: %s", env.CapabilitiesEnforced["host.server"])
	t.Logf("hostLink.image: %s", env.CapabilitiesEnforced["hostLink.image"])
	if !strings.Contains(env.CapabilitiesEnforced["host.server"], "24 calls") || !strings.Contains(env.CapabilitiesEnforced["host.server"], "4 shell sessions") {
		t.Errorf("host.server line: %s", env.CapabilitiesEnforced["host.server"])
	}
}

func resultText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return s
}
