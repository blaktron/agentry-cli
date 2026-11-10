package managed

// The models-and-keys rows as a run's model meets them: which providers a
// run may use (model.providers), what pays (model.source), the one model a
// lock allows (model.default), which subscriptions and plans of the
// member's own a run may spend (model.signIns), whether an endpoint of the
// member's own may be used and over plain http (model.customEndpoints),
// and which coding tools' harnesses may run (codingTools.allowed). Each is
// held where every run settles its model (localrun.ChooseRunModel), so a
// run from the CLI, the desktop, a schedule or a runhost is held alike.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// RunModel is a run's model as the rows judge it.
type RunModel struct {
	// Provider is the saved model's provider: a vendor's name, or
	// custom-<slug> for an endpoint of the member's own.
	Provider string
	// Model is the model id the provider is asked for.
	Model string
	// KeyKind is the credential's kind (policy.Model.APIKeyKind).
	KeyKind string
	// BaseURL is the endpoint of the member's own, if any.
	BaseURL string
	// Adapter is the harness the run uses.
	Adapter string
}

// Vendor is the catalogue's name for the run's provider.
func (m RunModel) Vendor() string { return vendorOf(m.Provider) }

// vendorOf is the catalogue's name for a policy's provider.
func vendorOf(provider string) string {
	if provider == policy.ProviderCustom || strings.HasPrefix(provider, policy.ProviderCustom+"-") {
		return managedsettings.ProviderCustom
	}
	return provider
}

// codingTools are the harnesses that are a coding tool's, by the
// catalogue's names.
var codingTools = map[string]string{
	"claude":                         managedsettings.CodingClaudeCode,
	"claude-image":                   managedsettings.CodingClaudeCode,
	"codex":                          managedsettings.CodingCodex,
	"codex-image":                    managedsettings.CodingCodex,
	harnessbuild.CopilotImageAdapter: managedsettings.CodingCopilot,
}

// signInKinds are the credential kinds that spend a subscription or plan of
// the member's own, by the catalogue's names.
var signInKinds = map[string]string{
	"subscription":              managedsettings.SignInSubscription,
	policy.KeyKindClaudeSignIn:  managedsettings.SignInClaude,
	policy.KeyKindChatGPTPlan:   managedsettings.SignInChatGPT,
	policy.KeyKindCopilotSignIn: managedsettings.SignInCopilot,
}

// sourceOf is a source row's source.
func sourceOf(s managedsettings.Setting) string {
	var v managedsettings.Source
	_ = json.Unmarshal(s.Value, &v)
	return v.Source
}

// modelOf is a model row's provider and model.
func modelOf(s managedsettings.Setting) managedsettings.Model {
	var m managedsettings.Model
	_ = json.Unmarshal(s.Value, &m)
	return m
}

// lockedAny is the first document's lock of id, whatever its value.
func (e Effective) lockedAny(id string) (managedsettings.Setting, string, bool) {
	return e.lock(id, func(managedsettings.Setting) bool { return true })
}

// ModelRefusal is the refusal of a run on m, in the organization's name and
// the setting's, or "" when the documents allow it.
func (e Effective) ModelRefusal(m RunModel) string {
	vendor := vendorOf(m.Provider)
	if s, org, ok := e.lockedAny(managedsettings.ModelProviders); ok {
		if allowed := set(s); !slices.Contains(allowed, vendor) {
			return fmt.Sprintf("Running on %s is not allowed by %s's managed settings (model.providers allows %s)", vendor, org, strings.Join(allowed, ", "))
		}
	}
	source := ""
	if s, org, ok := e.lockedAny(managedsettings.ModelSource); ok {
		source = sourceOf(s)
		switch {
		case (source == managedsettings.SourceOrganizationKey || source == managedsettings.SourceAgentryCredits) && vendor != policy.ProviderAgentry:
			what := "its own key"
			if source == managedsettings.SourceAgentryCredits {
				what = "its credits"
			}
			return fmt.Sprintf("%s's managed settings pay for models with %s, so runs use the Agentry provider, not %s (model.source); choose it with agentry setup", org, what, vendor)
		case source == managedsettings.SourceMember && vendor == policy.ProviderAgentry:
			return fmt.Sprintf("%s's managed settings have runs pay for models with your own keys and plans, not Agentry credits (model.source)", org)
		}
	}
	if s, org, ok := e.lockedAny(managedsettings.ModelDefault); ok {
		d := modelOf(s)
		// On the organization's key the registry runs the model the
		// settings name, whatever the run asks for.
		onOrgKey := source == managedsettings.SourceOrganizationKey && vendor == policy.ProviderAgentry
		if !onOrgKey && (vendor != d.Provider || m.Model != d.Model) {
			return fmt.Sprintf("%s's managed settings hold runs to %s's %s (model.default); this run is on %s's %s", org, d.Provider, d.Model, vendor, m.Model)
		}
	}
	if kind, spends := signInKinds[m.KeyKind]; spends {
		if s, org, ok := e.lockedAny(managedsettings.ModelSignIns); ok {
			if allowed := set(s); !slices.Contains(allowed, kind) {
				what := "none"
				if len(allowed) > 0 {
					what = strings.Join(allowed, ", ")
				}
				return fmt.Sprintf("Spending your %s is not allowed by %s's managed settings (model.signIns allows %s)", kind, org, what)
			}
		}
	}
	if vendor == managedsettings.ProviderCustom {
		if s, org, ok := e.lockedAny(managedsettings.ModelCustomEndpoints); ok {
			switch s.String() {
			case managedsettings.Forbidden:
				return fmt.Sprintf("A model endpoint of your own is forbidden by %s's managed settings (model.customEndpoints)", org)
			case managedsettings.EndpointsHTTPSOnly:
				if u, err := url.Parse(m.BaseURL); err != nil || u.Scheme != "https" {
					return fmt.Sprintf("A model endpoint of your own over plain http is forbidden by %s's managed settings (model.customEndpoints allows https only)", org)
				}
			}
		}
	}
	if tool, ok := codingTools[m.Adapter]; ok {
		if s, org, ok := e.lockedAny(managedsettings.CodingToolsAllowed); ok {
			if allowed := set(s); !slices.Contains(allowed, tool) {
				return fmt.Sprintf("The %s harness is not allowed by %s's managed settings (codingTools.allowed allows %s)", tool, org, strings.Join(allowed, ", "))
			}
		}
		if f := e.CodingToolModel(tool); f.Locked && m.Model != f.Value {
			return fmt.Sprintf("%s's managed settings hold the %s harness to the model %s (codingTools.models); this run is on %s", f.Org, tool, f.Value, m.Model)
		}
	}
	return ""
}

// ModelRefusal is the refusal of a run on m by the managed settings cached
// on this computer, or "".
func ModelRefusal(m RunModel) string { return Current().ModelRefusal(m) }

// SetupModel is what the documents pre-fill, or lock, in setup's model
// step: the provider (and model) a member's runs use, and the sentence that
// says why. A zero SetupModel leaves the step as it is.
type SetupModel struct {
	Provider string
	Model    string
	// Locked says the member cannot choose otherwise; the step is skipped.
	Locked bool
	// Line says what was set and by whom.
	Line string
}

// SetupModel reads model.source and model.default, locked or default: an
// organization that pays with its own key or its credits sets the Agentry
// provider; otherwise model.default names the provider and the model.
func (e Effective) SetupModel() SetupModel {
	for _, d := range e.Documents {
		if s, ok := d.Document.Settings[managedsettings.ModelSource]; ok {
			switch src := sourceOf(s); src {
			case managedsettings.SourceOrganizationKey, managedsettings.SourceAgentryCredits:
				what := "its own model key"
				if src == managedsettings.SourceAgentryCredits {
					what = "its credits"
				}
				return SetupModel{Provider: policy.ProviderAgentry, Locked: s.Locked,
					Line: fmt.Sprintf("model: %s pays with %s, through the Agentry provider (model.source, %s)", d.Document.Org, what, strength(s))}
			}
		}
	}
	for _, d := range e.Documents {
		if s, ok := d.Document.Settings[managedsettings.ModelDefault]; ok {
			m := modelOf(s)
			return SetupModel{Provider: m.Provider, Model: m.Model, Locked: s.Locked,
				Line: fmt.Sprintf("model: %s's %s, set by %s (model.default, %s)", m.Provider, m.Model, d.Document.Org, strength(s))}
		}
	}
	return SetupModel{}
}

// strength names a setting's strength.
func strength(s managedsettings.Setting) string {
	if s.Locked {
		return "locked"
	}
	return "organization default"
}
