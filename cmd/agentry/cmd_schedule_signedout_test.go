package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// the value of the model key the fakes hold; no file may contain it.
const sealedTestKey = "sk-749-sealed-never-in-the-clear"

// signedOutHarness is scheduleHarness with Linux, systemd 259, Docker
// Engine, a fake linger, a fake systemd-creds and a fake keychain.
type signedOutHarness struct {
	*scheduleHarness
	linger    *schedule.Linger
	loginctl  []string
	sealed    [][]byte // what systemd-creds was handed, per call
	policy    string
	accounts  []string // what was read from the keychain to seal
	dockerOS  string
	systemdV  int
	collected map[string]string
}

func newSignedOutHarness(t *testing.T) *signedOutHarness {
	t.Helper()
	isolateEnv(t)
	h := &signedOutHarness{scheduleHarness: newScheduleHarness(t), dockerOS: "Fedora Linux 44 (Server Edition)", systemdV: 259}
	h.now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	home, _ := runnerHome()
	h.policy = filepath.Join(home, "policy.yaml")
	pol, _, err := policy.EnsureDefault(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.APIKeySource = modelkey.SourceKeyring
	if err := policy.Save(h.policy, pol); err != nil {
		t.Fatal(err)
	}
	account := modelkey.Account(pol.Model.Provider)
	h.collected = map[string]string{account: sealedTestKey}

	// fireGaps asks the keychain whether the model key is there; this
	// process answers from a credential folder, as a test has no keychain.
	plain, _ := modelkey.SealPlaintext(map[string]string{account: sealedTestKey})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modelkey.CredentialName), plain, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(modelkey.EnvCredentialsDirectory, dir)

	h.linger = &schedule.Linger{User: "p", Dir: t.TempDir()}
	h.linger.Loginctl = func(_ io.Reader, _ io.Writer, args ...string) error {
		h.loginctl = append(h.loginctl, args[0])
		p := filepath.Join(h.linger.Dir, h.linger.User)
		if args[0] == "enable-linger" {
			return os.WriteFile(p, nil, 0o644)
		}
		return os.Remove(p)
	}
	savedGOOS, savedOS, savedLinger, savedCollect := scheduleGOOS, scheduleDockerOS, scheduleLinger, scheduleCollectKeys
	savedVersion, savedEncrypt := schedule.SystemdVersion, schedule.EncryptCredential
	t.Cleanup(func() {
		scheduleGOOS, scheduleDockerOS, scheduleLinger, scheduleCollectKeys = savedGOOS, savedOS, savedLinger, savedCollect
		schedule.SystemdVersion, schedule.EncryptCredential = savedVersion, savedEncrypt
	})
	scheduleGOOS = "linux"
	scheduleDockerOS = func(context.Context) (string, error) { return h.dockerOS, nil }
	scheduleLinger = func() (*schedule.Linger, error) { return h.linger, nil }
	scheduleCollectKeys = func(required, optional []string) (map[string]string, error) {
		h.accounts = append(append([]string{}, required...), optional...)
		return h.collected, nil
	}
	schedule.SystemdVersion = func() (int, error) { return h.systemdV, nil }
	schedule.EncryptCredential = func(_ string, plaintext []byte, out string) error {
		h.sealed = append(h.sealed, append([]byte{}, plaintext...))
		return os.WriteFile(out, []byte("ENCRYPTED"), 0o644)
	}
	return h
}

// addLocal keeps a schedule of the pure-text fixture with the harness's
// policy, as add would before --signed-out.
func (h *signedOutHarness) addLocal(t *testing.T, r schedule.Recurrence) schedule.Schedule {
	t.Helper()
	sc := schedule.Schedule{Name: "pure", Ref: "fixtures/pure-text", Version: "1.0.0", Digest: "sha256:aa", Local: repoPath(t, "testdata/runner/pure-text"),
		Run: schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, Policy: h.policy}, When: r, Created: h.now}
	if err := h.store.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := installAlarm(sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func (h *signedOutHarness) lingerOn() bool {
	on, _ := h.linger.On()
	return on
}

// assertNoKeyInStore is invariant 7: no file under the runner home holds
// the key in the clear.
func assertNoKeyInStore(t *testing.T, h *signedOutHarness) {
	t.Helper()
	home, _ := runnerHome()
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(sealedTestKey)) {
			t.Errorf("%s holds the key in the clear", p)
		}
		return nil
	})
}

func TestSignedOutRefusedOffLinux(t *testing.T) {
	h := newSignedOutHarness(t)
	scheduleGOOS = "darwin"
	_, _, err := runCLI(t, strings.NewReader(""), "schedule", "add", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--daily", "09:00", "--out", t.TempDir(), "--signed-out", "--allow-unsigned")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "Docker Desktop stops when you sign out") {
		t.Fatalf("refusal = %v", err)
	}
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	_, _, err = runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes")
	wantClassified(t, err, engine.ExitUsage)
	if got, _ := h.store.Get(sc.ID); got.SignedOut || len(h.sealed) != 0 || h.lingerOn() {
		t.Fatal("a refused --signed-out changed something")
	}
}

func TestSignedOutGaps(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	sc.Run.Knowledge = []string{t.TempDir()} // a pack's folder opens with no keychain
	if gaps := signedOutGaps(context.Background(), sc); len(gaps) != 0 {
		t.Fatalf("gaps = %q", gaps)
	}

	// An agent whose declared server signs in by OAuth.
	dir := t.TempDir()
	b, err := os.ReadFile(repoPath(t, "testdata/runner/mcp-remote/agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), bytes.Replace(b, []byte("auth: none"), []byte("auth: oauth"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("Read the wiki.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc.Local = dir
	sc.Run.Knowledge = []string{"team-handbook"}
	h.systemdV, h.dockerOS = 255, "Docker Desktop"
	gaps := strings.Join(signedOutGaps(context.Background(), sc), "\n")
	for _, want := range []string{
		"needs systemd 256 or later", "this computer has systemd 255", "never kept in a plain file",
		"needs Docker Engine: Docker Desktop stops when you sign out",
		"https://mcp.deepwiki.com/mcp signs in with an OAuth connection",
		"kept knowledge pack team-handbook opens with a key kept in the keychain",
	} {
		if !strings.Contains(gaps, want) {
			t.Errorf("gaps lack %q:\n%s", want, gaps)
		}
	}
	h.dockerOS = ""
	scheduleDockerOS = func(context.Context) (string, error) { return "", errors.New("docker info: daemon not running") }
	if gaps := strings.Join(signedOutGaps(context.Background(), sc), "\n"); !strings.Contains(gaps, "could not be asked which engine") {
		t.Errorf("an unaskable Docker passed: %s", gaps)
	}
	schedule.SystemdVersion = func() (int, error) { return 0, errors.New("systemctl: not found") }
	if gaps := strings.Join(signedOutGaps(context.Background(), sc), "\n"); !strings.Contains(gaps, "could not be read") {
		t.Errorf("an unreadable version passed: %s", gaps)
	}
}

func TestEditSignedOutOnAndOff(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})

	// No terminal and no --yes: linger is not turned on unasked.
	_, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "linger") || h.lingerOn() || len(h.sealed) != 0 {
		t.Fatalf("unconfirmed linger: %v, on %v, sealed %d", err, h.lingerOn(), len(h.sealed))
	}

	if _, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatalf("edit --signed-out: %v (%s)", err, stderr)
	}
	got, _ := h.store.Get(sc.ID)
	cred, _ := h.store.CredentialPath(sc.ID)
	e := h.alarm.installed[sc.ID]
	if !got.SignedOut || e.Credential != cred || !strings.Contains(strings.Join(e.Env, " "), "AGENTRY_NO_KEYRING=1") {
		t.Fatalf("signed out %v, entry %+v", got.SignedOut, e)
	}
	if fi, err := os.Stat(cred); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential %v %v", fi, err)
	}
	if len(h.sealed) != 1 || !bytes.Contains(h.sealed[0], []byte(sealedTestKey)) {
		t.Fatalf("sealed %q", h.sealed)
	}
	if !h.lingerOn() || len(h.loginctl) != 1 {
		t.Fatalf("linger on %v, calls %v", h.lingerOn(), h.loginctl)
	}
	for _, kv := range e.Env {
		if strings.Contains(kv, sealedTestKey) {
			t.Fatalf("the entry carries the key: %s", kv)
		}
	}
	assertNoKeyInStore(t, h)

	if _, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out=false"); err != nil {
		t.Fatalf("edit --signed-out=false: %v (%s)", err, stderr)
	}
	got, _ = h.store.Get(sc.ID)
	e = h.alarm.installed[sc.ID]
	if got.SignedOut || e.Credential != "" || strings.Contains(strings.Join(e.Env, " "), "AGENTRY_NO_KEYRING") {
		t.Fatalf("still signed out: %v %+v", got.SignedOut, e)
	}
	if _, err := os.Stat(cred); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the sealed keys outlived the signed-out schedule")
	}
	if h.lingerOn() {
		t.Fatal("linger stayed on with no signed-out schedule")
	}
}

func TestRemoveAndEndReleaseLinger(t *testing.T) {
	h := newSignedOutHarness(t)
	daily := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	once := h.addLocal(t, schedule.Recurrence{Kind: schedule.Once, Date: "2026-10-09", Time: "09:00", Zone: "UTC", Start: h.now})
	for _, id := range []string{daily.ID, once.ID} {
		if _, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "edit", id, "--signed-out", "--yes"); err != nil {
			t.Fatalf("edit %s: %v (%s)", id, err, stderr)
		}
	}
	// Linger was asked for once.
	if len(h.loginctl) != 1 {
		t.Fatalf("loginctl %v", h.loginctl)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "remove", daily.ID); err != nil {
		t.Fatal(err)
	}
	if !h.lingerOn() {
		t.Fatal("linger went off while a signed-out schedule was left")
	}
	// The once schedule fires, ends, and takes its keys and linger with it.
	h.now = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if err := fire(t, once.ID); err != nil {
		t.Fatal(err)
	}
	cred, _ := h.store.CredentialPath(once.ID)
	if _, err := os.Stat(cred); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an ended schedule kept its sealed keys")
	}
	if h.lingerOn() || strings.Join(h.loginctl, ",") != "enable-linger,disable-linger" {
		t.Fatalf("linger on %v, calls %v", h.lingerOn(), h.loginctl)
	}
}

func TestLingerThePersonKeptIsNeverTurnedOff(t *testing.T) {
	h := newSignedOutHarness(t)
	if err := os.WriteFile(filepath.Join(h.linger.Dir, h.linger.User), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	// Linger already on asks no confirm.
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "remove", sc.ID); err != nil {
		t.Fatal(err)
	}
	if !h.lingerOn() || len(h.loginctl) != 0 {
		t.Fatalf("the person's linger was touched: on %v, calls %v", h.lingerOn(), h.loginctl)
	}
}

func TestResealSignedOutAfterAKeyChange(t *testing.T) {
	h := newSignedOutHarness(t)
	live := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	ended := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "10:00", Zone: "UTC", Start: h.now})
	h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "11:00", Zone: "UTC", Start: h.now}) // signed in only
	for _, id := range []string{live.ID, ended.ID} {
		if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", id, "--signed-out", "--yes"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.SaveState(ended.ID, schedule.State{Ended: true}); err != nil {
		t.Fatal(err)
	}
	h.sealed = nil
	h.collected = map[string]string{modelkey.Account("openai"): "sk-new-key"}
	var errOut bytes.Buffer
	resealSignedOut(&errOut)
	if len(h.sealed) != 1 || !bytes.Contains(h.sealed[0], []byte("sk-new-key")) {
		t.Fatalf("sealed %d times: %q", len(h.sealed), h.sealed)
	}
	if !strings.Contains(errOut.String(), "sealed the keys of 1 schedule(s)") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestCredentialForReadsTheSealedToken(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "")
	plain, _ := modelkey.SealPlaintext(map[string]string{"registry.example.com": "tok-sealed"})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modelkey.CredentialName), plain, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(modelkey.EnvCredentialsDirectory, dir)
	t.Setenv(envNoKeyring, "1")
	if got := credentialFor("https://registry.example.com"); got != "tok-sealed" {
		t.Fatalf("credentialFor = %q", got)
	}
	if got := credentialFor("https://other.example.com"); got != "" {
		t.Fatalf("an unsealed registry = %q", got)
	}
	if got := keychainTokenAccount("https://registry.example.com"); got != "registry.example.com" {
		t.Fatalf("keychainTokenAccount = %q", got)
	}
}

func TestRemovingTheLastSignedOutScheduleTurnsLingerOff(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "remove", sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.lingerOn() || !strings.Contains(stderr, "linger is off for p again") {
		t.Fatalf("linger on %v after the last one went (stderr %q)", h.lingerOn(), stderr)
	}
}

// An ended signed-out schedule lost its keys; a new repeat seals and
// probes it again, and turns linger back on.
func TestReviveAnEndedSignedOutSchedule(t *testing.T) {
	h := newSignedOutHarness(t)
	once := h.addLocal(t, schedule.Recurrence{Kind: schedule.Once, Date: "2026-10-09", Time: "09:00", Zone: "UTC", Start: h.now})
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", once.ID, "--signed-out", "--yes"); err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if err := fire(t, once.ID); err != nil {
		t.Fatal(err)
	}
	if h.lingerOn() || credentialKept(h.store, once.ID) {
		t.Fatal("the ended schedule kept its keys or linger")
	}
	h.sealed = nil
	if _, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "edit", once.ID, "--daily", "09:00", "--yes"); err != nil {
		t.Fatalf("revive: %v (%s)", err, stderr)
	}
	if !credentialKept(h.store, once.ID) || len(h.sealed) != 1 || !h.lingerOn() {
		t.Fatalf("revived: keys %v, sealed %d, linger %v", credentialKept(h.store, once.ID), len(h.sealed), h.lingerOn())
	}
	if e := h.alarm.installed[once.ID]; e.Credential == "" {
		t.Fatalf("the revived alarm loads no keys: %+v", e)
	}
}

// Turning signed-out off drops the keys even when the alarm cannot be
// rewritten: the old alarm then fails to load them, closed.
func TestSignedOutOffDropsKeysWhenTheAlarmFails(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatal(err)
	}
	h.alarm.installErr = errors.New("systemctl: bus gone")
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out=false"); err == nil {
		t.Fatal("a failed alarm write was not reported")
	}
	if credentialKept(h.store, sc.ID) || h.lingerOn() {
		t.Fatalf("keys %v, linger %v after turning signed-out off", credentialKept(h.store, sc.ID), h.lingerOn())
	}
}

// A re-seal that fails removes the keys sealed before: after a logout, a
// stale token must not stay sealed.
func TestFailedResealDropsTheOldKeys(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatal(err)
	}
	scheduleCollectKeys = func([]string, []string) (map[string]string, error) {
		return nil, errors.New("the OS keychain has no entry provider:openai")
	}
	var errOut bytes.Buffer
	resealSignedOut(&errOut)
	if credentialKept(h.store, sc.ID) || !strings.Contains(errOut.String(), "schedule edit "+sc.ID+" --signed-out") {
		t.Fatalf("keys kept %v, stderr %q", credentialKept(h.store, sc.ID), errOut.String())
	}
	// The cure named: edit --signed-out seals it again.
	scheduleCollectKeys = func([]string, []string) (map[string]string, error) { return h.collected, nil }
	if _, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out"); err != nil || !credentialKept(h.store, sc.ID) {
		t.Fatalf("reseal by edit: %v (%s)", err, stderr)
	}
}

// Linger, on for one schedule, rings a signed-in-only one with nobody
// signed in: it runs nothing and claims nothing.
func TestSignedInScheduleStepsAsideWhileSignedOut(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	scheduleSignedIn = func() bool { return false }
	h.now = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.runs) != 0 {
		t.Fatalf("ran %v while signed out", h.runs)
	}
	// Signed in at the next ring, the occurrence runs.
	scheduleSignedIn = func() bool { return true }
	if err := fire(t, sc.ID); err != nil || len(h.runs) != 1 {
		t.Fatalf("signed in: %v, runs %v", err, h.runs)
	}
}

// A fire that could not turn linger off leaves it to the next sync.
func TestSyncRetriesTheLingerRelease(t *testing.T) {
	h := newSignedOutHarness(t)
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveState(sc.ID, schedule.State{Ended: true}); err != nil { // as if a fire ended it and polkit refused
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "sync"); err != nil {
		t.Fatal(err)
	}
	if h.lingerOn() {
		t.Fatal("sync left the CLI's linger on with nothing signed out")
	}
}

// keepConnect writes a connect's record into the sign-in store, as a
// connect leaves it: renewed (a Hugging Face token with its refresh token
// in the keychain) or expiring (a Poe key).
func keepConnect(t *testing.T, vendor string, rec map[string]any) {
	t.Helper()
	home, _ := runnerHome()
	base := map[string]any{"version": modelsignin.Version, "vendor": vendor, "kind": policy.KeyKindAPI,
		"provider": modelsignin.ProviderOfVendor(vendor), "connected": true, "createdAt": "2026-10-08T11:00:00Z"}
	for k, v := range rec {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	dir := modelsignin.Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vendor+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A connected Hugging Face token is renewed at each run's start from the
// keychain, which is locked while its person is signed out, and its sealed
// copy never is (agentry-cli#811). So --signed-out is refused for it, with
// the fix named, the re-seal refuses to seal it, and check-signed-out says
// so on a computer that can otherwise run signed out. A Poe key is
// refused once past its expiry.
func TestSignedOutRefusesAConnectedKeyItCannotRenew(t *testing.T) {
	h := newSignedOutHarness(t)
	pol, _, err := policy.Load(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.Provider, pol.Model.Name = policy.ProviderHuggingFace, "openai/gpt-oss-120b:groq"
	if err := policy.Save(h.policy, pol); err != nil {
		t.Fatal(err)
	}
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if gaps := signedOutGaps(context.Background(), sc); len(gaps) != 0 {
		t.Fatalf("a pasted Hugging Face key: gaps = %q", gaps)
	}

	keepConnect(t, modelsignin.HuggingFace, map[string]any{"secretIn": "keychain"})
	gaps := strings.Join(signedOutGaps(context.Background(), sc), "\n")
	for _, want := range []string{"Hugging Face token Agentry connected", "locked while you are signed out", "agentry setup"} {
		if !strings.Contains(gaps, want) {
			t.Errorf("gaps lack %q:\n%s", want, gaps)
		}
	}
	_, _, err = runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes")
	wantClassified(t, err, engine.ExitPolicy)
	if got, _ := h.store.Get(sc.ID); got.SignedOut || len(h.sealed) != 0 || h.lingerOn() {
		t.Fatal("a refused --signed-out changed something")
	}
	if err := sealScheduleKeys(sc, filepath.Join(t.TempDir(), "keys.cred")); err == nil || len(h.sealed) != 0 {
		t.Fatalf("the re-seal sealed a token no signed-out fire can renew: %v", err)
	}
	stdout, _, err := runCLI(t, nil, "schedule", "check-signed-out", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var a signedOutAvailability
	if err := json.Unmarshal([]byte(stdout), &a); err != nil {
		t.Fatal(err)
	}
	if !a.Available || !strings.Contains(a.ModelKey, "Hugging Face token Agentry connected") {
		t.Fatalf("check-signed-out = %+v", a)
	}
	stdout, _, _ = runCLI(t, nil, "schedule", "check-signed-out")
	if !strings.Contains(stdout, "refused while you are signed out") {
		t.Fatalf("check-signed-out said %q", stdout)
	}

	// Poe: until the expiry the person chose, and refused after it.
	pol.Model.Provider, pol.Model.Name = policy.ProviderPoe, "Claude-Sonnet-4.6"
	if err := policy.Save(h.policy, pol); err != nil {
		t.Fatal(err)
	}
	keepConnect(t, modelsignin.Poe, map[string]any{"keyExpiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if gaps := signedOutGaps(context.Background(), sc); len(gaps) != 0 {
		t.Fatalf("a live Poe key: gaps = %q", gaps)
	}
	keepConnect(t, modelsignin.Poe, map[string]any{"keyExpiresAt": "2026-01-01T00:00:00Z"})
	if gaps := strings.Join(signedOutGaps(context.Background(), sc), "\n"); !strings.Contains(gaps, "Poe key Agentry connected expired") {
		t.Fatalf("an expired Poe key: gaps = %q", gaps)
	}
}

// A connect changes the key in the model key's entry, so it seals a
// signed-out schedule's keys again, as setup does; connecting Hugging Face
// under one tells the person at once that it will not run signed out, and
// removes its sealed keys, rather than letting its fire find out
// (agentry-cli#811). A disconnect seals again too.
func TestAConnectSealsSignedOutSchedulesAgain(t *testing.T) {
	f := newConnectFixture(t)
	h := newSignedOutHarness(t)
	keyring.MockInit() // a connect needs the keychain on: never the person's
	t.Setenv(modelkey.EnvNoKeyring, "")
	pol, _, err := policy.Load(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.Provider, pol.Model.Name = policy.ProviderOpenRouter, "anthropic/claude-sonnet-4.5"
	if err := policy.Save(h.policy, pol); err != nil {
		t.Fatal(err)
	}
	// fireGaps reads the model key from the harness's stand-in keychain.
	keys := map[string]string{modelkey.Account(policy.ProviderOpenRouter): sealedTestKey, modelkey.Account(policy.ProviderHuggingFace): sealedTestKey}
	plain, _ := modelkey.SealPlaintext(keys)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modelkey.CredentialName), plain, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(modelkey.EnvCredentialsDirectory, dir)
	h.collected = keys
	sc := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	if _, errOut, err := runCLI(t, strings.NewReader(""), "schedule", "edit", sc.ID, "--signed-out", "--yes"); err != nil {
		t.Fatalf("edit: %v\n%s", err, errOut)
	}
	if !credentialKept(h.store, sc.ID) || len(h.sealed) != 1 {
		t.Fatalf("not sealed: %d", len(h.sealed))
	}
	_, errOut, err := runCLI(t, strings.NewReader(""), "model-signins", "add", "openrouter")
	if err != nil || len(h.sealed) != 2 || !strings.Contains(errOut, "sealed the keys of 1 schedule(s)") {
		t.Fatalf("a connect did not seal again: %v, %d seals\n%s", err, len(h.sealed), errOut)
	}
	if _, errOut, err = runCLI(t, nil, "model-signins", "remove", "openrouter"); err != nil || len(h.sealed) != 3 {
		t.Fatalf("a disconnect did not seal again: %v, %d seals\n%s", err, len(h.sealed), errOut)
	}

	pol.Model.Provider, pol.Model.Name = policy.ProviderHuggingFace, "openai/gpt-oss-120b:groq"
	if err := policy.Save(h.policy, pol); err != nil {
		t.Fatal(err)
	}
	_, errOut, err = runCLI(t, strings.NewReader(""), "model-signins", "add", "huggingface")
	if err != nil || f.key("huggingface") == "" {
		t.Fatalf("add huggingface: %v\n%s", err, errOut)
	}
	if len(h.sealed) != 3 || credentialKept(h.store, sc.ID) || !strings.Contains(errOut, "were not sealed again: the Hugging Face token Agentry connected") {
		t.Fatalf("a Hugging Face token was sealed, or the person not told: %d seals, kept %v\n%s", len(h.sealed), credentialKept(h.store, sc.ID), errOut)
	}
}
