package ship

import (
	"slices"
	"strings"
	"testing"
)

const commented = `# the package
schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "a test package"
license: Apache-2.0
entrypoints:
  instruction: AGENT.md
publish:
  include: [agent.yaml, AGENT.md]
`

func TestAddShipEditsTheTwoPlacesAndKeepsTheRest(t *testing.T) {
	e, err := AddShip([]byte(commented), "runbooks")
	if err != nil {
		t.Fatal(err)
	}
	got := string(e.YAML)
	for _, want := range []string{"# the package", "knowledge:", "ships:", "name: runbooks", "knowledge/**", `summary: "a test package"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the edited agent.yaml lacks %q:\n%s", want, got)
		}
	}
	if len(e.Did) != 2 {
		t.Errorf("did %q", e.Did)
	}
	names, err := Ships(e.YAML)
	if err != nil || !slices.Equal(names, []string{"runbooks"}) {
		t.Fatalf("ships %v %v", names, err)
	}
	// Adding it again changes nothing.
	again, err := AddShip(e.YAML, "runbooks")
	if err != nil || len(again.Did) != 0 || string(again.YAML) != got {
		t.Errorf("a second add changed the file: %q %v", again.Did, err)
	}
	// A second pack joins the first, with no second pattern.
	two, err := AddShip(e.YAML, "handbook")
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := Ships(two.YAML); !slices.Equal(names, []string{"runbooks", "handbook"}) || strings.Count(string(two.YAML), "knowledge/**") != 1 {
		t.Errorf("two packs: %v\n%s", names, two.YAML)
	}
}

func TestAddShipRefusesWhatItCannotEdit(t *testing.T) {
	if _, err := AddShip([]byte(strings.Replace(commented, `"2"`, `"1"`, 1)), "x"); err == nil || !strings.Contains(err.Error(), "schemaVersion 2") {
		t.Errorf("schemaVersion 1: %v", err)
	}
	if _, err := AddShip([]byte("name: x\n"), "x"); err == nil || !strings.Contains(err.Error(), "publish") {
		t.Errorf("no publish block: %v", err)
	}
	if _, err := AddShip([]byte("- a\n"), "x"); err == nil {
		t.Error("a list for a manifest")
	}
}

func TestRemoveShipTidiesWhatItLeavesEmpty(t *testing.T) {
	two, _ := AddShip([]byte(commented), "a")
	two, _ = AddShip(two.YAML, "b")
	one, err := RemoveShip(two.YAML, "a")
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := Ships(one.YAML); !slices.Equal(names, []string{"b"}) || !strings.Contains(string(one.YAML), "knowledge/**") {
		t.Errorf("one left:\n%s", one.YAML)
	}
	none, err := RemoveShip(one.YAML, "b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(none.YAML), "knowledge") || !strings.Contains(string(none.YAML), "# the package") {
		t.Errorf("none left:\n%s", none.YAML)
	}
	// A knowledge block that also accepts a pack stays.
	acc := strings.Replace(commented, "publish:", "knowledge:\n  accepts:\n    prompt: Your notes\npublish:", 1)
	withShip, _ := AddShip([]byte(acc), "a")
	left, _ := RemoveShip(withShip.YAML, "a")
	if !strings.Contains(string(left.YAML), "accepts:") || strings.Contains(string(left.YAML), "ships") {
		t.Errorf("accepts lost:\n%s", left.YAML)
	}
}
