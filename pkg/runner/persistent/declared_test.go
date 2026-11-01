package persistent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// ownCapabilities is a release's block declaring every key of an agent's
// own life, with the memory seed and the skill its archive carries.
var ownCapabilities = publishedRelease{
	extra: "capabilities:\n  persistent:\n    clock: true\n    memory: true\n    skills: true\n    secrets: [NOTES_TOKEN]\n    selfConfigure: true\n",
	files: map[string]string{
		"memory/seed.md":        "# What I know\n",
		"skills/notes/SKILL.md": "---\nname: notes\ndescription: Keep the person's notes tidy.\n---\nFile each note under its day.\n",
	},
}

// TestUndeclaredPersistentCapabilityRefused: each key of an agent's own
// life gives a published agent what it names only when its release
// declares it, and the same release without the block is given none of
// them: the heartbeat and the task tools for clock, the memory, its seed and
// recall for memory, its release's skills and the person's for skills, the
// person's stored secrets for secrets, and self_configure for
// selfConfigure.
func TestUndeclaredPersistentCapabilityRefused(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	full := installPublished(t, home, "acme", "ada", "1.0.0", ownCapabilities)
	bare := installPublished(t, home, "acme", "bea", "1.0.0", publishedRelease{files: ownCapabilities.files})
	has := func(a Agent, id string) (RosterEntry, bool) {
		return BuildRoster(a, Config{Enabled: true}, nil).Entry(id)
	}
	tools := func(a Agent) []string { return applink.Tools(a.AppLink()) }

	t.Run("clock", func(t *testing.T) {
		if e, ok := has(full, EntryHeartbeat); !ok || e.GrantedBy != GrantedByRelease || !slices.Contains(tools(full), applink.TaskAdd) || !full.KeepsTime() {
			t.Errorf("the declaring agent's heartbeat %+v, tools %v", e, tools(full))
		}
		if _, ok := has(bare, EntryHeartbeat); ok || slices.Contains(tools(bare), applink.TaskAdd) || bare.KeepsTime() {
			t.Error("an agent that does not declare clock has its heartbeat or task tools")
		}
	})
	t.Run("memory", func(t *testing.T) {
		seed, err := full.MemorySeed()
		if e, ok := has(full, EntryMemory); err != nil || !ok || e.GrantedBy != GrantedByRelease || string(seed["seed.md"]) != "# What I know\n" || !slices.Contains(tools(full), applink.Recall) {
			t.Errorf("the declaring agent's memory %+v, seed %v (%v), tools %v", e, seed, err, tools(full))
		}
		seed, err = bare.MemorySeed()
		if _, ok := has(bare, EntryMemory); ok || err != nil || seed != nil || slices.Contains(tools(bare), applink.Recall) || bare.KeepsMemory() {
			t.Errorf("an agent that does not declare memory has one: seed %v (%v)", seed, err)
		}
	})
	t.Run("skills", func(t *testing.T) {
		e, ok := has(full, SkillEntryID("notes"))
		if !ok || e.GrantedBy != GrantedByRelease || !e.On || !strings.Contains(e.Line, "its release ships") {
			t.Errorf("the declaring agent's skill %+v", e)
		}
		if _, ok := has(bare, SkillEntryID("notes")); ok || len(bare.Skills()) != 0 {
			t.Error("an agent that does not declare skills has its release's")
		}
		c := Config{}
		if err := addSkill(home, bare, &c, AddedSkill{Name: "mine", Description: "Mine.", Check: SkillUnchecked}); err == nil || !strings.Contains(err.Error(), "takes no skills") {
			t.Errorf("a skill was added to an agent that does not declare skills: %v", err)
		}
		if g, dir, err := stageSkills(home, bare, Config{Skills: []AddedSkill{{Name: "mine", Description: "Mine."}}}, func(string) bool { return true }); err != nil || g != nil || dir != "" {
			t.Errorf("an agent that does not declare skills was staged %v in %q (%v)", g, dir, err)
		}
	})
	t.Run("secrets", func(t *testing.T) {
		if err := addSecret(full, &Config{}, StoredSecret{Name: "NOTES_TOKEN"}); err != nil {
			t.Errorf("the declaring agent: %v", err)
		}
		if err := addSecret(bare, &Config{}, StoredSecret{Name: "NOTES_TOKEN"}); err == nil {
			t.Error("a secret was stored for an agent that does not declare secrets")
		}
		_, err := localrun.Segment{Release: bare.Release, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test",
			Grants: localrun.Grants{Secrets: map[string]string{"NOTES_TOKEN": "x"}}}.Plan(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "capabilities.persistent.secrets") {
			t.Errorf("a segment of an agent that does not declare secrets was planned with one: %v", err)
		}
	})
	t.Run("selfConfigure", func(t *testing.T) {
		if !slices.Contains(tools(full), applink.SelfConfigure) || !full.ConfiguresItself() {
			t.Errorf("the declaring agent's tools %v", tools(full))
		}
		if bare.AppLink() != "" || bare.ConfiguresItself() {
			t.Errorf("an agent that declares none of the app link's keys has the set %q", bare.AppLink())
		}
	})
}

// TestRosterReachesEveryGateForAReleasesOwnEntries: what a published
// agent's release grants reaches its segment's plan through Apply as a
// built-in's does: its memory volume, the skills its roster has on (and not
// one it has off), and the app link its declared keys compose.
func TestRosterReachesEveryGateForAReleasesOwnEntries(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := installPublished(t, home, "acme", "ada", "1.0.0", ownCapabilities)
	for _, on := range []bool{true, false} {
		c := Config{Enabled: true, Switches: map[string]bool{SkillEntryID("notes"): on}}
		g, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{})
		if err != nil {
			t.Fatal(err)
		}
		done()
		g.AppLink = filepath.Join(home, "app.sock")
		sp, err := localrun.Segment{Release: a.Release, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}.Plan(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		files := sp.Plan.Capabilities.AgentFiles
		if files == nil || !files.Memory {
			t.Errorf("the plan's folders %+v have no memory", files)
		}
		if got := slices.ContainsFunc(files.Skills, func(s spec.Skill) bool { return s.Name == "notes" }); got != on {
			t.Errorf("skill on %v: the plan's skills %v", on, files.Skills)
		}
		if g.SkillsDir != "" {
			if _, err := os.Stat(filepath.Join(g.SkillsDir, "notes", "SKILL.md")); err != nil {
				t.Errorf("the staged skill: %v", err)
			}
		}
		if want := applink.Compose(applink.PartSelf, applink.PartTasks, applink.PartRecall); sp.Plan.Capabilities.AppLink != want {
			t.Errorf("the plan's app link %q, want %q", sp.Plan.Capabilities.AppLink, want)
		}
	}
}

const testVoiceText = "# How Sam writes\n\nShort sentences. The verb early. No hedging.\n"

// TestAPublishedAgentsWritingVoiceIsOffUntilSwitchedOn: a published agent
// has a Writing Voice entry, off by default and asking agreement, which
// gives its segment the voice its opener reads only while it is on, and is
// refused when no opener can read it; a built-in has no such entry, and
// nothing switches one on for it.
func TestAPublishedAgentsWritingVoiceIsOffUntilSwitchedOn(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := installPublished(t, home, "acme", "ada", "1.0.0", publishedRelease{})
	e, ok := BuildRoster(a, Config{Enabled: true}, nil).Entry(EntryVoice)
	if !ok || e.On || e.GrantedBy != GrantedByYou || e.Kind != KindVoice {
		t.Fatalf("the voice entry %+v (%v)", e, ok)
	}
	voice, err := engine.AttachVoice([]byte(testVoiceText), "kept on this computer")
	if err != nil {
		t.Fatal(err)
	}
	opener := func(context.Context) (*engine.AttachedVoice, error) { return voice, nil }
	off := Config{Enabled: true}
	g, _, done, err := ApplyWith(context.Background(), home, a, off, BuildRoster(a, off, nil), Openers{Voice: opener}, nil, fakeSecrets{})
	if err != nil || g.Voice != nil {
		t.Fatalf("off: voice %v (%v)", g.Voice, err)
	}
	done()
	onCfg := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &onCfg, Change{Switch: &Switch{ID: EntryVoice, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	g, _, done, err = ApplyWith(context.Background(), home, a, onCfg, BuildRoster(a, onCfg, nil), Openers{Voice: opener}, nil, fakeSecrets{})
	if err != nil || g.Voice != voice {
		t.Fatalf("on: voice %v (%v)", g.Voice, err)
	}
	done()
	if _, _, _, err := ApplyWith(context.Background(), home, a, onCfg, BuildRoster(a, onCfg, nil), Openers{}, nil, fakeSecrets{}); err == nil {
		t.Error("a voice that is on was given with no opener to read it")
	}
	for _, b := range Agents() {
		if _, ok := BuildRoster(b, Config{Enabled: true}, nil).Entry(EntryVoice); ok {
			t.Errorf("the built-in %s has a voice entry", b.Name)
		}
		bc := Config{Enabled: true, Switches: map[string]bool{EntryVoice: true}}
		if g, _, done, err := ApplyWith(context.Background(), home, b, bc, BuildRoster(b, bc, nil), Openers{Voice: opener}, nil, fakeSecrets{}); err == nil {
			done()
			if g.Voice != nil {
				t.Errorf("the built-in %s was given a voice", b.Name)
			}
		}
	}
}
