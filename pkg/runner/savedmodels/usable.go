package savedmodels

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Here is where Usable looks for a saved model's credential on this
// computer. A nil field reads the computer's own.
type Here struct {
	// Getenv reads a key's variable; nil is os.Getenv.
	Getenv func(string) string
	// SignInKept reports whether a model sign-in of kind is kept under the
	// runner home home; nil asks the home's sign-in store.
	SignInKept func(home, kind string) bool
	// KeyKept reports whether the OS keychain keeps provider's key; nil is
	// modelkey.Present.
	KeyKept func(provider string) (bool, error)
}

// UnusableError is a saved model whose credential is not on this computer,
// so a run, a fire or a turn on it is refused by its name. Code is
// KeyMissing or NeedsSignIn, as ProviderState says of its provider.
type UnusableError struct {
	// Saved is the saved model; its ID is empty for a policy with none.
	Saved policy.SavedModel
	// Provider is the provider id the credential is kept under.
	Provider string
	Code     string
	msg      string
}

func (e *UnusableError) Error() string { return e.msg }

// Usable refuses a run on rm whose saved model's credential is not on this
// computer (exit 11), naming the saved model, its provider and the fix,
// before anything is laid down for it; other is the fix of running on
// another saved model instead, in the caller's words. What it cannot tell
// from here (a sign-in that no longer renews, a key the provider revoked)
// the run refuses when it meets it. The error unwraps to an UnusableError
// when the credential is missing.
func Usable(home string, rm localrun.RunModel, other string, here Here) error {
	m := rm.Policy.Model
	if m.Provider == "" {
		return nil
	}
	getenv := here.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	signInKept := here.SignInKept
	if signInKept == nil {
		signInKept = signInKeptIn
	}
	keyKept := here.KeyKept
	if keyKept == nil {
		keyKept = modelkey.Present
	}
	hp := rm.Policy.Harness[rm.Adapter]
	what := fmt.Sprintf("the saved model %q (provider %s)", savedName(rm.Saved), m.KeyProvider())
	if rm.Saved.ID == "" {
		what = fmt.Sprintf("the model (provider %s)", m.KeyProvider())
	}
	refuse := func(code, msg string) error {
		return engine.Classify(engine.ExitPolicy, &UnusableError{Saved: rm.Saved, Provider: m.KeyProvider(), Code: code, msg: msg})
	}
	kind := policy.KeyKind(hp, m)
	if policy.IsModelSignIn(kind) {
		if !signInKept(home, kind) {
			return refuse(NeedsSignIn, fmt.Sprintf("%s cannot be used: it spends %s, and none is kept on this computer: sign in with agentry model-signins add %s, or %s",
				what, policy.SignInSentence(kind), modelsignin.VendorOf(kind), other))
		}
		return nil
	}
	env := policy.KeyEnv(hp, m)
	if env == "" || strings.TrimSpace(getenv(env)) != "" {
		return nil
	}
	if policy.KeySource(hp, m) != modelkey.SourceKeyring {
		fix := "set it, or " + other
		if m.Provider == policy.ProviderCustom && hp.APIKeyEnv == "" {
			fix += "; if the endpoint takes no key, delete the provider's apiKeyEnv from the policy"
		}
		return refuse(KeyMissing, fmt.Sprintf("%s cannot be used: its key comes from $%s, which is not set: %s", what, env, fix))
	}
	ok, err := keyKept(m.KeyProvider())
	if err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s: the keychain: %w", what, err))
	}
	if !ok {
		return refuse(KeyMissing, fmt.Sprintf("%s cannot be used: its key is neither in $%s nor in the keychain (account %s): set $%s, keep the key again (agentry providers remove %s, then agentry providers add), or %s",
			what, env, modelkey.Account(m.KeyProvider()), env, m.KeyProvider(), other))
	}
	return nil
}

// IsUnusable reports whether err is Usable's refusal of a saved model whose
// credential is not on this computer, and gives it.
func IsUnusable(err error) (*UnusableError, bool) {
	var u *UnusableError
	ok := errors.As(err, &u)
	return u, ok
}

// signInKeptIn reports whether a model sign-in of kind is kept under home.
func signInKeptIn(home, kind string) bool {
	_, err := modelsignin.Open(home, nil).Get(modelsignin.VendorOf(kind))
	return !errors.Is(err, modelsignin.ErrNone)
}

// savedName is a saved model's name as a person knows it.
func savedName(sm policy.SavedModel) string {
	if sm.Name != "" {
		return sm.Name
	}
	return sm.ID
}
