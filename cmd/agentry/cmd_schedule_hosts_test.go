package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/pkg/schedule"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// targetHostQuickAgent is the pure-text fixture asking for a target host.
func targetHostQuickAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"agent.yaml", "AGENT.md"} {
		b, err := os.ReadFile(repoPath(t, filepath.Join("testdata/runner/pure-text", f)))
		if err != nil {
			t.Fatal(err)
		}
		if f == "agent.yaml" {
			b = append(b, []byte("capabilities:\n  targetHosts: [server]\nintake:\n  - id: server\n    prompt: The server\n    type: host\n    required: true\n")...)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeTargetHosts stands in for targethost.Prepare: it records what each
// call was asked and readies the host with key, or refuses as refuse says.
type fakeTargetHosts struct {
	key    ssh.PublicKey
	calls  []targethost.Options
	refuse error
}

func newFakeTargetHosts(t *testing.T) *fakeTargetHosts {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTargetHosts{key: k}
	prev := prepareTargetHost
	prepareTargetHost = func(_ context.Context, id string, a targethost.Answer, o targethost.Options) (*targethost.Host, error) {
		f.calls = append(f.calls, o)
		if f.refuse != nil {
			return nil, f.refuse
		}
		return &targethost.Host{ID: id, Answer: a, Address: netip.MustParseAddr("203.0.113.4"),
			HostKey:    targethost.HostKey{Key: f.key, From: targethost.TrustedNow},
			Credential: targethost.Credential{Auth: hostlink.AuthPassword, Password: "pw-863", From: targethost.FromPrompt}}, nil
	}
	t.Cleanup(func() { prepareTargetHost = prev })
	return f
}

// seedHostLinkImage gives the fake daemon the package record the host
// link image's build reads, as the prepare step of a release with a target
// host builds it.
func seedHostLinkImage(t *testing.T, f *dockerx.Fake, policyPath string) {
	t.Helper()
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	s := targethost.HostLinkImage(pol.RunnerImageTag(runnerimage.ContentVersion()), pol.Images.HostlinkImage())
	if f.ImageFiles == nil {
		f.ImageFiles = map[string]string{}
	}
	f.ImageFiles[s.Tag+":"+targethost.HostlinkPackagesFile] = "openssh-client-default-10.0_p2-r0\n"
}

// TestScheduleAddReadiesItsTargetHost is plan runtime-hosts.md D12
// (agentry-cli#863): a schedule's target host is readied when it is made,
// with the person here, its sign-in kept in the keychain (Keep); the
// schedule keeps the answer, the keychain account and the key's
// fingerprint, never a credential; and every fire passes the pin.
func TestScheduleAddReadiesItsTargetHost(t *testing.T) {
	h := newSignedOutHarness(t)
	hosts := newFakeTargetHosts(t)
	account := targethost.Answer{User: "admin", Host: "box.example", Port: 22}.Account()
	prevSaved := hostSignInSaved
	t.Cleanup(func() { hostSignInSaved = prevSaved })
	hostSignInSaved = func(a targethost.Answer) (bool, error) { return a.Account() == account, nil }

	_, err := scheduleAddLocal(t, h, targetHostQuickAgent(t), "--allow-unsigned", "--intake", "server=admin@box.example")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(hosts.calls) != 1 || !hosts.calls[0].Keep || hosts.calls[0].Pin != "" {
		t.Fatalf("Prepare was asked %+v, want once, with Keep and no pin", hosts.calls)
	}
	list, _, err := h.store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("kept %d schedule(s): %v", len(list), err)
	}
	sc := list[0]
	want := schedule.Host{ID: "server", Account: account, HostKey: ssh.FingerprintSHA256(hosts.key)}
	if !slices.Equal(sc.Run.Hosts, []schedule.Host{want}) {
		t.Fatalf("the schedule keeps %+v, want %+v", sc.Run.Hosts, want)
	}
	dir := filepath.Join(h.store.Dir, sc.ID)
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "pw-863") { //nolint:gosec // the test's own store
				t.Errorf("%s holds the sign-in", p)
			}
		}
		return nil
	})
	args := baseRunArgs(sc)
	if i := slices.Index(args, "--"+hostKeyPinFlag); i < 0 || args[i+1] != "server="+want.HostKey {
		t.Fatalf("a fire's run arguments do not pin the key: %q", args)
	}

	// The sign-in is gone from the keychain: the schedule says so.
	hostSignInSaved = func(targethost.Answer) (bool, error) { return false, nil }
	gaps := fireGaps(sc)
	if !slices.ContainsFunc(gaps, func(g string) bool { return strings.Contains(g, account) }) {
		t.Fatalf("a missing sign-in is no gap: %q", gaps)
	}
}

// A schedule whose target host is not answered is refused before it is
// kept, naming the --intake it needs; one whose host refuses (a key the
// person declined) is refused with the host's reason.
func TestScheduleAddRefusesATargetHostItCannotReady(t *testing.T) {
	h := newSignedOutHarness(t)
	hosts := newFakeTargetHosts(t)
	_, err := scheduleAddLocal(t, h, targetHostQuickAgent(t), "--allow-unsigned")
	if err == nil || !strings.Contains(err.Error(), "--intake server=") {
		t.Fatalf("an unanswered host: %v", err)
	}
	hosts.refuse = &targethost.Refusal{ID: "server", Detail: targethost.RefusedKeyUnknown, Err: errors.New("the host key was not accepted")}
	_, err = scheduleAddLocal(t, h, targetHostQuickAgent(t), "--allow-unsigned", "--intake", "server=admin@box.example")
	wantClassified(t, err, engine.ExitPolicy)
	if list, _, _ := h.store.List(); len(list) != 0 {
		t.Fatalf("a schedule whose host was refused was kept: %+v", list)
	}
}

// TestHostGaps: a made schedule its host was not readied for (one from
// before agentry-cli#863) says to make it again; a check (not made) is
// not told; a saved sign-in is no gap.
func TestHostGaps(t *testing.T) {
	saved := func(targethost.Answer) (bool, error) { return true, nil }
	sc := schedule.Schedule{ID: "abcdefghij", Run: schedule.RunSpec{Intake: []string{"server=admin@box"}}}
	if g := hostGaps([]string{"server"}, sc, saved); len(g) != 1 || !strings.Contains(g[0], "make it again") {
		t.Fatalf("a schedule never readied: %q", g)
	}
	sc.ID = ""
	if g := hostGaps([]string{"server"}, sc, saved); len(g) != 0 {
		t.Fatalf("a check: %q", g)
	}
	sc.ID = "abcdefghij"
	sc.Run.Hosts = []schedule.Host{{ID: "server", Account: "host:admin@box:22", HostKey: "SHA256:x"}}
	if g := hostGaps([]string{"server"}, sc, saved); len(g) != 0 {
		t.Fatalf("a readied schedule with its sign-in: %q", g)
	}
}

// TestASignedOutScheduleSealsItsHostSignIn (agentry-cli#863): a schedule
// that runs while its person is signed out seals each target host's saved
// sign-in, as required: its runs have no other.
func TestASignedOutScheduleSealsItsHostSignIn(t *testing.T) {
	h := newSignedOutHarness(t)
	account := targethost.Answer{User: "admin", Host: "box", Port: 22}.Account()
	h.collected[account] = `{"password":"sealed-863"}`
	sc := schedule.Schedule{Name: "hosts", Ref: "fixtures/pure-text", Version: "1.0.0", Digest: "sha256:aa", Local: targetHostQuickAgent(t),
		Run: schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, Policy: h.policy, Intake: []string{"server=admin@box"},
			Hosts: []schedule.Host{{ID: "server", Account: account, HostKey: "SHA256:x"}}},
		When: schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now}, Created: h.now, SignedOut: true}
	if err := h.store.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	path, err := h.store.CredentialPath(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	collect := scheduleCollectKeys
	t.Cleanup(func() { scheduleCollectKeys = collect })
	var required []string
	scheduleCollectKeys = func(req, opt []string) (map[string]string, error) {
		required = req
		return collect(req, opt)
	}
	if err := sealScheduleKeys(sc, path); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(required, account) {
		t.Fatalf("required %q, want the host's account", required)
	}
	if len(h.sealed) != 1 || !strings.Contains(string(h.sealed[0]), "sealed-863") {
		t.Fatalf("sealed %q", h.sealed)
	}
}

// TestARunTakesTheSchedulesPin (agentry-cli#863): `agentry run
// --host-key-pin id=SHA256:…`, which a fire passes, reaches the host's
// readying as its pin, and a run that is no schedule's keeps nothing in
// the keychain; a pin for no target host, or not a fingerprint, is a
// usage error.
func TestARunTakesTheSchedulesPin(t *testing.T) {
	hosts := newFakeTargetHosts(t)
	ids := []string{"server"}
	answers := func() *intake.Answers {
		return &intake.Answers{Hosts: map[string]*targethost.Host{"server": {ID: "server", Answer: targethost.Answer{User: "a", Host: "box", Port: 22}}}}
	}
	if err := readyHosts(stubCmd(t), answers(), ids, &policy.Policy{}, t.TempDir(), targetHostFlags{pins: []string{"server=SHA256:abc"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(hosts.calls) != 1 || hosts.calls[0].Pin != "SHA256:abc" || hosts.calls[0].Keep {
		t.Fatalf("Prepare was asked %+v", hosts.calls)
	}
	for _, pin := range []string{"other=SHA256:abc", "server=abc", "server"} {
		err := readyHosts(stubCmd(t), answers(), ids, &policy.Policy{}, t.TempDir(), targetHostFlags{pins: []string{pin}}, false)
		wantClassified(t, err, engine.ExitUsage)
	}
}
