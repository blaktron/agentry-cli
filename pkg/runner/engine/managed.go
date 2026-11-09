package engine

import (
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// currentManaged reads the managed settings documents cached on this
// computer.
var currentManaged = managed.Current

// applyManaged refuses a run the managed settings forbid, and holds the
// rest to them, read again at the run's start rather than taken from the
// plan: a reused plan compiled before a document arrived, or by a client
// that compiled it its own way, is held to the documents cached now. A
// document that locks a setting this client does not know, or managed
// settings that cannot be verified, refuse every run; a document refuses a
// release from a namespace it does not allow, a risk the member accepted
// for it, a persistent built-in it forbids, and a plan compiled in Unsafe
// Mode where it forbids that. A run it lets start has its budgets held to
// the documents' caps and its policy to their locks (the malware check
// among them).
func applyManaged(p *Params) error {
	held := currentManaged()
	if reason := held.RunRefusal(); reason != "" {
		return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	run := managedRun(p.Plan)
	run.AcceptedRisk = p.Attestation.Consent
	if reason := held.Refusal(run); reason != "" {
		return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	if planIsUnsafe(p.Plan) {
		if reason := held.UnsafeModeRefusal(); reason != "" {
			return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
		}
	}
	if reason := destinationRefusal(held, *p); reason != "" {
		return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	p.Budgets = held.Budgets(p.Budgets)
	if p.Policy != nil {
		p.Policy = p.Policy.WithHeld(p.Policy.Held().Merge(held.PolicyHeld()))
	}
	if reason := planLockRefusal(p.Plan, held.Hold(policy.Default())); reason != "" {
		return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	return nil
}

// planLockRefusal is the refusal of what a plan compiled before a lock
// arrived still carries, judged against the locks alone (pol holds them
// over the fresh policy): wildcard egress, a browser on any public site, the
// person's computer, its whole screen, applications the person set up, and
// a persistent release that is not built in.
func planLockRefusal(plan *spec.Plan, pol *policy.Policy) string {
	if plan == nil {
		return ""
	}
	c := plan.Capabilities
	if c.EgressMode == "all" {
		if r := pol.Refused(policy.HeldWildcardEgress); r != "" {
			return r
		}
	}
	if c.Browser != nil && c.Browser.Public {
		if r := pol.Refused(policy.HeldPublicWeb); r != "" {
			return r
		}
	}
	if d := c.Desktop; d != nil {
		if r := pol.Refused(policy.HeldDesktop); r != "" {
			return r
		}
		if r := pol.Refused(policy.HeldScreen); r != "" && d.ComputerUse == spec.ComputerUseScreen {
			return r
		}
		if r := pol.Refused(policy.HeldCustomServers); r != "" && d.PersonServers != spec.PersonServersRefused {
			return r
		}
	}
	if plan.Mode == string(manifest.ModePersistent) && plan.Source != "builtin" {
		if r := pol.Refused(policy.HeldPersistentReleases); r != "" {
			return r
		}
	}
	return ""
}

// destinationRefusal is the documents' refusal of a place the run was asked
// to deliver to, before it starts: a folder (DeliverTo), or a saved
// destination of a kind they leave out.
func destinationRefusal(held managed.Effective, p Params) string {
	if p.DeliverTo != "" {
		if reason := held.DestinationRefusal(receipt.DeliveryKindFolder); reason != "" {
			return reason
		}
	}
	for _, dest := range p.Destinations {
		if reason := held.DestinationRefusal(kindOf(dest)); reason != "" {
			return reason
		}
	}
	return ""
}

// managedRun is what the managed settings judge of a plan at the run's
// start: its release, and the persistent built-in it is. Where it runs is
// judged at compile, which knows whether another device sent it.
func managedRun(plan *spec.Plan) managed.Run {
	if plan == nil {
		return managed.Run{}
	}
	r := managed.Run{Ref: plan.Ref, BuiltIn: plan.Source == "builtin"}
	r.Agent = managed.AgentOf(plan.Ref, r.BuiltIn, plan.Mode == string(manifest.ModePersistent))
	if plan.Capabilities.Coder != nil {
		r.Agent = managed.AgentCoder
	}
	return r
}

// planIsUnsafe says the plan was compiled in Unsafe Mode: its exec image
// is the Unsafe Mode image, or Unsafe Mode allowed its screen scope.
func planIsUnsafe(plan *spec.Plan) bool {
	if plan == nil {
		return false
	}
	if plan.ExecImage != nil && plan.ExecImage.Unsafe != nil {
		return true
	}
	return plan.Capabilities.Desktop != nil && plan.Capabilities.Desktop.ScreenAllowedBy == spec.ScreenByUnsafe
}

// managedRecord is the plan's managed settings as the receipt records
// them.
func managedRecord(plan *spec.Plan) []receipt.Managed {
	if plan == nil || len(plan.Managed) == 0 {
		return nil
	}
	out := make([]receipt.Managed, 0, len(plan.Managed))
	for _, m := range plan.Managed {
		out = append(out, receipt.Managed{Organization: m.Organization, Version: m.Version, Applied: append([]string(nil), m.Applied...)})
	}
	return out
}
