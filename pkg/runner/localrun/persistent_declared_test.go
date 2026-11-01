package localrun

import (
	"slices"
	"testing"

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
	assistant := slices.Concat([]string{"self_configure", "task_add", "task_list", "task_update", "task_remove", "recall"}, helper,
		[]string{"meeting_arm", "meeting_list", "meeting_cancel"})
	for _, c := range []struct {
		b     Builtin
		tools []string
	}{{Main(), helper}, {Assistant(), assistant}, {Coder(), nil}} {
		set := compile.AppLinkSet(c.b.Declared())
		if got := applink.Tools(set); !slices.Equal(got, c.tools) {
			t.Errorf("%s's app link (%q) lists %v, want %v", c.b.Ref(), set, got, c.tools)
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
		Assistant().Ref(): {"clock", "memory", "skills", "secrets", "selfConfigure", "meetings", "helper", "calendars", "mail", "imageServers"},
		Coder().Ref():     {"skills", "secrets", "coder"},
		Main().Ref():      {"helper"},
	}
	for _, b := range PersistentBuiltins() {
		got := b.Declared().Keys()
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want[b.Ref()]))) {
			t.Errorf("%s declares %v, want %v", b.Ref(), got, want[b.Ref()])
		}
	}
	if (Builtin{}).Declared() != nil {
		t.Error("the zero Builtin declares something")
	}
}
