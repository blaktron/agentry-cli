package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// sourceQuickAgent is the pure-text fixture with a repository question.
func sourceQuickAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"agent.yaml", "AGENT.md"} {
		b, err := os.ReadFile(repoPath(t, filepath.Join("testdata/runner/pure-text", f)))
		if err != nil {
			t.Fatal(err)
		}
		if f == "agent.yaml" {
			b = append(b, []byte("intake:\n  - id: code\n    prompt: The repository\n    type: repository\n    required: true\n")...)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestASignedOutScheduleSealsItsSourceKey (agentry-cli#854): a schedule
// that runs while its person is signed out seals the key saved for its
// input source's host beside the model key, asked for as optional (a host
// with no saved key seals nothing, and the fetch stays anonymous), and
// `schedule reseal` seals it again: a key gone from the keychain is gone
// from the credential.
func TestASignedOutScheduleSealsItsSourceKey(t *testing.T) {
	h := newSignedOutHarness(t)
	account := sources.KeyAccount("github.com")
	h.collected[account] = "source-key-854"
	sc := schedule.Schedule{Name: "src", Ref: "fixtures/pure-text", Version: "1.0.0", Digest: "sha256:aa", Local: sourceQuickAgent(t),
		Run:  schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, Policy: h.policy, Intake: []string{"code=https://github.com/o/private"}},
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
	var required, optional []string
	scheduleCollectKeys = func(req, opt []string) (map[string]string, error) {
		required, optional = req, opt
		return collect(req, opt)
	}
	if err := sealScheduleKeys(sc, path); err != nil {
		t.Fatal(err)
	}
	scheduleCollectKeys = collect
	if !slices.Contains(optional, account) || slices.Contains(required, account) {
		t.Fatalf("required %q, optional %q: want %s optional, so a host with no saved key seals nothing", required, optional, account)
	}
	if len(h.sealed) != 1 || !bytes.Contains(h.sealed[0], []byte("source-key-854")) || !bytes.Contains(h.sealed[0], []byte(account)) {
		t.Fatalf("sealed %q", h.sealed)
	}

	// The key is removed from the keychain: the re-seal leaves it out.
	delete(h.collected, account)
	h.sealed = nil
	if _, _, err := runCLI(t, nil, "schedule", "reseal", "--json"); err != nil {
		t.Fatal(err)
	}
	if len(h.sealed) != 1 || bytes.Contains(h.sealed[0], []byte("source-key-854")) {
		t.Fatalf("re-sealed %q", h.sealed)
	}

	// A schedule with no source answer asks for no source key.
	h.accounts = nil
	plain := sc
	plain.Local, plain.Run.Intake = repoPath(t, "testdata/runner/pure-text"), nil
	if err := sealScheduleKeys(plain, path); err != nil {
		t.Fatal(err)
	}
	for _, a := range h.accounts {
		if strings.HasPrefix(a, "source:") {
			t.Fatalf("asked for %s with no source", a)
		}
	}
}
