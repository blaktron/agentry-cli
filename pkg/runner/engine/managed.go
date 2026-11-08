package engine

import (
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// currentManaged reads the managed settings documents cached on this
// computer.
var currentManaged = managed.Current

// checkManaged refuses a run the managed settings forbid, read again at
// the run's start rather than taken from the plan: a reused plan compiled
// before a document arrived, or by a client that compiled it its own way,
// is held to the documents cached now. A document that locks a setting
// this client does not know, or managed settings that cannot be verified,
// refuse every run; a document that forbids Unsafe Mode refuses a plan
// compiled in it.
func checkManaged(p Params) error {
	held := currentManaged()
	if reason := held.RunRefusal(); reason != "" {
		return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
	}
	if planIsUnsafe(p.Plan) {
		if reason := held.UnsafeModeRefusal(); reason != "" {
			return Classify(ExitPolicy, &compile.PolicyRefusal{Reason: reason})
		}
	}
	return nil
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
