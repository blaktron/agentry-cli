package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// screenshotEvent is a harness's event line for a computer-use screenshot,
// as docker-agent reports a tool's answer.
func screenshotEvent(data string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "tool_call_response", "agent_name": "root",
		"tool_definition": map[string]any{"name": "screenshot", "category": "mcp"},
		"result": map[string]any{
			"output": "Blender (window 1), 1280×720 pixels.",
			"images": []any{map[string]any{"data": data, "mimeType": "image/png"}},
		},
	})
	return b
}

// Every image an event line carries loses its data, whatever shape the
// harness gives it, and the line stays JSON that says what was left out;
// a line with no image comes back byte for byte.
func TestDropImagesLeavesNoPictureInAnEventLine(t *testing.T) {
	pixels := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("\x89PNGscreen"), 200))
	lines := map[string][]byte{
		"docker-agent images": screenshotEvent(pixels),
		"mcp content item":    []byte(`{"type":"tool_result","content":[{"type":"image","data":"` + pixels + `","mimeType":"image/png"},{"type":"text","text":"ok"}]}`),
		"base64 source":       []byte(`{"message":{"content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pixels + `"}}]}]}}`),
	}
	for name, line := range lines {
		out := dropImages(line)
		if bytes.Contains(out, []byte(pixels[:64])) {
			t.Errorf("%s: the picture is still in the line", name)
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Errorf("%s: not JSON: %v", name, err)
		}
		if !strings.Contains(string(out), "not kept: the run keeps no screenshot") {
			t.Errorf("%s: the line does not say what was left out: %s", name, out)
		}
	}
	plain := []byte(`{"type":"tool_call_response","result":{"output":"done","data":"short"}}`)
	if out := dropImages(plain); !bytes.Equal(out, plain) {
		t.Errorf("a line with no image changed: %s", out)
	}
	if out := dropImages([]byte("not json \"data\"")); string(out) != "not json \"data\"" {
		t.Errorf("a line that is not JSON changed: %s", out)
	}
}
