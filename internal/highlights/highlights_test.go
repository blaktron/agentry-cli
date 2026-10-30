package highlights

import (
	"strings"
	"testing"
)

func TestTheCompiledFileKeepsEveryRule(t *testing.T) {
	entries, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the compiled file holds no version")
	}
}

func TestALineIsOnePlainSentence(t *testing.T) {
	good := []string{
		"agentry run keeps its place when a model pauses.",
		"Can a schedule run while you are signed out?",
		"Searches answer faster on a large pack!",
		"The Drafter checks a draft's tools: what it calls, it must be given.",
		strings.Repeat("a", MaxLineLength-1) + ".",
	}
	for _, line := range good {
		if err := CheckLine(line); err != nil {
			t.Errorf("refused %q: %v", line, err)
		}
	}
	bad := map[string]string{
		"too long":          strings.Repeat("a", MaxLineLength) + ".",
		"empty":             "",
		"leading space":     " Faster runs.",
		"two lines":         "Faster runs.\nAnd more.",
		"no full stop":      "Faster runs",
		"bold":              "**Faster** runs.",
		"code":              "Run `agentry run` faster.",
		"html":              "Faster <script>alert(1)</script> runs.",
		"link":              "See [the docs](x) for runs.",
		"list item":         "- Faster runs.",
		"numbered item":     "1. Faster runs.",
		"issue reference":   "Faster runs (#123).",
		"register id":       "Faster runs, as DEV-264 decided.",
		"commit prefix":     "feat: faster runs.",
		"scoped prefix":     "fix(drafter): a draft is repaired.",
		"address":           "Read more at https://example.org/runs.",
		"host name":         "Runs reach dev.secureagentry.net faster.",
		"internal host":     "Built on agentry-runner2 faster.",
		"underscore symbol": "The tool_spec grant is kept.",
	}
	for name, line := range bad {
		if err := CheckLine(line); err == nil {
			t.Errorf("%s: accepted %q", name, line)
		}
	}
}

func TestAFileIsNewestFirstEachVersionOnce(t *testing.T) {
	ok := `{"versions":[{"version":"0.6.1008","highlights":["A."]},{"version":"0.6.999","highlights":["B.","C.","D.","E."]}]}`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("refused a good file: %v", err)
	}
	bad := map[string]string{
		"not json":        `{`,
		"unknown field":   `{"versions":[],"next":[]}`,
		"oldest first":    `{"versions":[{"version":"0.6.999","highlights":["A."]},{"version":"0.6.1008","highlights":["B."]}]}`,
		"twice":           `{"versions":[{"version":"0.6.9","highlights":["A."]},{"version":"0.6.9","highlights":["B."]}]}`,
		"a prerelease":    `{"versions":[{"version":"0.6.9-dev.1","highlights":["A."]}]}`,
		"a v":             `{"versions":[{"version":"v0.6.9","highlights":["A."]}]}`,
		"two parts":       `{"versions":[{"version":"0.6","highlights":["A."]}]}`,
		"leading zero":    `{"versions":[{"version":"0.06.9","highlights":["A."]}]}`,
		"no lines":        `{"versions":[{"version":"0.6.9","highlights":[]}]}`,
		"five lines":      `{"versions":[{"version":"0.6.9","highlights":["A.","B.","C.","D.","E."]}]}`,
		"a bad line":      `{"versions":[{"version":"0.6.9","highlights":["fix: a."]}]}`,
		"two json values": `{"versions":[]} {}`,
	}
	for name, f := range bad {
		if _, err := Parse([]byte(f)); err == nil {
			t.Errorf("%s: accepted %s", name, f)
		}
	}
}
