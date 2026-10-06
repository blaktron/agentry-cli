package drafter

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestTheEmbeddedServerListLoads: the lists the Drafter ships are the file
// in its embedded tree, and every entry keeps the file's rules
// (agentry-cli#658).
func TestTheEmbeddedServerListLoads(t *testing.T) {
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	var runs, not int
	for _, s := range l.Servers {
		switch s.List {
		case RunsHere:
			runs++
		case DoesNotRunHere:
			not++
		}
	}
	if runs == 0 || not == 0 || len(l.Dependencies) == 0 {
		t.Fatalf("runs here %d, does not run here %d, dependencies %d: the survey seeds all three", runs, not, len(l.Dependencies))
	}
}

// TestTheServerListHoldsItsRules: an entry the file's rules refuse fails
// the load and names the entry, so a hand edit cannot ship a reason outside
// the closed set, a Runs here entry with a reason, or an entry without the
// observation, the evidence and the date the honest-posture rule asks for.
func TestTheServerListHoldsItsRules(t *testing.T) {
	const good = `servers:
  - ecosystem: npm
    package: a
    versions: 1.0.0
    list: runs-here
    runtime: Node 24.21.0
    observed: started and listed 2 tools.
    evidence: survey 2026-10-05
    date: 2026-10-05
  - ecosystem: pypi
    package: b
    versions: all
    list: does-not-run-here
    reason: musl-native
    runtime: Python 3.12.14
    observed: did not install.
    evidence: survey 2026-10-05
    date: 2026-10-05
dependencies:
  - ecosystem: pypi
    package: onnxruntime
    reason: musl-native
    observed: publishes no musllinux wheel.
    evidence: survey 2026-10-05
    date: 2026-10-05
`
	load := func(doc string) error {
		_, err := LoadServerList(fstest.MapFS{ServerListFile: {Data: []byte(doc)}})
		return err
	}
	if err := load(good); err != nil {
		t.Fatalf("good list: %v", err)
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"reason outside the set":  {"reason: musl-native\n    runtime: Python", "reason: flaky\n    runtime: Python", "closed set"},
		"no reason":               {"    reason: musl-native\n    runtime: Python", "    runtime: Python", "closed set"},
		"reason on runs here":     {"list: runs-here\n", "list: runs-here\n    reason: musl-native\n", "carries no reason"},
		"unknown list":            {"list: runs-here", "list: maybe", "list must be"},
		"a version range":         {"versions: 1.0.0", `versions: ">=1.0.0"`, "not a range"},
		"a caret range":           {"versions: 1.0.0", "versions: ^1.0.0", "not a range"},
		"no evidence":             {"    evidence: survey 2026-10-05\n    date: 2026-10-05\n  - ecosystem: pypi", "    date: 2026-10-05\n  - ecosystem: pypi", "required"},
		"no date on a dependency": {"wheel.\n    evidence: survey 2026-10-05\n    date: 2026-10-05", "wheel.\n    evidence: survey 2026-10-05", "required"},
		"unknown ecosystem":       {"ecosystem: npm\n    package: a", "ecosystem: cargo\n    package: a", "ecosystem"},
		"unknown field":           {"    observed: started", "    safe: true\n    observed: started", "safe"},
		"listed twice":            {"dependencies:\n", "  - ecosystem: npm\n    package: a\n    versions: 2.0.0\n    list: runs-here\n    runtime: Node 24.21.0\n    observed: listed 3 tools.\n    evidence: survey 2026-10-06\n    date: 2026-10-06\ndependencies:\n", "listed twice"},
	} {
		doc := strings.Replace(good, c.old, c.new, 1)
		if doc == good {
			t.Fatalf("%s: the case does not change the list", name)
		}
		err := load(doc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
}
