package managed

// What each catalogue row holds a member to, and the sentence that says so.
// Every check here reads every verified document this computer holds: a
// document only narrows, so the most restrictive answer of any of them is
// the answer.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// forbidden says, for each allowed-or-forbidden row, what a lock to
// forbidden refuses, in the words of a refusal.
var forbiddenWhat = map[string]string{
	managedsettings.UnsafeMode:           "Unsafe Mode",
	managedsettings.InstallAcceptRisk:    "accepting a release's risk (--allow-unsigned, --allow-unscanned)",
	managedsettings.EgressWildcard:       `wildcard egress (network.egress: "*")`,
	managedsettings.BrowserPublicWeb:     "a browser that reaches any public site",
	managedsettings.MCPAllowVulnerable:   "running a declared MCP server the registry rates high or critical",
	managedsettings.DesktopApplications:  "using applications on your computer through Agentry Desktop",
	managedsettings.DesktopScreen:        "computer use of your whole screen",
	managedsettings.DesktopCustomServers: "using an application you set up yourself in Agentry Desktop",
	managedsettings.Messengers:           "reaching a persistent agent on a messenger",
	managedsettings.PersistentReleases:   "running a persistent QuickAgent that is not built into Agentry",
	managedsettings.AgentsCodingAgent:    "the Coding agent",
	managedsettings.AgentsAssistant:      "the Assistant",
	managedsettings.Schedules:            "scheduling runs",
	managedsettings.BugReports:           "sending bug reports",
}

// defaultFix is, for each row whose organization default fills a field of
// the runner policy, how the member turns it back on.
var defaultFix = map[string]string{
	managedsettings.DesktopApplications:  "set desktop.allow: true in the runner policy to use them",
	managedsettings.DesktopCustomServers: "set desktop.allowCustomServers: true in the runner policy to use them",
	managedsettings.Messengers:           "set allowMessengers: true in the runner policy to use them",
	managedsettings.PersistentReleases:   "set allowPersistentReleases: true in the runner policy to run them",
}

// policyRows are the rows that narrow a field of the runner policy, by the
// id the policy's checks know them by (policy.Held).
var policyRows = []string{
	managedsettings.EgressWildcard,
	managedsettings.BrowserPublicWeb,
	managedsettings.MCPAllowVulnerable,
	managedsettings.DesktopApplications,
	managedsettings.DesktopScreen,
	managedsettings.DesktopCustomServers,
	managedsettings.Messengers,
	managedsettings.PersistentReleases,
}

// sentenceCase starts s with a capital letter.
func sentenceCase(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// forbiddenSentence is the refusal of what a lock forbids.
func forbiddenSentence(id, org string) string {
	return fmt.Sprintf("%s is forbidden by %s's managed settings (%s)", sentenceCase(forbiddenWhat[id]), org, id)
}

// lock is the first document's locked setting of id, and its organization.
func (e Effective) lock(id string, accept func(managedsettings.Setting) bool) (managedsettings.Setting, string, bool) {
	for _, d := range e.Documents {
		if s, ok := d.Document.Settings[id]; ok && s.Locked && accept(s) {
			return s, d.Document.Org, true
		}
	}
	return managedsettings.Setting{}, "", false
}

// Forbidden is the refusal of what a document locks off by an
// allowed-or-forbidden row, or "" when none does.
func (e Effective) Forbidden(id string) string {
	if _, org, ok := e.lock(id, func(s managedsettings.Setting) bool { return s.String() == managedsettings.Forbidden }); ok {
		return forbiddenSentence(id, org)
	}
	return ""
}

// PolicyHeld is what the documents hold the runner policy to: a refusal
// for each row a document locks to forbidden, an organization default
// that turns a field off where the file leaves it unset, and the malware
// check a document requires.
func (e Effective) PolicyHeld() policy.Held {
	h := policy.Held{}
	for _, id := range policyRows {
		if r := e.Forbidden(id); r != "" {
			if h.Refused == nil {
				h.Refused = map[string]string{}
			}
			h.Refused[id] = r
			continue
		}
		fix, ok := defaultFix[id]
		if !ok {
			continue
		}
		for _, d := range e.Documents {
			if s, ok := d.Document.Settings[id]; ok && !s.Locked && s.String() == managedsettings.Forbidden {
				if h.Defaulted == nil {
					h.Defaulted = map[string]string{}
				}
				h.Defaulted[id] = fmt.Sprintf("%s is off by %s's organization default (%s); %s", sentenceCase(forbiddenWhat[id]), d.Document.Org, id, fix)
				break
			}
		}
	}
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[managedsettings.MalwareCheck]
		if !ok || s.String() != managedsettings.MalwareCheckRequired {
			continue
		}
		if s.Locked && h.MalwareCheck == "" {
			h.MalwareCheck = d.Document.Org
		}
		if !s.Locked {
			h.MalwareCheckDefault = true
		}
	}
	return h
}

// Hold is p held to e's documents (PolicyHeld). The result is a view for
// checks and runs, never saved.
func (e Effective) Hold(p *policy.Policy) *policy.Policy { return p.WithHeld(e.PolicyHeld()) }

// Hold is p held to the managed settings documents cached on this
// computer.
func Hold(p *policy.Policy) *policy.Policy { return Current().Hold(p) }

// set is a locked set row's members.
func set(s managedsettings.Setting) []string {
	var out []string
	_ = json.Unmarshal(s.Value, &out)
	return out
}

// notAmong is the refusal of a member a locked set row leaves out, or "".
func (e Effective) notAmong(id, member, what string) string {
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[id]
		if !ok || !s.Locked {
			continue
		}
		if members := set(s); !slices.Contains(members, member) {
			return fmt.Sprintf("%s is not allowed by %s's managed settings (%s allows %s)", sentenceCase(what), d.Document.Org, id, strings.Join(members, ", "))
		}
	}
	return ""
}

// What a run.where member is, in the words of a refusal.
var whereWhat = map[string]string{
	managedsettings.WhereLocal:      "running on this computer",
	managedsettings.WhereHosted:     "running In Agentry",
	managedsettings.WhereRunhost:    "running on a runhost",
	managedsettings.WhereTargetHost: "reaching a target host",
}

// WhereRefusal is the refusal of a run going where (one of the run.where
// members), or "" when every document lets it.
func (e Effective) WhereRefusal(where string) string {
	return e.notAmong(managedsettings.RunWhere, where, whereWhat[where])
}

// DestinationRefusal is the refusal of a run delivering to a destination of
// the kind (folder, webhook, s3), or "" when every document lets it.
func (e Effective) DestinationRefusal(kind string) string {
	return e.notAmong(managedsettings.DeliverablesDestinations, kind, "delivering to a "+kind+" destination")
}

// BuiltInNamespace is the namespace of the QuickAgents built into Agentry,
// which an install source list always admits.
const BuiltInNamespace = "agentry"

// SourceRefusal is the refusal of installing or running a release of the
// namespace, or "" when every document lets it. A document whose source
// list is the organization's and the built-ins' admits its own
// organization's namespace, the built-ins' and the namespaces it lists.
func (e Effective) SourceRefusal(namespace string) string {
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[managedsettings.InstallSources]
		if !ok || !s.Locked {
			continue
		}
		var v managedsettings.Sources
		if json.Unmarshal(s.Value, &v) != nil || v.Scope == managedsettings.SourcesAny {
			continue
		}
		ns := strings.ToLower(namespace)
		if ns == BuiltInNamespace || ns == strings.ToLower(d.Document.Org) || slices.Contains(v.Namespaces, ns) {
			continue
		}
		allowed := append([]string{d.Document.Org, BuiltInNamespace}, v.Namespaces...)
		return fmt.Sprintf("QuickAgents from %s are not allowed by %s's managed settings (%s allows %s)", namespace, d.Document.Org, managedsettings.InstallSources, strings.Join(allowed, ", "))
	}
	return ""
}

// AcceptRiskRefusal is the refusal of admitting a release by a member's
// acceptance of its risk (the consent, --allow-unsigned or
// --allow-unscanned), or "" when every document lets it or no risk was
// accepted.
func (e Effective) AcceptRiskRefusal(consent string) string {
	if consent == "" {
		return ""
	}
	if r := e.Forbidden(managedsettings.InstallAcceptRisk); r != "" {
		return r + "; " + consent + " is refused"
	}
	return ""
}

// capOf is the lowest locked cap of id across the documents, and its
// organization.
func (e Effective) capOf(id string) (float64, string, bool) {
	best, org, found := 0.0, "", false
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[id]
		if !ok || !s.Locked {
			continue
		}
		var n float64
		if json.Unmarshal(s.Value, &n) != nil || n <= 0 {
			continue
		}
		if !found || n < best {
			best, org, found = n, d.Document.Org, true
		}
	}
	return best, org, found
}

// defaultOf is the first document's organization default of a cap row.
func (e Effective) defaultOf(id string) (float64, bool) {
	for _, d := range e.Documents {
		s, ok := d.Document.Settings[id]
		if !ok || s.Locked {
			continue
		}
		var n float64
		if json.Unmarshal(s.Value, &n) == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// Budgets are b held to the documents' locked caps: a budget above a cap,
// or unlimited (0), becomes the cap, and one below it stays. A cap never
// lifts a member's own lower limit.
func (e Effective) Budgets(b spec.Budgets) spec.Budgets {
	if n, _, ok := e.capOf(managedsettings.RunTimeout); ok {
		c := int(n)
		if b.TimeoutSeconds <= 0 || b.TimeoutSeconds > c {
			b.TimeoutSeconds = c
		}
	}
	if n, _, ok := e.capOf(managedsettings.RunMaxTurns); ok {
		c := int(n)
		if b.MaxTurns <= 0 || b.MaxTurns > c {
			b.MaxTurns = c
		}
	}
	if n, _, ok := e.capOf(managedsettings.RunMaxCost); ok {
		if b.MaxCostUSD <= 0 || b.MaxCostUSD > n {
			b.MaxCostUSD = n
		}
	}
	return b
}

// PolicyBudgets are a runner policy's budgets with the documents'
// organization defaults filled in where the file sets none of its own (the
// fresh policy's value), then held to their locked caps.
func (e Effective) PolicyBudgets(b policy.Budgets) policy.Budgets {
	if n, ok := e.defaultOf(managedsettings.RunTimeout); ok && b.TimeoutSeconds == policy.DefaultBudgets.TimeoutSeconds {
		b.TimeoutSeconds = int(n)
	}
	if n, ok := e.defaultOf(managedsettings.RunMaxTurns); ok && b.MaxTurns == policy.DefaultBudgets.MaxTurns {
		b.MaxTurns = int(n)
	}
	if n, ok := e.defaultOf(managedsettings.RunMaxCost); ok && b.MaxCostUSD == policy.DefaultBudgets.MaxCostUSD {
		b.MaxCostUSD = n
	}
	held := e.Budgets(spec.Budgets{TimeoutSeconds: b.TimeoutSeconds, MaxTurns: b.MaxTurns, MaxCostUSD: b.MaxCostUSD})
	return policy.Budgets{TimeoutSeconds: held.TimeoutSeconds, MaxTurns: held.MaxTurns, MaxCostUSD: held.MaxCostUSD}
}
