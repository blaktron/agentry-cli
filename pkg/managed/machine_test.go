package managed

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// withMachine makes the machine settings raw, or none for nil, for the
// test, and forgets the process's pin.
func withMachine(t *testing.T, raw string) {
	t.Helper()
	prev := readMachineSource
	readMachineSource = func() ([]byte, string, bool, error) {
		if raw == "" {
			return nil, "/etc/agentry/managed.json", false, nil
		}
		return []byte(raw), "/etc/agentry/managed.json", true, nil
	}
	forgetPin()
	t.Cleanup(func() { readMachineSource = prev; forgetPin() })
}

// The machine settings take known fields alone, an https registry, an
// organization's handle with the registry it is on, and settings the
// catalogue accepts, naming no organization's key or credits.
func TestMachineSettingsAreChecked(t *testing.T) {
	ok := `{"registry": "https://registry.example/", "requireOrg": "acme", "settings": {"run.unsafeMode": {"value": "forbidden", "locked": true}}}`
	m, err := parseMachine([]byte(ok))
	if err != nil || m.Registry != "https://registry.example" || m.RequireOrg != "acme" || len(m.Settings) != 1 {
		t.Fatalf("a good file: %+v %v", m, err)
	}
	if _, err := parseMachine([]byte(`{"registry": "http://localhost:8080"}`)); err != nil {
		t.Fatalf("this computer's own registry over http: %v", err)
	}
	for name, raw := range map[string]string{
		"an unknown field":            `{"registry": "https://registry.example", "pin": true}`,
		"plain http":                  `{"registry": "http://registry.example"}`,
		"a registry with a password":  `{"registry": "https://u:p@registry.example"}`,
		"requireOrg with no registry": `{"requireOrg": "acme"}`,
		"a bad handle":                `{"registry": "https://registry.example", "requireOrg": "Acme Corp"}`,
		"a value the catalogue lacks": `{"settings": {"run.unsafeMode": {"value": "on", "locked": true}}}`,
		"an organization's credits":   `{"settings": {"review.source": {"value": {"source": "agentry-credits"}, "locked": true}}}`,
	} {
		if _, err := parseMachine([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := parseMachine([]byte(`{"settings": {"review.source": {"value": {"source": "off"}, "locked": true}}}`)); err != nil {
		t.Errorf("review off: %v", err)
	}
}

// The machine settings come first among the documents a run is held to,
// with no registry, so they hold whichever registry the run uses, and
// their lock wins over an organization's value.
func TestMachineSettingsComeFirstAndHoldEveryRegistry(t *testing.T) {
	t.Setenv(configdir.EnvVar, t.TempDir())
	withMachine(t, `{"settings": {"run.unsafeMode": {"value": "forbidden", "locked": true}}}`)
	e := Current()
	if e.Machine == nil || len(e.Documents) != 1 || e.Documents[0].Document.Org != MachineOrg || e.Documents[0].Registry != "" {
		t.Fatalf("documents %+v", e.Documents)
	}
	org := State{Registry: "https://registry.example", Document: docFor("https://registry.example", 3, map[string]managedsettings.Setting{
		managedsettings.UnsafeMode: {Value: str(managedsettings.UnsafeModeAllowed), Locked: true}})}
	e.Documents = append(e.Documents, org)
	scoped := e.Registry("https://registry.example")
	if r := scoped.UnsafeModeRefusal(); !strings.Contains(r, "forbidden by this computer's administrator's managed settings") {
		t.Fatalf("the machine's lock: %q", r)
	}
	if len(scoped.Documents) != 2 || scoped.Machine == nil {
		t.Fatalf("scoped to a registry: %+v", scoped)
	}
}

// requireOrg refuses every run until a verified document of that
// organization from the pinned registry is held.
func TestRequireOrgRefusesRunsUntilTheMemberIsSignedIn(t *testing.T) {
	t.Setenv(configdir.EnvVar, t.TempDir())
	withMachine(t, `{"registry": "https://registry.example", "requireOrg": "acme"}`)
	e := Current()
	if r := e.RunRefusal(); !strings.Contains(r, "requires you to be signed in to https://registry.example as an active member of acme") {
		t.Fatalf("signed out: %q", r)
	}
	e.Documents = append(e.Documents, State{Registry: "https://elsewhere.example", Document: docFor("https://elsewhere.example", 1, forbidden())})
	if e.RunRefusal() == "" {
		t.Fatal("another registry's document met the requirement")
	}
	e.Documents = append(e.Documents, State{Registry: "https://registry.example", Document: docFor("https://registry.example", 1, forbidden())})
	if r := e.RunRefusal(); r != "" {
		t.Fatalf("signed in as acme's member: %q", r)
	}
}

// Machine settings that cannot be used refuse every run, and pin nothing.
func TestUnusableMachineSettingsRefuseEveryRun(t *testing.T) {
	t.Setenv(configdir.EnvVar, t.TempDir())
	withMachine(t, `{"registry": "http://registry.example"}`)
	if r := Current().RunRefusal(); !strings.Contains(r, "/etc/agentry/managed.json") || !strings.Contains(r, "ask this computer's administrator") {
		t.Fatalf("refusal %q", r)
	}
	if pin, err := MachineRegistry(); pin != "" || err == nil {
		t.Fatalf("unusable settings: pin %q, err %v", pin, err)
	}
}

// The pin is the machine settings' registry, read once.
func TestMachineRegistryIsThePin(t *testing.T) {
	withMachine(t, `{"registry": "https://registry.example/"}`)
	if got, err := MachineRegistry(); got != "https://registry.example" || err != nil {
		t.Fatalf("pin %q %v", got, err)
	}
	withMachine(t, "")
	if got, err := MachineRegistry(); got != "" || err != nil {
		t.Fatalf("no machine settings pinned %q %v", got, err)
	}
}
