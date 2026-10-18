package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// scheduleDestinationsEnv saves two destinations in a throwaway config folder
// and keychain.
func scheduleDestinationsEnv(t *testing.T) {
	t.Helper()
	t.Setenv(modelkey.EnvNoKeyring, "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	t.Setenv("HOOK_SECRET", "whsec_scheduled")
	t.Setenv("HOOK_AUTH", "Bearer scheduled")
	for _, a := range [][]string{
		{"destinations", "add", "n8n", "--url", "https://hooks.example.com/a", "--secret-env", "HOOK_SECRET", "--authorization-env", "HOOK_AUTH"},
		{"destinations", "add", "lan", "--url", "http://192.168.1.20:5678/x", "--on-my-network", "--secret-env", "HOOK_SECRET"},
	} {
		if _, _, err := runCLI(t, nil, a...); err != nil {
			t.Fatal(err)
		}
	}
}

var scheduleDeliverWhen = schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}

func TestAScheduleKeepsWhereItsRunsDeliver(t *testing.T) {
	isolateEnv(t)
	scheduleDestinationsEnv(t)
	out, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	sc, err := scheduleFromFlags("digest", out, runSpecFlags{deliverTo: "~/Documents/Agentry", destinations: []string{"n8n", "lan", "n8n"}, maxTurns: -1, maxCost: -1}, scheduleDeliverWhen, scheduleDeliverWhen.Start)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Documents", "Agentry"); sc.Run.DeliverTo != want {
		t.Errorf("DeliverTo = %q, want %q", sc.Run.DeliverTo, want)
	}
	if !slices.Equal(sc.Run.Destinations, []string{"n8n", "lan"}) {
		t.Errorf("Destinations = %v", sc.Run.Destinations)
	}
	sc.Ref, sc.Version, sc.Digest = "acme/digest", "1.0.0", "sha256:aa"
	args := baseRunArgs(sc)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--deliver-to "+sc.Run.DeliverTo) || !strings.Contains(joined, "--destination n8n --destination lan") {
		t.Errorf("a fire's arguments: %s", joined)
	}
	b, _ := json.Marshal(sc)
	if !strings.Contains(string(b), `"deliverTo"`) || !strings.Contains(string(b), `"destinations":["n8n","lan"]`) || strings.Contains(string(b), "scheduled") {
		t.Errorf("the record: %s", b)
	}
	// An old schedule without either behaves as before.
	old := sc
	old.Run.DeliverTo, old.Run.Destinations = "", nil
	// and names no destination, so a default set the person makes later never
	// reaches its fires.
	if j := strings.Join(baseRunArgs(old), " "); strings.Contains(j, "--deliver-to") || strings.Contains(j, "--destination") || !strings.Contains(j, "--no-destination") {
		t.Errorf("a schedule with no delivery passes %s", j)
	}
	if j := strings.Join(baseRunArgs(sc), " "); strings.Contains(j, "--no-destination") {
		t.Errorf("a schedule with destinations passes %s", j)
	}
}

func TestAScheduleRefusesADeliveryItsFiresCouldNotMake(t *testing.T) {
	isolateEnv(t)
	scheduleDestinationsEnv(t)
	for name, spec := range map[string]runSpecFlags{
		"a relative folder":      {deliverTo: "docs", maxTurns: -1, maxCost: -1},
		"a destination not kept": {destinations: []string{"nope"}, maxTurns: -1, maxCost: -1},
	} {
		if _, err := scheduleFromFlags("digest", t.TempDir(), spec, scheduleDeliverWhen, scheduleDeliverWhen.Start); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	// One without a signing secret.
	if err := keyring.Delete(modelkey.Service, destination.SecretAccount("lan")); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduleFromFlags("digest", t.TempDir(), runSpecFlags{destinations: []string{"lan"}, maxTurns: -1, maxCost: -1}, scheduleDeliverWhen, scheduleDeliverWhen.Start); err == nil || !strings.Contains(err.Error(), "no signing secret") {
		t.Errorf("a destination with no secret: %v", err)
	}
}

func TestAScheduleEditChangesAndClearsWhereRunsDeliver(t *testing.T) {
	h := newScheduleHarness(t)
	scheduleDestinationsEnv(t)
	sc := h.add(t, scheduleDeliverWhen)
	docs := filepath.Join(t.TempDir(), "docs")
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--deliver-to", docs, "--destination", "n8n"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.Get(sc.ID)
	if got.Run.DeliverTo != docs || !slices.Equal(got.Run.Destinations, []string{"n8n"}) {
		t.Errorf("after edit: %+v", got.Run)
	}
	out, _, _ := runCLI(t, nil, "schedule", "list", "--json")
	if !strings.Contains(out, docs) || !strings.Contains(out, `"n8n"`) {
		t.Errorf("list --json: %s", out)
	}
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--deliver-to", "", "--destination", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = h.store.Get(sc.ID)
	if got.Run.DeliverTo != "" || len(got.Run.Destinations) != 0 {
		t.Errorf("after clearing: %+v", got.Run)
	}
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--destination", "nope"); err == nil {
		t.Error("an edit named a destination that is not kept")
	}
	// An edit that names neither leaves them as they were.
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--destination", "lan"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--timeout", "30"); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.store.Get(sc.ID); !slices.Equal(got.Run.Destinations, []string{"lan"}) {
		t.Errorf("an unrelated edit changed the set: %+v", got.Run)
	}
}

// TestASignedOutScheduleSealsItsDestinationsCredentials: a fire that runs while
// its person is signed out reads each destination's signing secret (required)
// and Authorization value (optional) from the keys sealed beside it, and a
// changed set seals them again.
func TestASignedOutScheduleSealsItsDestinationsCredentials(t *testing.T) {
	h := newSignedOutHarness(t)
	scheduleDestinationsEnv(t)
	h.collected[destination.SecretAccount("n8n")] = "whsec_scheduled"
	h.collected[destination.AuthorizationAccount("n8n")] = "Bearer scheduled"
	sc := schedule.Schedule{Name: "src", Ref: "fixtures/pure-text", Version: "1.0.0", Digest: "sha256:aa", Local: repoPath(t, "testdata/runner/pure-text"),
		Run:  schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, Policy: h.policy, Destinations: []string{"n8n"}},
		When: scheduleDeliverWhen, Created: h.now, SignedOut: true}
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
	if !slices.Contains(required, destination.SecretAccount("n8n")) || !slices.Contains(optional, destination.AuthorizationAccount("n8n")) || slices.Contains(required, destination.AuthorizationAccount("n8n")) {
		t.Errorf("required %q, optional %q", required, optional)
	}
	if len(h.sealed) != 1 || !strings.Contains(string(h.sealed[0]), "whsec_scheduled") {
		t.Errorf("sealed %q", h.sealed)
	}
}

// TestAnEndedSignedOutScheduleKeepsNoSealedKeysThroughAnEdit: changing the
// destinations of a signed-out schedule that has ended seals nothing, so its keys
// still outlive no schedule that needs them and a later revival seals and probes
// as a new one does.
func TestAnEndedSignedOutScheduleKeepsNoSealedKeysThroughAnEdit(t *testing.T) {
	h := newSignedOutHarness(t)
	scheduleDestinationsEnv(t)
	h.collected[destination.SecretAccount("n8n")] = "whsec_scheduled"
	// The harness answers as a signed-out fire does; the edit is the person's,
	// signed in, and reads the keychain.
	t.Setenv(modelkey.EnvCredentialsDirectory, "")
	sc := schedule.Schedule{Name: "src", Ref: "fixtures/pure-text", Version: "1.0.0", Digest: "sha256:aa", Local: repoPath(t, "testdata/runner/pure-text"),
		Run:  schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, Policy: h.policy},
		When: scheduleDeliverWhen, Created: h.now, SignedOut: true}
	if err := h.store.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveState(sc.ID, schedule.State{Ended: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "schedule", "edit", sc.ID, "--destination", "n8n"); err != nil {
		t.Fatal(err)
	}
	if credentialKept(h.store, sc.ID) || len(h.sealed) != 0 {
		t.Errorf("an ended schedule's edit sealed keys: kept %v, sealed %d", credentialKept(h.store, sc.ID), len(h.sealed))
	}
}

// TestARunTakesNoDefaultSetWhenToldNone: --no-destination delivers to no
// destination whatever default set exists, and does not go with --destination.
func TestARunTakesNoDefaultSetWhenToldNone(t *testing.T) {
	isolateEnv(t)
	scheduleDestinationsEnv(t)
	if _, _, err := runCLI(t, nil, "destinations", "default", "acme/digest", "n8n"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, nil, "run", "acme/digest", "--no-destination", "--destination", "n8n", "--plan")
	if err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Errorf("both flags: %v", err)
	}
}
