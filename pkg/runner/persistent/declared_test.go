package persistent

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
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

// TestAPublishedMemoryComesWithItsSkillsAndSchema: a published agent that
// declares a memory is given the memory's own skills from Agentry's tree,
// which its compaction and consolidation follow, even without skills, and
// no skill of its release's or the person's then; its memory starts from
// Agentry's schema, index and log with its release's memory/ files laid
// over them, but never over the schema.
func TestAPublishedMemoryComesWithItsSkillsAndSchema(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := installPublished(t, home, "acme", "ada", "1.0.0", publishedRelease{
		extra: "capabilities:\n  persistent:\n    memory: true\n",
		files: map[string]string{"memory/seed.md": "# What I know\n", "memory/SCHEMA.md": "Keep nothing.\n"},
	})
	var names []string
	for _, s := range a.Skills() {
		if s.GrantedBy != GrantedByBuiltin {
			t.Errorf("the skill %s is granted by %s", s.Name, s.GrantedBy)
		}
		names = append(names, s.Name)
	}
	if !slices.Equal(names, compile.MemorySkills) {
		t.Errorf("the memory agent's skills %v, want %v", names, compile.MemorySkills)
	}
	c := Config{Skills: []AddedSkill{{Name: "mine", Description: "Mine."}}}
	staged, dir, err := stageSkills(home, a, c, func(string) bool { return true })
	if err != nil || len(staged) != len(compile.MemorySkills) || dir == "" {
		t.Fatalf("staged %v in %q (%v)", staged, dir, err)
	}
	for _, s := range staged {
		if _, err := os.Stat(filepath.Join(dir, s.Name, "SKILL.md")); err != nil {
			t.Errorf("the staged %s: %v", s.Name, err)
		}
	}
	seed, err := a.MemorySeed()
	if err != nil {
		t.Fatal(err)
	}
	ours, _ := localrun.Assistant().MemorySeed()
	if string(seed["seed.md"]) != "# What I know\n" || string(seed["SCHEMA.md"]) != string(ours["SCHEMA.md"]) || len(seed["index.md"]) == 0 {
		t.Errorf("the seed holds %v, its schema %q", slices.Sorted(maps.Keys(seed)), seed["SCHEMA.md"])
	}
	g, _, done, err := Apply(context.Background(), home, a, Config{Enabled: true}, BuildRoster(a, Config{Enabled: true}, nil), nil, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if _, err := (localrun.Segment{Release: a.Release, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}).Plan(context.Background(), nil); err != nil {
		t.Errorf("a memory agent without skills was refused its memory's skills: %v", err)
	}
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
		if _, err := os.Stat(filepath.Join(g.SkillsDir, "notes", "SKILL.md")); (err == nil) != on {
			t.Errorf("skill on %v: the staged skill: %v", on, err)
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

// TestNoGrantKeyedOnName: the reach a built-in had by its name is a
// published agent's by declaring it, under any name: the coding toolchain's
// switches, calendar feeds, mail providers and the image servers the runner
// pins, each granted by its release; the same release without the block is
// given none of them.
func TestNoGrantKeyedOnName(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	reach := installPublished(t, home, "acme", "builder", "1.0.0", publishedRelease{
		extra: "capabilities:\n  persistent:\n    coder: true\n    calendars: true\n    mail: [google, fastmail]\n    imageServers: [google-workspace]\n",
	})
	bare := installPublished(t, home, "acme", "reader", "1.0.0", publishedRelease{})
	if !reach.Codes() || !reach.ReadsCalendars() || bare.Codes() || bare.ReadsCalendars() {
		t.Fatalf("codes %v/%v, calendars %v/%v", reach.Codes(), bare.Codes(), reach.ReadsCalendars(), bare.ReadsCalendars())
	}
	var mail []string
	for _, p := range reach.MailProviders() {
		mail = append(mail, p.Key)
	}
	if !slices.Equal(mail, []string{"google", "fastmail"}) || len(bare.MailProviders()) != 0 {
		t.Errorf("mail providers %v, bare %v", mail, bare.MailProviders())
	}
	images := reach.ImageServers(policy.Default().Images)
	if len(images) != 1 || images[0].Key != "gog" || len(bare.ImageServers(policy.Default().Images)) != 0 {
		t.Errorf("image servers %v", images)
	}
	if offered := reach.OfferedServers(nil); len(offered) != 1 || offered[0].MailProvider != "fastmail" || len(bare.OfferedServers(nil)) != 0 {
		t.Errorf("offered servers %v", offered)
	}
	c := Config{Enabled: true, MailProviders: []string{"google"}}
	withBuiltinOffers(t)
	withImageServers(t)
	c.MailProviders = append(c.MailProviders, "fastmail")
	r := BuildRoster(reach, c, nil)
	for _, id := range []string{EntryCommands, EntryEgress, ImageServerEntryID("gog"), ServerEntryID(reach.OfferedServers(nil)[0].Server)} {
		if e, ok := r.Entry(id); !ok || e.GrantedBy != GrantedByRelease {
			t.Errorf("the entry %s: %+v (%v)", id, e, ok)
		}
	}
	for _, id := range []string{EntryCommands, ImageServerEntryID("gog")} {
		if _, ok := BuildRoster(bare, c, nil).Entry(id); ok {
			t.Errorf("an agent that declares no reach has the entry %s", id)
		}
	}
}

// TestAPublishedAgentsWebAndReviewReachTheSegment: a published agent that
// declares web or review has an entry for it, granted by its release and on;
// its segment is given the keys its opener reads and its plan the grant;
// switched off, or with no opener, it is not given them (no opener refuses
// the segment, naming the switch); one that declares neither has no entry.
func TestAPublishedAgentsWebAndReviewReachTheSegment(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	openers := Openers{
		Web: func(context.Context) (*engine.WebRun, error) {
			return &engine.WebRun{Scan: "jev", JevKey: "jev-test"}, nil
		},
		Review: func(context.Context) (*engine.ReviewRun, error) { return &engine.ReviewRun{JevKey: "jev-test"}, nil },
	}
	for _, key := range []string{"web", "review"} {
		a := installPublished(t, home, "acme", "w"+key, "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    " + key + ": true\n"})
		c := Config{Enabled: true}
		e, ok := BuildRoster(a, c, nil).Entry(key)
		if !ok || !e.On || e.GrantedBy != GrantedByRelease {
			t.Fatalf("%s: entry %+v (%v)", key, e, ok)
		}
		g, _, done, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), openers, nil, fakeSecrets{})
		if err != nil {
			t.Fatal(err)
		}
		done()
		if (g.Web != nil) != (key == "web") || (g.Review != nil) != (key == "review") {
			t.Errorf("%s: given web %v, review %v", key, g.Web, g.Review)
		}
		sp, err := localrun.Segment{Release: a.Release, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}.Plan(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s: plan: %v", key, err)
		}
		if sp.Plan.Capabilities.WebTools != (key == "web") || sp.Plan.Capabilities.ReviewTools != (key == "review") {
			t.Errorf("%s: the plan's web %v, review %v", key, sp.Plan.Capabilities.WebTools, sp.Plan.Capabilities.ReviewTools)
		}
		off := Config{Enabled: true, Switches: map[string]bool{key: false}}
		if g, _, done, err := ApplyWith(context.Background(), home, a, off, BuildRoster(a, off, nil), openers, nil, fakeSecrets{}); err != nil || g.Web != nil || g.Review != nil {
			t.Errorf("%s off: given %v %v (%v)", key, g.Web, g.Review, err)
		} else {
			done()
		}
		if _, _, _, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), Openers{}, nil, fakeSecrets{}); err == nil {
			t.Errorf("%s: given with no opener to read its key", key)
		}
	}
	bare := installPublished(t, home, "acme", "plain", "1.0.0", publishedRelease{})
	for _, id := range []string{EntryWeb, EntryReview} {
		if _, ok := BuildRoster(bare, Config{Enabled: true}, nil).Entry(id); ok {
			t.Errorf("an agent that declares neither has the entry %s", id)
		}
	}
}

// TestTheReviewLineSaysWhatItsContainerReaches: alone, the review grant's
// container reaches TypeSafe's API alone; beside the web tools, its entry
// says the container it shares reaches the public web and holds the key.
func TestTheReviewLineSaysWhatItsContainerReaches(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	alone := installPublished(t, home, "acme", "rev", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    review: true\n"})
	beside := installPublished(t, home, "acme", "both", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    review: true\n    web: true\n    calendars: true\n"})
	line := func(a Agent) string {
		e, ok := BuildRoster(a, Config{Enabled: true}, nil).Entry(EntryReview)
		if !ok {
			t.Fatalf("%s has no review entry", a.Name)
		}
		return e.Line
	}
	if got := line(alone); got != reviewLine {
		t.Errorf("alone: %q", got)
	}
	if got := line(beside); got != reviewBesideWebLine || !strings.Contains(got, "alone, from") || !strings.Contains(got, "public web") {
		t.Errorf("beside the web tools: %q", got)
	}
	if _, ok := BuildRoster(beside, Config{Enabled: true}, nil).Entry(EntryWeb); !ok {
		t.Error("the agent beside review has no web entry")
	}
}

// TestABuiltinsWebToolsStartOffAndAskWhenSwitchedOn: the Assistant declares
// the web tools, and they start off, so an update that brings them asks the
// person nothing and gives the agent nothing. Switching them on asks for
// agreement to their sentence, and once agreed the segment has them. A
// published agent's are on, agreed at its install.
func TestABuiltinsWebToolsStartOffAndAskWhenSwitchedOn(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	openers := Openers{Web: func(context.Context) (*engine.WebRun, error) {
		return &engine.WebRun{Scan: "jev", JevKey: "jev-test"}, nil
	}}
	a := assistantAgent(t)
	c := Config{Enabled: true}
	e, ok := BuildRoster(a, c, nil).Entry(EntryWeb)
	if !ok || e.On || e.State != StateOff || e.GrantedBy != GrantedByBuiltin || e.Line == "" {
		t.Fatalf("the Assistant's web entry: %+v (%v)", e, ok)
	}
	if un := Unconsented(BuildRoster(a, c, nil), c); slices.ContainsFunc(un, func(e RosterEntry) bool { return e.ID == EntryWeb }) {
		t.Fatal("a web entry the person never switched on asks for agreement")
	}
	g, _, done, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), openers, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Web != nil {
		t.Fatalf("given the web tools before they were switched on: %+v", g.Web)
	}

	on := Change{Switch: &Switch{ID: EntryWeb, On: true}}
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, on); !errors.As(err, &ce) || !slices.ContainsFunc(ce.Entries, func(e RosterEntry) bool { return e.ID == EntryWeb }) {
		t.Fatalf("switching the web tools on without agreement: %v", err)
	}
	on.Consent = true
	if _, err := ApplyChange(home, a, &c, on); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	if e, _ := r.Entry(EntryWeb); !e.On {
		t.Fatalf("switched on and agreed: %+v", e)
	}
	g, _, done, err = ApplyWith(context.Background(), home, a, c, r, openers, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Web == nil {
		t.Fatal("switched on and agreed, the segment has no web tools")
	}

	p := installPublished(t, home, "acme", "webby", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    web: true\n"})
	if e, ok := BuildRoster(p, Config{Enabled: true}, nil).Entry(EntryWeb); !ok || !e.On || e.GrantedBy != GrantedByRelease {
		t.Fatalf("a published agent's web entry: %+v (%v)", e, ok)
	}
}
