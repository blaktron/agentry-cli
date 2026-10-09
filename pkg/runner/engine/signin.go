package engine

// A run paid by a model sign-in (plan model-sign-ins.md §4.2, invariants
// I3, I4, I7, I9; agentry-cli#738, DR-86). The caller hands the engine
// what the run may have of the person's sign-in, and nothing else of it:
// for a token-file sign-in, a short-lived access token, which the engine
// writes into one file in a folder of the run's own, mounted read-only
// into the agent container, and rewrites before it expires by asking the
// caller again (the renewer of connections.go, DR-73 and DR-77, extended
// to the agent container); for a config-directory sign-in, the harness's
// own config directory, which the engine mounts and never reads.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// ModelSignInGrant is what a run is given of the person's model sign-in.
type ModelSignInGrant struct {
	// Kind is the sign-in's kind (policy.IsModelSignIn), which the run
	// holds against the kind its plan was compiled for (I7).
	Kind string
	// Token is the access token of a token-file sign-in: an access token
	// alone, never the refresh token or a client secret (I3).
	Token ConnectionToken
	// ConfigDir is a config-directory sign-in's directory: an absolute
	// host path, or the name of a docker volume, which the harness alone
	// reads and writes.
	ConfigDir string
}

// modelSignInDir is the run's folder of its model sign-in's token file,
// deleted at teardown whatever the run kept.
func (rd *runDirs) modelSignInDir() string { return filepath.Join(rd.cfg, "model-signin") }

// ownVolumeRE is a docker volume of Agentry's own.
var ownVolumeRE = regexp.MustCompile(`^agentry-[a-z0-9][a-z0-9_.-]*$`)

// OwnSignInDir reports whether dir may hold a config-directory sign-in: a
// docker volume named agentry-…, or a directory strictly under the runner
// home's model sign-in store (home empty: volumes only).
func OwnSignInDir(home, dir string) bool {
	if !filepath.IsAbs(dir) {
		return ownVolumeRE.MatchString(dir)
	}
	if home == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(home, "model-signins"), filepath.Clean(dir))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// checkModelSignIn holds what the caller gave the run against the plan,
// before anything touches the daemon: the kind the plan was compiled for
// (I7), a grant of the shape its delivery needs, and no cost budget for a
// run that reports tokens (D13). A plan compiled for a key that is handed
// a sign-in is refused too, so a grant never rides along unasked.
func checkModelSignIn(p Params) error {
	msi, g := p.Plan.Harness.ModelSignIn, p.ModelSignIn
	switch {
	case msi == nil && g == nil:
		return nil
	case msi == nil:
		return Classify(ExitPolicy, fmt.Errorf("this run was compiled for an API key and was handed a model sign-in (%s); recompile under a policy that names it", g.Kind))
	case g == nil:
		return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("this run spends %s (policy apiKeyKind %s) and was given no sign-in: sign in first (agentry setup)",
			policy.SignInSentence(msi.Kind), msi.Kind), receipt.ReasonSignInMissing, ""))
	case g.Kind != msi.Kind:
		return Classify(ExitPolicy, fmt.Errorf("this run was compiled for %s (%s) and was handed %s (%s); recompile under the policy that names the sign-in in use",
			policy.SignInSentence(msi.Kind), msi.Kind, policy.SignInSentence(g.Kind), g.Kind))
	}
	if p.Budgets.MaxCostUSD > 0 {
		return Classify(ExitUsage, errors.New("--max-cost cannot apply: "+policy.MaxCostSignInSentence))
	}
	switch msi.Delivery {
	case spec.ModelSignInToken:
		if g.Token.AccessToken == "" {
			return Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("the %s gave this run no access token: sign in again (agentry setup)", policy.SignInSentence(msi.Kind)),
				receipt.ReasonSignInMissing, ""))
		}
		if !accessTokenRE.MatchString(g.Token.AccessToken) {
			return Classify(ExitCLI, errors.New("the model sign-in's access token is not in the shape an access token takes (RFC 6750), so it is not written into the run"))
		}
	case spec.ModelSignInConfigDir:
		// Only a directory of Agentry's own may be mounted read-write into
		// the agent container: one under the runner home's model sign-in
		// store, or a docker volume named agentry-…, never a person's
		// own folder (plan D20: never their ~/.claude).
		d := g.ConfigDir
		switch {
		case filepath.IsAbs(d):
			if !OwnSignInDir(p.RunnerHome, d) {
				return Classify(ExitPolicy, fmt.Errorf("the %s's directory %s is not one of Agentry's own (under %s): sign in again (agentry setup)",
					policy.SignInSentence(msi.Kind), d, filepath.Join(p.RunnerHome, "model-signins")))
			}
			if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
				return Classify(ExitPolicy, fmt.Errorf("the %s's directory is missing: sign in again (agentry setup)", policy.SignInSentence(msi.Kind)))
			}
		case !OwnSignInDir("", d):
			return Classify(ExitCLI, fmt.Errorf("the %s's directory %q is neither one of Agentry's own folders nor a docker volume named agentry-…", policy.SignInSentence(msi.Kind), d))
		}
	default:
		return Classify(ExitCLI, fmt.Errorf("the plan names an unknown model sign-in delivery %q", msi.Delivery))
	}
	return nil
}

// modelSignInValueKey is where a sign-in's access token sits in the run's
// value map, for the redactor; no secrets-file key can be it.
const modelSignInValueKey = "model-signin"

// writeModelSignIn gives the agent container the run's model sign-in: a
// token-file sign-in's token in its folder (0700, the file 0600, labelled
// for the container), added to values for the redactor; a config
// directory is only named, for the replacer. The renewer holds the token
// from here (rd.conns).
func writeModelSignIn(p Params, rd *runDirs, values map[string]string) error {
	msi := p.Plan.Harness.ModelSignIn
	if msi == nil {
		return nil
	}
	if msi.Delivery == spec.ModelSignInConfigDir {
		rd.modelSignIn = p.ModelSignIn.ConfigDir
		return nil
	}
	rd.modelSignIn = rd.modelSignInDir()
	t := withExpiry(p.ModelSignIn.Token, time.Now())
	values[modelSignInValueKey] = t.AccessToken
	if err := writeModelSignInToken(context.Background(), p, rd, t, false); err != nil {
		return err
	}
	h := &heldToken{what: "the model sign-in", shaped: true, due: renewAt(t, time.Now())}
	if p.RenewModelSignIn != nil {
		h.ask = p.RenewModelSignIn
		h.write = func(ctx context.Context, t ConnectionToken) error { return writeModelSignInToken(ctx, p, rd, t, true) }
	}
	rd.conns = append(rd.conns, h)
	return nil
}

// writeModelSignInToken replaces the token file by a rename, so the
// harness mid-read sees the old token or the new one; on a renewal the
// new file is labelled before it is renamed into place.
func writeModelSignInToken(ctx context.Context, p Params, rd *runDirs, t ConnectionToken, renewal bool) error {
	dir := rd.modelSignInDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Classify(ExitCLI, fmt.Errorf("make the model sign-in's folder: %w", err))
	}
	var label func(string)
	if renewal {
		label = func(tmp string) { relabelShared(ctx, p.Stderr, tmp) }
	}
	if err := writeFileAtomic(filepath.Join(dir, spec.ModelSignInTokenFile), []byte(t.AccessToken), 0o600, label); err != nil {
		return Classify(ExitCLI, fmt.Errorf("write the model sign-in's token file: %w", err))
	}
	if !renewal {
		relabelShared(ctx, p.Stderr, dir)
	}
	return nil
}

// withSignInRenewal says in the receipt that a token-file sign-in's token
// is fixed for the run when the caller gave no renewal: the compiled line
// says the runner rewrites it, which is true only when it can. A copy.
func withSignInRenewal(enforced map[string]string, p Params) map[string]string {
	msi := p.Plan.Harness.ModelSignIn
	if msi == nil || msi.Delivery != spec.ModelSignInToken || p.RenewModelSignIn != nil {
		return enforced
	}
	out := maps.Clone(enforced)
	out["model.signIn"] = strings.Replace(out["model.signIn"], "rewritten by the runner before it expires", "given for the whole run and not renewed (this run's client gave no renewal)", 1)
	return out
}

// planLimitError is the plan-limit outcome (D12, I9): the budget class,
// with its own reason, the vendor's words and the reset time only when the
// harness gave one. Never retried, never a success.
func planLimitError(text string, resets time.Time) error {
	msg := "plan limit reached: the person's plan (or this app's weekly cap on it) refused the model's calls"
	if text != "" {
		msg += "; the vendor said: " + text
	}
	if !resets.IsZero() {
		msg += "; it resets at " + resets.UTC().Format(time.RFC3339)
	}
	return Classify(ExitBudget, receipt.WithReason(errors.New(msg), receipt.ReasonPlanLimit, ""))
}

// credentialKind is the receipt's credential kind for a plan: the model
// sign-in's kind, else the kind a key was compiled for, else api when the
// harness is given a key; empty when it is given none. A word of a closed
// list, never an account or a token (I6).
func credentialKind(plan *spec.Plan) string {
	if msi := plan.Harness.ModelSignIn; msi != nil {
		return msi.Kind
	}
	env := plan.Harness.Agent.Env
	if plan.Harness.Host != nil {
		env = plan.Harness.Host.Env
	}
	kind := ""
	for _, e := range env {
		switch {
		case e.ExpectedKind != nil:
			return e.ExpectedKind.Kind
		case e.FromHostEnv != "":
			kind = policy.KeyKindAPI
		}
	}
	return kind
}
