package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// wantOnlyScaffold runs `agentry validate --json` on dir and fails the test
// unless the check refuses the package for exactly the scaffold lines named
// ("file:marker", in any order) and for nothing else: no manifest error, no
// inventory or scan finding.
func wantOnlyScaffold(t *testing.T, dir string, want ...string) {
	t.Helper()
	jout, _, err := runCLI(t, nil, "--json", "validate", dir)
	var rep validateOutput
	if uerr := json.Unmarshal([]byte(jout), &rep); uerr != nil {
		t.Fatalf("validate --json: %v (%v)\n%s", uerr, err, jout)
	}
	c := rep.Counts
	if c.Errors != 0 || c.InventoryFailing != 0 || c.Findings != 0 {
		t.Fatalf("validate refused more than scaffold lines: %+v\n%s", c, jout)
	}
	got := make([]string, 0, len(rep.Unfinished))
	for _, u := range rep.Unfinished {
		got = append(got, u.File+":"+u.Marker)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("scaffold lines = %v, want %v", got, want)
	}
	if len(want) > 0 && (err == nil || rep.Passed || !strings.Contains(err.Error(), "scaffold line")) {
		t.Fatalf("validate passed a package with scaffold lines: %v passed=%v", err, rep.Passed)
	}
	if len(want) == 0 && (err != nil || !rep.Passed) {
		t.Fatalf("validate refused a finished package: %v", err)
	}
}
