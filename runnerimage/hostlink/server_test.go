package hostlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPassword = "correct-horse-battery-staple"
	testSudo     = "sudo-secret-8812"
)

// fakeHost stands in for ssh and sftp: ssh answers a command from cmds
// (stdout, stderr, exit), sftp reads its batch and serves get from files
// and keeps what put sends.
type fakeHost struct {
	mu       sync.Mutex
	cmds     map[string]fakeResult
	files    map[string][]byte
	put      map[string][]byte
	chmods   []string
	argvs    [][]string
	envs     [][]string
	stdins   []string
	startErr string
}

type fakeResult struct {
	stdout, stderr string
	code           int
}

func (f *fakeHost) run(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argvs = append(f.argvs, argv)
	f.envs = append(f.envs, env)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdins = append(f.stdins, in)
	switch argv[0] {
	case "ssh":
		cmd := argv[len(argv)-1]
		if cmd == "true" && f.startErr != "" {
			_, _ = io.WriteString(stderr, f.startErr)
			return 255, nil
		}
		if strings.HasPrefix(cmd, "sleep") {
			<-ctx.Done()
			return -1, ctx.Err()
		}
		r, ok := f.cmds[cmd]
		if !ok && strings.HasPrefix(cmd, "sudo -S") {
			r, ok = f.cmds["sudo"]
		}
		if !ok {
			r = fakeResult{}
		}
		_, _ = io.WriteString(stdout, r.stdout)
		_, _ = io.WriteString(stderr, r.stderr)
		return r.code, nil
	case "sftp":
		sc := bufio.NewScanner(strings.NewReader(in))
		for sc.Scan() {
			f2 := splitQuoted(sc.Text())
			switch f2[0] {
			case "get":
				b, ok := f.files[f2[1]]
				if !ok {
					_, _ = io.WriteString(stderr, "sftp> get\nFile \""+f2[1]+"\" not found.\n")
					return 1, nil
				}
				if err := os.WriteFile(f2[2], b, 0o600); err != nil {
					return 1, err
				}
			case "put":
				b, err := os.ReadFile(f2[1])
				if err != nil {
					return 1, err
				}
				f.put[f2[2]] = b
			case "chmod":
				f.chmods = append(f.chmods, f2[1]+" "+f2[2])
			}
		}
		return 0, nil
	}
	return 127, nil
}

// splitQuoted reads one sftp batch line as sftp does for the host link's
// quoting: words, a quoted word unescaped.
func splitQuoted(line string) []string {
	var out []string
	var cur strings.Builder
	inQ, esc, any := false, false, false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && inQ:
			esc = true
		case r == '"':
			inQ, any = !inQ, true
		case r == ' ' && !inQ:
			if cur.Len() > 0 || any {
				out = append(out, cur.String())
			}
			cur.Reset()
			any = false
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 || any {
		out = append(out, cur.String())
	}
	return out
}

type fixture struct {
	cfg   Config
	fake  *fakeHost
	logs  string
	files string
}

func newFixture(t *testing.T, targets ...Target) *fixture {
	t.Helper()
	root := t.TempDir()
	hosts, logs, files, tmp := filepath.Join(root, "hosts"), filepath.Join(root, "log"), filepath.Join(root, "files"), filepath.Join(root, "tmp")
	for _, d := range []string{hosts, logs, files, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, tg := range targets {
		ids = append(ids, tg.ID)
		dir := filepath.Join(hosts, tg.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		write := func(name, v string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(tg)
		write(TargetFile, string(b))
		write(KnownHostsFile, HostKeyAlias+" ssh-ed25519 AAAA\n")
		if tg.Auth == AuthKey {
			write(KeyFile, "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n")
			if tg.Passphrase {
				write(PassphraseFile, "pass-phrase-77")
			}
		} else {
			write(PasswordFile, testPassword)
		}
		if tg.Sudo {
			write(SudoFile, testSudo)
		}
	}
	spec, _ := json.Marshal(Spec{SpecVersion: SpecVersion, Hosts: ids, MaxResultBytes: 4 << 10, MaxResultLines: 200})
	specPath := filepath.Join(root, "hostspec.json")
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHost{cmds: map[string]fakeResult{}, files: map[string][]byte{}, put: map[string][]byte{}}
	return &fixture{
		fake: fake, logs: logs, files: files,
		cfg: Config{
			SpecPath: specPath, HostsDir: hosts, LogDir: logs, FilesDir: files, TmpDir: tmp,
			Run:    fake.run,
			Lookup: func(a string) (string, error) { return "172.20.0." + a[len(a)-1:], nil },
			Now:    func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) },
		},
	}
}

// session serves the lines and returns each response by id.
func (fx *fixture) session(t *testing.T, calls ...string) (map[string]json.RawMessage, string, error) {
	t.Helper()
	var in bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}` + "\n")
	for i, c := range calls {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`+"\n", i+1, c)
	}
	var out, logw bytes.Buffer
	err := Serve(context.Background(), &in, &out, &logw, fx.cfg)
	res := map[string]json.RawMessage{}
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Error != nil {
			res[string(r.ID)] = r.Error
		} else {
			res[string(r.ID)] = r.Result
		}
	}
	return res, logw.String(), err
}

type callResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Resource struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"resource"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func answer(t *testing.T, raw json.RawMessage) (string, bool, string) {
	t.Helper()
	var r callResult
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Content) != 1 {
		t.Fatalf("not a tool result: %s", raw)
	}
	c := r.Content[0]
	if c.Type == "resource" {
		return c.Resource.Text, r.IsError, c.Type
	}
	return c.Text, r.IsError, c.Type
}

func call(tool string, args any) string {
	b, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	return string(b)
}

// TestAHostsOutputReachesTheAgentUnchanged is D9 and D19: a key printed on
// the host comes back as it is, in a resource the gateway's secret block
// does not read; the run's own password does not.
func TestAHostsOutputReachesTheAgentUnchanged(t *testing.T) {
	fx := newFixture(t, passwordTarget())
	planted := "AKIAIOSFODNN7EXAMPLE aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	fx.fake.cmds["cat ~/.aws/credentials"] = fakeResult{stdout: planted + "\n"}
	fx.fake.cmds["echo $PW"] = fakeResult{stdout: "pw is " + testPassword + " and " + base64.StdEncoding.EncodeToString([]byte(testPassword)) + "\n"}
	res, _, err := fx.session(t, call(ToolRun, map[string]any{"command": "cat ~/.aws/credentials"}), call(ToolRun, map[string]any{"command": "echo $PW"}))
	if err != nil {
		t.Fatal(err)
	}
	text, isErr, typ := answer(t, res["1"])
	if isErr || typ != "resource" || !strings.Contains(text, planted) || !strings.Contains(text, "exit status: 0") {
		t.Errorf("host_run answered (%s, error %v): %s", typ, isErr, text)
	}
	text, _, _ = answer(t, res["2"])
	if strings.Contains(text, testPassword) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(testPassword))) || !strings.Contains(text, Redacted) {
		t.Errorf("the run's own password reached the agent: %s", text)
	}
}

func TestSudoIsFedThePasswordTheAgentNeverSees(t *testing.T) {
	fx := newFixture(t, passwordTarget())
	fx.fake.cmds["sudo"] = fakeResult{stdout: "root\n"}
	res, _, err := fx.session(t, call(ToolRun, map[string]any{"command": "whoami", "sudo": true}))
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr, _ := answer(t, res["1"]); isErr || !strings.Contains(text, "root") {
		t.Fatalf("sudo answered %q", text)
	}
	last := len(fx.fake.argvs) - 1
	if got := fx.fake.stdins[last]; got != testSudo+"\n" {
		t.Errorf("sudo's stdin %q", got)
	}
	for _, a := range fx.fake.argvs[last] {
		if strings.Contains(a, testSudo) {
			t.Errorf("the sudo password is in ssh's argv: %q", a)
		}
	}
	for _, e := range fx.fake.envs[last] {
		if strings.Contains(e, testSudo) || strings.Contains(e, testPassword) {
			t.Errorf("a credential is in ssh's environment: %q", e)
		}
	}
	logb, _ := os.ReadFile(filepath.Join(fx.logs, LogFile("server")))
	if !strings.Contains(string(logb), `"command":"whoami","sudo":true`) || !strings.Contains(string(logb), `"exit":0`) {
		t.Errorf("the host's log: %s", logb)
	}
}

func TestSudoWithoutAPasswordIsRefused(t *testing.T) {
	tg := passwordTarget()
	tg.Sudo = false
	fx := newFixture(t, tg)
	res, _, _ := fx.session(t, call(ToolRun, map[string]any{"command": "whoami", "sudo": true}))
	if text, isErr, _ := answer(t, res["1"]); !isErr || !strings.Contains(text, "no sudo password") {
		t.Errorf("answered %q", text)
	}
}

func TestALargeOutputIsKeptAndPaged(t *testing.T) {
	fx := newFixture(t, passwordTarget())
	var big strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&big, "line %04d of the journal\n", i)
	}
	fx.fake.cmds["journalctl"] = fakeResult{stdout: big.String()}
	res, _, err := fx.session(t,
		call(ToolRun, map[string]any{"command": "journalctl"}),
		call(ToolRun, map[string]any{"continue": 1, "offset": 3000}),
		call(ToolRun, map[string]any{"continue": 9}))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := answer(t, res["1"])
	if !strings.HasPrefix(first, "[call 1]\nexit status: 0\nstdout (") || !strings.Contains(first, `continue with host_run with {"continue": 1, "offset": `) || len(first) > 4<<10 {
		t.Errorf("first page (%d bytes): %.300s … %s", len(first), first, first[max(0, len(first)-200):])
	}
	next, _, _ := answer(t, res["2"])
	if !strings.Contains(next, "[page: bytes 3000–") {
		t.Errorf("next page: %.200s", next)
	}
	if text, isErr, _ := answer(t, res["3"]); !isErr || !strings.Contains(text, "no kept output") {
		t.Errorf("an unknown call: %q", text)
	}
}

func TestATimedOutCommandIsEnded(t *testing.T) {
	fx := newFixture(t, passwordTarget())
	res, _, _ := fx.session(t, call(ToolRun, map[string]any{"command": "sleep 99", "timeout": 1}))
	if text, isErr, _ := answer(t, res["1"]); isErr || !strings.Contains(text, "ended after 1 seconds") {
		t.Errorf("answered %q", text)
	}
}

func TestTheStartCheckNamesWhyAHostRefused(t *testing.T) {
	for stderr, detail := range map[string]string{
		"admin@agentry-target: Permission denied (publickey,password).":   RefusedAuth,
		"@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@":        RefusedHostKey,
		"ssh: connect to host 172.20.0.4 port 2222: Connection refused\n": RefusedReach,
	} {
		fx := newFixture(t, passwordTarget())
		fx.fake.startErr = stderr + " " + testPassword
		_, logw, err := fx.session(t)
		if err == nil {
			t.Errorf("%s: the host link served", detail)
			continue
		}
		want := "target host server refused (" + detail + "): "
		if !strings.Contains(logw, want) || strings.Contains(logw, testPassword) {
			t.Errorf("%s: stderr %q", detail, logw)
		}
	}
}

func TestTheFileToolsMoveFilesThroughTheSharedFolder(t *testing.T) {
	fx := newFixture(t, keyTarget())
	fx.fake.files["/etc/hostname"] = []byte("box\n")
	fx.fake.files["/bin/tool"] = []byte{0x7f, 'E', 'L', 'F', 0, 1, 2}
	fx.fake.files["/var/log/app.log"] = []byte("app log\n")
	if err := os.WriteFile(filepath.Join(fx.files, "setup.sh"), []byte("#!/bin/sh\necho hi\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	res, _, err := fx.session(t,
		call(ToolRead, map[string]any{"path": "/etc/hostname"}),
		call(ToolRead, map[string]any{"path": "/bin/tool"}),
		call(ToolWrite, map[string]any{"path": "/etc/motd", "content": "hello", "mode": "0640"}),
		call(ToolPut, map[string]any{"from": "/scratch/hosts/setup.sh", "to": "setup.sh", "mode": "0755"}),
		call(ToolGet, map[string]any{"from": "/var/log/app.log", "to": "logs/app.log"}),
		call(ToolPut, map[string]any{"from": "/scratch/other.txt", "to": "x"}),
		call(ToolGet, map[string]any{"from": "/nope", "to": "x"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if text, _, typ := answer(t, res["1"]); text != "box\n" || typ != "resource" {
		t.Errorf("host_read text: %q (%s)", text, typ)
	}
	if text, _, _ := answer(t, res["2"]); !strings.Contains(text, base64.StdEncoding.EncodeToString(fx.fake.files["/bin/tool"])) {
		t.Errorf("host_read binary: %q", text)
	}
	if got := string(fx.fake.put["/etc/motd"]); got != "hello" {
		t.Errorf("host_write put %q", got)
	}
	if got := string(fx.fake.put["setup.sh"]); !strings.Contains(got, "echo hi") {
		t.Errorf("host_put put %q", got)
	}
	if strings.Join(fx.fake.chmods, ",") != `640,/etc/motd,755,setup.sh` && strings.Join(fx.fake.chmods, ",") != "640 /etc/motd,755 setup.sh" {
		t.Errorf("chmods %q", fx.fake.chmods)
	}
	if b, err := os.ReadFile(filepath.Join(fx.files, "logs", "app.log")); err != nil || string(b) != "app log\n" {
		t.Errorf("host_get copied %q, %v", b, err)
	}
	if text, isErr, _ := answer(t, res["6"]); !isErr || !strings.Contains(text, "not under /scratch/hosts") {
		t.Errorf("a put from outside the shared folder: %q", text)
	}
	if text, isErr, _ := answer(t, res["7"]); !isErr || !strings.Contains(text, "not found") {
		t.Errorf("a get of a missing file: %q", text)
	}
}

func TestASharedFolderLinkIsNotFollowedOut(t *testing.T) {
	fx := newFixture(t, keyTarget())
	secret := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(fx.files, "link")); err != nil {
		t.Fatal(err)
	}
	res, _, _ := fx.session(t, call(ToolPut, map[string]any{"from": "link", "to": "x"}), call(ToolPut, map[string]any{"from": "../../etc/passwd", "to": "x"}))
	for _, id := range []string{"1", "2"} {
		if _, isErr, _ := answer(t, res[id]); !isErr {
			t.Errorf("call %s copied from outside the shared folder", id)
		}
	}
	if len(fx.fake.put) != 0 {
		t.Errorf("put %v", fx.fake.put)
	}
}

func TestTwoHostsAreNamedByTheirQuestions(t *testing.T) {
	a, b := passwordTarget(), keyTarget()
	a.ID, b.ID, b.Address = "web", "db", "203.0.113.5"
	fx := newFixture(t, a, b)
	res, _, err := fx.session(t, call(ToolRun, map[string]any{"command": "uptime"}), call(ToolRun, map[string]any{"host": "db", "command": "uptime"}))
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr, _ := answer(t, res["1"]); !isErr || !strings.Contains(text, "name the target host") {
		t.Errorf("no host named: %q", text)
	}
	last := fx.fake.argvs[len(fx.fake.argvs)-1]
	if last[len(last)-2] != "172.20.0.5" {
		t.Errorf("db's call went to %q", last[len(last)-2])
	}
}

func TestAskpassPrintsOnlyAHostsPasswordOrPassphrase(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.MkdirAll(filepath.Join(hosts, "server"), 0o700); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(hosts, "server", PasswordFile)
	if err := os.WriteFile(pw, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if code := askpass(pw, hosts, &out, &errw); code != 0 || out.String() != testPassword+"\n" {
		t.Errorf("askpass = %d, %q, %q", code, out.String(), errw.String())
	}
	for _, bad := range []string{"", filepath.Join(hosts, "server", KeyFile), hosts + "/server/../server/" + PasswordFile, "/etc/shadow", filepath.Join(dir, "server", PasswordFile)} {
		out.Reset()
		if code := askpass(bad, hosts, &out, &errw); code == 0 || out.Len() != 0 {
			t.Errorf("askpass printed %q for %q", out.String(), bad)
		}
	}
}
