package engine

import (
	"strings"
	"testing"
)

// The files AI Review reads of a source on a hosted run.
func TestIsMarkdownName(t *testing.T) {
	for name, want := range map[string]bool{
		"README.md": true, "docs/Guide.MARKDOWN": true, "site/page.mdx": true,
		"README": true, "sub/CONTRIBUTING": true, "readme": true,
		"README.txt": false, "main.go": false, "notes.txt": false, "LICENSE": false, "md": false,
	} {
		if got := isMarkdownName(name); got != want {
			t.Errorf("isMarkdownName(%q) = %v, want %v", name, got, want)
		}
	}

	// A source's Markdown file is counted once however many texts it has, and
	// its other files are left out; an upload's texts are all kept.
	t.Run("a source's Markdown is counted once a file", func(t *testing.T) {
		texts := []reviewText{
			{path: "up.txt"}, {path: "repo/README.md"}, {path: "repo/README.md", label: "(as Markdown)"}, {path: "repo/main.go"}, {path: "repo/docs/a.md"},
		}
		kept, counts := sourceReviewTexts(texts, []string{"repo"})
		if len(kept) != 4 || counts["repo"] != 2 {
			t.Errorf("kept %d texts and counted %v, want 4 texts and 2 Markdown files", len(kept), counts)
		}
		for _, k := range kept {
			if k.path == "repo/main.go" {
				t.Error("a source's code was kept for review")
			}
		}
	})
	t.Run("the refusal counts its files in the singular and the plural", func(t *testing.T) {
		for _, c := range []struct {
			n    int
			want string
		}{{1, "has 1 Markdown file for AI Review, and 1 of them do not fit"}, {3, "has 3 Markdown files for AI Review, and 1 of them do not fit"}} {
			err := sourceNotReviewed([]string{"repo/README.md"}, []string{"repo"}, map[string]int{"repo": c.n})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%d files: %v, want it to say %q", c.n, err, c.want)
			}
		}
		if err := sourceNotReviewed([]string{"upload.txt"}, []string{"repo"}, map[string]int{"repo": 1}); err != nil {
			t.Errorf("an upload withheld refused a source: %v", err)
		}
	})
}
