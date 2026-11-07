package desktoplink

import (
	"encoding/json"
	"slices"
	"testing"
)

// The computer-use tools are the actions of a computer-use agent less a
// lone press or release of the mouse button, plus windows and focus_window;
// each has an object schema that refuses arguments it does not name, and a
// class of read or act.
func TestTheComputerUseToolSet(t *testing.T) {
	want := []string{"screenshot", "zoom", "left_click", "right_click", "middle_click", "double_click", "triple_click", "left_click_drag",
		"mouse_move", "cursor_position", "scroll", "type", "key", "hold_key", "wait", "windows", "focus_window"}
	if got := ComputerToolNames(); !slices.Equal(got, want) {
		t.Fatalf("the tools are %v", got)
	}
	for _, tool := range ComputerTools() {
		var s struct {
			Type                 string         `json:"type"`
			Properties           map[string]any `json:"properties"`
			AdditionalProperties *bool          `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &s); err != nil || s.Type != "object" || s.Properties == nil || s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("%s's schema: %s (%v)", tool.Name, tool.InputSchema, err)
		}
		if tool.Class != ComputerRead && tool.Class != ComputerAct {
			t.Errorf("%s's class %q", tool.Name, tool.Class)
		}
		if c, ok := ComputerToolClass(tool.Name); !ok || c != tool.Class {
			t.Errorf("ComputerToolClass(%s) = %q, %v", tool.Name, c, ok)
		}
	}
	for _, never := range []string{"left_mouse_down", "left_mouse_up"} {
		if _, ok := ComputerToolClass(never); ok {
			t.Errorf("%s is a computer-use tool", never)
		}
	}
	if ComputerID != "computer" {
		t.Fatalf("the computer-use entry's id is %q", ComputerID)
	}
	// A caller's copy cannot change the set.
	tools := ComputerTools()
	tools[0].Name = "changed"
	if ComputerToolNames()[0] != "screenshot" {
		t.Fatal("ComputerTools hands out the set itself")
	}
}
