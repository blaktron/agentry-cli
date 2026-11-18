package applink

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestTheSettingsPartIsBothBuiltinsOwn: the settings part lists
// settings_read and settings_change, each taking its own fields and nothing
// else; both built-ins' sets have it, the helper part lists neither and
// keeps settings_open, which changes nothing, and only settings_change says
// it asks the person, each time.
func TestTheSettingsPartIsBothBuiltinsOwn(t *testing.T) {
	takes := map[string][]string{
		SettingsRead:   {"section"},
		SettingsChange: {"id", "value"},
	}
	tools := setTools(Compose(PartSettings))
	if got := Tools(Compose(PartSettings)); !slices.Equal(got, []string{SettingsRead, SettingsChange}) {
		t.Fatalf("the settings part lists %v", got)
	}
	for _, tool := range tools {
		props := tool.Schema["properties"].(map[string]any)
		if got := slices.Sorted(maps.Keys(props)); !slices.Equal(got, takes[tool.Name]) || tool.Schema["additionalProperties"] != false {
			t.Errorf("%s takes %v", tool.Name, got)
		}
		if !IsSettingsTool(tool.Name) {
			t.Errorf("IsSettingsTool(%s) = false", tool.Name)
		}
		if said := strings.Contains(tool.Description, "asked in Agentry first, each time"); said != (tool.Name == SettingsChange) {
			t.Errorf("%s says it asks the person each time: %v", tool.Name, said)
		}
	}
	for _, tool := range tools {
		switch tool.Name {
		case SettingsRead:
			if !strings.Contains(tool.Description, "never its value") {
				t.Errorf("settings_read does not say a credential's value is never given: %s", tool.Description)
			}
		case SettingsChange:
			if req, _ := tool.Schema["required"].([]string); !slices.Equal(req, []string{"id", "value"}) {
				t.Errorf("settings_change requires %v", req)
			}
			for _, want := range []string{"credential", "locked", "settings_open"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("settings_change does not say what it refuses (%q): %s", want, tool.Description)
				}
			}
		}
	}
	for _, name := range Tools(Compose(PartHelper)) {
		if IsSettingsTool(name) {
			t.Errorf("the helper part lists %s", name)
		}
	}
	if !slices.Contains(Tools(Compose(PartHelper)), SettingsOpen) {
		t.Error("the helper part does not list settings_open")
	}
	if IsSettingsTool(SettingsOpen) || IsSettingsTool("docs_read") || IsSettingsTool("") {
		t.Error("IsSettingsTool answers yes for a tool that is not one")
	}
	if !Has(SetMain, PartSettings) || !Has(SetAssistant, PartSettings) {
		t.Errorf("the built-ins' sets are %q and %q", SetMain, SetAssistant)
	}
}
