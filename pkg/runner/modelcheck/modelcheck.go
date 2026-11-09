// Package modelcheck checks a saved model before it is saved: that its
// provider can be reached with the credential the provider names, and that
// the model id is one the provider offers. Each kind is asked the way its
// harness or its vendor answers:
//
//   - a key or a connected account: the provider's models listing must list
//     the id (setup.ListModels, which follows no redirect, takes http or
//     https alone, bounds the time and the size, and scrubs the key from
//     every error);
//   - an endpoint of the person's own: the same listing, and its context
//     window as the endpoint reports it;
//   - a ChatGPT plan: the plan's own model list;
//   - a Copilot plan: Copilot CLI's own model list, with auto, which it never
//     lists, always taken;
//   - a Claude subscription: one short message through Claude Code, since it
//     has no list, which counts toward the plan's usage (ClaudeNotice);
//   - Agentry credits: a credits session opens, since there is one model.
//
// A check that cannot ask refuses the save with the reason. Plain http off
// this computer is said and never refused.
package modelcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// The kinds of refusal.
const (
	// NotOffered: the provider answered, and does not offer the model id.
	NotOffered = "not-offered"
	// CannotCheck: the provider could not be asked, or its answer read; the
	// save may be tried again.
	CannotCheck = "cannot-check"
	// NeedsSignIn: the provider's sign-in is not kept, or no longer works.
	NeedsSignIn = "needs-sign-in"
	// KeyMissing: the provider's key is neither in its variable nor in the
	// keychain.
	KeyMissing = "key-missing"
	// NotEligible: the account's plan does not include this use.
	NotEligible = "not-eligible"
)

// Refusal is a save the check refused: Kind says why, Reason in words.
// Offered is what the provider does offer, where it said.
type Refusal struct {
	Kind    string
	Reason  string
	Offered []string
}

func (r *Refusal) Error() string { return r.Reason }

// ClaudeNotice is said before a Claude subscription's model is checked.
const ClaudeNotice = "To check this model, Agentry sends it one short message through Claude Code. The message counts toward your Claude plan's usage."

// CopilotAuto is the Copilot model that lets GitHub choose: never listed,
// and the only one Copilot Free has.
const CopilotAuto = "auto"

// Result is a check that passed.
type Result struct {
	// Resolved is the model id a Claude alias resolved to at the check: it
	// moves with Claude Code's version, so it is shown beside the alias and
	// never saved in its place.
	Resolved string
	// ContextWindow is the window an endpoint of the person's own reports
	// for the model, with Source where it said so; 0 when it said nothing.
	ContextWindow int64
	Source        string
	// Warning is the plain-http sentence for an endpoint the key reaches
	// unencrypted.
	Warning string
}

// ClaudeAnswer is Claude Code's answer to the check's one message.
type ClaudeAnswer struct {
	// OK: the model answered.
	OK bool
	// Resolved is the model id the answer was made with.
	Resolved string
	// Status is the API's error status, 0 for none.
	Status int
	// NotSignedIn: Claude Code holds no sign-in.
	NotSignedIn bool
	// Message is Claude Code's own words for an error.
	Message string
}

// Checkers are how each kind is asked: the CLI wires the real ones, a test
// fakes them. A nil checker refuses its kind as one that cannot be checked.
type Checkers struct {
	// Key is the key m names, from its variable or the keychain entry of its
	// provider; "" when neither holds one.
	Key func(m policy.Model) (string, error)
	// ListModels is setup.ListModelsToCheck; nil uses it.
	ListModels func(ctx context.Context, provider, baseURL, key string) (*setup.Catalog, error)
	// ProbeContextWindow is setup.ProbeContextWindow; nil asks nothing.
	ProbeContextWindow func(ctx context.Context, baseURL, model, key string) (setup.ContextWindow, error)
	// ChatGPTModels is the kept ChatGPT sign-in's plan models.
	ChatGPTModels func(ctx context.Context) ([]string, error)
	// CopilotModels is Copilot CLI's model list on plan.
	CopilotModels func(ctx context.Context, plan string) ([]string, error)
	// Claude sends Claude Code the check's one message on model.
	Claude func(ctx context.Context, model string) (ClaudeAnswer, error)
	// Credits opens a credits session for m and ends it.
	Credits func(ctx context.Context, m policy.Model) error
}

// Check checks the model m reads as (policy.Policy.ModelView).
func Check(ctx context.Context, c Checkers, m policy.Model) (Result, error) {
	switch {
	case m.Provider == policy.ProviderAgentry:
		return checkCredits(ctx, c, m)
	case m.APIKeyKind == policy.KeyKindChatGPTPlan:
		return checkPlan(ctx, "ChatGPT", m.Name, func() ([]string, error) {
			if c.ChatGPTModels == nil {
				return nil, errNoChecker
			}
			return c.ChatGPTModels(ctx)
		})
	case m.APIKeyKind == policy.KeyKindCopilotSignIn:
		if m.Name == CopilotAuto {
			return Result{}, nil
		}
		return checkPlan(ctx, "Copilot", m.Name, func() ([]string, error) {
			if c.CopilotModels == nil {
				return nil, errNoChecker
			}
			plan := policy.CopilotPlanOf(m.ProxyAllow)
			if plan == "" {
				plan = policy.CopilotPlanIndividual
			}
			return c.CopilotModels(ctx, plan)
		})
	case m.APIKeyKind == policy.KeyKindClaudeSignIn:
		return checkClaude(ctx, c, m.Name)
	}
	return checkListed(ctx, c, m)
}

var errNoChecker = errors.New("this build has no way to ask it for its models")

func cannot(what string, err error) *Refusal {
	return &Refusal{Kind: CannotCheck, Reason: fmt.Sprintf("could not check the model: %s: %v", what, err)}
}

// checkListed asks the provider's models listing with its key.
func checkListed(ctx context.Context, c Checkers, m policy.Model) (Result, error) {
	key := ""
	if c.Key != nil {
		k, err := c.Key(m)
		if err != nil {
			return Result{}, cannot("reading the key", err)
		}
		key = strings.TrimSpace(k)
	}
	if key == "" && !m.OwnEndpoint() {
		where := "$" + m.APIKeyEnv + " is not set"
		if m.APIKeySource == "keyring" {
			where += " and the OS keychain has no entry provider:" + m.KeyProvider()
		}
		return Result{}, &Refusal{Kind: KeyMissing, Reason: fmt.Sprintf("could not check the model: no key for %s (%s)", m.KeyProvider(), where)}
	}
	list := c.ListModels
	if list == nil {
		list = setup.ListModelsToCheck
	}
	provider, base := m.Provider, ""
	if m.OwnEndpoint() {
		provider, base = policy.ProviderCustom, m.BaseURL
	}
	cat, err := list(ctx, provider, base, key)
	if err != nil {
		return Result{}, &Refusal{Kind: CannotCheck, Reason: "could not check the model: " + err.Error()}
	}
	offered := make([]string, 0, len(cat.Models))
	for _, lm := range cat.Models {
		offered = append(offered, lm.ID)
	}
	if !offers(m.Provider, offered, m.Name) {
		return Result{}, notOffered(m.KeyProvider(), m.Name, offered)
	}
	res := Result{Warning: cat.Warning}
	if m.OwnEndpoint() && c.ProbeContextWindow != nil {
		pctx, cancel := context.WithTimeout(ctx, setup.ContextProbeBudget)
		w, err := c.ProbeContextWindow(pctx, m.BaseURL, m.Name, key)
		cancel()
		if err == nil && w.Tokens > 0 && policy.CheckContextWindow(w.Tokens) == nil {
			res.ContextWindow, res.Source = w.Tokens, w.Source
		}
	}
	return res, nil
}

// offers reports whether a listing names model. A typed catalogue's id may
// carry a routing suffix after its last ':' (OpenRouter's :nitro, Hugging
// Face's :<inference provider>), which its listing does not repeat.
func offers(provider string, offered []string, model string) bool {
	if slices.Contains(offered, model) {
		return true
	}
	if provider != policy.ProviderOpenRouter && provider != policy.ProviderHuggingFace {
		return false
	}
	base, _, ok := strings.Cut(model, ":")
	return ok && slices.Contains(offered, base)
}

// notOffered is the refusal of an id the provider's list does not name.
func notOffered(provider, model string, offered []string) *Refusal {
	shown := offered
	const most = 20
	more := ""
	if len(shown) > most {
		shown, more = shown[:most], fmt.Sprintf(" and %d more", len(offered)-most)
	}
	return &Refusal{Kind: NotOffered, Offered: offered,
		Reason: fmt.Sprintf("%s does not offer %q: it lists %s%s", provider, model, strings.Join(shown, ", "), more)}
}

// checkPlan asks a sign-in's plan for its models.
func checkPlan(_ context.Context, plan, model string, list func() ([]string, error)) (Result, error) {
	models, err := list()
	var ne *mcpauth.ChatGPTNotEligibleError
	switch {
	case errors.As(err, &ne):
		return Result{}, &Refusal{Kind: NotEligible, Reason: "could not check the model: " + err.Error()}
	case errors.Is(err, modelsignin.ErrNoSignIn), errors.Is(err, modelsignin.ErrNone):
		return Result{}, &Refusal{Kind: NeedsSignIn, Reason: fmt.Sprintf("could not check the model: no %s sign-in is kept on this computer (agentry providers add)", plan)}
	case err != nil:
		return Result{}, cannot("asking the "+plan+" plan for its models", err)
	}
	if !slices.Contains(models, model) {
		r := &Refusal{Kind: NotOffered, Offered: models, Reason: setup.NotInPlanError(plan, model, models).Error()}
		return Result{}, r
	}
	return Result{}, nil
}

// checkClaude sends Claude Code the one message.
func checkClaude(ctx context.Context, c Checkers, model string) (Result, error) {
	if c.Claude == nil {
		return Result{}, cannot("sending Claude Code a message", errNoChecker)
	}
	a, err := c.Claude(ctx, model)
	switch {
	case err != nil:
		return Result{}, cannot("sending Claude Code a message", err)
	case a.OK:
		return Result{Resolved: a.Resolved}, nil
	case a.NotSignedIn:
		return Result{}, &Refusal{Kind: NeedsSignIn, Reason: "could not check the model: Claude Code is not signed in (agentry providers add anthropic --sign-in)"}
	case a.Status == 404:
		return Result{}, &Refusal{Kind: NotOffered, Reason: a.Message}
	}
	return Result{}, &Refusal{Kind: CannotCheck, Reason: "could not check the model: Claude Code answered: " + a.Message}
}

// checkCredits opens a credits session: there is one model.
func checkCredits(ctx context.Context, c Checkers, m policy.Model) (Result, error) {
	if m.Name != setup.AgentryModelName {
		return Result{}, &Refusal{Kind: NotOffered, Offered: []string{setup.AgentryModelName},
			Reason: fmt.Sprintf("runs paid with Agentry credits use the one model the registry's staff chose, saved as %q, not %q", setup.AgentryModelName, m.Name)}
	}
	if c.Credits == nil {
		return Result{}, cannot("opening a credits session", errNoChecker)
	}
	if err := c.Credits(ctx, m); err != nil {
		return Result{}, cannot("opening a credits session", err)
	}
	return Result{}, nil
}

// ClaudeArgs are the check's arguments to Claude Code: a run's own flags,
// one turn, no tools, no settings and no session kept, and a one-line
// system prompt that keeps the message short.
func ClaudeArgs(model string) []string {
	return []string{
		"-p", "--output-format", "json", "--setting-sources", "", "--tools", "",
		"--strict-mcp-config", "--no-session-persistence", "--max-turns", "1",
		"--system-prompt", "Reply with the single word OK.",
		"--model", model, "OK?",
	}
}

// ParseClaudeResult reads Claude Code's JSON result of the check: is_error
// and api_error_status say what happened (subtype says success even on an
// error, and the stderr's model warning is not an answer).
func ParseClaudeResult(stdout []byte) (ClaudeAnswer, error) {
	var r struct {
		IsError        bool                       `json:"is_error"`
		Result         string                     `json:"result"`
		APIErrorStatus int                        `json:"api_error_status"`
		ModelUsage     map[string]json.RawMessage `json:"modelUsage"`
	}
	text := strings.TrimSpace(string(stdout))
	// The result is the last line Claude Code prints.
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		text = text[i+1:]
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return ClaudeAnswer{}, fmt.Errorf("the answer is not Claude Code's JSON result: %w", err)
	}
	if !r.IsError {
		used := make([]string, 0, len(r.ModelUsage))
		for id := range r.ModelUsage {
			used = append(used, id)
		}
		sort.Strings(used)
		a := ClaudeAnswer{OK: true}
		if len(used) > 0 {
			a.Resolved = used[0]
		}
		return a, nil
	}
	msg := strings.TrimSpace(r.Result)
	return ClaudeAnswer{
		Status:      r.APIErrorStatus,
		NotSignedIn: r.APIErrorStatus == 0 && strings.Contains(strings.ToLower(msg), "not logged in"),
		Message:     msg,
	}, nil
}
