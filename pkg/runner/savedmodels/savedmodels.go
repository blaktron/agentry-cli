// Package savedmodels holds the rules a client applies to the saved models
// and providers on a computer beyond what the policy file itself holds: what
// on the computer names a saved model, how a saved model or a provider is
// removed while something names it, whether a provider's credential is
// here, and the provider an endpoint of the person's own makes. The CLI's
// models and providers commands and Agentry Desktop's Settings both call it,
// so the two apply the same rules.
//
// A removal never moves anything to the default on its own: while a
// schedule or a persistent agent names a model being removed, the removal is
// refused with them listed, unless the person names the saved model they
// move to. Removing the default needs the new default, named the same way.
package savedmodels

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// The kinds of thing that name a saved model.
const (
	KindSchedule = "schedule"
	KindAgent    = "agent"
)

// User is something on this computer that names a saved model: a schedule
// (by its id) or a persistent agent (by its name).
type User struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Model is the id of the saved model it names.
	Model string `json:"model"`
}

// named is what Users found, with the records a move rewrites.
type named struct {
	users     []User
	schedules []schedule.Schedule
	agents    []persistent.Agent
}

// Users are the schedules on this computer that name one of the saved models
// sms, by id or by name, or that run on the harness whose block in pol one of
// them was read from and name no saved model, which run on it; then the
// persistent agents that name one of them.
func Users(home string, pol *policy.Policy, sms []policy.SavedModel) ([]User, error) {
	n, err := find(home, pol, sms)
	return n.users, err
}

func find(home string, pol *policy.Policy, sms []policy.SavedModel) (named, error) {
	var n named
	list, _, err := schedule.StoreIn(home).List()
	if err != nil {
		return n, err
	}
	match := func(ref string) (policy.SavedModel, bool) {
		for _, sm := range sms {
			if sm.ID == ref || strings.EqualFold(sm.Name, ref) {
				return sm, true
			}
		}
		return policy.SavedModel{}, false
	}
	for _, sc := range list {
		ref := strings.TrimSpace(sc.Run.Model)
		if ref == "" {
			if sm, ok := pol.HarnessModel(sc.Run.Adapter); ok {
				ref = sm.ID
			}
		}
		if ref == "" {
			continue
		}
		if sm, ok := match(ref); ok {
			n.users = append(n.users, User{Kind: KindSchedule, ID: sc.ID, Name: sc.Name, Model: sm.ID})
			n.schedules = append(n.schedules, sc)
		}
	}
	for _, a := range persistent.AgentsIn(home) {
		if a.DesktopOnly {
			continue
		}
		cfg, err := persistent.LoadConfig(home, a.Name)
		if err != nil || cfg.Model == "" {
			continue
		}
		if sm, ok := match(cfg.Model); ok && sm.ID == cfg.Model {
			n.users = append(n.users, User{Kind: KindAgent, ID: a.Name, Name: a.DisplayName(cfg), Model: sm.ID})
			n.agents = append(n.agents, a)
		}
	}
	return n, nil
}

// InUseError is a removal refused because something names a saved model it
// would remove, and no saved model to move them to was named.
type InUseError struct{ Users []User }

func (e *InUseError) Error() string {
	lines := make([]string, 0, len(e.Users))
	for _, u := range e.Users {
		lines = append(lines, fmt.Sprintf("%s %s (%s)", u.Kind, u.Name, u.ID))
	}
	return "named by " + strings.Join(lines, ", ") + ": move them to another saved model first"
}

// Removal is what a removal did.
type Removal struct {
	// Removed are the saved models removed.
	Removed []policy.SavedModel
	// Moved are the schedules and agents moved, to MovedTo.
	Moved   []User
	MovedTo policy.SavedModel
}

// Target is the saved model ref names as the one to move to, which must not
// be among the ones removed.
func Target(pol *policy.Policy, ref string, removed []policy.SavedModel) (policy.SavedModel, error) {
	to, ok := pol.FindModel(ref)
	if !ok {
		return to, fmt.Errorf("no saved model %q on this computer", ref)
	}
	if slices.ContainsFunc(removed, func(sm policy.SavedModel) bool { return sm.ID == to.ID }) {
		return to, fmt.Errorf("%q is removed too", to.Name)
	}
	return to, nil
}

// Remove removes the saved models sms from pol and writes it to path, with
// remove doing the policy's part (policy.RemoveModel or RemoveProvider),
// given the new default. What names them moves to the saved model moveTo,
// which is also the new default when the default is among them; with no
// moveTo, a removal of anything something names is refused with an
// InUseError, and nothing changes. The schedules and agents move before the
// policy is written, so none ever names a model that is gone; a running
// agent takes the move at its next turn. client is who asks
// (persistent.ClientCLI, persistent.ClientDesktop).
//
// The errors carry the CLI's exit classes: usage for a move target or a
// removal the policy refuses, the CLI's for an agent that could not be
// moved, the policy's for the write. An InUseError is the caller's to word.
func Remove(ctx context.Context, home string, pol *policy.Policy, path string, sms []policy.SavedModel, moveTo, client string, remove func(newDefault string) error) (Removal, error) {
	n, err := find(home, pol, sms)
	if err != nil {
		return Removal{}, err
	}
	var to policy.SavedModel
	if strings.TrimSpace(moveTo) != "" {
		if to, err = Target(pol, moveTo, sms); err != nil {
			return Removal{}, engine.Classify(engine.ExitUsage, fmt.Errorf("--move-to: %w", err))
		}
	} else if len(n.users) > 0 {
		return Removal{}, &InUseError{Users: n.users}
	}
	if err := remove(to.ID); err != nil {
		return Removal{}, engine.Classify(engine.ExitUsage, err)
	}
	st := schedule.StoreIn(home)
	for _, sc := range n.schedules {
		sc.Run.Model = to.ID
		if err := st.Save(sc); err != nil {
			return Removal{}, fmt.Errorf("move schedule %s to %s: %w", sc.Name, to.Name, err)
		}
	}
	for _, a := range n.agents {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		id := to.ID
		_, _, err := persistent.Configure(cctx, home, a, client, persistent.Change{Model: &id})
		cancel()
		if err != nil {
			return Removal{}, engine.Classify(engine.ExitCLI, fmt.Errorf("move the %s agent to %s: %w", a.Name, to.Name, err))
		}
	}
	if err := policy.Save(path, pol); err != nil {
		return Removal{}, engine.Classify(engine.ExitPolicy, err)
	}
	return Removal{Removed: slices.Clone(sms), Moved: n.users, MovedTo: to}, nil
}

// The states a provider's credential is in.
const (
	// Ready: the credential is here.
	Ready = "ready"
	// KeyMissing: the key is neither in its variable nor in the keychain.
	KeyMissing = "key-missing"
	// NeedsSignIn: the sign-in is not kept, no longer works, or (Agentry
	// credits) this computer is not signed in to the registry.
	NeedsSignIn = "needs-sign-in"
)

// State is whether a provider's credential is here, never the credential:
// Kind says what it is paid with, Words the state as the CLI lists it, and
// Code the state a client acts on (Ready, KeyMissing, NeedsSignIn). Account
// is a sign-in's account or Agentry credits' registry, where known; Broken
// is why a kept sign-in no longer works.
type State struct {
	Kind    string
	Words   string
	Code    string
	Account string
	Broken  string
	// Vendor is the model sign-in store's vendor the credential is kept
	// under, "" for a key or credits.
	Vendor string
}

// SignInVendor is the model sign-in store's vendor a provider's credential
// is kept under: a model sign-in's vendor, or a connectable vendor's own id;
// "" for a key or credits.
func SignInVendor(id string, pr policy.Provider) string {
	switch pr.APIKeyKind {
	case policy.KeyKindChatGPTPlan:
		return modelsignin.ChatGPT
	case policy.KeyKindClaudeSignIn:
		return modelsignin.Claude
	case policy.KeyKindCopilotSignIn:
		return modelsignin.Copilot
	}
	if modelsignin.IsConnectVendor(id) {
		return id
	}
	return ""
}

// ProviderState says whether the provider id's credential is here. token is
// this computer's sign-in token for a registry ("" when signed out), which
// Agentry credits are paid through.
func ProviderState(home, id string, pr policy.Provider, token func(registry string) (string, error)) State {
	vendor := policy.ProviderVendor(id)
	if vendor == policy.ProviderAgentry {
		reg, _ := policy.AgentryRegistry(policy.Model{BaseURL: pr.BaseURL})
		s := State{Kind: "Agentry credits", Account: reg}
		if tok, err := token(reg); err != nil || tok == "" {
			s.Words, s.Code = "needs sign-in (agentry login)", NeedsSignIn
			return s
		}
		s.Words, s.Code = "signed in at "+reg, Ready
		return s
	}
	if sv := SignInVendor(id, pr); sv != "" && pr.APIKeyKind != policy.KeyKindAPI && pr.APIKeyKind != "" {
		s := State{Kind: policy.SignInSentence(pr.APIKeyKind), Code: NeedsSignIn, Vendor: sv}
		r, err := modelsignin.Open(home, nil).Get(sv)
		switch {
		case err != nil:
			s.Words = "needs sign-in"
		case r.Broken != "":
			s.Words, s.Broken = "needs sign-in: "+r.Broken, r.Broken
		case r.Label != "":
			s.Words, s.Code, s.Account = "signed in as "+r.Label, Ready, r.Label
		default:
			s.Words, s.Code = "signed in", Ready
		}
		return s
	}
	if pr.APIKeyEnv == "" && vendor == policy.ProviderCustom {
		return State{Kind: "takes no key", Words: "ready", Code: Ready}
	}
	s := State{Kind: "key from $" + pr.APIKeyEnv}
	if pr.APIKeySource == modelkey.SourceKeyring {
		s.Kind += ", else the OS keychain entry " + modelkey.Account(id)
	}
	if r, err := modelsignin.Open(home, nil).Get(id); err == nil && modelsignin.IsConnectVendor(id) && r.Connected {
		s.Kind, s.Vendor, s.Account = "connected account", id, r.Label
	}
	if os.Getenv(pr.APIKeyEnv) != "" {
		s.Words, s.Code = "key set", Ready
		return s
	}
	if pr.APIKeySource == modelkey.SourceKeyring {
		if ok, err := modelkey.Present(id); err == nil && ok {
			s.Words, s.Code = "key in the keychain", Ready
			return s
		}
	}
	s.Words, s.Code = "key missing", KeyMissing
	return s
}

// CustomKeyEnv is the variable an endpoint of the person's own, provider id
// custom-<slug>, reads its key from before the keychain.
func CustomKeyEnv(id string) string {
	return strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_API_KEY"
}

// CustomProvider is the provider an endpoint of the person's own called name
// at baseURL makes, under its id custom-<slug>. keychain says its key is
// kept in the OS keychain, read after the variable CustomKeyEnv names;
// without it the endpoint takes no key, and a run on it is given none.
// warning is the plain-http sentence for an address the key and the prompts
// reach unencrypted.
func CustomProvider(name, baseURL string, keychain bool) (id string, pr policy.Provider, warning string, err error) {
	name, baseURL = strings.TrimSpace(name), strings.TrimSpace(baseURL)
	if name == "" || baseURL == "" {
		return "", pr, "", errors.New("an endpoint of your own needs a name and its address")
	}
	if id, err = policy.CustomProviderID(name); err != nil {
		return "", pr, "", err
	}
	hp, err := setup.EndpointHostPort(baseURL)
	if err != nil {
		return "", pr, "", err
	}
	pr = policy.Provider{Name: name, BaseURL: baseURL, ProxyAllow: []string{hp}}
	if keychain {
		pr.APIKeyEnv, pr.APIKeySource = CustomKeyEnv(id), modelkey.SourceKeyring
	}
	return id, pr, setup.PlainHTTPWarning(baseURL), nil
}
