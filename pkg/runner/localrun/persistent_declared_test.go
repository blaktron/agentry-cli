package localrun

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// TestBuiltinCatalogsUnchanged: the app link each built-in's declared
// capabilities compose lists exactly the tools it listed when its set was
// chosen by name: Agentry Helper's helper tools, the Assistant's own tools,
// the helper tools and the meeting tools, and none for the Coding agent.
func TestBuiltinCatalogsUnchanged(t *testing.T) {
	helper := []string{"registry_search", "quickagent_show", "quickagent_install", "quickagent_run", "run_result", "installed_list",
		"schedule_add", "schedule_list", "schedule_pause", "schedule_remove", "settings_open", "docs_read"}
	assistant := slices.Concat([]string{"self_configure", "task_add", "task_list", "task_update", "task_remove", "tell_person", "recall"}, helper,
		[]string{"meeting_arm", "meeting_list", "meeting_cancel"})
	for _, c := range []struct {
		b     Builtin
		tools []string
	}{{Main(), helper}, {Assistant(), assistant}, {Coder(), nil}} {
		set := compile.AppLinkSet(c.b.Declared())
		if got := applink.Tools(set); !slices.Equal(got, c.tools) {
			t.Errorf("%s's app link (%q) lists %v, want %v", c.b.Ref(), set, got, c.tools)
		}
		if c.b.AppLink() != set {
			t.Errorf("%s's AppLink is %q, want %q", c.b.Ref(), c.b.AppLink(), set)
		}
	}
	if compile.AppLinkSet(Main().Declared()) != applink.SetMain || compile.AppLinkSet(Assistant().Declared()) != applink.SetAssistant {
		t.Error("the built-ins' declared sets are not applink's SetMain and SetAssistant")
	}
}

// TestBuiltinsDeclareTheirOwnCapabilities: what each built-in has is its
// manifest's to declare, and each declares what it had: the Assistant its
// own life, the helper and meeting tools, calendars, mail and image
// servers; the Coding agent skills, stored secrets and the coding
// toolchain; Agentry Helper the helper tools.
func TestBuiltinsDeclareTheirOwnCapabilities(t *testing.T) {
	want := map[string][]string{
		Assistant().Ref(): {"clock", "memory", "skills", "secrets", "selfConfigure", "meetings", "desktop", "helper", "calendars", "mail", "imageServers"},
		Coder().Ref():     {"skills", "secrets", "coder"},
		Main().Ref():      {"helper"},
	}
	for _, b := range PersistentBuiltins() {
		got := b.Declared().Keys()
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want[b.Ref()]))) {
			t.Errorf("%s declares %v, want %v", b.Ref(), got, want[b.Ref()])
		}
		// Each question the built-in answers is its own manifest's key.
		for _, q := range []struct {
			name string
			got  bool
			key  string
		}{
			{"KeepsTime", b.KeepsTime(), "clock"}, {"KeepsMemory", b.KeepsMemory(), "memory"}, {"KeepsSecrets", b.KeepsSecrets(), "secrets"},
			{"ArmsMeetings", b.ArmsMeetings(), "meetings"}, {"Codes", b.Codes(), "coder"}, {"ReadsCalendars", b.ReadsCalendars(), "calendars"},
		} {
			if q.got != slices.Contains(want[b.Ref()], q.key) {
				t.Errorf("%s: %s is %v", b.Ref(), q.name, q.got)
			}
		}
	}
	if (Builtin{}).Declared() != nil {
		t.Error("the zero Builtin declares something")
	}
	// Each question is its own key's: a built-in that declares one key answers
	// yes to that question alone.
	values := map[string]string{"clock": "true", "memory": "true", "secrets": "[A]", "meetings": "true", "coder": "true", "calendars": "true", "mail": "[google]", "web": "true"}
	for key := range values {
		text := "schemaVersion: \"2\"\nname: only-" + key + "\nnamespace: test\nversion: 1.0.0\nsummary: s\nmodes: [persistent]\ncapabilities:\n  persistent:\n    " + key + ": " + values[key] + "\n"
		b := Builtin{namespace: "test", name: "only-" + key, version: "1.0.0", fsys: fstest.MapFS{"agent.yaml": {Data: []byte(text)}}}
		for _, q := range []struct {
			name string
			got  bool
			key  string
		}{
			{"KeepsTime", b.KeepsTime(), "clock"}, {"KeepsMemory", b.KeepsMemory(), "memory"}, {"KeepsSecrets", b.KeepsSecrets(), "secrets"},
			{"ArmsMeetings", b.ArmsMeetings(), "meetings"}, {"Codes", b.Codes(), "coder"}, {"ReadsCalendars", b.ReadsCalendars(), "calendars"},
		} {
			if q.got != (q.key == key) {
				t.Errorf("a built-in declaring only %s: %s is %v", key, q.name, q.got)
			}
		}
	}
	if (Builtin{}).KeepsTime() || (Builtin{}).KeepsMemory() || (Builtin{}).KeepsSecrets() || (Builtin{}).ArmsMeetings() || (Builtin{}).Codes() || (Builtin{}).ReadsCalendars() {
		t.Error("the zero Builtin keeps or reads something")
	}

	// The Assistant's memory starts with the files beneath its tree's memory
	// folder; a built-in that declares no memory is given none, whatever its
	// tree holds, and a tree with no memory folder has no seed.
	seed, err := Assistant().MemorySeed()
	if err != nil || len(seed) == 0 {
		t.Fatalf("the Assistant's memory seed: %d files, %v", len(seed), err)
	}
	for _, b := range []Builtin{Main(), Coder()} {
		if seed, err := b.MemorySeed(); err != nil || seed != nil {
			t.Errorf("%s declares no memory and was given a seed: %v %v", b.Ref(), seed, err)
		}
	}
	seeded := func(name, keys string) Builtin {
		text := "schemaVersion: \"2\"\nname: " + name + "\nnamespace: test\nversion: 1.0.0\nsummary: s\nmodes: [persistent]\ncapabilities:\n  persistent:\n" + keys
		return Builtin{namespace: "test", name: name, version: "1.0.0", fsys: fstest.MapFS{
			"agent.yaml":       {Data: []byte(text)},
			"memory/people.md": {Data: []byte("Ada\n")},
			"memory/a/b.md":    {Data: []byte("b\n")},
		}}
	}
	if got, err := seeded("keeps", "    memory: true\n").MemorySeed(); err != nil || len(got) != 2 || string(got["people.md"]) != "Ada\n" || string(got["a/b.md"]) != "b\n" {
		t.Errorf("the seed of a tree with a memory folder: %v %v", got, err)
	}
	if got, err := seeded("forgets", "    clock: true\n").MemorySeed(); err != nil || got != nil {
		t.Errorf("a built-in that declares no memory was given a seed: %v %v", got, err)
	}
	if got, err := MemorySeedOf(fstest.MapFS{"agent.yaml": {Data: []byte("x")}}, "test/none@1.0.0"); err != nil || got != nil {
		t.Errorf("a tree with no memory folder: %v %v", got, err)
	}
}
