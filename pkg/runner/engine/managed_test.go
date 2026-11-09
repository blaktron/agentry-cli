package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func holdManaged(t *testing.T, settings map[string]managedsettings.Setting) {
	t.Helper()
	prev := currentManaged
	t.Cleanup(func() { currentManaged = prev })
	doc := managedsettings.Document{
		Schema: managedsettings.Schema, Registry: "https://registry.example", Org: "acme",
		Version: 7, IssuedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Settings: settings,
	}
	currentManaged = func() managed.Effective {
		return managed.Effective{Documents: []managed.State{{Registry: doc.Registry, Document: doc}}}
	}
}

func setting(value string, locked bool) managedsettings.Setting {
	v, _ := json.Marshal(value)
	return managedsettings.Setting{Value: v, Locked: locked}
}

// A plan compiled in Unsafe Mode before the organization forbade it, or by
// a client that reused it, is refused when the run starts, before anything
// touches the daemon; the receipt names the plan's managed settings.
func TestRunRefusesAnUnsafePlanAManagedLockForbids(t *testing.T) {
	p, f := fixture(t)
	p.Plan.ExecImage = &spec.ExecImageSpec{Tag: "agentry-exec:unsafe", Unsafe: &spec.UnsafeExecSpec{}}
	p.Plan.Managed = []spec.ManagedSpec{{Organization: "acme", Version: 6, Applied: []string{managedsettings.UnsafeMode}}}
	holdManaged(t, map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeForbidden, true)})
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy {
		t.Fatalf("want exit %d, got %v", ExitPolicy, err)
	}
	if !strings.Contains(env.Error, "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)") {
		t.Errorf("envelope error %q", env.Error)
	}
	if len(f.Ops()) != 0 {
		t.Errorf("the refused run touched the daemon: %v", f.Ops())
	}
	if len(env.Managed) != 1 || env.Managed[0].Organization != "acme" || env.Managed[0].Version != 6 ||
		len(env.Managed[0].Applied) != 1 || env.Managed[0].Applied[0] != managedsettings.UnsafeMode {
		t.Errorf("env.Managed = %+v", env.Managed)
	}
	raw, _ := json.Marshal(env)
	if strings.Contains(string(raw), `"`+managedsettings.UnsafeModeForbidden+`"`) {
		t.Error("the receipt records a managed setting's value")
	}
}

// Every run and session is held to the managed settings cached when it
// starts: Unsafe Mode plans under a forbidding lock, and every plan under a
// lock this client does not know or a copy it cannot verify.
func TestARunIsHeldToTheManagedSettingsCachedWhenItStarts(t *testing.T) {
	unsafePlan := &spec.Plan{ExecImage: &spec.ExecImageSpec{Unsafe: &spec.UnsafeExecSpec{}}}
	screenPlan := &spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{ScreenAllowedBy: spec.ScreenByUnsafe}}}
	safePlan := &spec.Plan{}
	for name, tc := range map[string]struct {
		settings map[string]managedsettings.Setting
		plan     *spec.Plan
		want     string
	}{
		"forbidden, exec image":   {map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeForbidden, true)}, unsafePlan, "forbidden by acme"},
		"forbidden, screen scope": {map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeForbidden, true)}, screenPlan, "forbidden by acme"},
		"forbidden, safe plan":    {map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeForbidden, true)}, safePlan, ""},
		"forbidden default":       {map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeForbidden, false)}, unsafePlan, ""},
		"allowed":                 {map[string]managedsettings.Setting{managedsettings.UnsafeMode: setting(managedsettings.UnsafeModeAllowed, true)}, unsafePlan, ""},
		"unknown lock, safe plan": {map[string]managedsettings.Setting{"run.somethingNewer": setting("strict", true)}, safePlan, "update Agentry"},
		"unknown default, safe":   {map[string]managedsettings.Setting{"run.somethingNewer": setting("strict", false)}, safePlan, ""},
		"unverified, safe plan":   {nil, safePlan, "cannot be verified"},
	} {
		t.Run(name, func(t *testing.T) {
			holdManaged(t, tc.settings)
			if tc.settings == nil {
				currentManaged = func() managed.Effective {
					return managed.Effective{Problems: []managed.Problem{{Registry: "https://registry.example", Err: errors.New("the document accepted from the registry is no longer on this computer")}}}
				}
			}
			p := Params{Plan: tc.plan}
			err := applyManaged(&p)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("applyManaged = %v, want nil", err)
				}
				return
			}
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("applyManaged = %v, want exit %d naming %q", err, ExitPolicy, tc.want)
			}
		})
	}
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A run's budgets, raised by its flags or unlimited, are held to the locked
// caps when it starts, and its policy to the locked malware check.
func TestApplyManagedHoldsBudgetsAndThePolicy(t *testing.T) {
	holdManaged(t, map[string]managedsettings.Setting{
		managedsettings.RunTimeout:   {Value: raw(t, 600), Locked: true},
		managedsettings.RunMaxCost:   {Value: raw(t, 3), Locked: true},
		managedsettings.MalwareCheck: setting(managedsettings.MalwareCheckRequired, true),
	})
	off := false
	pol := policy.Default()
	pol.MalwareCheck = &off
	p := Params{Plan: &spec.Plan{Ref: "acme/x@1.0.0"}, Policy: pol, Budgets: spec.Budgets{TimeoutSeconds: 7200, MaxTurns: 9}}
	if err := applyManaged(&p); err != nil {
		t.Fatal(err)
	}
	if p.Budgets != (spec.Budgets{TimeoutSeconds: 600, MaxTurns: 9, MaxCostUSD: 3}) {
		t.Errorf("budgets = %+v", p.Budgets)
	}
	if !p.Policy.MalwareCheckOn() || pol.MalwareCheckOn() {
		t.Errorf("the run's policy holds the check %v, the caller's %v", p.Policy.MalwareCheckOn(), pol.MalwareCheckOn())
	}
}

// What a run was admitted with, and where it is asked to deliver, is held
// to the documents when it starts.
func TestApplyManagedRefusesRiskAgentsAndDestinations(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]managedsettings.Setting
		params   Params
		want     string
	}{
		{"an accepted risk", map[string]managedsettings.Setting{managedsettings.InstallAcceptRisk: setting(managedsettings.Forbidden, true)},
			Params{Plan: &spec.Plan{Ref: "acme/x@1.0.0"}, Attestation: receipt.Attestation{Consent: receipt.ConsentAllowUnscanned}}, "install.acceptRisk"},
		{"the Assistant", map[string]managedsettings.Setting{managedsettings.AgentsAssistant: setting(managedsettings.Forbidden, true)},
			Params{Plan: &spec.Plan{Ref: "agentry/assistant@1.21.0", Source: "builtin", Mode: "persistent"}}, "agents.assistant"},
		{"the Coding agent's grant", map[string]managedsettings.Setting{managedsettings.AgentsCodingAgent: setting(managedsettings.Forbidden, true)},
			Params{Plan: &spec.Plan{Ref: "agentry/coder@1.5.0", Capabilities: spec.CapSummary{Coder: &spec.CoderGrant{}}}}, "agents.codingAgent"},
		{"a namespace", map[string]managedsettings.Setting{managedsettings.InstallSources: {Value: raw(t, managedsettings.Sources{Scope: managedsettings.SourcesOrganizationBuiltIns}), Locked: true}},
			Params{Plan: &spec.Plan{Ref: "stranger/x@1.0.0", Source: "registry"}}, "install.sources"},
		{"a folder", map[string]managedsettings.Setting{managedsettings.DeliverablesDestinations: {Value: raw(t, []string{"s3"}), Locked: true}},
			Params{Plan: &spec.Plan{Ref: "acme/x@1.0.0"}, DeliverTo: "/tmp/out"}, "deliverables.destinations"},
		{"a webhook", map[string]managedsettings.Setting{managedsettings.DeliverablesDestinations: {Value: raw(t, []string{"folder"}), Locked: true}},
			Params{Plan: &spec.Plan{Ref: "acme/x@1.0.0"}, Destinations: []destination.Resolved{{Kind: destination.KindWebhook}}}, "deliverables.destinations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holdManaged(t, tc.settings)
			p := tc.params
			err := applyManaged(&p)
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "acme") {
				t.Fatalf("applyManaged = %v, want exit %d naming acme and %s", err, ExitPolicy, tc.want)
			}
		})
	}
}

// A plan compiled before a lock arrived still carries what the lock now
// forbids, and is refused when it starts.
func TestApplyManagedRefusesWhatAnEarlierPlanCarries(t *testing.T) {
	forbid := func(id string) map[string]managedsettings.Setting {
		return map[string]managedsettings.Setting{id: setting(managedsettings.Forbidden, true)}
	}
	cases := []struct {
		name string
		id   string
		plan spec.Plan
	}{
		{"wildcard egress", managedsettings.EgressWildcard, spec.Plan{Capabilities: spec.CapSummary{EgressMode: "all"}}},
		{"a public browser", managedsettings.BrowserPublicWeb, spec.Plan{Capabilities: spec.CapSummary{Browser: &spec.BrowserCap{Public: true}}}},
		{"the person's computer", managedsettings.DesktopApplications, spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{}}}},
		{"the whole screen", managedsettings.DesktopScreen, spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{ComputerUse: spec.ComputerUseScreen}}}},
		{"the person's own applications", managedsettings.DesktopCustomServers, spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{PersonServers: spec.PersonServersAllowed}}}},
		{"a persistent release", managedsettings.PersistentReleases, spec.Plan{Mode: "persistent", Source: "registry"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holdManaged(t, forbid(tc.id))
			plan := tc.plan
			plan.Ref = "acme/x@1.0.0"
			p := Params{Plan: &plan}
			err := applyManaged(&p)
			if err == nil || !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("applyManaged = %v, want a refusal naming %s", err, tc.id)
			}
			holdManaged(t, nil)
			p = Params{Plan: &plan}
			if err := applyManaged(&p); err != nil {
				t.Fatalf("without the lock: %v", err)
			}
		})
	}
}
