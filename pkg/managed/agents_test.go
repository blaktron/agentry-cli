package managed

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"
)

// An agent configuration row reads as a field: its value, the
// organization, and whether it is locked; a lock refuses the member's
// change, a default does not.
func TestAnAgentConfigurationRowReadsAsAField(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.AgentsAssistantName:        lock(t, "Ada"),
		managedsettings.AgentsAssistantPersonality: preset(t, "Brief."),
		managedsettings.AgentsAssistantRunOnStart:  preset(t, true),
		managedsettings.CodingToolsModels:          lock(t, map[string]string{"codex": "gpt-5.5"}),
	})
	if f := e.Text(managedsettings.AgentsAssistantName); !f.Set || !f.Locked || f.Value != "Ada" || f.Org != "acme" {
		t.Fatalf("name %+v", f)
	}
	if err := e.Text(managedsettings.AgentsAssistantName).Refusal("The Assistant's name", managedsettings.AgentsAssistantName); err == nil ||
		!strings.Contains(err.Error(), "managed by acme's managed settings (agents.assistant.name)") {
		t.Fatalf("a locked name's refusal: %v", err)
	}
	if f := e.Text(managedsettings.AgentsAssistantPersonality); !f.Set || f.Locked || f.Refusal("x", "y") != nil {
		t.Fatalf("a default personality %+v", f)
	}
	if f := e.Toggle(managedsettings.AgentsAssistantRunOnStart); !f.Set || !f.Value || f.Locked {
		t.Fatalf("runOnStart %+v", f)
	}
	if f := e.CodingToolModel("codex"); !f.Locked || f.Value != "gpt-5.5" {
		t.Fatalf("codex's model %+v", f)
	}
	if f := e.CodingToolModel("claude-code"); f.Set {
		t.Fatalf("a tool the row leaves out %+v", f)
	}
	if f := (Effective{}).Text(managedsettings.AgentsAssistantName); f.Set {
		t.Fatalf("no document %+v", f)
	}
}

// A locked coding tool model refuses a run of that tool on another model;
// a run on the model, or of another tool, is allowed.
func TestACodingToolsModelLockHoldsItsTool(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.CodingToolsModels: lock(t, map[string]string{"codex": "gpt-5.5"}),
	})
	if r := e.ModelRefusal(RunModel{Provider: "openai", Model: "gpt-4", Adapter: "codex"}); !strings.Contains(r, "hold the codex harness to the model gpt-5.5 (codingTools.models)") {
		t.Fatalf("another model: %q", r)
	}
	if r := e.ModelRefusal(RunModel{Provider: "openai", Model: "gpt-5.5", Adapter: "codex"}); r != "" {
		t.Fatalf("the model: %q", r)
	}
	if r := e.ModelRefusal(RunModel{Provider: "anthropic", Model: "claude-sonnet-5-5", Adapter: "claude"}); r != "" {
		t.Fatalf("another tool: %q", r)
	}
	def := held("acme", map[string]managedsettings.Setting{
		managedsettings.CodingToolsModels: preset(t, map[string]string{"codex": "gpt-5.5"}),
	})
	if r := def.ModelRefusal(RunModel{Provider: "openai", Model: "gpt-4", Adapter: "codex"}); r != "" {
		t.Fatalf("a default refused a run: %q", r)
	}
}

// The Without AI Review bypass is refused only under a lock to forbidden.
func TestTheUnreviewedRowRefusesOnlyALock(t *testing.T) {
	locked := held("acme", map[string]managedsettings.Setting{managedsettings.AgentsUnreviewed: lock(t, "forbidden")})
	if r := locked.UnreviewedRefusal(); !strings.Contains(r, "Without AI Review is forbidden by acme's managed settings (agents.unreviewed)") {
		t.Fatalf("locked: %q", r)
	}
	def := held("acme", map[string]managedsettings.Setting{managedsettings.AgentsUnreviewed: preset(t, "forbidden")})
	if r := def.UnreviewedRefusal(); r != "" || !def.UnreviewedDefaultOff() {
		t.Fatalf("default: %q %v", r, def.UnreviewedDefaultOff())
	}
	if locked.UnreviewedDefaultOff() || (Effective{}).UnreviewedRefusal() != "" {
		t.Fatal("a lock or no document read as a default")
	}
}
