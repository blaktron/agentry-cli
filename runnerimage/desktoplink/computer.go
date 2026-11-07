package desktoplink

import (
	"encoding/json"
	"slices"
)

// Computer use. A release that declares computer use gets one more desktop
// link entry, agentry-desktop-computer, whose tools are the actions of a
// computer-use agent: one tool an action, named as the common computer-use
// tool sets name them, so a model trained on one finds its way, plus two of
// the link's own, windows and focus_window. Positions are pixels of the
// window's own image at the size the last screenshot reported, never of the
// display. No tool presses or releases the mouse button alone: a press left
// down when a later call is refused is the one state a stop cannot undo.
// There is no clipboard tool, no file dialog and no tool that opens an
// application. Agentry Desktop answers every one, within the scope the
// person allowed; these definitions are the run's, so the agent sees the
// same tools whatever version of the app answers them.

// The classes of the computer-use tools, as an application server's tools
// are classed: read looks, act changes something on the person's screen.
const (
	ComputerRead = "read"
	ComputerAct  = "act"
)

// ComputerTool is one computer-use tool and its class.
type ComputerTool struct {
	Tool
	Class string
}

// The longest a wait or a held key may last, in seconds.
const (
	MaxWaitSeconds    = 30
	MaxHoldKeySeconds = 10
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

const (
	coordinateProp = `"coordinate":{"type":"array","items":{"type":"integer","minimum":0},"minItems":2,"maxItems":2,"description":"[x, y] in pixels of the window's image, as the last screenshot showed it"}`
	modifiersProp  = `"text":{"type":"string","description":"keys held during the action, as key names joined by +, for example shift or ctrl+alt"}`
	windowProp     = `"window":{"type":"string","description":"a window's id from windows; the focused window when omitted"}`
)

var computerTools = []ComputerTool{
	{Class: ComputerRead, Tool: Tool{Name: "screenshot", Description: "Capture the focused window (or the window named) as a PNG image. Positions in every other tool are pixels of this image, at the size it reports.",
		InputSchema: schema(`{"type":"object","properties":{` + windowProp + `},"additionalProperties":false}`)}},
	{Class: ComputerRead, Tool: Tool{Name: "zoom", Description: "Capture a region of the focused window at a larger scale, to read small text. The region is [x0, y0, x1, y1] in pixels of the last screenshot.",
		InputSchema: schema(`{"type":"object","properties":{"region":{"type":"array","items":{"type":"integer","minimum":0},"minItems":4,"maxItems":4,"description":"[x0, y0, x1, y1] in pixels of the last screenshot"}},"required":["region"],"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "left_click", Description: "Click the left mouse button at a position in the focused window.", InputSchema: clickSchema}},
	{Class: ComputerAct, Tool: Tool{Name: "right_click", Description: "Click the right mouse button at a position in the focused window.", InputSchema: clickSchema}},
	{Class: ComputerAct, Tool: Tool{Name: "middle_click", Description: "Click the middle mouse button at a position in the focused window.", InputSchema: clickSchema}},
	{Class: ComputerAct, Tool: Tool{Name: "double_click", Description: "Double-click the left mouse button at a position in the focused window.", InputSchema: clickSchema}},
	{Class: ComputerAct, Tool: Tool{Name: "triple_click", Description: "Triple-click the left mouse button at a position in the focused window.", InputSchema: clickSchema}},
	{Class: ComputerAct, Tool: Tool{Name: "left_click_drag", Description: "Press the left mouse button at one position, move to another and release it, all inside the focused window.",
		InputSchema: schema(`{"type":"object","properties":{"start_coordinate":{"type":"array","items":{"type":"integer","minimum":0},"minItems":2,"maxItems":2,"description":"[x, y] where the drag starts"},` + coordinateProp + `,` + modifiersProp + `},"required":["start_coordinate","coordinate"],"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "mouse_move", Description: "Move the pointer to a position in the focused window.",
		InputSchema: schema(`{"type":"object","properties":{` + coordinateProp + `},"required":["coordinate"],"additionalProperties":false}`)}},
	{Class: ComputerRead, Tool: Tool{Name: "cursor_position", Description: "The pointer's position in pixels of the focused window's image, or that it is outside the window.",
		InputSchema: schema(`{"type":"object","properties":{},"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "scroll", Description: "Scroll at a position in the focused window.",
		InputSchema: schema(`{"type":"object","properties":{` + coordinateProp + `,"scroll_direction":{"type":"string","enum":["up","down","left","right"]},"scroll_amount":{"type":"integer","minimum":1,"maximum":50,"description":"how many steps of the wheel"},` + modifiersProp + `},"required":["coordinate","scroll_direction","scroll_amount"],"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "type", Description: "Type text into the focused window, as keystrokes.",
		InputSchema: schema(`{"type":"object","properties":{"text":{"type":"string","maxLength":4096}},"required":["text"],"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "key", Description: "Press a key or a combination in the focused window, for example Return, ctrl+s or shift+Tab.",
		InputSchema: schema(`{"type":"object","properties":{"text":{"type":"string","maxLength":128,"description":"key names joined by +"}},"required":["text"],"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "hold_key", Description: "Hold a key or a combination down in the focused window for a number of seconds, then release it.",
		InputSchema: schema(`{"type":"object","properties":{"text":{"type":"string","maxLength":128,"description":"key names joined by +"},"duration":{"type":"number","exclusiveMinimum":0,"maximum":10}},"required":["text","duration"],"additionalProperties":false}`)}},
	{Class: ComputerRead, Tool: Tool{Name: "wait", Description: "Wait a number of seconds before the next action, for the application to finish what it is doing.",
		InputSchema: schema(`{"type":"object","properties":{"duration":{"type":"number","exclusiveMinimum":0,"maximum":30}},"required":["duration"],"additionalProperties":false}`)}},
	{Class: ComputerRead, Tool: Tool{Name: "windows", Description: "The windows this run may see and use: each one's id, application, title and size, and which is focused.",
		InputSchema: schema(`{"type":"object","properties":{},"additionalProperties":false}`)}},
	{Class: ComputerAct, Tool: Tool{Name: "focus_window", Description: "Bring one of the windows this run may use to the front and give it the keyboard.",
		InputSchema: schema(`{"type":"object","properties":{"window":{"type":"string","description":"the window's id from windows"}},"required":["window"],"additionalProperties":false}`)}},
}

var clickSchema = schema(`{"type":"object","properties":{` + coordinateProp + `,` + modifiersProp + `},"additionalProperties":false}`)

// ComputerTools are the computer-use tools, in a fixed order.
func ComputerTools() []ComputerTool { return slices.Clone(computerTools) }

// ComputerToolNames are the computer-use tools' names, in the same order.
func ComputerToolNames() []string {
	out := make([]string, len(computerTools))
	for i, t := range computerTools {
		out[i] = t.Name
	}
	return out
}

// ComputerToolClass is a computer-use tool's class, and whether the tool is
// one.
func ComputerToolClass(name string) (string, bool) {
	for _, t := range computerTools {
		if t.Name == name {
			return t.Class, true
		}
	}
	return "", false
}
