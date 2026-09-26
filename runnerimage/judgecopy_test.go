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
// pkg/judge and pkg/websearch to the module this CLI builds against, byte for
// byte (DR-43; plan builder-rework.md D25). The image compiles from
// stdlib-only embedded sources with no module download (DR-2), so its tools
// server cannot import the core; a copy that drifted would ask Jev something
// the registry does not, which is the fork D20 refused. After the core
// dependency moves, scripts/sync-image-judge.sh refreshes both.
func TestTheImageJudgeIsTheCores(t *testing.T) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/blaktron/agentry")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("locate the core module: %v", err)
	}
	coreDir := strings.TrimSpace(string(out))
	for _, pkg := range []string{"judge", "websearch"} {
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
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
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
