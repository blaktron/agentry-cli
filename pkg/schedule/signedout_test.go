package schedule

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func signedOutEntry() Entry {
	e := goldenEntry()
	e.Env = append(e.Env, "AGENTRY_NO_KEYRING=1")
	e.Credential = "/home/p/.agentry/runner/schedules/abcdefghij/keys.cred"
	return e
}

// Golden: a signed-out schedule's service loads its encrypted credential
// and turns the keychain off; it holds a path, never a key (invariant 7).
func TestSystemdUnitsSignedOutGolden(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID, sc.SignedOut = "abcdefghij", true
	service, timer, err := SystemdUnits(sc, signedOutEntry())
	if err != nil {
		t.Fatal(err)
	}
	wantService := `# Written by agentry schedule; the schedule itself is abcdefghij in the runner home.
[Unit]
Description=Agentry schedule: Morning digest

[Service]
Type=oneshot
LoadCredentialEncrypted=agentry-keys:/home/p/.agentry/runner/schedules/abcdefghij/keys.cred
Environment="PATH=/usr/local/bin:/usr/bin"
Environment="AGENTRY_DOCKER=/usr/bin/docker"
Environment="AGENTRY_NO_KEYRING=1"
ExecStart="/usr/bin/agentry" "schedule" "fire" "abcdefghij"
`
	if service != wantService {
		t.Errorf("service:\n%s\nwant:\n%s", service, wantService)
	}
	// The timer is the signed-in one's: Persistent=true catches up once at
	// boot after the computer was off (D19).
	_, plainTimer, _ := SystemdUnits(sc, goldenEntry())
	if timer != plainTimer || !strings.Contains(timer, "Persistent=true") {
		t.Errorf("timer differs from a signed-in schedule's:\n%s", timer)
	}

	e := signedOutEntry()
	e.Credential = "/home/p/100%/keys.cred"
	service, _, err = SystemdUnits(sc, e)
	if err != nil || !strings.Contains(service, "LoadCredentialEncrypted=agentry-keys:/home/p/100%%/keys.cred\n") {
		t.Fatalf("percent not escaped: %v\n%s", err, service)
	}
	for _, bad := range []string{"keys.cred", "/home/p/x\nExecStart=/bin/sh"} {
		e.Credential = bad
		if _, _, err := SystemdUnits(sc, e); err == nil {
			t.Fatalf("credential %q accepted", bad)
		}
	}
}

func TestSystemdProbeLoadsTheCredential(t *testing.T) {
	unixAlarm(t)
	var got []string
	s := &Systemd{SystemdRun: func(args ...string) ([]byte, error) {
		got = args
		return []byte("ok\n"), nil
	}}
	e := signedOutEntry()
	e.Args = []string{"schedule", "probe"}
	if _, err := s.Probe(e); err != nil {
		t.Fatal(err)
	}
	want := []string{"--wait", "--pipe", "--collect", "--quiet",
		"--property=LoadCredentialEncrypted=agentry-keys:/home/p/.agentry/runner/schedules/abcdefghij/keys.cred",
		"--setenv=PATH=/usr/local/bin:/usr/bin", "--setenv=AGENTRY_DOCKER=/usr/bin/docker", "--setenv=AGENTRY_NO_KEYRING=1",
		"--", "/usr/bin/agentry", "schedule", "probe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args %q\nwant %q", got, want)
	}
}

// launchd and Task Scheduler never take a signed-out entry: Docker
// Desktop stops at sign-out there (D17).
func TestOtherAlarmsRefuseACredential(t *testing.T) {
	sc := sample(t)
	sc.ID = "abcdefghij"
	if _, err := LaunchAgentPlist(sc, signedOutEntry(), time.UTC, sc.Created); !errors.Is(err, ErrSignedOutUnsupported) {
		t.Fatalf("plist: %v", err)
	}
	if _, err := TaskXML(sc, signedOutEntry(), "S-1-5-21-1", `C:\Windows\System32\conhost.exe`, time.UTC, sc.Created); !errors.Is(err, ErrSignedOutUnsupported) {
		t.Fatalf("task: %v", err)
	}
}

func TestSealAccounts(t *testing.T) {
	pol := &policy.Policy{Model: policy.Model{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring"}}
	pol.Judge.APIKeySource = "keyring"
	pol.WebSearch = policy.WebSearchKey{Provider: "tavily", APIKeySource: "keyring"}
	pol.HybridAnalysis.APIKeySource = "env"
	req, opt := SealAccounts(pol)
	if !reflect.DeepEqual(req, []string{"provider:openai"}) || !reflect.DeepEqual(opt, []string{"provider:typesafe", "provider:tavily"}) {
		t.Fatalf("SealAccounts = %v %v", req, opt)
	}
	// An adapter's own block can take the key from the keychain.
	pol = &policy.Policy{Model: policy.Model{Provider: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		Harness: map[string]policy.HarnessPolicy{"claude": {APIKeySource: "keyring"}}}
	if req, _ := SealAccounts(pol); !reflect.DeepEqual(req, []string{"provider:anthropic"}) {
		t.Fatalf("harness block: %v", req)
	}
	pol.Harness = nil
	if req, opt := SealAccounts(pol); len(req)+len(opt) != 0 {
		t.Fatalf("env-only policy seals %v %v", req, opt)
	}
}

// fakeEncrypt stands in for systemd-creds: it records what came on stdin
// and writes a "ciphertext" that does not hold it.
func fakeEncrypt(t *testing.T) *[]byte {
	t.Helper()
	var got []byte
	saved := EncryptCredential
	t.Cleanup(func() { EncryptCredential = saved })
	EncryptCredential = func(name string, plaintext []byte, out string) error {
		if name != modelkey.CredentialName {
			t.Fatalf("credential name %q", name)
		}
		got = append([]byte{}, plaintext...)
		return os.WriteFile(out, []byte("ENCRYPTED"), 0o644)
	}
	return &got
}

func TestSealCredentialWritesOnlyCiphertext(t *testing.T) {
	plain := fakeEncrypt(t)
	s := Store{Dir: t.TempDir()}
	sc := sample(t)
	sc.SignedOut = true
	if err := s.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingCredentialPath()
	if err != nil {
		t.Fatal(err)
	}
	const key = "sk-sealed-749-never-in-the-clear"
	if err := SealCredential(pending, map[string]string{"provider:openai": key}); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveCredential(pending, sc.ID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(*plain, []byte(key)) {
		t.Fatal("the key never reached systemd-creds")
	}
	path, _ := s.CredentialPath(sc.ID)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential %v %v, want 0600", fi, err)
	}
	// Invariant 7: nothing in the store holds the key in the clear.
	_ = filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(key)) {
			t.Errorf("%s holds the key in the clear", p)
		}
		return nil
	})
	// A failed encryption leaves no file behind.
	EncryptCredential = func(string, []byte, string) error { return errors.New("no tpm") }
	if err := SealCredential(path, map[string]string{"provider:openai": "k2"}); err == nil {
		t.Fatal("a failed encryption was not reported")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed encryption left its temp file")
	}
}

func TestParseSystemdVersion(t *testing.T) {
	for out, want := range map[string]int{"systemd 259 (259.8-1.fc44)\n+PAM +AUDIT": 259, "systemd 255 (255.4-1ubuntu8)": 255} {
		if got, err := parseSystemdVersion(out); err != nil || got != want {
			t.Errorf("%q = %d %v", out, got, err)
		}
	}
	if _, err := parseSystemdVersion("bash: systemctl: not found"); err == nil {
		t.Error("nonsense parsed")
	}
}

// fakeLinger is logind's linger folder, which the fake loginctl writes.
func fakeLinger(t *testing.T) (*Linger, *[]string) {
	t.Helper()
	l := &Linger{User: "p", Dir: t.TempDir()}
	var calls []string
	l.Loginctl = func(_ io.Reader, _ io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "enable-linger":
			return os.WriteFile(filepath.Join(l.Dir, l.User), nil, 0o644)
		case "disable-linger":
			return os.Remove(filepath.Join(l.Dir, l.User))
		}
		return errors.New("unknown loginctl verb")
	}
	return l, &calls
}

func TestLingerTurnedOnAndOffOnlyByTheCLI(t *testing.T) {
	l, calls := fakeLinger(t)
	s := Store{Dir: t.TempDir()}
	sc := sample(t)
	sc.SignedOut = true
	if err := s.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	now := sc.Created
	if on, err := s.EnableLinger(l, nil, io.Discard, now); err != nil || !on {
		t.Fatalf("EnableLinger = %v %v", on, err)
	}
	// Once on, it is not asked for again.
	if on, err := s.EnableLinger(l, nil, io.Discard, now); err != nil || on {
		t.Fatalf("second EnableLinger = %v %v", on, err)
	}
	// While a signed-out schedule is left, release does nothing.
	if off, err := s.ReleaseLinger(l, nil, io.Discard); err != nil || off {
		t.Fatalf("ReleaseLinger with one left = %v %v", off, err)
	}
	// An ended one does not count; the last one gone turns linger off.
	if err := s.SaveState(sc.ID, State{Ended: true}); err != nil {
		t.Fatal(err)
	}
	if off, err := s.ReleaseLinger(l, nil, io.Discard); err != nil || !off {
		t.Fatalf("ReleaseLinger = %v %v", off, err)
	}
	if on, _ := l.On(); on {
		t.Fatal("linger still on")
	}
	if want := []string{"enable-linger", "disable-linger"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("loginctl calls %v, want %v", *calls, want)
	}
	// The record went with it: a second release asks nothing.
	if off, _ := s.ReleaseLinger(l, nil, io.Discard); off {
		t.Fatal("released twice")
	}
}

func TestLingerThePersonTurnedOnIsLeftOn(t *testing.T) {
	l, calls := fakeLinger(t)
	if err := os.WriteFile(filepath.Join(l.Dir, l.User), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: t.TempDir()}
	if on, err := s.EnableLinger(l, nil, io.Discard, time.Now()); err != nil || on {
		t.Fatalf("EnableLinger = %v %v", on, err)
	}
	if off, err := s.ReleaseLinger(l, nil, io.Discard); err != nil || off {
		t.Fatalf("ReleaseLinger = %v %v", off, err)
	}
	if on, _ := l.On(); !on || len(*calls) != 0 {
		t.Fatalf("the person's linger was touched: on %v, calls %v", on, *calls)
	}
}

func TestLingerRefusedByPolkitIsNotRecorded(t *testing.T) {
	l, _ := fakeLinger(t)
	l.Loginctl = func(io.Reader, io.Writer, ...string) error { return errors.New("Access denied") }
	s := Store{Dir: t.TempDir()}
	if _, err := s.EnableLinger(l, nil, io.Discard, time.Now()); err == nil {
		t.Fatal("a refused enable-linger was not reported")
	}
	if rec, _ := s.lingerRecord(); rec.Ours {
		t.Fatal("a refused enable-linger was recorded as the CLI's")
	}
}
