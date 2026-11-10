package managed

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"
)

func assignments(t *testing.T, org string, locked bool, list ...managedsettings.Assignment) State {
	t.Helper()
	d := docFor("https://reg.example", 1, map[string]managedsettings.Setting{
		managedsettings.QuickAgents: {Value: raw(t, list), Locked: locked},
	})
	d.Org = org
	return State{Registry: "https://reg.example", Document: d}
}

func TestAssignmentsReadLockedRowsAndTheFirstDocumentWins(t *testing.T) {
	req := managedsettings.Assignment{Ref: "acme/a", Version: "1.0.0", Mode: managedsettings.AssignRequired}
	off := managedsettings.Assignment{Ref: "acme/b", Version: managedsettings.VersionLatest, Mode: managedsettings.AssignOffered}
	e := Effective{Documents: []State{
		assignments(t, "acme", true, req, off),
		assignments(t, "other", true, managedsettings.Assignment{Ref: "acme/a", Version: "2.0.0", Mode: managedsettings.AssignOffered}),
		assignments(t, "third", false, managedsettings.Assignment{Ref: "acme/c", Version: "1.0.0", Mode: managedsettings.AssignRequired}),
	}}
	got := e.Assignments()
	if len(got) != 2 || got[0].Ref != "acme/a" || got[0].Version != "1.0.0" || got[0].Organization != "acme" || got[1].Ref != "acme/b" {
		t.Fatalf("Assignments = %+v", got)
	}
	if r := e.Required(); len(r) != 1 || r[0].Selector() != "acme/a@1.0.0" {
		t.Fatalf("Required = %+v", r)
	}
	if got[1].Selector() != "acme/b" {
		t.Errorf("a latest assignment resolves %q", got[1].Selector())
	}
	if e.RequiredBy("acme/a") != "acme" || e.RequiredBy("acme/b") != "" || e.RequiredBy("acme/c") != "" {
		t.Errorf("RequiredBy: %q %q %q", e.RequiredBy("acme/a"), e.RequiredBy("acme/b"), e.RequiredBy("acme/c"))
	}
	if r := e.UninstallRefusal("acme/a"); !strings.Contains(r, "acme's managed settings (quickagents)") {
		t.Errorf("UninstallRefusal = %q", r)
	}
	if r := e.UninstallRefusal("acme/b"); r != "" {
		t.Errorf("an offered QuickAgent is refused: %q", r)
	}
}
