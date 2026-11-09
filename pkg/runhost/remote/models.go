package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// A remote runhost's saved models: a subset of a paired computer's, each
// copied there with its credential through the relay its kind takes (a key
// sealed into the runhost's store, a model sign-in made on the runhost from
// the device's browser, a credits token minted for the runhost) and checked
// on the runhost before it is kept. The runhost's persistent agents and its
// schedules name one of them, or follow its default.

// The kinds of a saved model's credential, as a runhost holds it.
const (
	ModelKindKey     = "key"
	ModelKindNoKey   = "no-key"
	ModelKindSignIn  = "sign-in"
	ModelKindCredits = "credits"
)

// SavedModel is one of a runhost's saved models as the runhost says it:
// never a credential.
type SavedModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	Default       bool   `json:"default,omitempty"`
	// Kind is what pays for it: ModelKindKey, ModelKindNoKey,
	// ModelKindSignIn or ModelKindCredits.
	Kind string `json:"kind"`
	// Refusal says why a run on it would be refused on the runhost now:
	// its key, its sign-in or its credits token is not there, or no longer
	// works. Empty when it can be used.
	Refusal string `json:"refusal,omitempty"`
	// Users are the runhost's persistent agents and schedules that name
	// it.
	Users []ModelUser `json:"users,omitempty"`
}

// ModelUser is a persistent agent or a schedule on a runhost that names a
// saved model.
type ModelUser struct {
	// Kind is "agent" or "schedule".
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Models is a runhost's list of saved models.
type Models struct {
	Models []SavedModel `json:"models"`
	// Default is the default's id; empty when the runhost holds none.
	Default string `json:"default"`
	// Max is how many a runhost keeps.
	Max int `json:"max"`
}

// Find is the saved model ref names, by its id or its name (any case).
func (m Models) Find(ref string) (SavedModel, bool) {
	ref = strings.TrimSpace(ref)
	for _, sm := range m.Models {
		if sm.ID == ref {
			return sm, true
		}
	}
	for _, sm := range m.Models {
		if strings.EqualFold(sm.Name, ref) {
			return sm, true
		}
	}
	return SavedModel{}, false
}

// DefaultModel is the runhost's default saved model; false when it holds
// none.
func (m Models) DefaultModel() (SavedModel, bool) {
	for _, sm := range m.Models {
		if sm.ID == m.Default {
			return sm, true
		}
	}
	return SavedModel{}, false
}

// ModelProvider is a saved model's provider as a device sends it: never its
// key's value.
type ModelProvider struct {
	// Name is an endpoint of the person's own's name; empty for a vendor.
	Name       string   `json:"name,omitempty"`
	APIKeyEnv  string   `json:"apiKeyEnv,omitempty"`
	APIKeyKind string   `json:"apiKeyKind,omitempty"`
	BaseURL    string   `json:"baseUrl,omitempty"`
	ProxyAllow []string `json:"proxyAllow"`
}

// PutModel is a saved model a device makes available on a runhost: the
// saved model, its provider, and the provider's key, which lives in flight
// and in the runhost's store alone.
type PutModel struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	ProviderID    string        `json:"providerId"`
	Provider      ModelProvider `json:"provider"`
	Model         string        `json:"model"`
	ContextWindow int64         `json:"contextWindow,omitempty"`
	Key           string        `json:"key,omitempty"`
	// Yes is the person's agreement to the check's one message to a Claude
	// subscription, which counts toward the plan's usage.
	Yes bool `json:"yes,omitempty"`
}

// Checked is what the check on the runhost said of a saved model it kept.
type Checked struct {
	// Resolved is the id a Claude alias resolved to.
	Resolved string `json:"resolved,omitempty"`
	// ContextWindow is the window an endpoint of the person's own reported.
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// Warning is the plain-http sentence for an endpoint reached
	// unencrypted.
	Warning string `json:"warning,omitempty"`
}

// PutAnswer is what a runhost answers a saved model made available on it:
// the saved model as kept, what its check said, and its list now.
type PutAnswer struct {
	Model   SavedModel `json:"model"`
	Checked Checked    `json:"checked"`
	Models  Models     `json:"models"`
}

// RemoveModel asks a runhost to take one of its saved models away. MoveTo
// names the saved model what names it moves to, and the new default when it
// is the default; KeepCredential leaves its provider's credential there
// when no other saved model uses it.
type RemoveModel struct {
	Ref            string `json:"ref"`
	MoveTo         string `json:"moveTo,omitempty"`
	KeepCredential bool   `json:"keepCredential,omitempty"`
}

// Removed is what taking a saved model off a runhost did.
type Removed struct {
	Removed SavedModel  `json:"removed"`
	Moved   []ModelUser `json:"moved"`
	MovedTo string      `json:"movedTo,omitempty"`
	// Credential says what became of its provider's credential on the
	// runhost: "removed", "kept" (another saved model uses it, or
	// KeepCredential), or why it could not be removed.
	Credential string `json:"credential,omitempty"`
	Models     Models `json:"models"`
}

// ListModels asks the runhost for its saved models.
func ListModels(ctx context.Context, c Caller) (Models, error) {
	var out Models
	err := callStore(ctx, c, "models", nil, nil, &out)
	return out, err
}

// PutSavedModel makes a saved model available on the runhost, checked
// there; say takes the runhost's narration. A refusal keeps the runhost's
// exit code.
func PutSavedModel(ctx context.Context, c Caller, doc PutModel, say func(string)) (PutAnswer, error) {
	var out PutAnswer
	err := callStore(ctx, c, "put-saved-model", doc, say, &out)
	return out, err
}

// RemoveSavedModel takes a saved model off the runhost.
func RemoveSavedModel(ctx context.Context, c Caller, req RemoveModel, say func(string)) (Removed, error) {
	var out Removed
	err := callStore(ctx, c, "remove-saved-model", req, say, &out)
	return out, err
}

// SetDefaultModel makes the runhost's saved model ref its default.
func SetDefaultModel(ctx context.Context, c Caller, ref string) (Models, error) {
	var out Models
	err := callStore(ctx, c, "default-model", RemoveModel{Ref: ref}, nil, &out)
	return out, err
}

// callStore asks the store verb's sub with body as its JSON input (none
// for nil), and reads its answer into into. A refusal keeps the runhost's
// exit code as the error's class.
func callStore(ctx context.Context, c Caller, sub string, body any, say func(string), into any) error {
	a, err := c.Call(ctx, "store", []string{sub}, func(w io.Writer) error {
		if body == nil {
			return nil
		}
		return json.NewEncoder(w).Encode(body)
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut && say != nil {
			say(f.Text)
		}
		return nil
	})
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if a.Code != 0 {
		return engine.Classify(a.Code, errors.New(nonEmptyOr(a.Error, "the runhost's store did not answer")))
	}
	return json.Unmarshal(a.Result, into)
}
