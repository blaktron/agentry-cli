package main

import "github.com/blaktron/agentry-cli/pkg/runner/spec"

// budgetFlags are the budgets a command that runs a built-in package was
// given on its command line, as `draft` and `knowledge create|add` both
// take them.
type budgetFlags struct {
	// timeout is the run's wall-clock budget in seconds; 0 names none.
	timeout int
	// maxTurns and maxCost are named when not negative; 0 lifts the cap.
	maxTurns int
	maxCost  float64
}

// over lays what the command line named over the budgets b the run would
// otherwise start from. A named budget always wins, shorter or longer:
// that is a person's choice for one run, and it is how a sandbox host's
// job carries its own caps, which are always named.
func (f budgetFlags) over(b spec.Budgets) spec.Budgets {
	if f.timeout > 0 {
		b.TimeoutSeconds = f.timeout
	}
	if f.maxTurns >= 0 {
		b.MaxTurns = f.maxTurns
	}
	if f.maxCost >= 0 {
		b.MaxCostUSD = f.maxCost
	}
	return b
}
