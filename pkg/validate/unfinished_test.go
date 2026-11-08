package validate

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// markers lists a check's results as "file:line:marker", the line 0 for a
// manifest field.
func markers(us []Unfinished) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.File + ":" + strconv.Itoa(u.Line) + ":" + u.Marker
	}
	return out
}

// write overwrites files of the workspace dir.
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFindUnfinishedNamesEachScaffoldLine: every scaffold the estate writes
// (the template, `agentry init`, the Drafter) is caught by the line it
// leaves, and the author's own prose that only resembles one is not.
func TestFindUnfinishedNamesEachScaffoldLine(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a finished package", nil, nil},
		{"init's TODOs and licence note", map[string]string{
			"agent.yaml": strings.Replace(mustRead(t, "agent.yaml"), `summary: "Says hello."`, `summary: "TODO one line (at most 140 chars) saying what this QuickAgent does"`, 1),
			"SKILL.md":   "---\nname: demo\ndescription: TODO one line (at most 140 chars) saying what this QuickAgent does\n---\n\n# demo\n\nTODO write the instructions the agent follows.\n",
			"README.md":  "# demo\n\nTODO describe this QuickAgent. This file renders on the QuickAgent page.\n",
			"LICENSE":    "PLACEHOLDER LICENSE NOTE\n\nReplace this file.\n",
		}, []string{"agent.yaml:0:TODO summary", "LICENSE:1:placeholder licence note", "README.md:3:TODO", "SKILL.md:3:TODO", "SKILL.md:8:TODO"}},
		{"the template's namespace, author, copyright line and README", map[string]string{
			"agent.yaml": strings.Replace(mustRead(t, "agent.yaml"), "namespace: acme", "namespace: your-namespace\nauthors:\n  - name: Your Name", 1),
			"README.md":  "# QuickAgent template\n\n**This README is about the template.** The README you publish is shown on\n",
			"LICENSE":    "MIT License\n\nCopyright (c) <year> <your name>\n",
		}, []string{"agent.yaml:0:placeholder namespace", "agent.yaml:0:placeholder author", "LICENSE:3:placeholder copyright line", "README.md:3:the template's README"}},
		{"the Drafter's closing line", map[string]string{
			"README.md": "# demo\n\nSays hello.\n\nDrafted by the Drafter (`agentry draft`). Review the manifest, the instructions and this file before you publish;\nthis README renders on the QuickAgent's page.\n",
		}, []string{"README.md:5:the Drafter's closing line"}},
		{"prose that only resembles a scaffold", map[string]string{
			"SKILL.md":  "---\nname: demo\ndescription: Says hello.\n---\n\nList every TODO comment in the code.\nTODO comments name unfinished work.\n",
			"README.md": "# demo\n\nIt reads the TODO list you give it. Drafted by the Drafter, then rewritten by hand.\n",
			"LICENSE":   "Copyright [yyyy] [name of copyright owner]\n\nCopyright (c) 2026 Ada Lovelace\n",
		}, nil},
		{"a file the package does not ship is not read", map[string]string{
			"NOTES.md": "TODO write the instructions the agent follows.\n",
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := workspace(t)
			write(t, dir, c.files)
			got, err := UnfinishedIn(dir)
			if err != nil {
				t.Fatal(err)
			}
			if g := markers(got); !slices.Equal(g, c.want) && (len(g) > 0 || len(c.want) > 0) {
				t.Errorf("unfinished = %v, want %v", g, c.want)
			}
			for _, u := range got {
				if u.Message == "" {
					t.Errorf("%s:%s carries no message", u.File, u.Marker)
				}
			}
		})
	}
}

// mustRead is the base workspace's text of one file, for a case to edit.
func mustRead(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestWorkspaceRefusesUnfinishedOnlyWhenAsked: a scaffold line fails the
// check that asks for the refusal (`agentry validate`), and is listed but
// not counted against a check that does not (a draft's post-step); the
// report names the stages that ran and those that did not, either way.
func TestWorkspaceRefusesUnfinishedOnlyWhenAsked(t *testing.T) {
	dir := workspace(t)
	write(t, dir, map[string]string{"LICENSE": "PLACEHOLDER LICENSE NOTE\n"})

	for _, refuse := range []bool{false, true} {
		var out strings.Builder
		r, err := Workspace(dir, Options{RefuseUnfinished: refuse, Out: &out})
		if err != nil {
			t.Fatal(err)
		}
		if r.Passed == refuse || r.Counts.Unfinished != 1 || len(r.Unfinished) != 1 || r.RefuseUnfinished != refuse {
			t.Errorf("refuse=%v: passed=%v unfinished=%+v", refuse, r.Passed, r.Unfinished)
		}
		label := "NOTE "
		if refuse {
			label = "ERROR"
		}
		if !strings.Contains(out.String(), "unfinished: 1 scaffold line(s)") || !strings.Contains(out.String(), label+" LICENSE:1: placeholder licence note") {
			t.Errorf("refuse=%v narration:\n%s", refuse, out.String())
		}
		if len(r.Stages.Ran) != 4 || !slices.Contains(r.Stages.NotRun, "S5 AI Review") || !slices.Contains(r.Stages.NotRun, "S2 secrets") {
			t.Errorf("stages = %+v", r.Stages)
		}
		if strings.Contains(out.String(), "clean") {
			t.Errorf("the narration says clean:\n%s", out.String())
		}
	}

	var block strings.Builder
	r, _ := Workspace(workspace(t), Options{RefuseUnfinished: true})
	PrintStages(&block, r)
	for _, want := range []string{"ran here:     manifest rules ", "S1 inventory", "S3 hidden content", "S4 injection signatures (bundle ",
		"not run here: S2 secrets; S5 AI Review; S6 support scripts and undeclared tools; S7 declared MCP servers; S8 publisher signals; S9 the diff against the previous version",
		"its verdict decides"} {
		if !strings.Contains(block.String(), want) {
			t.Errorf("stage block lacks %q:\n%s", want, block.String())
		}
	}
}
