package compile

import (
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// currentManaged reads the managed settings documents cached on this
// computer. The compiler reads them itself rather than take them from its
// caller, so that no client compiles past an organization's lock.
var currentManaged = managed.Current

// ManagedSpecs are the documents as a plan records them: the organization,
// the version and the ids of the settings this client applies, never a
// value.
func ManagedSpecs(e managed.Effective) []spec.ManagedSpec {
	var out []spec.ManagedSpec
	for i := range e.Documents {
		d := &e.Documents[i]
		out = append(out, spec.ManagedSpec{Organization: d.Document.Org, Version: d.Document.Version, Applied: d.Applied()})
	}
	return out
}

// CurrentManagedSpecs are ManagedSpecs of the documents cached now: what a
// plan compiled now records, so a caller that reuses a plan can tell one
// compiled under another version.
func CurrentManagedSpecs() []spec.ManagedSpec { return ManagedSpecs(currentManaged()) }

// managedRefusal refuses a compile that the managed settings forbid: any
// compile while a document locks a setting this client does not know,
// and one in Unsafe Mode while a document forbids it.
func managedRefusal(in Inputs, held managed.Effective) error {
	if reason := held.RunRefusal(); reason != "" {
		return &PolicyRefusal{Reason: reason}
	}
	if in.Unsafe != nil {
		if reason := held.UnsafeModeRefusal(); reason != "" {
			return &PolicyRefusal{Reason: reason}
		}
	}
	return nil
}
