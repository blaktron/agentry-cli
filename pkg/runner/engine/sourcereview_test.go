package engine

import "testing"

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
}
