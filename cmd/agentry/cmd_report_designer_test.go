package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/reportdesigner/build"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// fakeDesigner stands in for the Report Designer: it records the options it
// was given and answers d as the design it made, written where a run
// writes it.
func fakeDesigner(t *testing.T, d reportdesign.Design, looked bool) *build.Options {
	t.Helper()
	got := &build.Options{}
	prev := designerRun
	t.Cleanup(func() { designerRun = prev })
	designerRun = func(_ context.Context, _ dockerx.Client, o build.Options) (*build.Result, error) {
		*got = o
		dir := filepath.Join(o.OutDir, "design")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "design.json"), d.Canonical(), 0o644); err != nil { //nolint:gosec // test output
			return nil, err
		}
		return &build.Result{Envelope: &receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok"}, Design: d, Looked: looked, Previews: 3, Dir: dir}, nil
	}
	return got
}

// TestADesignerRunNeverChangesTheDefault: --save adds the design under a
// name of its own, beside a design of the same name and never in its place,
// and the account's default stays the one the person picked.
func TestADesignerRunNeverChangesTheDefault(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := newDesignRegistry(t)
	t.Setenv(envToken, "agy_test")
	house, _ := reportdesign.Builtin("classic")
	house.Name = "House"
	houseID := reg.add(t, house.Canonical(), true)
	same, _ := reportdesign.Builtin("modern")
	same.Name = "Northwind"
	reg.add(t, same.Canonical(), false)

	made, _ := reportdesign.Builtin("studio")
	made.Name = "Northwind"
	got := fakeDesigner(t, made, true)
	out := t.TempDir()
	stdout, _, err := reportDesign(t, reg, "design", "--brief", "bold, for a design studio", "--from", "House", "--save", "--out", out)
	if err != nil {
		t.Fatalf("design --save: %v", err)
	}
	if got.Job != build.Design || got.From == nil || got.From.Name != "House" || got.Brief != "bold, for a design studio" {
		t.Errorf("the run was given %+v", got)
	}
	designs, dflt := reg.stored()
	if dflt != "House" {
		t.Errorf("the default is %q after a Designer run, want House", dflt)
	}
	if len(designs) != 3 || designs["Northwind (2)"] == nil {
		t.Fatalf("library %v, want the new design beside the one of its name", keysOf(designs))
	}
	if string(designs["Northwind"]) != string(same.Canonical()) {
		t.Error("the design of the same name was changed")
	}
	if !strings.Contains(stdout, `"Northwind (2)"`) || !strings.Contains(stdout, "It is not your default") {
		t.Errorf("output %q", stdout)
	}
	if reg.byName("House").id != houseID || !reg.byName("House").isDefault {
		t.Error("the default design moved")
	}
	if _, err := os.Stat(filepath.Join(out, "design", "design.json")); err != nil {
		t.Errorf("the design was not written to the output folder: %v", err)
	}
}

// TestADesignerRunWithoutSaveLeavesTheLibraryAlone: without --save the
// design is a file, and the library is not written; a model that could not
// look says so first.
func TestADesignerRunWithoutSaveLeavesTheLibraryAlone(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := newDesignRegistry(t)
	t.Setenv(envToken, "agy_test")
	made, _ := reportdesign.Builtin("field")
	fakeDesigner(t, made, false)
	stdout, _, err := reportDesign(t, reg, "design", "--brief", "warm", "--out", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if reg.writes != 0 {
		t.Errorf("%d writes to the library without --save", reg.writes)
	}
	if !strings.HasPrefix(stdout, "The Report Designer says it could not look at its renders") || !strings.Contains(stdout, "agentry report-design import") {
		t.Errorf("output %q", stdout)
	}
}

// TestTheDesignerCommandsTakeWhatTheyNeed: a cover needs a design, a
// design a brief, and --save a sign-in, each refused before any run.
func TestTheDesignerCommandsTakeWhatTheyNeed(t *testing.T) {
	isolateEnv(t)
	newInstall(t)
	reg := newDesignRegistry(t)
	ran := false
	prev := designerRun
	t.Cleanup(func() { designerRun = prev })
	designerRun = func(context.Context, dockerx.Client, build.Options) (*build.Result, error) {
		ran = true
		return nil, build.ErrNoDesign
	}
	if _, _, err := reportDesign(t, reg, "cover", "--brief", "b"); exitOf(err) != 2 || !strings.Contains(err.Error(), "--design") {
		t.Errorf("cover without --design = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "design", "--brief", "b", "--save"); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("--save signed out = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "design", "--brief", "b", "--from", "No Such Design"); exitOf(err) != 2 {
		t.Errorf("--from of nothing = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "design", "--brief", "b", "--url", "https://a.example/", "--url", "https://b.example/", "--url", "https://c.example/"); exitOf(err) != 2 {
		t.Errorf("three addresses = %v", err)
	}
	if ran {
		t.Error("a refused command ran the Designer")
	}
	// A built-in theme is a design to start from by its id or its name.
	got := fakeDesigner(t, reportdesign.Default(), true)
	if _, _, err := reportDesign(t, reg, "cover", "--brief", "b", "--design", "boardroom", "--out", t.TempDir()); err != nil || got.From == nil || got.From.Name != "Boardroom" || got.Job != build.Cover {
		t.Errorf("cover of a built-in = %v, %+v", err, got.From)
	}
}

func TestFreeName(t *testing.T) {
	list := libraryList{Designs: []librarySummary{{ID: "a", Name: "Harbour"}, {ID: "b", Name: "harbour (2)"}}}
	if got := freeName(list, "Harbour"); got != "Harbour (3)" {
		t.Errorf("freeName = %q", got)
	}
	if got := freeName(list, "Other"); got != "Other" {
		t.Errorf("freeName = %q", got)
	}
	long := strings.Repeat("x", reportdesign.MaxNameRunes)
	list.Designs = append(list.Designs, librarySummary{ID: "c", Name: long})
	if got := freeName(list, long); len([]rune(got)) > reportdesign.MaxNameRunes || !strings.HasSuffix(got, " (2)") {
		t.Errorf("freeName of a full-length name = %q", got)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
