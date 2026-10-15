package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

const (
	plantedHostPassword = "planted-host-password-0451"
	plantedSudo         = "planted-sudo-password-9910"
)

// targetHostFixture is the engine fixture with a target host declared, and one
// readied as the CLI readies it: an address, a key the person trusted, a
// password and a sudo password.
func targetHostFixture(t *testing.T) (Params, *dockerx.Fake, *targethost.Host) {
	t.Helper()
	p, f := fixtureEdited(t, nil, declareTargetHost)
	h := readiedTargetHost(t)
	p.Intake = &intake.Answers{Hosts: map[string]*targethost.Host{"server": h}}
	return p, f, h
}

// declareTargetHost is the manifest edit that asks for one target host.
func declareTargetHost(m *manifest.Manifest) {
	m.Intake = append(m.Intake, manifest.IntakeQuestion{ID: "server", Prompt: "The server to set up", Type: manifest.IntakeHost, Required: true})
	m.Capabilities.TargetHosts = []string{"server"}
}

// readiedTargetHost is a target host readied as the CLI readies it, and
// the host link image stubbed.
func readiedTargetHost(t *testing.T) *targethost.Host {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	h := &targethost.Host{
		ID:      "server",
		Answer:  targethost.Answer{User: "admin", Host: "box.example", Port: 2222},
		Address: netip.MustParseAddr("203.0.113.4"),
		HostKey: targethost.HostKey{Key: key, From: targethost.TrustedNow},
		Credential: targethost.Credential{Auth: hostlink.AuthPassword, Password: plantedHostPassword, From: targethost.FromFlag,
			Sudo: plantedSudo, SudoFrom: targethost.FromPrompt},
	}
	prev := ensureHostLinkImage
	ensureHostLinkImage = func(_ context.Context, _ dockerx.Client, s targethost.ImageSpec, _ string, _ io.Writer) (targethost.Image, error) {
		return targethost.Image{Tag: s.Tag, Base: s.Base, Packages: []string{"openssh-client-default-10.0_p1-r10"}}, nil
	}
	t.Cleanup(func() { ensureHostLinkImage = prev })
	return h
}

func credentialForms() []string {
	var out []string
	for _, v := range []string{plantedHostPassword, plantedSudo} {
		out = append(out, v, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return out
}

func assertNoHostCredential(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // the test's own run folders
		for _, v := range credentialForms() {
			if rerr == nil && strings.Contains(string(b), v) {
				t.Errorf("%s holds a target host's sign-in (%.6s…)", p, v)
			}
		}
		return nil
	})
}

// TestATargetHostIsReachedThroughTheHostLinkAlone: the sign-in is
// in no container's argv, environment or mounts the runner makes (the
// gateway launches the host link with the folder bound into it alone),
// in no file the run leaves, raw or base64; the agent container gains no
// network; and the receipt names the host as the run reached it.
func TestATargetHostIsReachedThroughTheHostLinkAlone(t *testing.T) {
	p, f, h := targetHostFixture(t)
	happyFake(f)
	p.KeepRunDir = true
	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")
	hostsDir := filepath.Join(runDir, "cfg", spec.HostsDir)
	var duringRun struct {
		mode    fs.FileMode
		dirMode fs.FileMode
		pw      string
		target  string
		known   string
	}
	f.Foreground["-agent"].Before = func(dockerx.RunSpec) {
		if fi, err := os.Stat(filepath.Join(hostsDir, "server", hostlink.PasswordFile)); err == nil {
			duringRun.mode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Join(hostsDir, "server")); err == nil {
			duringRun.dirMode = fi.Mode().Perm()
		}
		b, _ := os.ReadFile(filepath.Join(hostsDir, "server", hostlink.PasswordFile))
		duringRun.pw = string(b)
		b, _ = os.ReadFile(filepath.Join(hostsDir, "server", hostlink.TargetFile))
		duringRun.target = string(b)
		b, _ = os.ReadFile(filepath.Join(hostsDir, "server", hostlink.KnownHostsFile))
		duringRun.known = string(b)
		// The host link logs two calls, one quoting the run's password.
		log := `{"time":"t","tool":"host_run","command":"echo ` + plantedHostPassword + `","exit":0}` + "\n" +
			`{"time":"t","tool":"host_read","path":"/etc/hostname","bytes":4}` + "\n"
		_ = os.WriteFile(filepath.Join(runDir, "cfg", spec.HostLogDir, hostlink.LogFile("server")), []byte(log), 0o600)
		// And a shell session's transcript, where the host printed it
		// back; another host's name is not this run's.
		shell := "── session 1: a login shell on server\n$ cat pw.txt\n" + plantedHostPassword + "\n── closed by the agent\n"
		_ = os.WriteFile(filepath.Join(runDir, "cfg", spec.HostLogDir, hostlink.ShellLogFile("server", 1)), []byte(shell), 0o600)
		_ = os.WriteFile(filepath.Join(runDir, "cfg", spec.HostLogDir, hostlink.ShellLogFile("other", 1)), []byte(shell), 0o600)
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if duringRun.pw != plantedHostPassword || duringRun.mode != 0o600 || duringRun.dirMode != 0o700 {
		t.Fatalf("the host's folder during the run: %q, file %v, folder %v", duringRun.pw, duringRun.mode, duringRun.dirMode)
	}
	tgt, err := hostlink.ParseTarget([]byte(duringRun.target))
	if err != nil || tgt.Address != "203.0.113.4" || tgt.Port != 2222 || tgt.User != "admin" || tgt.Auth != hostlink.AuthPassword || !tgt.Sudo ||
		tgt.HostKeyAlgorithm != "ssh-ed25519" || !strings.HasPrefix(duringRun.known, hostlink.HostKeyAlias+" ssh-ed25519 ") {
		t.Fatalf("target.json %s (%v), known_hosts %q", duringRun.target, err, duringRun.known)
	}

	// The gateway's catalog, as the run rendered it: one relay to the
	// address and port, the host link as the person's uid.
	cat, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := hostLinkUser()
	for _, want := range []string{"203.0.113.4:2222/tcp", hostsDir + ":" + hostlink.HostsMount + ":ro", "user: '" + user + "'"} {
		if !strings.Contains(string(cat), want) {
			t.Errorf("the rendered catalog lacks %q:\n%s", want, cat)
		}
	}
	if strings.Contains(string(cat), "@@RUN_") {
		t.Errorf("a placeholder is left in the rendered catalog:\n%s", cat)
	}

	for _, c := range f.Calls {
		r := c.Run
		for _, v := range credentialForms() {
			for _, a := range r.Argv {
				if strings.Contains(a, v) {
					t.Errorf("%s: a sign-in is in its argv", r.Name)
				}
			}
			for k, e := range r.Env {
				if strings.Contains(e, v) || strings.Contains(os.Getenv(k), v) {
					t.Errorf("%s: a sign-in is in its environment (%s)", r.Name, k)
				}
			}
		}
		for _, m := range r.Mounts {
			if strings.HasPrefix(m.Source, hostsDir) {
				t.Errorf("%s mounts the host folder %s: only the host link may, and the gateway launches it", r.Name, m.Source)
			}
		}
	}
	agent, ok := f.FindRun("-agent")
	if !ok || agent.Network != "agentry-cafe0000-net" || len(agent.ExtraNetworks) != 0 {
		t.Fatalf("the agent container's network: %+v", agent)
	}

	// The sign-ins die with the run, kept run folder or not, and no file
	// the run leaves holds one, raw or base64.
	if _, err := os.Stat(hostsDir); !os.IsNotExist(err) {
		t.Errorf("the host folder outlived the run: %v", err)
	}
	assertNoHostCredential(t, p.OutDir)
	assertNoHostCredential(t, runDir)

	line := env.CapabilitiesEnforced["host.server"]
	for _, want := range []string{"admin at box.example (203.0.113.4 resolved by this machine), port 2222", ssh.FingerprintSHA256(h.HostKey.Key),
		targethost.TrustedNow, "a password given by a flag", "a sudo password typed at the run's start", "2 calls", "outside the run",
		"1 shell session, its transcript in receipts/host-server-shell-1.log"} {
		if !strings.Contains(line, want) {
			t.Errorf("host.server line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, plantedHostPassword) {
		t.Error("the host line holds the password")
	}
	if c := env.CapabilitiesEnforced["containers"]; !strings.Contains(c, "the host link") {
		t.Errorf("containers line: %s", c)
	}
	if img := env.CapabilitiesEnforced["hostLink.image"]; !strings.Contains(img, "openssh-client-default") {
		t.Errorf("hostLink.image line: %s", img)
	}
	log, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", hostlink.LogFile("server")))
	if err != nil || !strings.Contains(string(log), "/etc/hostname") || strings.Contains(string(log), plantedHostPassword) {
		t.Errorf("receipts/host-server.log: %s (%v)", log, err)
	}
	transcript, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", hostlink.ShellLogFile("server", 1)))
	if err != nil || !strings.Contains(string(transcript), "$ cat pw.txt") || strings.Contains(string(transcript), plantedHostPassword) {
		t.Errorf("receipts/host-server-shell-1.log: %s (%v)", transcript, err)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", hostlink.ShellLogFile("other", 1))); !os.IsNotExist(err) {
		t.Errorf("a transcript of a host the run does not have was kept: %v", err)
	}
	doc, _ := os.ReadFile(filepath.Join(runDir, "inputs", "intake.json"))
	if !strings.Contains(string(doc), `"address": "203.0.113.4"`) && !strings.Contains(string(doc), `"address":"203.0.113.4"`) {
		t.Errorf("intake.json does not name the host: %s", doc)
	}
}

// TestAHostedRunRefusesATargetHost: before any container starts,
// whatever the release declares.
func TestAHostedRunRefusesATargetHost(t *testing.T) {
	p, f, _ := targetHostFixture(t)
	happyFake(f)
	p.InputReview = &fakeReview{}
	env, err := Run(context.Background(), f, p)
	if env == nil || env.ExitCode != ExitPolicy {
		t.Fatalf("hosted run: %v (%+v)", err, env)
	}
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: receipt.HostHosted, Names: []string{"server"}})
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunDetached") || strings.HasPrefix(op, "RunForeground") || strings.HasPrefix(op, "Pull") || strings.HasPrefix(op, "NetworkCreate") {
			t.Errorf("a hosted run made %s before it refused", op)
		}
	}
}

func TestATargetHostThatWasNotReadiedIsRefused(t *testing.T) {
	p, f, h := targetHostFixture(t)
	happyFake(f)
	h.HostKey = targethost.HostKey{}
	env, err := Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: receipt.HostCredential, Names: []string{"server"}})
	p, f, _ = targetHostFixture(t)
	happyFake(f)
	p.Intake = nil
	env, err = Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: receipt.HostCredential, Names: []string{"server"}})
}

// TestAHostThatRefusesTheSignInStopsTheRun: the host link's start check
// names why, and the run stops before the agent with that reason.
func TestAHostThatRefusesTheSignInStopsTheRun(t *testing.T) {
	for detail, want := range map[string]struct {
		code   int
		reason string
	}{
		hostlink.RefusedAuth:    {ExitPolicy, receipt.HostAuth},
		hostlink.RefusedHostKey: {ExitPolicy, receipt.HostKeyChanged},
		hostlink.RefusedReach:   {ExitGateway, receipt.HostUnreachable},
	} {
		p, f, _ := targetHostFixture(t)
		happyFake(f)
		f.LogsByName["agentry-cafe0000-gateway"] = "  - " + hostlink.ServerName + ": " + hostlink.RefusedLine("server", detail, "Permission denied (password) "+plantedHostPassword) + "\n" +
			"  > Can't start " + hostlink.ServerName + ": exit 1\n"
		env, err := Run(context.Background(), f, p)
		if env == nil || env.ExitCode != want.code {
			t.Fatalf("%s: exit %v, %v", detail, env, err)
		}
		wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: want.reason, Names: []string{"server"}})
		if strings.Contains(err.Error(), plantedHostPassword) {
			t.Errorf("%s: the error holds the password", detail)
		}
		if slices.Contains(f.Ops(), "RunForeground:agentry-cafe0000-agent") {
			t.Errorf("%s: the agent started", detail)
		}
	}
}

// TestARunWithoutATargetHostTakesNone: a host given to a release that
// declares none is refused, never quietly carried.
func TestARunWithoutATargetHostTakesNone(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.Intake = &intake.Answers{Hosts: map[string]*targethost.Host{"server": {ID: "server"}}}
	if _, err := Run(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "declares none") {
		t.Fatalf("Run: %v", err)
	}
}

func TestTheHostLinksPasswdNamesItsUser(t *testing.T) {
	got := string(hostLinkPasswd(1000, 1000))
	if !strings.Contains(got, "agentry:x:1000:1000::/tmp:/sbin/nologin\n") || !strings.Contains(got, "hostlink:x:10001:") {
		t.Errorf("passwd %q", got)
	}
	if strings.Contains(string(hostLinkPasswd(10001, 10001)), "agentry:") || strings.Contains(string(hostLinkPasswd(0, 0)), "agentry:") {
		t.Error("a uid the image already names was added again")
	}
}

// TestTheEngineHoldsTheAddressToTheRuleWhoeverReadiedIt: a host handed in
// at loopback, or at an address the policy denies, is refused whatever
// readied it.
func TestTheEngineHoldsTheAddressToTheRuleWhoeverReadiedIt(t *testing.T) {
	p, f, h := targetHostFixture(t)
	happyFake(f)
	h.Address = netip.MustParseAddr("127.0.0.1")
	env, err := Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: receipt.HostAddress, Names: []string{"server"}})
	p, f, _ = targetHostFixture(t)
	happyFake(f)
	p.Policy.Hosts.Deny = []string{"203.0.113.0/24"}
	env, err = Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonHostRefused, Detail: receipt.HostAddress, Names: []string{"server"}})
}

// TestASessionTakesItsTargetHostAtStartAndKeepsItToItsEnd: the host is
// readied before the session starts, as a run's is; the host link's sign-in files are in place for as long as the session
// lasts, its turns included, and gone when it ends; the host's calls and
// shell transcripts become receipts and the receipt names the host.
func TestASessionTakesItsTargetHostAtStartAndKeepsItToItsEnd(t *testing.T) {
	p, f := sessionFixtureEdited(t, declareTargetHost)
	h := readiedTargetHost(t)
	p.Intake = &intake.Answers{Hosts: map[string]*targethost.Host{"server": h}}
	happyFake(f)
	harness := &fakeHarness{}
	runDir := filepath.Join(p.RunRoot, "runs", "5e5510")
	hostsDir := filepath.Join(runDir, "cfg", spec.HostsDir)
	var duringTurn string
	harness.onPrompt = func(string) {
		b, _ := os.ReadFile(filepath.Join(hostsDir, "server", hostlink.PasswordFile))
		duringTurn = string(b)
		log := `{"time":"t","tool":"host_run","command":"uptime","exit":0}` + "\n"
		_ = os.WriteFile(filepath.Join(runDir, "cfg", spec.HostLogDir, hostlink.LogFile("server")), []byte(log), 0o600)
		shell := "── session 1: a login shell on server\n$ sudo -v\n" + plantedSudo + "\n── closed by the agent\n"
		_ = os.WriteFile(filepath.Join(runDir, "cfg", spec.HostLogDir, hostlink.ShellLogFile("server", 1)), []byte(shell), 0o600)
	}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: harness.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	cat, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml"))
	if err != nil || !strings.Contains(string(cat), "203.0.113.4:2222/tcp") || !strings.Contains(string(cat), hostsDir+":"+hostlink.HostsMount+":ro") {
		t.Fatalf("the session's catalog has no host link to the host (%v):\n%s", err, cat)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "check the server"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	env, err := s.End(context.Background(), false)
	<-done
	_ = get
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if duringTurn != plantedHostPassword {
		t.Fatalf("during a turn the host's sign-in file held %q", duringTurn)
	}
	if _, err := os.Stat(hostsDir); !os.IsNotExist(err) {
		t.Errorf("the host folder outlived the session: %v", err)
	}
	line := env.CapabilitiesEnforced["host.server"]
	for _, want := range []string{"admin at box.example (203.0.113.4 resolved by this machine), port 2222", "1 calls", "1 shell session"} {
		if !strings.Contains(line, want) {
			t.Errorf("host.server line lacks %q: %s", want, line)
		}
	}
	if b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, hostlink.LogFile("server"))); err != nil || !strings.Contains(string(b), "uptime") {
		t.Errorf("receipts/host-server.log: %s (%v)", b, err)
	}
	assertNoHostCredential(t, p.OutDir)
	assertNoHostCredential(t, runDir)
}
