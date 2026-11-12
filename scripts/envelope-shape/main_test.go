package main

import (
	"reflect"
	"strings"
	"testing"
)

// recorded loads the two envelopes the same fixture QuickAgent's allowed run
// wrote through docker-agent and through the Agentry harness.
func recorded(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	a, err := load("testdata/docker-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := load("testdata/agentry-harness.json")
	if err != nil {
		t.Fatal(err)
	}
	return a.(map[string]any), b.(map[string]any)
}

func TestTwoAdaptersWriteOneShape(t *testing.T) {
	a, b := recorded(t)
	if diffs := Compare(a, b); len(diffs) > 0 {
		t.Fatalf("the recorded envelopes should agree:\n%s", strings.Join(diffs, "\n"))
	}
}

func TestTheRecordedEnvelopesDifferWhereTheComparisonLooksAway(t *testing.T) {
	// The recordings are only evidence if they really differ where two
	// adapters' runs always do.
	a, b := recorded(t)
	differ := []path{
		{"runId"}, {"harness"}, {"resultFile"},
		{"deliverables", "files", "[]", "title"},
		{"deliverables", "files", "[]", "bytes"},
		{"deliverables", "files", "[]", "sha256"},
		{"capabilities_enforced", "agentContainer"},
		{"capabilities_enforced", "containers"},
		{"capabilities_enforced", "toolResults"},
	}
	for _, p := range differ {
		av, bv := at(a, p), at(b, p)
		if av == nil || bv == nil {
			t.Errorf("%v is missing from a recording", p)
			continue
		}
		if reflect.DeepEqual(av, bv) {
			t.Errorf("%v is the same in both recordings: %v", p, av)
		}
	}
}

// at answers the value a path names, taking the first element of an array.
func at(v any, p path) any {
	for _, k := range p {
		switch x := v.(type) {
		case map[string]any:
			v = x[k]
		case []any:
			if len(x) == 0 {
				return nil
			}
			v = x[0]
		default:
			return nil
		}
	}
	return v
}

func TestADifferenceInShapeOrInAnAgreedValueIsFoundAndNoOther(t *testing.T) {
	cases := []struct {
		name   string
		change func(b map[string]any)
		want   string
	}{
		{"a top-level key missing", func(b map[string]any) { delete(b, "exitClass") }, "exitClass: in a, missing from b"},
		{"a key only in b", func(b map[string]any) { b["extra"] = true }, "extra: in b, missing from a"},
		{"a deliverable's title missing", func(b map[string]any) {
			delete(files(b)[0].(map[string]any), "title")
		}, "deliverables.files[0].title: in a, missing from b"},
		{"a presence-only field missing", func(b map[string]any) {
			delete(files(b)[0].(map[string]any), "sha256")
		}, "deliverables.files[0].sha256: in a, missing from b"},
		{"a deliverable marked by the agent in one run only", func(b map[string]any) {
			f := files(b)[0].(map[string]any)
			f["marked"] = !f["marked"].(bool)
		}, ""},
		{"a presence-only field of another kind", func(b map[string]any) { b["resultFile"] = 7.0 }, "resultFile: string in a, number in b"},
		{"an enforcement claim that differs", func(b map[string]any) {
			b["capabilities_enforced"].(map[string]any)["network.egress"] = "anywhere"
		}, `capabilities_enforced["network.egress"]: `},
		{"the outcome differs", func(b map[string]any) { b["status"] = "failed" }, `status: "ok" in a, "failed" in b`},
		{"a deliverable more", func(b map[string]any) {
			d := b["deliverables"].(map[string]any)
			d["files"] = append(files(b), files(b)[0])
		}, "deliverables.files: 1 element(s) in a, 2 in b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := recorded(t)
			tc.change(b)
			diffs := Compare(a, b)
			if tc.want == "" {
				if len(diffs) > 0 {
					t.Fatalf("want no difference, got %q", diffs)
				}
				return
			}
			if len(diffs) != 1 || !strings.HasPrefix(diffs[0], tc.want) {
				t.Fatalf("want one difference starting %q, got %q", tc.want, diffs)
			}
		})
	}
}

func TestTheFieldsOfOneRunAreLeftOut(t *testing.T) {
	a, b := recorded(t)
	for _, k := range []string{"runId", "usage", "result", "harness", "receipts", "outDir"} {
		delete(b, k)
	}
	delete(b["capabilities_enforced"].(map[string]any), "harness.isolation")
	if diffs := Compare(a, b); len(diffs) > 0 {
		t.Fatalf("dropping a run's own fields made a difference:\n%s", strings.Join(diffs, "\n"))
	}
}

func files(env map[string]any) []any {
	return env["deliverables"].(map[string]any)["files"].([]any)
}
