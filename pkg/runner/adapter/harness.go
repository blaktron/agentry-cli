// Pieces every harness adapter shares. The point is that a QuickAgent
// behaves the same whichever harness runs it: the same instruction text
// reaches the model, the same result kind comes back, and the conformance
// suite can hold every adapter to one contract.
package adapter

import "bytes"

// schemaInstruction appends the structured-output postscript to the
// rendered instruction and reports the plan's result kind. The schema rides
// in the prompt as guidance and the ENGINE validates the final message, so
// the wording is the same for every harness — a per adapter phrasing would
// make the same QuickAgent answer differently depending on which harness
// ran it.
func schemaInstruction(instruction string, schemaJSON []byte) (text, resultKind string) {
	if len(schemaJSON) == 0 {
		return instruction, "text"
	}
	return instruction + "\n\nYour final message MUST be a single JSON document matching this JSON Schema, with no surrounding prose:\n\n" +
		string(bytes.TrimSpace(schemaJSON)) + "\n", "json"
}
