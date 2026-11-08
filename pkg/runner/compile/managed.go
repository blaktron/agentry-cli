package compile

import (
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// currentManaged reads the managed settings documents cached on this
// computer. The compiler reads them itself rather than take them from its
// caller, so that no client compiles past an organization's lock.
var currentManaged = managed.Current

// managedSpecs are the documents a plan records: this computer's, then
// those of the device that sent the run.
func managedSpecs(held managed.Effective, sender *managed.Narrowing) []spec.ManagedSpec {
	out := held.Specs()
	if sender != nil {
		out = append(out, sender.Documents...)
	}
	return out
}

// CurrentManagedSpecs are the documents a plan compiled now records, with
// sender's (nil for a run started on this computer): a caller that reuses
// a plan compiled under other documents compiles again.
func CurrentManagedSpecs(sender *managed.Narrowing) []spec.ManagedSpec {
	return managedSpecs(currentManaged(), sender)
}

// managedRefusal refuses a compile that the managed settings forbid: any
// compile while a document locks a setting this client does not know or
// what is held cannot be verified, and one in Unsafe Mode while this
// computer's documents, or the sending device's, forbid it.
func managedRefusal(in Inputs, held managed.Effective) error {
	if reason := held.RunRefusal(); reason != "" {
		return &PolicyRefusal{Reason: reason}
	}
	if err := in.Sender.Validate(); err != nil {
		return &PolicyRefusal{Reason: err.Error()}
	}
	if in.Unsafe == nil {
		return nil
	}
	if reason := held.UnsafeModeRefusal(); reason != "" {
		return &PolicyRefusal{Reason: reason}
	}
	if in.Sender != nil && in.Sender.UnsafeModeRefusal != "" {
		return &PolicyRefusal{Reason: in.Sender.UnsafeModeRefusal + ", on the device that sent this run"}
	}
	return nil
}
