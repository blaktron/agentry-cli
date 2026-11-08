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

func TestCheckManaged(t *testing.T) {
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
	} {
		t.Run(name, func(t *testing.T) {
			holdManaged(t, tc.settings)
			err := checkManaged(Params{Plan: tc.plan})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkManaged = %v, want nil", err)
				}
				return
			}
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkManaged = %v, want exit %d naming %q", err, ExitPolicy, tc.want)
			}
		})
	}
}
