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
// the module this CLI builds against, byte for byte (DR-43; plan
// builder-rework.md D25; DR-87, plan report-designs.md D22). The image compiles from
// stdlib-only embedded sources with no module download (DR-2), so its tools
// server cannot import the core; a copy that drifted would ask Jev something
// the registry does not, which is the fork D20 refused; a design copy that
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
		if e.IsDir() || !(strings.HasSuffix(n, ".go") || strings.HasSuffix(n, ".json")) || strings.HasSuffix(n, "_test.go") {
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
