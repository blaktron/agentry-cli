package managed

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// held is one organization's document holding settings.
func held(org string, settings map[string]managedsettings.Setting) Effective {
	d := docFor("https://reg.example", 1, settings)
	d.Org = org
	return Effective{Documents: []State{{Registry: "https://reg.example", Document: d}}}
}

func lock(t *testing.T, v any) managedsettings.Setting {
	return managedsettings.Setting{Value: raw(t, v), Locked: true}
}

func preset(t *testing.T, v any) managedsettings.Setting {
	return managedsettings.Setting{Value: raw(t, v)}
}

func TestEveryCatalogueRowIsAppliedByTheRunnerOrAClient(t *testing.T) {
	for _, r := range managedsettings.Catalogue() {
		if Applies(r.ID) == slices.Contains(ClientRows, r.ID) {
			t.Errorf("%s: applied by the runner %v, left to a client %v; want exactly one", r.ID, Applies(r.ID), slices.Contains(ClientRows, r.ID))
		}
	}
	for _, id := range enforced {
		if _, ok := managedsettings.Lookup(id); !ok {
			t.Errorf("%s is applied and not in the catalogue", id)
		}
	}
}

func TestAForbiddingLockNamesTheOrganizationAndTheSetting(t *testing.T) {
	for id := range forbiddenWhat {
		e := held("acme", map[string]managedsettings.Setting{id: lock(t, managedsettings.Forbidden)})
		got := e.Forbidden(id)
		if !strings.Contains(got, "acme's managed settings") || !strings.Contains(got, "("+id+")") {
			t.Errorf("Forbidden(%s) = %q", id, got)
		}
		if got := held("acme", map[string]managedsettings.Setting{id: lock(t, managedsettings.Allowed)}).Forbidden(id); got != "" {
			t.Errorf("an allowing lock of %s refused: %q", id, got)
		}
		if got := held("acme", map[string]managedsettings.Setting{id: preset(t, managedsettings.Forbidden)}).Forbidden(id); got != "" {
			t.Errorf("a default of %s refused as a lock: %q", id, got)
		}
	}
}

func TestPolicyHeldRefusesLocksAndFillsDefaultsOnlyWhereTheFileIsSilent(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.EgressWildcard: lock(t, managedsettings.Forbidden),
		managedsettings.Messengers:     preset(t, managedsettings.Forbidden),
		managedsettings.MalwareCheck:   lock(t, managedsettings.MalwareCheckRequired),
	})
	yes, no := true, false
	pol := policy.Default()
	pol.AllowWildcardEgress = true
	pol.MalwareCheck = &no
	h := e.Hold(pol)
	if r := h.Refused(policy.HeldWildcardEgress); !strings.Contains(r, "acme") {
		t.Errorf("wildcard egress allowed in the file is not refused: %q", r)
	}
	if !h.MalwareCheckOn() {
		t.Error("a locked malware check is off because the file turned it off")
	}
	if ok, why := h.MessengerAllowed("telegram"); ok || !strings.Contains(why, "organization default") {
		t.Errorf("a default forbidding messengers left an unset field on: %v %q", ok, why)
	}
	pol.AllowMessengers = &yes
	if ok, why := e.Hold(pol).MessengerAllowed("telegram"); !ok {
		t.Errorf("a default overrode the member's own choice: %q", why)
	}
	if pol.Held().Refused != nil {
		t.Error("holding changed the caller's policy")
	}
}

func TestAMalwareCheckDefaultAppliesOnlyWhereTheFileIsSilent(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{managedsettings.MalwareCheck: preset(t, managedsettings.MalwareCheckRequired)})
	pol := policy.Default()
	if !e.Hold(pol).MalwareCheckOn() {
		t.Error("the default did not turn an unset malware check on")
	}
	off := false
	pol.MalwareCheck = &off
	if e.Hold(pol).MalwareCheckOn() {
		t.Error("the default overrode the member's own choice")
	}
}

func TestALockedClientRowRefusesRunsUntilTheClientHoldsIt(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{managedsettings.BugReports: lock(t, managedsettings.Forbidden)})
	if r := e.RunRefusal(); !strings.Contains(r, "does not apply") {
		t.Errorf("RunRefusal before the client holds bugReports = %q", r)
	}
	enforceClientRows(t)
	if r := e.RunRefusal(); r != "" {
		t.Errorf("RunRefusal once the client holds it = %q", r)
	}
}

func TestWhereAndDestinationsHoldToTheLockedSet(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.RunWhere:                 lock(t, []string{managedsettings.WhereHosted}),
		managedsettings.DeliverablesDestinations: lock(t, []string{managedsettings.DestinationFolder}),
	})
	if r := e.WhereRefusal(managedsettings.WhereLocal); !strings.Contains(r, "acme") || !strings.Contains(r, "run.where allows hosted") {
		t.Errorf("WhereRefusal(local) = %q", r)
	}
	if r := e.WhereRefusal(managedsettings.WhereHosted); r != "" {
		t.Errorf("WhereRefusal(hosted) = %q", r)
	}
	if r := e.DestinationRefusal(receipt.DeliveryKindWebhook); !strings.Contains(r, "deliverables.destinations") {
		t.Errorf("DestinationRefusal(webhook) = %q", r)
	}
	if r := e.DestinationRefusal(receipt.DeliveryKindFolder); r != "" {
		t.Errorf("DestinationRefusal(folder) = %q", r)
	}
	if r := (Effective{}).WhereRefusal(managedsettings.WhereLocal); r != "" {
		t.Errorf("no document refused: %q", r)
	}
}

func TestSourcesAdmitTheOrganizationTheBuiltInsAndTheList(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{managedsettings.InstallSources: lock(t, managedsettings.Sources{
		Scope: managedsettings.SourcesOrganizationBuiltIns, Namespaces: []string{"partner"},
	})})
	for _, ns := range []string{"acme", "agentry", "partner", "Acme"} {
		if r := e.SourceRefusal(ns); r != "" {
			t.Errorf("SourceRefusal(%s) = %q", ns, r)
		}
	}
	if r := e.SourceRefusal("stranger"); !strings.Contains(r, "stranger") || !strings.Contains(r, "install.sources") {
		t.Errorf("SourceRefusal(stranger) = %q", r)
	}
	anyw := held("acme", map[string]managedsettings.Setting{managedsettings.InstallSources: lock(t, managedsettings.Sources{Scope: managedsettings.SourcesAny})})
	if r := anyw.SourceRefusal("stranger"); r != "" {
		t.Errorf("scope any refused: %q", r)
	}
}

// enforceClientRows stands in for a client that holds the rows left to
// clients, until the test ends.
func enforceClientRows(t *testing.T) {
	t.Helper()
	was := slices.Clone(enforced)
	t.Cleanup(func() { enforced = was })
	Enforce(ClientRows...)
}

func TestRefusalJudgesTheRunsReleaseRiskAndAgent(t *testing.T) {
	enforceClientRows(t)
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.InstallSources:    lock(t, managedsettings.Sources{Scope: managedsettings.SourcesOrganizationBuiltIns}),
		managedsettings.InstallAcceptRisk: lock(t, managedsettings.Forbidden),
		managedsettings.AgentsCodingAgent: lock(t, managedsettings.Forbidden),
		managedsettings.RunWhere:          lock(t, []string{managedsettings.WhereLocal}),
	})
	cases := []struct {
		run  Run
		want string
	}{
		{Run{Ref: "acme/triage@1.0.0", Local: true}, ""},
		{Run{Ref: "stranger/x@1.0.0", Local: true}, "install.sources"},
		{Run{Ref: "stranger/x@1.0.0", BuiltIn: true}, ""},
		{Run{Ref: "acme/x@1.0.0", AcceptedRisk: receipt.ConsentAllowUnsigned}, "install.acceptRisk"},
		{Run{Ref: "agentry/coder@1.0.0", BuiltIn: true, Agent: AgentCoder}, "agents.codingAgent"},
		{Run{Ref: "agentry/assistant@1.0.0", BuiltIn: true, Agent: AgentAssistant}, ""},
		{Run{Ref: "acme/x@1.0.0", TargetHosts: true}, "run.where"},
	}
	for _, tc := range cases {
		got := e.Refusal(tc.run)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("Refusal(%+v) = %q, want %q", tc.run, got, tc.want)
		}
	}
}

func TestACapLowersAndNeverRaises(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.RunTimeout:  lock(t, 600),
		managedsettings.RunMaxTurns: lock(t, 50),
		managedsettings.RunMaxCost:  lock(t, 2.5),
	})
	got := e.Budgets(spec.Budgets{TimeoutSeconds: 3600, MaxTurns: 0, MaxCostUSD: 1})
	if want := (spec.Budgets{TimeoutSeconds: 600, MaxTurns: 50, MaxCostUSD: 1}); got != want {
		t.Errorf("Budgets = %+v, want %+v", got, want)
	}
	if got := e.Budgets(spec.Budgets{TimeoutSeconds: 60, MaxTurns: 5, MaxCostUSD: 0}); got != (spec.Budgets{TimeoutSeconds: 60, MaxTurns: 5, MaxCostUSD: 2.5}) {
		t.Errorf("a cap lifted a lower limit, or left one unlimited: %+v", got)
	}
}

func TestADefaultBudgetFillsOnlyAnUnchosenOne(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.RunTimeout:  preset(t, 900),
		managedsettings.RunMaxTurns: preset(t, 40),
	})
	got := e.PolicyBudgets(policy.DefaultBudgets)
	if got.TimeoutSeconds != 900 || got.MaxTurns != 40 {
		t.Errorf("PolicyBudgets(default) = %+v", got)
	}
	got = e.PolicyBudgets(policy.Budgets{TimeoutSeconds: 120, MaxTurns: 10})
	if got.TimeoutSeconds != 120 || got.MaxTurns != 10 {
		t.Errorf("a default overrode the member's own budgets: %+v", got)
	}
}

func TestAnOlderClientIsRefusedAndNamesTheUpdate(t *testing.T) {
	was := clientVersion
	t.Cleanup(func() { clientVersion = was })
	e := held("acme", map[string]managedsettings.Setting{managedsettings.UpdatesMinimumVersion: lock(t, "0.7.41")})
	for v, refused := range map[string]bool{
		"v0.7.40": true, "0.7.41-dev.1": true, "0.0.0-dev": true, "not-a-version": true,
		"v0.7.41": false, "0.7.42": false, "1.0.0": false, "v0.7.41+abc": false,
	} {
		clientVersion = func() string { return v }
		got := e.VersionRefusal()
		if (got != "") != refused {
			t.Errorf("VersionRefusal at %s = %q, want refused %v", v, got, refused)
		}
		if refused && (!strings.Contains(got, "0.7.41 or later") || !strings.Contains(got, "update Agentry")) {
			t.Errorf("VersionRefusal at %s = %q", v, got)
		}
		if got != e.RunRefusal() {
			t.Errorf("RunRefusal at %s does not refuse with the version: %q", v, e.RunRefusal())
		}
	}
}

func TestTheMostRestrictiveOfSeveralDocumentsHolds(t *testing.T) {
	a := held("acme", map[string]managedsettings.Setting{managedsettings.RunTimeout: lock(t, 900)})
	b := held("beta", map[string]managedsettings.Setting{
		managedsettings.RunTimeout:     lock(t, 300),
		managedsettings.EgressWildcard: lock(t, managedsettings.Forbidden),
	})
	e := Effective{Documents: append(a.Documents, b.Documents...)}
	if got := e.Budgets(spec.Budgets{TimeoutSeconds: 1800}); got.TimeoutSeconds != 300 {
		t.Errorf("Budgets = %+v, want the lower cap", got)
	}
	if r := e.Forbidden(managedsettings.EgressWildcard); !strings.Contains(r, "beta") {
		t.Errorf("Forbidden = %q", r)
	}
}

func TestANarrowingCarriesTheDevicesLocksAndCaps(t *testing.T) {
	e := held("acme", map[string]managedsettings.Setting{
		managedsettings.BrowserPublicWeb: lock(t, managedsettings.Forbidden),
		managedsettings.MalwareCheck:     lock(t, managedsettings.MalwareCheckRequired),
		managedsettings.RunMaxTurns:      lock(t, 20),
	})
	n := e.Narrowing()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var got Narrowing
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := got.Held(policy.Held{})
	if r := h.Refused[policy.HeldPublicWeb]; !strings.Contains(r, "acme") || !strings.Contains(r, "the device that sent this run") {
		t.Errorf("refusal = %q", r)
	}
	if h.MalwareCheck != "acme" {
		t.Errorf("malware check = %q", h.MalwareCheck)
	}
	if b := got.Budgets(spec.Budgets{MaxTurns: 100}); b.MaxTurns != 20 {
		t.Errorf("Budgets = %+v", b)
	}

	bad := got
	bad.Refused = map[string]string{"run.unsafeMode": "x"}
	if bad.Validate() == nil {
		t.Error("a refusal of a setting the policy does not hold was accepted")
	}
	bad = got
	bad.Refused = map[string]string{policy.HeldPublicWeb: "two\nlines"}
	if bad.Validate() == nil {
		t.Error("a refusal of two lines was accepted")
	}
	bad = got
	bad.Caps = &spec.Budgets{MaxTurns: -1}
	if bad.Validate() == nil {
		t.Error("a negative cap was accepted")
	}
}

// policyChecks are what a policy allows, each true when it allows more.
func policyChecks(p *policy.Policy) map[string]bool {
	msg, _ := p.MessengerAllowed("telegram")
	return map[string]bool{
		"wildcard":   p.Refused(policy.HeldWildcardEgress) == "" && p.AllowWildcardEgress,
		"publicWeb":  p.Refused(policy.HeldPublicWeb) == "" && p.Browser.AllowPublicWeb,
		"vulnerable": p.Refused(policy.HeldVulnerableServers) == "" && p.MCPServers.AllowVulnerable,
		"desktop":    p.DesktopRefusal() == "",
		"screen":     p.Refused(policy.HeldScreen) == "" && p.Desktop.AllowScreen,
		"custom":     p.CustomServersRefusal() == "" && p.Desktop.CustomServersAllowed(),
		"messengers": msg,
		"persistent": p.PersistentReleasesRefusal() == "",
		"noMalware":  !p.MalwareCheckOn(),
	}
}

// TestNoValueLoosensAPolicy holds invariant 1 on this client: whatever a
// document sets, locked or as a default, every check of a held policy is at
// most as open as the file's own, and every budget at most as high.
func TestNoValueLoosensAPolicy(t *testing.T) {
	yes, no := true, false
	open := policy.Default()
	open.AllowWildcardEgress, open.Browser.AllowPublicWeb, open.MCPServers.AllowVulnerable = true, true, true
	open.Desktop.AllowScreen = true
	open.MalwareCheck = &no
	open.Budgets = policy.Budgets{TimeoutSeconds: 3600, MaxTurns: 0, MaxCostUSD: 0}
	closed := policy.Default()
	closed.AllowMessengers, closed.AllowPersistentReleases, closed.MalwareCheck = &no, &no, &yes
	closed.Desktop.Allow, closed.Desktop.AllowCustomServers = &no, &no
	closed.Budgets = policy.Budgets{TimeoutSeconds: 60, MaxTurns: 5, MaxCostUSD: 1}
	for _, row := range managedsettings.Catalogue() {
		var values []any
		switch row.Kind {
		case managedsettings.KindChoice:
			for _, v := range row.Values {
				values = append(values, v)
			}
		case managedsettings.KindCap:
			values = []any{1, 59, 600, row.Max}
		case managedsettings.KindSet:
			values = []any{row.Values[:1], row.Values}
		case managedsettings.KindSources:
			values = []any{managedsettings.Sources{Scope: managedsettings.SourcesAny}, managedsettings.Sources{Scope: managedsettings.SourcesOrganizationBuiltIns}}
		case managedsettings.KindMinimumVersion:
			values = []any{"0.0.1", "99.0.0"}
		}
		for _, v := range values {
			for _, locked := range []bool{true, false} {
				s := managedsettings.Setting{Value: raw(t, v), Locked: locked}
				if row.Check(s) != nil {
					continue
				}
				e := held("acme", map[string]managedsettings.Setting{row.ID: s})
				for name, pol := range map[string]*policy.Policy{"open": open, "closed": closed} {
					before, after := policyChecks(pol), policyChecks(e.Hold(pol))
					for check, was := range before {
						if after[check] && !was {
							t.Errorf("%s=%s (locked %v) opened %s on the %s policy", row.ID, raw(t, v), locked, check, name)
						}
					}
					b := e.PolicyBudgets(pol.Budgets)
					if loosens(pol.Budgets, b) {
						t.Errorf("%s=%s (locked %v) raised the %s policy's budgets %+v to %+v", row.ID, raw(t, v), locked, name, pol.Budgets, b)
					}
				}
			}
		}
	}
}

// loosens says b allows more than a: a higher limit, or none where a had
// one.
func loosens(a, b policy.Budgets) bool {
	more := func(x, y float64) bool { return (x > 0 && y <= 0) || (x > 0 && y > x) }
	return more(float64(a.TimeoutSeconds), float64(b.TimeoutSeconds)) || more(float64(a.MaxTurns), float64(b.MaxTurns)) || more(a.MaxCostUSD, b.MaxCostUSD)
}
