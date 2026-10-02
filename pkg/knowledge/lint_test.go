package knowledge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// page builds a page with the knowledge/v1 frontmatter and the given body.
func page(title, summary, body string) string {
	return "---\ntitle: " + title + "\ntype: topic\nsummary: " + summary +
		"\nsources: [doc:guide.txt]\nupdated: 2026-09-25\n---\n\n# " + title + "\n\n" + body
}

// goodPack is a tree that passes the lint: two pages that link each other
// and cite a source, the generated index, a log.
func goodPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"wiki/log.md":             "# Log\n",
		"wiki/topics/buckets.md":  page("Buckets", "Naming and versioning", "See [policies](policies.md).\nNames are unique [src: doc:guide.txt §2].\n"),
		"wiki/topics/policies.md": page("Policies", "Who may do what", "Back to [buckets](../topics/buckets.md#naming).\nA policy is JSON [src: doc:guide.txt; doc:notes/iam.md].\nSee also [the docs](https://docs.example.com/x) and [a note](#top).\n"),
		"sources/guide.txt":       "the guide\n",
		"sources/notes/iam.md":    "iam notes\n",
	})
	regenerate(t, root, "aws-docs")
	return root
}

func regenerate(t *testing.T, root, name string) {
	t.Helper()
	files, err := GenerateIndex(root, name)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range files {
		write(t, root, map[string]string{rel: string(b)})
	}
}

func lintOf(t *testing.T, root string) []Finding {
	t.Helper()
	findings, err := Lint(root, "aws-docs")
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func TestLintPassesAGoodPack(t *testing.T) {
	if fs := lintOf(t, goodPack(t)); len(fs) != 0 {
		t.Fatalf("findings on a good pack: %v", fs)
	}
}

func TestLintFindings(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		// want is "<page>|<rule>|<line>|<message fragment>"
		want string
	}{
		{name: "no frontmatter", files: map[string]string{"wiki/topics/bare.md": "# Bare\n"},
			want: "wiki/topics/bare.md|frontmatter|1|no frontmatter"},
		{name: "a missing key", files: map[string]string{"wiki/topics/nosum.md": "---\ntitle: X\ntype: topic\nsources: [doc:guide.txt]\nupdated: 2026-09-25\n---\n"},
			want: "wiki/topics/nosum.md|frontmatter|1|summary"},
		{name: "a malformed date", files: map[string]string{"wiki/topics/date.md": "---\ntitle: X\ntype: topic\nsummary: s\nsources: [doc:guide.txt]\nupdated: yesterday\n---\n"},
			want: "wiki/topics/date.md|frontmatter|1|updated"},
		{name: "an uppercase slug", files: map[string]string{"wiki/topics/Big.md": page("Big", "s", "x\n")},
			want: "wiki/topics/Big.md|slug|0|slug"},
		{name: "a page outside a section", files: map[string]string{"wiki/loose.md": page("Loose", "s", "x\n")},
			want: "wiki/loose.md|slug|0|wiki/<section>/<slug>.md"},
		{name: "one slug in two sections", files: map[string]string{"wiki/procedures/buckets.md": page("Buckets again", "s", "x\n")},
			want: "wiki/topics/buckets.md|slug|0|also wiki/procedures/buckets.md"},
		{name: "a broken link", files: map[string]string{"wiki/topics/links.md": page("Links", "s", "one\n[gone](gone.md)\n")},
			want: "wiki/topics/links.md|link|12|gone.md"},
		{name: "a link out of the pack", files: map[string]string{"wiki/topics/out.md": page("Out", "s", "[x](../../../etc/passwd)\n")},
			want: "wiki/topics/out.md|link|11|outside the pack"},
		{name: "a citation to no source", files: map[string]string{"wiki/topics/cite.md": page("Cite", "s", "claim [src: doc:missing.pdf]\n")},
			want: "wiki/topics/cite.md|citation|11|doc:missing.pdf"},
		{name: "a citation key of another kind", files: map[string]string{"wiki/topics/key.md": page("Key", "s", "claim [src: url:https://x.test]\n")},
			want: "wiki/topics/key.md|citation|11|doc:<path>"},
		{name: "a web citation that is not an address", files: map[string]string{"wiki/topics/web.md": page("Web", "s", "claim [src: web:ftp://x.test/a]\n")},
			want: "wiki/topics/web.md|citation|11|web:<an http or https address>"},
		{name: "a web citation with no host", files: map[string]string{"wiki/topics/web2.md": page("Web two", "s", "claim [src: web:https:/nohost]\n")},
			want: "wiki/topics/web2.md|citation|11|web:<an http or https address>"},
		{name: "a web citation carrying a login", files: map[string]string{"wiki/topics/web3.md": page("Web three", "s", "claim [src: web:https://u:p@x.test/]\n")},
			want: "wiki/topics/web3.md|citation|11|web:<an http or https address>"},
		{name: "a model citation with something after it", files: map[string]string{"wiki/topics/mod.md": page("Mod", "s", "claim [src: models]\n")},
			want: "wiki/topics/mod.md|citation|11|web:<url> or model"},
		{name: "a file that is not a page", files: map[string]string{"wiki/topics/pic.png": "\x89PNG"},
			want: "wiki/topics/pic.png|layout|0|only markdown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := goodPack(t)
			write(t, root, tc.files)
			regenerate(t, root, "aws-docs")
			parts := strings.SplitN(tc.want, "|", 4)
			var got []string
			for _, f := range lintOf(t, root) {
				got = append(got, f.String())
				if f.Page == parts[0] && string(f.Rule) == parts[1] && strconv.Itoa(f.Line) == parts[2] && strings.Contains(f.Message, parts[3]) {
					return
				}
			}
			t.Fatalf("no finding %q among %v", tc.want, got)
		})
	}

	t.Run("web and model citations are declared keys", func(t *testing.T) {
		// agentry-cli#523 (plan knowledge-and-drafter.md D17): a page the
		// Creator read on the web, and what its model knew, name no source.
		root := goodPack(t)
		write(t, root, map[string]string{"wiki/topics/web.md": page("Web", "s",
			"A fact [src: web:https://docs.example.com/a?b=c#d].\nAnother [src: model].\nBoth [src: doc:guide.txt; web:http://x.test/; model].\n")})
		regenerate(t, root, "aws-docs")
		if fs := lintOf(t, root); len(fs) != 0 {
			t.Fatalf("findings: %v", fs)
		}
	})

	t.Run("links in code are not links", func(t *testing.T) {
		root := goodPack(t)
		write(t, root, map[string]string{"wiki/topics/code.md": page("Code", "s", "`[x](nope.md)`\n```\n[y](nope.md)\n```\n")})
		regenerate(t, root, "aws-docs")
		if fs := lintOf(t, root); len(fs) != 0 {
			t.Fatalf("findings: %v", fs)
		}
	})
	t.Run("an index that drifted", func(t *testing.T) {
		root := goodPack(t)
		write(t, root, map[string]string{"wiki/topics/new.md": page("New", "s", "x [src: doc:guide.txt]\n")})
		found := false
		for _, f := range lintOf(t, root) {
			if f.Page == "wiki/index.md" && f.Rule == RuleIndex {
				found = true
			}
		}
		if !found {
			t.Fatal("no index finding after a page was added without regenerating")
		}
	})
}

func TestGenerateIndex(t *testing.T) {
	root := goodPack(t)
	b, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ntitle: Index\ntype: index\nsummary: Every page in aws-docs, one line each, by section. Generated; do not edit by hand.\n---\n\n" +
		"# aws-docs\n\n2 pages.\n\n## Topics\n\n" +
		"- [Buckets](topics/buckets.md) — Naming and versioning\n" +
		"- [Policies](topics/policies.md) — Who may do what\n"
	if string(b) != want {
		t.Fatalf("index =\n%s\nwant\n%s", b, want)
	}
}

func TestGenerateIndexSplitsBySection(t *testing.T) {
	root := goodPack(t)
	write(t, root, map[string]string{"wiki/procedures/rotate.md": page("Rotate keys", "Two keys", "x\n")})
	files, err := generateIndex(root, "aws-docs", 2)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := "---\ntitle: Index\ntype: index\nsummary: Every section of aws-docs, with the pages each holds. Generated; do not edit by hand.\n---\n\n" +
		"# aws-docs\n\n3 pages in 2 sections; each section's index lists its pages.\n\n" +
		"- [Procedures](procedures/index.md) — 1 page\n" +
		"- [Topics](topics/index.md) — 2 pages\n"
	if got := string(files["wiki/index.md"]); got != wantRoot {
		t.Fatalf("root index =\n%s\nwant\n%s", got, wantRoot)
	}
	wantTopics := "---\ntitle: Topics\ntype: index\nsummary: Every page in the Topics section of aws-docs. Generated; do not edit by hand.\n---\n\n" +
		"# Topics\n\n2 pages. [All sections](../index.md).\n\n" +
		"- [Buckets](buckets.md) — Naming and versioning\n" +
		"- [Policies](policies.md) — Who may do what\n"
	if got := string(files["wiki/topics/index.md"]); got != wantTopics {
		t.Fatalf("topics index =\n%s\nwant\n%s", got, wantTopics)
	}
}
