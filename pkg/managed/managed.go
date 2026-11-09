// Package managed is a client's copy of the managed settings an
// organization's admins set for its members: the registry's signed
// document (github.com/blaktron/agentry/pkg/managedsettings), fetched with
// the account's own token, verified against the keys pinned for that
// registry at sign-in, held to that registry and to the organization the
// account is an active member of, and kept in the config directory so
// that it still applies offline.
//
// Every load verifies the cached copy again: the cache is the signed
// envelope as the registry served it, never a decoded copy, so an edit to
// it is refused. A record beside it says what the registry last answered,
// so a document that was accepted and is now missing is noticed rather
// than read as "none apply". A document only ever narrows what a member
// could choose, so the engine applies every verified document it holds,
// whichever registry a run comes from.
package managed

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Route is the registry's answer to the signed-in caller.
const Route = "/api/v1/user/managed-settings"

// FetchInterval is how old the last answer may be before a run asks the
// registry again.
const FetchInterval = time.Hour

// enforced are the settings the runner applies: the list is this
// package's own, not the catalogue's. A registry may sign rows that a newer
// catalogue holds and this client cannot yet hold a member to; a locked one
// refuses every run, exactly as a row the catalogue does not know does, and
// a default one is shown and passed over. Rows that a client holds through
// paths of its own are added by that client (Enforce).
var enforced = []string{
	managedsettings.UnsafeMode,
	managedsettings.InstallSources,
	managedsettings.InstallAcceptRisk,
	managedsettings.RunTimeout,
	managedsettings.RunMaxTurns,
	managedsettings.RunMaxCost,
	managedsettings.EgressWildcard,
	managedsettings.BrowserPublicWeb,
	managedsettings.MCPAllowVulnerable,
	managedsettings.DesktopApplications,
	managedsettings.DesktopScreen,
	managedsettings.DesktopCustomServers,
	managedsettings.Messengers,
	managedsettings.PersistentReleases,
	managedsettings.AgentsCodingAgent,
	managedsettings.AgentsAssistant,
	managedsettings.Schedules,
	managedsettings.DeliverablesDestinations,
	managedsettings.MalwareCheck,
	managedsettings.UpdatesMinimumVersion,
}

// ClientRows are the rows the runner cannot apply alone, because a client
// has paths of its own that they govern: where runs may go (the runner
// holds runs on this computer, on a runhost and to target hosts; a client
// that starts runs In Agentry holds those) and bug reports, which a client
// sends. A client adds them with Enforce once every path it has obeys them.
var ClientRows = []string{managedsettings.RunWhere, managedsettings.BugReports}

// Enforce adds rows this client applies through its own paths beside the
// runner's. It is called once, as the client starts, before any run.
func Enforce(ids ...string) {
	for _, id := range ids {
		if !slices.Contains(enforced, id) {
			enforced = append(enforced, id)
		}
	}
}

// Applies says this client applies the setting.
func Applies(id string) bool { return slices.Contains(enforced, id) }

// State is one registry's verified managed settings.
type State struct {
	// Registry is the registry's normalised address.
	Registry string
	// Document is the verified document, bound to Registry.
	Document managedsettings.Document
	// FetchedAt is when this client last read it from the registry.
	FetchedAt time.Time
}

// Locked is the value of a setting the organization locked, and whether
// it is locked.
func (s *State) Locked(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	v, ok := s.Document.Settings[id]
	if !ok || !v.Locked {
		return "", false
	}
	return v.String(), true
}

// Applied are the ids of the document's settings this client applies,
// sorted.
func (s *State) Applied() []string {
	var out []string
	for id := range s.Document.Settings {
		if Applies(id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// lockedNotApplied is the first locked setting, by id, that the
// catalogue knows and this client does not apply, or "".
func (s *State) lockedNotApplied() string {
	ids := make([]string, 0, len(s.Document.Settings))
	for id, v := range s.Document.Settings {
		if _, known := managedsettings.Lookup(id); known && v.Locked && !Applies(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)
	return ids[0]
}

// Summary is how one line names the document: "managed by acme (version
// 7)".
func (s *State) Summary() string {
	return fmt.Sprintf("managed by %s (version %d)", s.Document.Org, s.Document.Version)
}

// Problem is managed settings this client holds and cannot apply: a
// cached copy that does not verify, one that is older than a version
// already applied, or one that was accepted and is no longer there.
type Problem struct {
	// Registry is the registry's normalised address, or the file's name
	// when the file names none.
	Registry string
	Err      error
}

// Effective is every cached document together, as the engine applies
// them.
type Effective struct {
	// Documents are the verified documents, by registry.
	Documents []State
	// Problems are what could not be verified. While there is one, no run
	// starts: what it held is unknown, and it may lock anything.
	Problems []Problem
}

// Registry is the part of e that is registryURL's.
func (e Effective) Registry(registryURL string) Effective {
	reg := normalize(registryURL)
	var out Effective
	for _, d := range e.Documents {
		if d.Registry == reg {
			out.Documents = append(out.Documents, d)
		}
	}
	for _, p := range e.Problems {
		if p.Registry == reg {
			out.Problems = append(out.Problems, p)
		}
	}
	return out
}

// RunRefusal is why no run may start at all, or "": a document locks a
// setting this client does not know, so it cannot hold the member to it,
// or managed settings this client holds cannot be verified.
func (e Effective) RunRefusal() string {
	for _, d := range e.Documents {
		for _, u := range d.Document.Unknown() {
			if u.Locked {
				return fmt.Sprintf("%s's managed settings lock %s, which this version of Agentry does not know; update Agentry to run", d.Document.Org, u.ID)
			}
		}
		if id := d.lockedNotApplied(); id != "" {
			return fmt.Sprintf("%s's managed settings lock %s, which this version of Agentry does not apply; update Agentry to run", d.Document.Org, id)
		}
	}
	if len(e.Problems) > 0 {
		return problemRefusal("No run starts", e.Problems[0])
	}
	return e.VersionRefusal()
}

// Run is what the managed settings judge of a run before it starts.
type Run struct {
	// Ref is the release's ns/name@version.
	Ref string
	// BuiltIn says the release is built into Agentry.
	BuiltIn bool
	// Local says the run starts on this computer, rather than arriving
	// from another device that judged where it goes.
	Local bool
	// TargetHosts says the run reaches target hosts.
	TargetHosts bool
	// AcceptedRisk is the member's consent that admitted a release with no
	// attestation or no scan (--allow-unsigned, --allow-unscanned), or "".
	AcceptedRisk string
	// Agent is the persistent built-in the run is, by name ("assistant",
	// "coder"), or "".
	Agent string
}

// Persistent built-ins a document may forbid, by name.
const (
	AgentAssistant = "assistant"
	AgentCoder     = "coder"
)

// AgentOf is the persistent built-in a release is, by its reference
// (ns/name or ns/name@version), or "": only Agentry's own built-ins, run
// as persistent agents, are one.
func AgentOf(ref string, builtIn, persistent bool) string {
	if !builtIn || !persistent {
		return ""
	}
	ns, rest, ok := strings.Cut(ref, "/")
	if !ok || ns != BuiltInNamespace {
		return ""
	}
	name, _, _ := strings.Cut(rest, "@")
	switch name {
	case AgentAssistant, AgentCoder:
		return name
	}
	return ""
}

// Refusal is why the run may not start, or "": everything RunRefusal
// refuses, then where it goes, where its release comes from, a risk the
// member accepted for it, and the persistent built-in it is.
func (e Effective) Refusal(r Run) string {
	if reason := e.RunRefusal(); reason != "" {
		return reason
	}
	if r.Local {
		if reason := e.WhereRefusal(managedsettings.WhereLocal); reason != "" {
			return reason
		}
	}
	if r.TargetHosts {
		if reason := e.WhereRefusal(managedsettings.WhereTargetHost); reason != "" {
			return reason
		}
	}
	if !r.BuiltIn {
		if ns, _, ok := strings.Cut(r.Ref, "/"); ok {
			if reason := e.SourceRefusal(ns); reason != "" {
				return reason
			}
		}
	}
	if reason := e.AcceptRiskRefusal(r.AcceptedRisk); reason != "" {
		return reason
	}
	switch r.Agent {
	case AgentAssistant:
		return e.Forbidden(managedsettings.AgentsAssistant)
	case AgentCoder:
		return e.Forbidden(managedsettings.AgentsCodingAgent)
	}
	return ""
}

// UnsafeModeRefusal is why Unsafe Mode may not be turned on, or "" when it
// may: a document that locks run.unsafeMode to forbidden, or managed
// settings this client holds that cannot be verified, which might.
func (e Effective) UnsafeModeRefusal() string {
	for _, d := range e.Documents {
		if v, ok := d.Locked(managedsettings.UnsafeMode); ok && v == managedsettings.UnsafeModeForbidden {
			return fmt.Sprintf("Unsafe Mode is forbidden by %s's managed settings (%s)", d.Document.Org, managedsettings.UnsafeMode)
		}
	}
	if len(e.Problems) > 0 {
		return problemRefusal("Unsafe Mode is refused", e.Problems[0])
	}
	return ""
}

func problemRefusal(what string, p Problem) string {
	where := p.Registry
	if where == "" {
		where = "this computer"
	}
	return fmt.Sprintf("%s while the managed settings held for %s cannot be verified (%v); `agentry managed refresh` reads them again", what, where, p.Err)
}

// Specs are the documents as a plan and a receipt record them: the
// organization, the version and the ids of the settings this client
// applies, never a value.
func (e Effective) Specs() []spec.ManagedSpec {
	var out []spec.ManagedSpec
	for i := range e.Documents {
		d := &e.Documents[i]
		out = append(out, spec.ManagedSpec{Organization: d.Document.Org, Version: d.Document.Version, Applied: d.Applied()})
	}
	return out
}

// Narrowing is what a device's managed settings say of a run it sends to
// another machine (a runhost), which has its own sign-in and its own
// documents. It can only refuse, so the machine that runs it applies it as
// sent: a forged one could do no more than stop the run.
type Narrowing struct {
	// UnsafeModeRefusal is the device's refusal of Unsafe Mode, empty when
	// the device allows it.
	UnsafeModeRefusal string `json:"unsafeModeRefusal,omitempty"`
	// Documents are the device's documents as the run's plan and receipt
	// record them.
	Documents []spec.ManagedSpec `json:"documents,omitempty"`
	// Refused are the device's locks on the runner policy's checks, by
	// the id the policy knows them by (policy.Held), each with the
	// sentence that refuses it.
	Refused map[string]string `json:"refused,omitempty"`
	// MalwareCheck is the organization whose lock requires the malware
	// check on the device, or "".
	MalwareCheck string `json:"malwareCheck,omitempty"`
	// Caps are the device's locked budget caps; a zero field caps nothing.
	Caps *spec.Budgets `json:"caps,omitempty"`
}

// Narrowing is what e says of a run sent to another machine; nil when it
// says nothing.
func (e Effective) Narrowing() *Narrowing {
	held := e.PolicyHeld()
	n := &Narrowing{UnsafeModeRefusal: e.UnsafeModeRefusal(), Documents: e.Specs(), Refused: held.Refused, MalwareCheck: held.MalwareCheck}
	if caps := e.Budgets(spec.Budgets{}); caps != (spec.Budgets{}) {
		n.Caps = &caps
	}
	if n.UnsafeModeRefusal == "" && len(n.Documents) == 0 && len(n.Refused) == 0 && n.MalwareCheck == "" && n.Caps == nil {
		return nil
	}
	return n
}

// Held is held, the receiving machine's own holds, with n's locks added:
// a refusal of either holds.
func (n *Narrowing) Held(held policy.Held) policy.Held {
	if n == nil {
		return held
	}
	sent := policy.Held{MalwareCheck: n.MalwareCheck}
	for id, r := range n.Refused {
		if sent.Refused == nil {
			sent.Refused = map[string]string{}
		}
		sent.Refused[id] = r + ", on the device that sent this run"
	}
	return held.Merge(sent)
}

// Budgets are b held to n's caps as well.
func (n *Narrowing) Budgets(b spec.Budgets) spec.Budgets {
	if n == nil || n.Caps == nil {
		return b
	}
	c := *n.Caps
	if c.TimeoutSeconds > 0 && (b.TimeoutSeconds <= 0 || b.TimeoutSeconds > c.TimeoutSeconds) {
		b.TimeoutSeconds = c.TimeoutSeconds
	}
	if c.MaxTurns > 0 && (b.MaxTurns <= 0 || b.MaxTurns > c.MaxTurns) {
		b.MaxTurns = c.MaxTurns
	}
	if c.MaxCostUSD > 0 && (b.MaxCostUSD <= 0 || b.MaxCostUSD > c.MaxCostUSD) {
		b.MaxCostUSD = c.MaxCostUSD
	}
	return b
}

// Limits of a Narrowing a machine accepts.
const (
	maxNarrowingDocuments = 16
	maxRefusalBytes       = 1024
)

var (
	orgHandleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	settingIDRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(\.[A-Za-z][A-Za-z0-9]*)*$`)
)

// oneLine says s is at most one short line of text.
func oneLine(s string) bool {
	return len(s) <= maxRefusalBytes && !strings.ContainsFunc(s, unicode.IsControl)
}

// Validate holds a Narrowing that arrived from another machine to what one
// can carry: a one-line refusal, organizations' handles, setting ids.
func (n *Narrowing) Validate() error {
	if n == nil {
		return nil
	}
	if !oneLine(n.UnsafeModeRefusal) {
		return errors.New("the managed settings' refusal is not one line of text")
	}
	if len(n.Refused) > len(policyRows) {
		return errors.New("the managed settings refuse more than the policy has to refuse")
	}
	for id, r := range n.Refused {
		if !slices.Contains(policyRows, id) || r == "" || !oneLine(r) {
			return fmt.Errorf("the managed settings refuse %q in a way that cannot be one", id)
		}
	}
	if n.MalwareCheck != "" && !orgHandleRE.MatchString(n.MalwareCheck) {
		return errors.New("the managed settings name an organization that cannot be one for the malware check")
	}
	if c := n.Caps; c != nil && (c.TimeoutSeconds < 0 || c.MaxTurns < 0 || c.MaxCostUSD < 0 || math.IsNaN(c.MaxCostUSD) || math.IsInf(c.MaxCostUSD, 0)) {
		return errors.New("the managed settings' caps cannot be negative")
	}
	if len(n.Documents) > maxNarrowingDocuments {
		return fmt.Errorf("the managed settings name %d documents, more than %d", len(n.Documents), maxNarrowingDocuments)
	}
	for _, d := range n.Documents {
		if !orgHandleRE.MatchString(d.Organization) || d.Version < 1 {
			return fmt.Errorf("the managed settings name a document %q version %d that cannot be one", d.Organization, d.Version)
		}
		if len(d.Applied) > 64 {
			return errors.New("the managed settings name too many settings")
		}
		for _, id := range d.Applied {
			if !settingIDRE.MatchString(id) || len(id) > 128 {
				return fmt.Errorf("the managed settings name a setting %q that cannot be one", id)
			}
		}
	}
	return nil
}
