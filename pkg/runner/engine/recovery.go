package engine

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// RecoverJSONResult applies the run's JSON convention to a terminal object
// in a refused message, for a built-in's post-step. Only a standalone
// closing fence may follow it. dropped is the byte offset of the object
// in the trimmed message; the original message stays in the receipts.
// The run itself still uses the strict JSONResult convention.
func RecoverJSONResult(text string, schema *jsonschema.Schema) (res json.RawMessage, dropped int, err error) {
	t := strings.TrimSpace(text)
	if start := strings.LastIndexByte(t, '\n'); start >= 0 && strings.TrimSpace(t[start+1:]) == "```" {
		t = strings.TrimSpace(t[:start])
	}
	err = errors.New("no schema-valid JSON object ends the message")
	for i := strings.IndexByte(t, '{'); i >= 0; {
		dec := json.NewDecoder(strings.NewReader(t[i:]))
		var raw json.RawMessage
		if dec.Decode(&raw) == nil && strings.TrimSpace(t[i+int(dec.InputOffset()):]) == "" {
			if res, err = JSONResult(string(raw), schema); err == nil {
				return res, i, nil
			}
		}
		next := strings.IndexByte(t[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return nil, 0, err
}
