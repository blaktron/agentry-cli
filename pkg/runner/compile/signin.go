package compile

// Model sign-ins and harness image pins in the plan (plan
// model-sign-ins.md §4.2, invariants I4, I7 and I8; agentry-cli#738,
// DR-86).

import (
	"fmt"
	"path"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// modelSignIn holds what the adapter compiled against the key kind the
// policy names, and gives the agent container the one mount a model
// sign-in needs. The compiler, not each adapter, adds it, so "the agent
// container mounts the plan and, for a model sign-in, the token file or
// the harness's config directory, and nothing else" (I4) is one rule in
// one place. A policy naming a sign-in that the adapter cannot spend is a
// refusal by policy (exit 11), the policy being what has to change.
func modelSignIn(adapterName, kind string, res *adapter.Result) error {
	msi := res.ModelSignIn
	if !policy.IsModelSignIn(kind) {
		if msi != nil {
			return fmt.Errorf("adapter %s: compiled a model sign-in for key kind %q", adapterName, kind)
		}
		return nil
	}
	what := policy.SignInSentence(kind)
	if res.Host != nil || msi == nil {
		return signInRefusal(adapterName, kind)
	}
	if msi.Kind != kind || msi.Provider != policy.SignInProvider(kind) {
		return fmt.Errorf("adapter %s: compiled a %s/%s sign-in for the policy's %s", adapterName, msi.Provider, msi.Kind, kind)
	}
	for _, e := range res.Agent.Env {
		if e.FromHostEnv != "" || e.FromKeyring != "" {
			return fmt.Errorf("adapter %s: a run paid by %s carries no key, and the agent container names %s", adapterName, what, e.Name)
		}
	}
	m := spec.Mount{Source: spec.PHModelSignIn}
	switch msi.Delivery {
	case spec.ModelSignInToken:
		if msi.Path != spec.ModelSignInMount+"/"+spec.ModelSignInTokenFile {
			return fmt.Errorf("adapter %s: a token file is read at %s, not %s", adapterName, spec.ModelSignInMount+"/"+spec.ModelSignInTokenFile, msi.Path)
		}
		m.Target, m.ReadOnly = spec.ModelSignInMount, true
	case spec.ModelSignInConfigDir:
		// The harness's own config directory holds its refresh token, the
		// one exception to I3, and only Claude Code's is allowed it (D20).
		if kind != policy.KeyKindClaudeSignIn {
			return fmt.Errorf("adapter %s: only %s is delivered as a config directory, not %s", adapterName, policy.KeyKindClaudeSignIn, kind)
		}
		if !path.IsAbs(msi.Path) || path.Clean(msi.Path) != msi.Path || msi.Path == "/" ||
			strings.HasPrefix(msi.Path+"/", spec.PlanMount+"/") || strings.HasPrefix(spec.PlanMount+"/", msi.Path+"/") {
			return fmt.Errorf("adapter %s: a config directory at %q", adapterName, msi.Path)
		}
		m.Target = msi.Path
	default:
		return fmt.Errorf("adapter %s: unknown model sign-in delivery %q", adapterName, msi.Delivery)
	}
	res.Agent.Mounts = append(res.Agent.Mounts, m)
	return nil
}

// signInRefusal is the refusal of a sign-in the adapter cannot spend.
func signInRefusal(adapterName, kind string) error {
	return &PolicyRefusal{Reason: fmt.Sprintf("the %s adapter cannot spend %s (policy apiKeyKind %s): choose the adapter that runs one, or an API key",
		adapterName, policy.SignInSentence(kind), kind)}
}

// modelSignInLine is the receipt's model.signIn line.
func modelSignInLine(msi *spec.ModelSignInSpec) string {
	line := policy.SignInSentence(msi.Kind) + " (policy apiKeyKind " + msi.Kind + "), the person's own, chosen in model setup; "
	if msi.Delivery == spec.ModelSignInConfigDir {
		return line + "the harness's own config directory, which only it writes, mounted read-write at " + msi.Path +
			" into this run's agent container alone; the runner never reads it"
	}
	return line + "a short-lived access token in one read-only file at " + msi.Path +
		", rewritten by the runner before it expires; no refresh token and no client secret enter the run"
}

// imagePin is how the agent container's image is pinned: h's pin when the
// adapter ran the image the policy names, else what the ref itself says.
// Nil for a host harness.
func imagePin(image string, h policy.HarnessImage) *spec.ImagePin {
	if image == "" {
		return nil
	}
	if image != h.Ref {
		h = policy.HarnessImage{Ref: image}
	}
	by, value := h.Pin()
	return &spec.ImagePin{By: by, Value: value}
}
