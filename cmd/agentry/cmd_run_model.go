package main

import (
	"errors"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/savedmodels"
)

// refuseModelElsewhere refuses --model where this computer's saved models
// do not apply: a hosted run pays with Agentry's own model access, and a
// runhost's own run of a job takes the model the job names. A one-off run
// with --on carries the saved model it names; a schedule with --on names
// one the runhost holds.
func refuseModelElsewhere(opts runOpts) error {
	switch {
	case opts.installed:
		return engine.Classify(engine.ExitUsage, errors.New("--model chooses among this computer's saved models, and a runhost's own run takes the model its job names"))
	case hostedFor(opts):
		return engine.Classify(engine.ExitUsage, errors.New("--model chooses among this computer's saved models, and a hosted run uses Agentry's own model access"))
	}
	return nil
}

// signInKept reports whether a model sign-in of kind is kept on this
// computer; a variable so a test can answer for the store.
var signInKept = func(home, kind string) bool {
	_, err := modelsignin.Open(home, connectionKeys()).Get(modelsignin.VendorOf(kind))
	return !errors.Is(err, modelsignin.ErrNone)
}

// keyKept is modelkey.Present; a variable so a test can answer for the
// keychain.
var keyKept = modelkey.Present

// savedModelUsable refuses a run whose saved model's credential is not on
// this computer (exit 11), naming the saved model, its provider and the
// fix, before anything is laid down for it; other is the fix of running on
// another model instead. The rule is savedmodels.Usable, which Agentry
// Desktop applies too.
func savedModelUsable(home string, rm localrun.RunModel, getenv func(string) string, other string) error {
	return savedmodels.Usable(home, rm, other, savedmodels.Here{Getenv: getenv, SignInKept: signInKept, KeyKept: keyKept})
}

// runOnAnother is a run's fix of running on another saved model.
const runOnAnother = "run on another saved model (--model; agentry models list)"
