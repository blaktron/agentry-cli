package managed

// The quickagents row as a client meets it: the QuickAgents an organization
// gives its members. A required one is installed at each fetch and cannot be
// uninstalled while a document requires it; an offered one is listed for the
// member to install on demand. The row is a lock, so a document that sets it
// as a default is passed over.

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/blaktron/agentry/pkg/managedsettings"
)

// Assigned is one assignment of a held document with the organization and
// registry it came from.
type Assigned struct {
	managedsettings.Assignment
	// Organization is the handle of the organization whose document lists it.
	Organization string
	// Registry is the registry the document is bound to.
	Registry string
}

// Required says the assignment is installed for every member.
func (a Assigned) Required() bool { return a.Mode == managedsettings.AssignRequired }

// Latest says the assignment follows the newest version.
func (a Assigned) Latest() bool { return a.Version == managedsettings.VersionLatest }

// Selector is the reference an install resolves: ns/name@version, or
// ns/name for the newest.
func (a Assigned) Selector() string {
	if a.Latest() || a.Version == "" {
		return a.Ref
	}
	return a.Ref + "@" + a.Version
}

// Assignments are the locked quickagents rows of the held documents, in
// document order. A ref listed by two documents is the first document's.
func (e Effective) Assignments() []Assigned {
	var out []Assigned
	seen := map[string]bool{}
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[managedsettings.QuickAgents]
		if !ok || !s.Locked {
			continue
		}
		var list []managedsettings.Assignment
		if json.Unmarshal(s.Value, &list) != nil {
			continue
		}
		for _, a := range list {
			if seen[a.Ref] {
				continue
			}
			seen[a.Ref] = true
			out = append(out, Assigned{Assignment: a, Organization: d.Document.Org, Registry: d.Registry})
		}
	}
	return out
}

// Required are the assignments installed for every member.
func (e Effective) Required() []Assigned {
	return slices.DeleteFunc(e.Assignments(), func(a Assigned) bool { return !a.Required() })
}

// Offered are the assignments a member may install: the offered ones and
// the required ones.
func (e Effective) Offered() []Assigned { return e.Assignments() }

// AssignmentOf is the assignment of the release ns/name, if a document
// lists it.
func (e Effective) AssignmentOf(ref string) (Assigned, bool) {
	for _, a := range e.Assignments() {
		if a.Ref == ref {
			return a, true
		}
	}
	return Assigned{}, false
}

// RequiredBy is the organization whose document requires the release
// ns/name, or "".
func (e Effective) RequiredBy(ref string) string {
	if a, ok := e.AssignmentOf(ref); ok && a.Required() {
		return a.Organization
	}
	return ""
}

// UninstallRefusal is the refusal of removing the release ns/name from this
// computer, or "" when no document requires it.
func (e Effective) UninstallRefusal(ref string) string {
	org := e.RequiredBy(ref)
	if org == "" {
		return ""
	}
	return fmt.Sprintf("%s is required by %s's managed settings (%s) and cannot be uninstalled while it is", ref, org, managedsettings.QuickAgents)
}
