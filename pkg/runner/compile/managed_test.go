package compile

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func TestMain(m *testing.M) {
	// No test reads the managed settings cached on the machine running it;
	// the tests below say what is held.
	currentManaged = func() managed.Effective { return managed.Effective{} }
	os.Exit(m.Run())
}

// holding makes the compiler see doc as the one cached document.
func holding(t *testing.T, docs ...managedsettings.Document) {
	t.Helper()
	prev := currentManaged
	t.Cleanup(func() { currentManaged = prev })
	var e managed.Effective
	for _, d := range docs {
		e.Documents = append(e.Documents, managed.State{Registry: d.Registry, Document: d})
	}
	currentManaged = func() managed.Effective { return e }
}

func managedDoc(version int64, settings map[string]managedsettings.Setting) managedsettings.Document {
	return managedsettings.Document{
		Schema: managedsettings.Schema, Registry: "https://registry.example", Org: "acme",
		Version: version, IssuedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Settings: settings,
	}
}

func unsafeMode(value string, locked bool) map[string]managedsettings.Setting {
	v, _ := json.Marshal(value)
	return map[string]managedsettings.Setting{managedsettings.UnsafeMode: {Value: v, Locked: locked}}
}

func compileManaged(t *testing.T, unsafe bool) (*spec.Plan, error) {
	t.Helper()
	m := parseManifest(t, fullCapsYAML)
	in := Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test"}
	if unsafe {
		in.Unsafe = testUnsafeData(t)
	}
	return Compile(in, t.TempDir())
}

func TestUnsafeModeForbiddenByAManagedLockIsRefused(t *testing.T) {
	holding(t, managedDoc(3, unsafeMode(managedsettings.UnsafeModeForbidden, true)))
	_, err := compileManaged(t, true)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Compile = %v, want a policy refusal", err)
	}
	if want := "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)"; refusal.Reason != want {
		t.Errorf("reason %q, want %q", refusal.Reason, want)
	}
	// Without Unsafe Mode the same lock compiles, and the plan names it.
	plan, err := compileManaged(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Managed) != 1 || plan.Managed[0].Organization != "acme" || plan.Managed[0].Version != 3 ||
		len(plan.Managed[0].Applied) != 1 || plan.Managed[0].Applied[0] != managedsettings.UnsafeMode {
		t.Errorf("plan.Managed = %+v", plan.Managed)
	}
	raw, _ := plan.MarshalCanonical()
	if strings.Contains(string(raw), managedsettings.UnsafeModeForbidden) {
		t.Error("the plan records a managed setting's value")
	}
}

func TestUnsafeModeAllowedOrADefaultCompiles(t *testing.T) {
	for name, settings := range map[string]map[string]managedsettings.Setting{
		"allowed":           unsafeMode(managedsettings.UnsafeModeAllowed, true),
		"forbidden default": unsafeMode(managedsettings.UnsafeModeForbidden, false),
	} {
		t.Run(name, func(t *testing.T) {
			holding(t, managedDoc(1, settings))
			if _, err := compileManaged(t, true); err != nil {
				t.Fatalf("Compile: %v", err)
			}
		})
	}
}

func TestAManagedLockTheClientDoesNotKnowRefusesEveryCompile(t *testing.T) {
	v, _ := json.Marshal("strict")
	holding(t, managedDoc(1, map[string]managedsettings.Setting{"run.somethingNewer": {Value: v, Locked: true}}))
	_, err := compileManaged(t, false)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "run.somethingNewer") || !strings.Contains(refusal.Reason, "update Agentry") {
		t.Fatalf("Compile = %v, want a refusal naming the setting", err)
	}
}

func TestThePlanHashTakesTheManagedVersion(t *testing.T) {
	hashAt := func(docs ...managedsettings.Document) string {
		t.Helper()
		holding(t, docs...)
		plan, err := compileManaged(t, false)
		if err != nil {
			t.Fatal(err)
		}
		h, err := plan.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	none := hashAt()
	v3 := hashAt(managedDoc(3, unsafeMode(managedsettings.UnsafeModeForbidden, true)))
	v4 := hashAt(managedDoc(4, unsafeMode(managedsettings.UnsafeModeForbidden, true)))
	if none == v3 || v3 == v4 {
		t.Errorf("plan hashes: none %s, v3 %s, v4 %s; want all different", none, v3, v4)
	}
	if again := hashAt(managedDoc(3, unsafeMode(managedsettings.UnsafeModeForbidden, true))); again != v3 {
		t.Errorf("the same document compiled to another hash: %s, %s", v3, again)
	}
}

// A runhost's run carries the sending device's managed settings: they
// refuse Unsafe Mode there as this computer's own would, and the plan
// records their documents beside this computer's.
func TestTheSendingDevicesManagedSettingsHoldARunhostsRun(t *testing.T) {
	holding(t)
	m := parseManifest(t, fullCapsYAML)
	sender := &managed.Narrowing{
		UnsafeModeRefusal: "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)",
		Documents:         []spec.ManagedSpec{{Organization: "acme", Version: 9, Applied: []string{managedsettings.UnsafeMode}}},
	}
	in := Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: workspaceFor(t, m, nil),
		Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test", Unsafe: testUnsafeData(t), Sender: sender}
	_, err := Compile(in, t.TempDir())
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "forbidden by acme") || !strings.Contains(refusal.Reason, "the device that sent this run") {
		t.Fatalf("Compile = %v, want the sending device's refusal", err)
	}
	in.Unsafe = nil
	plan, err := Compile(in, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Managed) != 1 || plan.Managed[0].Organization != "acme" || plan.Managed[0].Version != 9 {
		t.Errorf("plan.Managed = %+v", plan.Managed)
	}
	if got := CurrentManagedSpecs(sender); len(got) != 1 || got[0].Version != 9 {
		t.Errorf("CurrentManagedSpecs = %+v", got)
	}
	// One that cannot be a narrowing refuses the compile.
	in.Sender = &managed.Narrowing{UnsafeModeRefusal: "two\nlines"}
	if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
		t.Fatalf("Compile with a malformed narrowing = %v, want a refusal", err)
	}
}

func TestManagedSettingsThatCannotBeVerifiedRefuseEveryCompile(t *testing.T) {
	prev := currentManaged
	t.Cleanup(func() { currentManaged = prev })
	currentManaged = func() managed.Effective {
		return managed.Effective{Problems: []managed.Problem{{Registry: "https://registry.example", Err: errors.New("no signing key pinned verifies the document")}}}
	}
	_, err := compileManaged(t, false)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "cannot be verified") {
		t.Fatalf("Compile = %v, want a refusal", err)
	}
}
