package persistent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry/pkg/manifest"
)

// lived gives the Assistant everything a person and its segments leave
// behind: a configuration with every choice made, a stored secret and
// Google Workspace signed in (their values in keys), an installed server
// and an added skill on disk, a transcript, tasks, usage, share verdicts
// and share text, and a share folder holding a file of the person's.
func lived(t *testing.T, home string) (Config, fakeSecrets, string) {
	t.Helper()
	withImageServers(t)
	enable(t, home)
	share := filepath.Join(t.TempDir(), "share")
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "letter.docx"), []byte("the person's"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunOnStart, cfg.DisplayName, cfg.Personality, cfg.PersonaGiven = true, "Petunia", "warm", "sha256:x"
	cfg.Segments, cfg.Share = 4, share
	cfg.Conversation = &Conversation{Adapter: "codex-image", ID: "c1"}
	cfg.Servers = []manifest.MCPServer{{PyPI: "office-word-mcp-server", Version: "1.1.11"}}
	cfg.Knowledge = []string{"pack"}
	cfg.Secrets = []StoredSecret{{Name: "GITHUB_TOKEN"}}
	cfg.SignIns = map[string]ImageSignIn{"gog": {Account: "person@example.com"}}
	cfg.Switches = map[string]bool{"share": false, SecretEntryID("GITHUB_TOKEN"): true, "server:gog": true, "server:gog:writes": true}
	cfg.Consented = map[string]string{"share": "Shares the folder", SecretEntryID("GITHUB_TOKEN"): "Gives GITHUB_TOKEN", "server:gog": "Reads Google"}
	if err := SaveConfig(home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	dir := Dir(home, "assistant")
	for _, f := range []string{
		filepath.Join(serversDir(home, "assistant"), "pypi-office-word-mcp-server", "server.json"),
		filepath.Join(AddedSkillsPath(home, "assistant"), "letters", "SKILL.md"),
		filepath.Join(dir, transcriptFile), filepath.Join(dir, tasksFile), filepath.Join(dir, usageFile),
		filepath.Join(dir, shareVerdictsFile), filepath.Join(dir, memorySeededFile),
		filepath.Join(dir, shareTextDir, "letter.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keys := fakeSecrets{
		SecretAccount("assistant", "GITHUB_TOKEN"): ghValue,
		ImageKeyringAccount("assistant", "gog"):    gogPassword,
		"provider:openai":                          "kept-0123456789",
	}
	old := resetSecrets
	resetSecrets = keys
	t.Cleanup(func() { resetSecrets = old })
	return cfg, keys, share
}

// removedVolumes are the volumes the fake engine was asked to remove.
func removedVolumes(d *dockerx.Fake) []string {
	var out []string
	for _, c := range d.Calls {
		if c.Op == "VolumeRemove" {
			out = append(out, c.Arg)
		}
	}
	slices.Sort(out)
	return out
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// historyVolumes are the Assistant's own volumes: its conversation, its
// files and its memory.
func historyVolumes(home string) []string {
	v := []string{SessionVolumeIn(home, "assistant"), DataVolumeIn(home, "assistant"), MemoryVolumeIn(home, "assistant")}
	slices.Sort(v)
	return v
}

// TestResetHistoryKeepsWhatThePersonChose: Reset history removes what the
// Assistant did (its conversation, files and memory, its transcript, tasks
// and what it read from the share) and keeps every choice, consent,
// secret and sign-in, so its next start is a new conversation that asks
// nothing again. Its usage, its share folder's files and Google
// Workspace's volume stay.
func TestResetHistoryKeepsWhatThePersonChose(t *testing.T) {
	home := shortHome(t)
	before, keys, share := lived(t, home)
	d := &dockerx.Fake{}
	if err := ResetHistory(context.Background(), home, "assistant", d); err != nil {
		t.Fatal(err)
	}
	if got, want := removedVolumes(d), historyVolumes(home); !reflect.DeepEqual(got, want) {
		t.Errorf("volumes removed = %v, want %v", got, want)
	}
	dir := Dir(home, "assistant")
	for _, gone := range []string{transcriptFile, tasksFile, shareVerdictsFile, memorySeededFile, shareTextDir} {
		if exists(t, filepath.Join(dir, gone)) {
			t.Errorf("Reset history kept %s", gone)
		}
	}
	for _, kept := range []string{filepath.Join(dir, usageFile), serversDir(home, "assistant"), AddedSkillsPath(home, "assistant"), filepath.Join(share, "letter.docx")} {
		if !exists(t, kept) {
			t.Errorf("Reset history removed %s", kept)
		}
	}
	after, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.Segments, want.Conversation, want.PersonaGiven = 0, nil, ""
	if !reflect.DeepEqual(after, want) {
		t.Errorf("config after Reset history = %+v\nwant %+v", after, want)
	}
	if len(keys) != 3 {
		t.Errorf("the keychain after Reset history: %v", keys)
	}
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(c Config) []string {
		var out []string
		for _, e := range Unconsented(BuildRoster(a, c, nil), c) {
			out = append(out, e.ID)
		}
		return out
	}
	if got, was := ids(after), ids(before); !reflect.DeepEqual(got, was) {
		t.Errorf("after Reset history the next start asks for %v, before it %v", got, was)
	}
}

// TestResetAgentKeepsTheCredentialsSwitchedOff: Reset agent puts every
// choice back to its default and removes the history too, but keeps the
// stored secrets and the image servers' sign-ins (their keychain entries
// and volumes), switched off and not agreed to, so turning one on again
// asks once and needs no new set-up. The share folder's location, that the
// agent is enabled and its usage stay.
func TestResetAgentKeepsTheCredentialsSwitchedOff(t *testing.T) {
	home := shortHome(t)
	before, keys, share := lived(t, home)
	d := &dockerx.Fake{}
	if err := ResetAgent(context.Background(), home, "assistant", d); err != nil {
		t.Fatal(err)
	}
	if got, want := removedVolumes(d), historyVolumes(home); !reflect.DeepEqual(got, want) {
		t.Errorf("volumes removed = %v, want %v (Google Workspace's is kept)", got, want)
	}
	dir := Dir(home, "assistant")
	for _, gone := range []string{transcriptFile, tasksFile, shareVerdictsFile, memorySeededFile, shareTextDir} {
		if exists(t, filepath.Join(dir, gone)) {
			t.Errorf("Reset agent kept %s", gone)
		}
	}
	for _, gone := range []string{serversDir(home, "assistant"), AddedSkillsPath(home, "assistant")} {
		if exists(t, gone) {
			t.Errorf("Reset agent kept %s", gone)
		}
	}
	for _, kept := range []string{filepath.Join(dir, usageFile), filepath.Join(share, "letter.docx")} {
		if !exists(t, kept) {
			t.Errorf("Reset agent removed %s", kept)
		}
	}
	after, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Version: ConfigVersion, Enabled: true, Share: share,
		Secrets: before.Secrets, SignIns: before.SignIns,
		Switches: map[string]bool{SecretEntryID("GITHUB_TOKEN"): false},
	}
	if !reflect.DeepEqual(after, want) {
		t.Errorf("config after Reset agent = %+v\nwant %+v", after, want)
	}
	if len(keys) != 3 {
		t.Errorf("the keychain after Reset agent: %v", keys)
	}
	a, err := Lookup("assistant")
	if err != nil {
		t.Fatal(err)
	}
	// Its mail providers are disabled, so gog is off the roster until
	// Google is enabled again, with its kept sign-in.
	if _, ok := BuildRoster(a, after, nil).Entry("server:gog"); ok {
		t.Error("gog is on the roster after Reset agent, with Google disabled")
	}
	enabled := after
	enabled.MailProviders = []string{"google"}
	r := BuildRoster(a, enabled, nil)
	for _, id := range []string{SecretEntryID("GITHUB_TOKEN"), "server:gog", "server:gog:writes"} {
		e, ok := r.Entry(id)
		if !ok {
			t.Errorf("the roster after Reset agent has no %s", id)
			continue
		}
		if e.On {
			t.Errorf("%s is on after Reset agent", id)
		}
	}
	// Switched on again, the kept secret is asked for before it is given.
	on := after
	on.Switches = map[string]bool{SecretEntryID("GITHUB_TOKEN"): true}
	if !slices.ContainsFunc(Unconsented(BuildRoster(a, on, nil), on), func(e RosterEntry) bool { return e.ID == SecretEntryID("GITHUB_TOKEN") }) {
		t.Error("the kept secret switched on after Reset agent is not asked for again")
	}
}

// TestResetAllStillRemovesTheCredentials: plain Reset is unchanged: the
// secrets, the sign-in, its volume and its keyring password go too.
func TestResetAllStillRemovesTheCredentials(t *testing.T) {
	home := shortHome(t)
	_, keys, _ := lived(t, home)
	d := &dockerx.Fake{}
	if err := Reset(context.Background(), home, "assistant", d); err != nil {
		t.Fatal(err)
	}
	want := append(historyVolumes(home), ImageVolumeIn(home, "assistant", "gog"), ImageVolumeIn(home, "assistant", "ms365"))
	slices.Sort(want)
	if got := removedVolumes(d); !reflect.DeepEqual(got, want) {
		t.Errorf("volumes removed = %v, want %v", got, want)
	}
	if len(keys) != 1 {
		t.Errorf("the keychain after Reset: %v", keys)
	}
	if exists(t, filepath.Join(Dir(home, "assistant"), usageFile)) {
		t.Error("Reset kept the usage")
	}
}

// TestPartialResetsAreTheAssistantsAlone: the Coding agent and Main have
// one Reset; asking either for Reset history or Reset agent changes
// nothing.
func TestPartialResetsAreTheAssistantsAlone(t *testing.T) {
	home := shortHome(t)
	for _, name := range []string{"coder", "main"} {
		for kind, reset := range map[string]func(context.Context, string, string, dockerx.Client) error{"history": ResetHistory, "agent": ResetAgent} {
			d := &dockerx.Fake{}
			err := reset(context.Background(), home, name, d)
			if err == nil || !strings.Contains(err.Error(), "the Assistant") {
				t.Errorf("Reset %s of %s = %v, want a refusal naming the Assistant", kind, name, err)
			}
			if len(d.Calls) != 0 {
				t.Errorf("Reset %s of %s reached the engine: %v", kind, name, d.Calls)
			}
		}
	}
}

// TestPartialResetsRefuseARunningAgent: as Reset does.
func TestPartialResetsRefuseARunningAgent(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	sv := startAssistant(t, home, &starter{})
	defer func() { _, _ = sv.Stop(context.Background()) }()
	for kind, reset := range map[string]func(context.Context, string, string, dockerx.Client) error{"history": ResetHistory, "agent": ResetAgent} {
		if err := reset(context.Background(), home, "assistant", &dockerx.Fake{}); err == nil {
			t.Errorf("Reset %s of a running agent was not refused", kind)
		}
	}
}
