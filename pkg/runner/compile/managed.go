package compile

import (
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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

// managedRun is what the managed settings judge of the run in compiles:
// its release, whether it starts here, whether it reaches target hosts,
// and the persistent built-in it is.
func managedRun(in Inputs) managed.Run {
	r := managed.Run{BuiltIn: in.Source == "builtin", Local: in.Sender == nil}
	m := in.Manifest
	if m == nil {
		return r
	}
	r.Ref = m.Namespace + "/" + m.Name + "@" + m.Version
	r.TargetHosts = len(m.Capabilities.TargetHosts) > 0
	r.Agent = managed.AgentOf(r.Ref, r.BuiltIn, in.Mode == manifest.ModePersistent)
	if in.Coder != nil {
		r.Agent = managed.AgentCoder
	}
	return r
}

// managedRefusal refuses a compile that the managed settings forbid: any
// compile while a document locks a setting this client does not know or
// what is held cannot be verified, one the documents refuse for where it
// runs, where its release comes from or the persistent built-in it is, and
// one in Unsafe Mode while this computer's documents, or the sending
// device's, forbid it.
func managedRefusal(in Inputs, held managed.Effective) error {
	if reason := held.RunRefusal(); reason != "" {
		return &PolicyRefusal{Reason: reason}
	}
	if err := in.Sender.Validate(); err != nil {
		return &PolicyRefusal{Reason: err.Error()}
	}
	if reason := held.Refusal(managedRun(in)); reason != "" {
		return &PolicyRefusal{Reason: reason}
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

// heldPolicy is the policy a compile reads: pol held to this computer's
// documents and the sending device's, with the documents' budget defaults
// and caps. It is a copy; the caller's policy is untouched.
func heldPolicy(pol *policy.Policy, held managed.Effective, sender *managed.Narrowing) *policy.Policy {
	if pol == nil {
		return nil
	}
	out := pol.WithHeld(sender.Held(pol.Held().Merge(held.PolicyHeld())))
	b := held.PolicyBudgets(out.Budgets)
	capped := sender.Budgets(spec.Budgets{TimeoutSeconds: b.TimeoutSeconds, MaxTurns: b.MaxTurns, MaxCostUSD: b.MaxCostUSD})
	out.Budgets = policy.Budgets{TimeoutSeconds: capped.TimeoutSeconds, MaxTurns: capped.MaxTurns, MaxCostUSD: capped.MaxCostUSD}
	return out
}
