package managed

import (
	"encoding/json"
	"fmt"

	"github.com/blaktron/agentry/pkg/managedsettings"
)

// The rows that configure a member's persistent agents, and the coding
// tools' models.

// Field is one managed field of an agent's configuration: its value as the
// held documents set it, the organization that set it, and whether it is
// locked (the member's own value gives way) or a default (it fills a value
// the member has not set).
type Field[T any] struct {
	Value  T
	Org    string
	Locked bool
	// Set says a document sets the field at all.
	Set bool
}

// field is the first document's setting of id: its lock if any document
// locks it, else the first default.
func (e Effective) field(id string) (managedsettings.Setting, string, bool) {
	if s, org, ok := e.lock(id, func(managedsettings.Setting) bool { return true }); ok {
		return s, org, true
	}
	for _, d := range e.Documents {
		if s, ok := d.Document.Settings[id]; ok {
			return s, d.Document.Org, true
		}
	}
	return managedsettings.Setting{}, "", false
}

// Text is a text row (an agent's name or personality).
func (e Effective) Text(id string) Field[string] {
	s, org, ok := e.field(id)
	if !ok {
		return Field[string]{}
	}
	var v string
	if json.Unmarshal(s.Value, &v) != nil {
		return Field[string]{}
	}
	return Field[string]{Value: v, Org: org, Locked: s.Locked, Set: true}
}

// Toggle is a toggle row (whether an agent starts with Agentry).
func (e Effective) Toggle(id string) Field[bool] {
	s, org, ok := e.field(id)
	if !ok {
		return Field[bool]{}
	}
	var v bool
	if json.Unmarshal(s.Value, &v) != nil {
		return Field[bool]{}
	}
	return Field[bool]{Value: v, Org: org, Locked: s.Locked, Set: true}
}

// Refusal is the refusal of a member's change to the field what (row id)
// when a document locks it, or nil.
func (f Field[T]) Refusal(what, id string) error {
	if !f.Locked {
		return nil
	}
	return fmt.Errorf("%s is managed by %s's managed settings (%s) and cannot be changed", what, f.Org, id)
}

// UnreviewedRefusal is the refusal of letting a persistent agent read web
// pages Without AI Review, when a document forbids it, or "".
func (e Effective) UnreviewedRefusal() string {
	if _, org, ok := e.lock(managedsettings.AgentsUnreviewed, func(s managedsettings.Setting) bool { return s.String() == managedsettings.Forbidden }); ok {
		return fmt.Sprintf("Reading web pages Without AI Review is forbidden by %s's managed settings (%s)", org, managedsettings.AgentsUnreviewed)
	}
	return ""
}

// UnreviewedDefaultOff says an organization default forbids the Without AI
// Review bypass: the member's switch starts off and they may turn it on.
func (e Effective) UnreviewedDefaultOff() bool {
	f := e.textChoice(managedsettings.AgentsUnreviewed)
	return f.Set && !f.Locked && f.Value == managedsettings.Forbidden
}

// textChoice is a choice row as a Field.
func (e Effective) textChoice(id string) Field[string] {
	s, org, ok := e.field(id)
	if !ok {
		return Field[string]{}
	}
	return Field[string]{Value: s.String(), Org: org, Locked: s.Locked, Set: true}
}

// CodingToolModel is the model a document sets for the coding tool (a
// member of codingTools.models), locked or as a default.
func (e Effective) CodingToolModel(tool string) Field[string] {
	s, org, ok := e.field(managedsettings.CodingToolsModels)
	if !ok {
		return Field[string]{}
	}
	var v map[string]string
	if json.Unmarshal(s.Value, &v) != nil || v[tool] == "" {
		return Field[string]{}
	}
	return Field[string]{Value: v[tool], Org: org, Locked: s.Locked, Set: true}
}

// Servers is a servers row: the remote MCP servers a document puts on an
// agent's roster.
func (e Effective) Servers(id string) Field[[]managedsettings.Server] {
	s, org, ok := e.field(id)
	if !ok {
		return Field[[]managedsettings.Server]{}
	}
	var v []managedsettings.Server
	if json.Unmarshal(s.Value, &v) != nil {
		return Field[[]managedsettings.Server]{}
	}
	return Field[[]managedsettings.Server]{Value: v, Org: org, Locked: s.Locked, Set: true}
}

// Packs is a packs row: the organization's shared knowledge packs, by
// name, a document puts on an agent's roster.
func (e Effective) Packs(id string) Field[[]string] {
	s, org, ok := e.field(id)
	if !ok {
		return Field[[]string]{}
	}
	var v []string
	if json.Unmarshal(s.Value, &v) != nil {
		return Field[[]string]{}
	}
	return Field[[]string]{Value: v, Org: org, Locked: s.Locked, Set: true}
}
