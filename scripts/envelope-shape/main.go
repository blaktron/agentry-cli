// Command envelope-shape compares the result envelopes that two adapters
// wrote for the same QuickAgent run, and says where they disagree.
//
//	go run ./scripts/envelope-shape <a.json> <b.json>
//
// Every adapter must write one envelope shape: the same keys at every depth,
// and the same values wherever the value is a fact about the QuickAgent, the
// policy or the run's outcome rather than about the harness or the model. So
// the comparison is of the whole tree, with three exceptions:
//
//   - fields that belong to one run (its id, times, folders, usage, the
//     model's answer, the harness's own description) are left out;
//   - fields whose value differs by design between adapters or between any
//     two runs (the result file's path, a deliverable's title, bytes,
//     digest and whether the agent marked it, the receipt lines that
//     describe the harness's own container) are compared by presence and
//     JSON kind only;
//   - everything else is compared exactly.
//
// It prints one line per difference and exits 1 when there is any, 2 on a
// usage or read error.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

// A path names a field: map keys, with "[]" standing for any array element.
type path []string

// omitted are left out of the comparison: they belong to one run.
var omitted = []path{
	{"runId"},
	{"usage"},
	{"result"},
	{"harness"},
	{"receipts"},
	{"outDir"},
	{"startedAt"},
	{"endedAt"},
	// A harness that runs as a host process says so, because the run
	// network is not what bounds it; a container harness has no such line.
	// Every other enforcement claim must match.
	{"capabilities_enforced", "harness.isolation"},
}

// presenceOnly are compared by presence and JSON kind: their values differ
// between adapters or between any two runs by design.
var presenceOnly = []path{
	// The path holds the adapter's name.
	{"resultFile"},
	// A deliverable's title, bytes and digest are the model's own words,
	// and whether it was marked is the model's choice: the agent names the
	// title when it marks the file, and the runner picks it when it does not.
	{"deliverables", "files", "[]", "title"},
	{"deliverables", "files", "[]", "marked"},
	{"deliverables", "files", "[]", "bytes"},
	{"deliverables", "files", "[]", "sha256"},
	// The receipt lines that describe the harness's own container: what it
	// mounts, how its containers are bounded, how its tool results are kept.
	{"capabilities_enforced", "agentContainer"},
	{"capabilities_enforced", "containers"},
	{"capabilities_enforced", "toolResults"},
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: envelope-shape <a.json> <b.json>")
		os.Exit(2)
	}
	a, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	b, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	diffs := Compare(a, b)
	report(os.Stdout, os.Args[1], os.Args[2], diffs)
	if len(diffs) > 0 {
		os.Exit(1)
	}
}

func load(name string) (any, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

func report(w io.Writer, an, bn string, diffs []string) {
	if len(diffs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%d difference(s) between %s and %s:\n", len(diffs), an, bn)
	for _, d := range diffs {
		_, _ = fmt.Fprintln(w, "  "+d)
	}
}

// Compare returns one line per place where b's envelope disagrees with a's,
// in a stable order; none means the two agree.
func Compare(a, b any) []string {
	var out []string
	compare(a, b, nil, "", &out)
	return out
}

func compare(a, b any, at path, shown string, out *[]string) {
	if matches(at, omitted) {
		return
	}
	if kind(a) != kind(b) {
		*out = append(*out, fmt.Sprintf("%s: %s in a, %s in b", label(shown), kind(a), kind(b)))
		return
	}
	if matches(at, presenceOnly) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv := b.(map[string]any)
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child := append(append(path{}, at...), k)
			if matches(child, omitted) {
				continue
			}
			ak, inA := av[k]
			bk, inB := bv[k]
			switch {
			case !inB:
				*out = append(*out, fmt.Sprintf("%s: in a, missing from b", join(shown, k)))
			case !inA:
				*out = append(*out, fmt.Sprintf("%s: in b, missing from a", join(shown, k)))
			default:
				compare(ak, bk, child, join(shown, k), out)
			}
		}
	case []any:
		bv := b.([]any)
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: %d element(s) in a, %d in b", label(shown), len(av), len(bv)))
			return
		}
		child := append(append(path{}, at...), "[]")
		for i := range av {
			compare(av[i], bv[i], child, fmt.Sprintf("%s[%d]", shown, i), out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: %s in a, %s in b", label(shown), brief(a), brief(b)))
		}
	}
}

func matches(at path, set []path) bool {
	for _, p := range set {
		if reflect.DeepEqual([]string(p), []string(at)) {
			return true
		}
	}
	return false
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	}
	return fmt.Sprintf("%T", v)
}

func join(shown, key string) string {
	if strings.ContainsAny(key, ".[] ") {
		key = fmt.Sprintf("[%q]", key)
		return shown + key
	}
	if shown == "" {
		return key
	}
	return shown + "." + key
}

func label(shown string) string {
	if shown == "" {
		return "(the envelope)"
	}
	return shown
}

func brief(v any) string {
	data, _ := json.Marshal(v)
	s := string(data)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
