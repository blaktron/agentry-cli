package drafter

import (
	"encoding/json"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// recoverAnswer leaves the refused envelope intact: its message remains
// the run's record while the post-step continues with the recovered object.
func recoverAnswer(env *receipt.Envelope) (*receipt.Envelope, int, error) {
	var text string
	if err := json.Unmarshal(env.Result, &text); err != nil {
		return nil, 0, err
	}
	schemaBytes, err := fs.ReadFile(FS(), compile.OutputSchemaFile)
	if err != nil {
		return nil, 0, err
	}
	schema, err := compile.CompileOutputSchema(schemaBytes)
	if err != nil {
		return nil, 0, err
	}
	result, dropped, err := engine.RecoverJSONResult(text, schema)
	if err != nil {
		return nil, 0, err
	}
	recovered := *env
	recovered.Result = result
	return &recovered, utf8.RuneCountInString(strings.TrimSpace(text)[:dropped]), nil
}
