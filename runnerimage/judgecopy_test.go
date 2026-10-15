package runnerimage

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corejudge "github.com/blaktron/agentry/pkg/judge"

	imagejudge "github.com/blaktron/agentry-cli/runnerimage/judge"
)

// TestTheImageJudgeIsTheCores holds the runner image's copies of the core's
// pkg/judge, pkg/websearch and pkg/reportdesign (with its built-in themes) to
// the module this CLI builds against, byte for byte. The image compiles
// from stdlib-only embedded sources with no module download, so its tools
// server cannot import the core; a copy that drifted would ask Jev something
// the registry does not; a design copy that
// drifted would draw a design the core refuses, or refuse one it stored.
// After the core dependency moves, scripts/sync-image-judge.sh refreshes
// all three.
func TestTheImageJudgeIsTheCores(t *testing.T) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/blaktron/agentry")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("locate the core module: %v", err)
	}
	coreDir := strings.TrimSpace(string(out))
	for _, pkg := range []string{"judge", "websearch", "reportdesign", "reportdesign/builtins"} {
		want := goSources(t, filepath.Join(coreDir, "pkg", pkg))
		got := goSources(t, pkg)
		if strings.Join(keys(want), ",") != strings.Join(keys(got), ",") {
			t.Fatalf("runnerimage/%s holds %v, the core's pkg/%s %v: run scripts/sync-image-judge.sh", pkg, keys(got), pkg, keys(want))
		}
		for name, b := range want {
			if !bytes.Equal(got[name], b) {
				t.Errorf("runnerimage/%s/%s differs from the core's pkg/%s/%s: run scripts/sync-image-judge.sh", pkg, name, pkg, name)
			}
		}
	}

	// And they ask the same thing: the question set's hash covers the
	// model, the questions, the chunking, the thresholds and the band.
	cc, err := corejudge.New(corejudge.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ic, err := imagejudge.New(imagejudge.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if c, i := corejudge.NewModelInputScanner(cc).QuestionSet(), imagejudge.NewModelInputScanner(ic).QuestionSet(); c != i || c == "" {
		t.Fatalf("question sets differ: core %s, image %s", c, i)
	}
}

// ruleset045RubricHash is the rubric hash of the shipped request shape under
// ruleset 0.4.5: C3 holds only a dangerous capability
// that is undeclared and out of line with the purpose in the state, and the
// runner grants name a declared target host. It moves only when the core's
// rubric does, and then so must this literal.
const ruleset045RubricHash = "sha256:82844f1983ce676ce036eb057e6e72bfe5e0d1ac539991cb51be86d89792bdeb"

// TestTheImageRubricAsksWhatTheCoreAsks holds the image's copy of AI Review's
// rubric to the core's by what it asks, beyond the byte comparison of the
// sources: the six questions' wording, the state a request carries (with the
// purpose C3 weighs a capability against), the severities a score is given and
// the rubric hash a scan records, which is also the literal ruleset 0.4.5
// shipped under.
func TestTheImageRubricAsksWhatTheCoreAsks(t *testing.T) {
	cq, iq := corejudge.RubricQuestions(), imagejudge.RubricQuestions()
	if len(cq) != 6 || len(iq) != len(cq) {
		t.Fatalf("the rubric has %d questions in the image, %d in the core", len(iq), len(cq))
	}
	for i := range cq {
		if cq[i].ID != iq[i].ID || cq[i].Text != iq[i].Text {
			t.Errorf("question %d differs: core %q, image %q", i, cq[i].ID, iq[i].ID)
		}
	}
	for _, want := range []string{"is not in line with the QuickAgent's purpose as stated in purpose", "An action that serves the stated purpose does not count even when it is not declared"} {
		if !strings.Contains(imagejudge.RubricUndeclaredCapability, want) {
			t.Errorf("C3 in the image no longer says %q", want)
		}
	}
	if imagejudge.RunnerGrants != corejudge.RunnerGrants || imagejudge.PurposeCap != corejudge.PurposeCap ||
		imagejudge.DeclaredCapabilitiesCap != corejudge.DeclaredCapabilitiesCap {
		t.Error("the runner's grants or a bound of the state differs from the core's")
	}
	for _, id := range []string{corejudge.RubricIDInjection, corejudge.RubricIDExfiltration, corejudge.RubricIDUndeclaredCapability,
		corejudge.RubricIDDeception, corejudge.RubricIDObfuscation, corejudge.RubricIDDestructive} {
		for score := 0; score <= 3; score++ {
			if c, i := corejudge.RubricSeverity(id, score), imagejudge.RubricSeverity(id, score); c != i {
				t.Errorf("%s at %d: severity %q in the image, the core's %q", id, score, i, c)
			}
		}
	}
	long := strings.Repeat("é", imagejudge.PurposeCap)
	for _, doc := range []string{
		`{"name":"sast-scan","summary":"Scans a repository for injection flaws.","guide":{"overview":"Reads source files and reports findings."}}`,
		`{"name":"only-a-name"}`, `{"summary":"  a summary  "}`, `{}`, `not json`,
		`{"name":"n","summary":` + `"` + long + `"}`,
	} {
		c, i := corejudge.Purpose([]byte(doc)), imagejudge.Purpose([]byte(doc))
		if c != i || c == "" {
			t.Errorf("Purpose(%.60s) = %q in the image, the core's %q", doc, i, c)
		}
		cs, is := corejudge.RubricState("SKILL.md", "chunk", "{}", c), imagejudge.RubricState("SKILL.md", "chunk", "{}", i)
		if len(cs) != len(is) || len(cs) != 5 {
			t.Fatalf("the state carries %d fields in the image, %d in the core", len(is), len(cs))
		}
		for k, v := range cs {
			if is[k] != v {
				t.Errorf("state field %s differs: image %q, core %q", k, is[k], v)
			}
		}
	}
	cc, err := corejudge.New(corejudge.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ic, err := imagejudge.New(imagejudge.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []int{corejudge.RubricBatchOne, corejudge.RubricBatchTwo, corejudge.RubricBatchSix} {
		if c, i := corejudge.NewRubricScannerShape(cc, shape).Hash(), imagejudge.NewRubricScannerShape(ic, shape).Hash(); c != i || c == "" {
			t.Errorf("rubric hash of shape %d: image %s, core %s", shape, i, c)
		}
	}
	if got := imagejudge.NewRubricScanner(ic).Hash(); got != ruleset045RubricHash {
		t.Errorf("the image's rubric hash is %s, ruleset 0.4.5's is %s", got, ruleset045RubricHash)
	}
}

func goSources(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		n := e.Name()
		// The Go sources, and the JSON beside them (reportdesign's schema
		// and built-in themes).
		if e.IsDir() || !strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, ".json") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // the test's own tree and the module cache
		if err != nil {
			t.Fatal(err)
		}
		out[n] = b
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
