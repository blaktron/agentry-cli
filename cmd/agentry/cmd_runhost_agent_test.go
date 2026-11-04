package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// verbCaller answers the agent verb in process, as the service does over the
// run key: the input stays open until send returns.
type verbCaller struct{}

func (verbCaller) Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(runhost.Frame) error) (runhost.Answer, error) {
	if verb != "agent" {
		return runhost.Answer{}, errors.New("not the agent verb")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		if send != nil {
			_ = send(inW)
		}
		_ = inW.Close()
	}()
	go func() {
		fw := runhost.NewFrameWriter(outW)
		code, err := runhostAgentVerb(ctx, runhost.Request{Verb: verb, Args: args}, inR, fw)
		_ = fw.Exit(code, err)
		_ = outW.Close()
		_ = inR.Close()
	}()
	return runhost.ReadFrames(outR, each)
}

// fakeAgentCLI is a runhost CLI whose `agent` commands write to each stream
// and exit as the test says: the arguments to stdout, a line to stderr and
// one to descriptor 3, and stdin echoed back on stdout.
func fakeAgentCLI(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agentry")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	prev := runhostAgentExe
	runhostAgentExe = func() (string, error) { return p, nil }
	t.Cleanup(func() { runhostAgentExe = prev })
}

func testCommand(stdin string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	c := &cobra.Command{}
	c.SetContext(context.Background())
	c.SetIn(strings.NewReader(stdin))
	c.SetOut(&out)
	c.SetErr(&errOut)
	return c, &out, &errOut
}

// The runhost runs only the agent subcommands a device may drive there, and
// none of the flags that name a file or a credential on the device.
func TestARunhostRunsOnlyTheAgentCommandsADeviceDrives(t *testing.T) {
	for _, ok := range [][]string{{"start", "assistant"}, {"grant", "assistant", "--yes"}, {"configure", "coder", "--name=Ada"}, {"list", "--json"}, {"secret", "set", "assistant", "X"}, {"configure", "coder", "--no-account=github.com"}, {"configure", "coder", "--account=github.com", "--own"}, {"configure", "assistant", "--microsoft-sign-in=a@b.example"}, {"configure", "assistant", "--fastmail-token=true", "--yes"}, {"configure", "assistant", "--calendar-feed=work"},
		{"messenger", "add", "assistant", "telegram"}, {"messenger", "pair", "assistant", "telegram", "--no-wait=true"}, {"messenger", "list", "assistant"},
		{"messenger", "test", "assistant", "slack"}, {"messenger", "remove", "acme/helper", "discord"}, {"messenger", "add", "assistant", "matrix", "--homeserver=https://matrix.example.org"}} {
		if err := remote.CheckAgentArgs(ok[0], ok[1:]); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"install", "acme/helper"}, {"supervise", "assistant"}, {"skill", "add", "x"}, {"messenger", "add", "assistant", "matrix", "--user=@a:example.org"},
		{"start", "assistant", "--policy=/etc/passwd"}, {"configure", "coder", "--share=/srv"}, {"configure", "coder", "--dev-key=/k"},
		{"configure", "assistant", "--google-sign-in"}, {"configure", "assistant", "--google-client=/c.json"}, {"configure", "assistant", "--calendar-sign-in"},
	} {
		if err := remote.CheckAgentArgs(bad[0], bad[1:]); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
}

// The verb runs the runhost's own agent command: its stdout, stderr and
// descriptor 3 come back to the device's own, the device's input reaches its
// stdin, and its exit code is the device's.
func TestTheAgentVerbRunsTheRunhostsOwnCommand(t *testing.T) {
	newRunhostServer(t)
	fakeAgentCLI(t, `echo "args: $*"; echo "a warning" >&2; echo '{"event":"x"}' >&3; cat; [ "$3" = fail ] && exit 11; exit 0`+"\n")
	cmd, out, errOut := testCommand("")
	code, stderr, err := runOnRunhostAgent(cmd, verbCaller{}, "list", []string{"--json"}, "")
	if err != nil || code != 0 {
		t.Fatalf("list: %d %v", code, err)
	}
	if !strings.Contains(out.String(), "args: agent list --json") || !strings.Contains(errOut.String(), "a warning") || !strings.Contains(stderr, "a warning") {
		t.Fatalf("out %q err %q", out, errOut)
	}
	cmd, out, _ = testCommand("{\"send\":\"hi\"}\n")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "attach", []string{"assistant", "--json"}, ""); err != nil || code != 0 {
		t.Fatalf("attach: %d %v", code, err)
	}
	if !strings.Contains(out.String(), `{"send":"hi"}`) {
		t.Fatalf("attach's stdin did not reach the runhost: %q", out)
	}
	cmd, _, _ = testCommand("")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "show", []string{"fail"}, ""); err != nil || code != engine.ExitPolicy {
		t.Fatalf("a failing command: %d %v", code, err)
	}
	cmd, _, _ = testCommand("")
	if _, _, err := runOnRunhostAgent(cmd, verbCaller{}, "install", []string{"acme/helper"}, ""); err == nil {
		t.Fatal("a subcommand a runhost does not run was sent")
	}
	// The runhost refuses it too, whoever asks.
	var frames bytes.Buffer
	if code, _ := runhostAgentVerb(context.Background(), runhost.Request{Verb: "agent", Args: []string{"run", "install", "acme/helper"}}, strings.NewReader(""), runhost.NewFrameWriter(&frames)); code != engine.ExitUsage {
		t.Fatalf("the runhost ran a subcommand it does not run: %d", code)
	}
}

// An agent the service started is started again when the service starts;
// one stopped is not.
func TestTheServiceStartsItsAgentsAgain(t *testing.T) {
	newRunhostServer(t)
	runhostAgentsDir = filepath.Join(t.TempDir(), "agents")
	log := filepath.Join(t.TempDir(), "started")
	fakeAgentCLI(t, `echo "$*" >>`+log+"\n")
	cmd, _, _ := testCommand("")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "start", []string{"assistant"}, ""); err != nil || code != 0 {
		t.Fatalf("start: %d %v", code, err)
	}
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "start", []string{"coder"}, ""); err != nil || code != 0 {
		t.Fatalf("start: %d %v", code, err)
	}
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "stop", []string{"coder"}, ""); err != nil || code != 0 {
		t.Fatalf("stop: %d %v", code, err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startRunhostAgents(context.Background(), t.Logf)
	b, _ := os.ReadFile(log) //nolint:gosec // the test's own file
	if got := strings.TrimSpace(string(b)); got != "agent start assistant" {
		t.Fatalf("started again: %q", got)
	}
}

// A chat on the device reaches the agent's owner socket on the runhost
// through a socket of the device's own, byte for byte.
func TestTheAgentSocketIsRelayedToTheDevice(t *testing.T) {
	newRunhostServer(t)
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	sock := persistent.SocketPath(home, "assistant")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		line, _ := bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte("echo " + line))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devHome, _, stop, err := remote.RelayAgentSocket(ctx, verbCaller{}, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c, err := net.Dial("unix", persistent.SocketPath(devHome, "assistant"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("status\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := bufio.NewReader(c).ReadString('\n')
	if got != "echo status\n" {
		t.Fatalf("relayed %q", got)
	}
	if _, ok := remote.AgentSocketKey("acme/helper"); !ok {
		t.Error("an installed agent's reference has no key")
	}
	if _, ok := remote.AgentSocketKey("../x"); ok {
		t.Error("a path was taken as an agent's name")
	}
}

// The runhost's own model, its persistent agents', is an API provider's,
// its key in the runhost's store and its policy naming the provider and the
// model with the store as the key's source.
func TestARunhostKeepsTheModelItsAgentsRunOn(t *testing.T) {
	sealingRunhost(t)
	t.Cleanup(modelkey.UseKeychain(runhostModelKeys{}))
	in := strings.NewReader(`{"provider":"anthropic","model":"claude-test-1","apiKey":"sk-ant-test-agents-key"}`)
	var out bytes.Buffer
	fw := runhost.NewFrameWriter(&out)
	code, err := runhostStoreVerb(context.Background(), runhost.Request{Verb: "store", Args: []string{"put-model"}}, in, fw)
	if code != 0 {
		t.Fatalf("put-model: %d %v", code, err)
	}
	if got, err := modelkey.Lookup("anthropic"); err != nil || got != "sk-ant-test-agents-key" {
		t.Fatalf("the runhost's key: %q %v", got, err)
	}
	path, _ := policy.DefaultPath()
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if pol.Model.Provider != "anthropic" || pol.Model.Name != "claude-test-1" || pol.Model.APIKeySource != modelkey.SourceKeyring {
		t.Fatalf("the runhost's model %+v", pol.Model)
	}
	b, _ := os.ReadFile(path) //nolint:gosec // the test's own policy
	if bytes.Contains(b, []byte("sk-ant-test-agents-key")) {
		t.Fatal("the key is in the policy")
	}
	for _, bad := range []string{`{"provider":"nope","model":"m","apiKey":"k"}`, `{"provider":"anthropic","model":"m","apiKey":"two words"}`, `{"provider":"anthropic","model":"m","apiKey":"k","extra":1}`} {
		var o bytes.Buffer
		if code, _ := runhostStoreVerb(context.Background(), runhost.Request{Verb: "store", Args: []string{"put-model"}}, strings.NewReader(bad), runhost.NewFrameWriter(&o)); code != engine.ExitUsage {
			t.Errorf("%s: code %d", bad, code)
		}
	}
}

// An agent's stored secret and git host account are kept in the runhost's
// store, each as an entry of its own named for no part of its account; no
// other kind of account is taken.
func TestARunhostKeepsItsAgentsSecretsInItsStore(t *testing.T) {
	for _, ok := range []string{"agent-secret:coder:GITHUB_TOKEN", "agent-secret:acme.helper:API_KEY", "account:github.com:octo-bot",
		"agent-secret:assistant:ms365-keyring", "calendar-feed:assistant:work", "calendar-session:assistant:work:2",
		"messenger:assistant:telegram", "messenger:acme.helper:matrix"} {
		name, err := runhostKeychainEntry(ok)
		if err != nil || !strings.HasPrefix(name, "agent-") || len(name) > 32 || strings.Contains(name, "GITHUB") || strings.Contains(name, "octo") || strings.Contains(name, "work") {
			t.Errorf("%s: %q %v", ok, name, err)
		}
	}
	a, _ := runhostKeychainEntry("agent-secret:coder:A")
	b, _ := runhostKeychainEntry("agent-secret:coder:B")
	if a == b {
		t.Error("two secrets share an entry")
	}
	for _, bad := range []string{"agent-secret:../x:A", "agent-secret:coder:../A", "account:github.com:a/b", "agent-secret:coder:", "other:x", "calendar-feed:assistant:../x", "calendar-session:assistant:work:x",
		"messenger:assistant:../x", "messenger:../x:telegram", "messenger:assistant:", "messenger:assistant:Telegram"} {
		if _, err := runhostKeychainEntry(bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
}

// A stored secret's value is read on the device, agreed to, and reaches the
// runhost's own secret set on its stdin, and nothing but secret and
// configure is given stdin.
func TestAStoredSecretsValueTravelsToTheRunhostsSecretSet(t *testing.T) {
	newRunhostServer(t)
	fakeAgentCLI(t, `echo "args: $*"; echo "stdin: $(cat)"`+"\n")
	cmd, _, _ := testCommand("s3cret-value\n")
	v, _, err := runhostAgentValue(cmd, "lab", "secret", []string{"set", "coder", "GITHUB_TOKEN"}, []string{"set", "coder", "GITHUB_TOKEN", "--yes=true"})
	if err != nil || v != "s3cret-value" {
		t.Fatalf("the value read here: %q %v", v, err)
	}
	cmd, out, _ := testCommand("")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "secret", []string{"set", "coder", "GITHUB_TOKEN", "--yes=true"}, v); err != nil || code != 0 {
		t.Fatalf("secret set: %d %v", code, err)
	}
	if !strings.Contains(out.String(), "stdin: s3cret-value") || !strings.Contains(out.String(), "args: agent secret set coder GITHUB_TOKEN") {
		t.Fatalf("the runhost got %q", out)
	}
	cmd, out, _ = testCommand("")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "show", []string{"coder"}, "leaked"); err != nil || code != 0 {
		t.Fatalf("show: %d %v", code, err)
	}
	if strings.Contains(out.String(), "leaked") {
		t.Fatalf("show was given stdin: %q", out)
	}
	cmd, _, _ = testCommand("s3cret\n")
	if _, _, err := runhostAgentValue(cmd, "lab", "secret", []string{"set", "coder", "X"}, []string{"set", "coder", "X"}); err == nil {
		t.Fatal("a value was kept with nobody at a terminal to agree and no --yes")
	}
}

// A messenger's credential is read on the device, the way its kind gives
// it, agreed to, and reaches the runhost's own messenger add on its stdin.
// A Matrix account's password signs in from the device, which sends the
// session's access token alone: the password and --user never travel.
func TestAMessengersCredentialTravelsToTheRunhostsMessengerAdd(t *testing.T) {
	newRunhostServer(t)
	fakeAgentCLI(t, `echo "args: $*"; echo "stdin: $(cat)"`+"\n")
	cmd, _, _ := testCommand("123456:telegram-bot-token\n")
	args := []string{"add", "assistant", "telegram"}
	v, fwd, err := runhostAgentValue(cmd, "lab", "messenger", args, append(args, "--yes=true"))
	if err != nil || v != "123456:telegram-bot-token" {
		t.Fatalf("the token read here: %q %v", v, err)
	}
	cmd, out, _ := testCommand("")
	if code, _, err := runOnRunhostAgent(cmd, verbCaller{}, "messenger", fwd, v); err != nil || code != 0 {
		t.Fatalf("messenger add: %d %v", code, err)
	}
	if !strings.Contains(out.String(), "stdin: 123456:telegram-bot-token") || !strings.Contains(out.String(), "args: agent messenger add assistant telegram --yes=true") {
		t.Fatalf("the runhost got %q", out)
	}

	// Slack's two tokens, from a pipe, travel as one line.
	cmd, _, _ = testCommand("xoxb-bot-1\nxapp-app-1\n")
	args = []string{"add", "assistant", "slack"}
	if v, _, err := runhostAgentValue(cmd, "lab", "messenger", args, append(args, "--yes=true")); err != nil || v != "xapp-app-1 xoxb-bot-1" {
		t.Fatalf("Slack's tokens: %q %v", v, err)
	}

	// Matrix with --user signs in here.
	var gotPassword string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPassword = body.Password
		_, _ = io.WriteString(w, `{"access_token":"syt_matrix_access","user_id":"@agent:example.org","device_id":"D1"}`)
	}))
	defer hs.Close()
	prev := messengerAPI
	messengerAPI = hs.URL
	t.Cleanup(func() { messengerAPI = prev })
	cmd, _, _ = testCommand("hunter2-password\n")
	args = []string{"add", "assistant", "matrix"}
	v, fwd, err = runhostAgentValue(cmd, "lab", "messenger", args, append(args, "--homeserver=https://matrix.example.org", "--user=@agent:example.org", "--yes=true"))
	if err != nil || v != "syt_matrix_access" || gotPassword != "hunter2-password" {
		t.Fatalf("Matrix's sign-in here: %q %v (password %q)", v, err, gotPassword)
	}
	if strings.Contains(strings.Join(fwd, " "), "--user") || !slices.Contains(fwd, "--homeserver=https://matrix.example.org") {
		t.Fatalf("forwarded %q", fwd)
	}

	// Only add reads a credential; nothing is kept without agreement.
	cmd, _, _ = testCommand("leaked\n")
	args = []string{"pair", "assistant", "telegram"}
	if v, _, err := runhostAgentValue(cmd, "lab", "messenger", args, args); err != nil || v != "" {
		t.Fatalf("pair read a value: %q %v", v, err)
	}
	cmd, _, _ = testCommand("123456:telegram-bot-token\n")
	args = []string{"add", "assistant", "telegram"}
	if _, _, err := runhostAgentValue(cmd, "lab", "messenger", args, args); err == nil {
		t.Fatal("a credential was kept with nobody at a terminal to agree and no --yes")
	}
}

// On the runhost a messenger's credential is an entry of its store, which
// the supervisor reads back through the keychain it is given there: no
// file under the runhost's directories holds it in the clear, and remove
// takes it away.
func TestARunhostKeepsAMessengersCredentialInItsStore(t *testing.T) {
	s := sealingRunhost(t)
	t.Cleanup(modelkey.UseKeychain(runhostModelKeys{}))
	const token = "123456:runhost-messenger-token"
	account := persistent.MessengerKeychainAccount("assistant", "telegram")
	keys := persistent.KeychainSecrets{}
	if err := keys.Set(account, token); err != nil {
		t.Fatal(err)
	}
	if got, err := keys.Get(account); err != nil || got != token {
		t.Fatalf("read back: %q %v", got, err)
	}
	st, err := openRunhostStore()
	if err != nil {
		t.Fatal(err)
	}
	if ents, err := st.Entries(); err != nil || len(ents) != 1 || !strings.HasPrefix(ents[0], "agent-") {
		t.Fatalf("the store's entries: %q %v", ents, err)
	}
	if err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // the test's own files
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("%s holds the credential in the clear", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := keys.Delete(account); err != nil {
		t.Fatal(err)
	}
	if got, err := keys.Get(account); err == nil && got != "" {
		t.Fatalf("remove left %q", got)
	}
}
