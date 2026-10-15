// The traversal corpus: every escape
// shape we know — dot-dot, absolute, backslash, and symlink aliases — must
// be refused by resolveUnder, and the paths-mode grant matcher must not be
// fooled by prefixes or un-cleaned input.

package toolsrv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveUnder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		rel     string
		want    string // relative to root; ignored when wantErr
		wantErr string // substring of the error
	}{
		{name: "plain file", rel: "a/b.txt", want: "a/b.txt"},
		{name: "root itself", rel: ".", want: "."},
		{name: "dot segments collapse", rel: "a/./b.txt", want: "a/b.txt"},
		{name: "internal dotdot stays inside", rel: "a/../a/b.txt", want: "a/b.txt"},
		{name: "nonexistent target ok", rel: "new/dir/file.txt", want: "new/dir/file.txt"},
		{name: "empty", rel: "", wantErr: "empty path"},
		{name: "parent escape", rel: "../x", wantErr: "escapes the tool root"},
		{name: "nested escape", rel: "a/../../x", wantErr: "escapes the tool root"},
		{name: "absolute", rel: "/etc/passwd", wantErr: "absolute"},
		{name: "backslash", rel: `a\b`, wantErr: "backslash"},
		{name: "bare dotdot", rel: "..", wantErr: "escapes the tool root"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUnder(root, tc.rel)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveUnder(%q) = %q, want error containing %q", tc.rel, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveUnder(%q) error = %v, want it to contain %q", tc.rel, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveUnder(%q): %v", tc.rel, err)
			}
			want := filepath.Join(root, filepath.FromSlash(tc.want))
			if got != want {
				t.Fatalf("resolveUnder(%q) = %q, want %q", tc.rel, got, want)
			}
		})
	}
}

func TestResolveUnderSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dir link and a file link out of the root, and a dir link that
	// stays inside it.
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{name: "through escaping dir link", rel: "escape-dir/secret", wantErr: true},
		{name: "escaping file link", rel: "escape-file", wantErr: true},
		{name: "new file under escaping dir link", rel: "escape-dir/new/file", wantErr: true},
		{name: "link staying inside is fine", rel: "inside-link/b.txt", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveUnder(root, tc.rel)
			if tc.wantErr && err == nil {
				t.Fatalf("resolveUnder(%q) succeeded, want symlink-escape refusal", tc.rel)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("resolveUnder(%q): %v", tc.rel, err)
			}
		})
	}
}

func TestAllowedByGrant(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		rel   string
		want  bool
	}{
		{name: "empty grant allows all", paths: nil, rel: "anything/at/all", want: true},
		{name: "exact file", paths: []string{"a/b"}, rel: "a/b", want: true},
		{name: "under dir entry", paths: []string{"a/b"}, rel: "a/b/c", want: true},
		{name: "deeper under dir entry", paths: []string{"a/b"}, rel: "a/b/c/d.txt", want: true},
		{name: "parent of entry", paths: []string{"a/b"}, rel: "a", want: false},
		{name: "sibling prefix trap", paths: []string{"a/b"}, rel: "a/bc", want: false},
		{name: "unrelated", paths: []string{"a/b"}, rel: "z", want: false},
		{name: "second entry matches", paths: []string{"docs", "config.yaml"}, rel: "config.yaml", want: true},
		{name: "cleaned rel matches entry", paths: []string{"a/b"}, rel: "a/./b", want: true},
		{name: "cleaned rel escapes entry", paths: []string{"docs"}, rel: "docs/../secret", want: false},
		{name: "entry cleaned before compare", paths: []string{"docs/"}, rel: "docs/x.md", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowedByGrant(tc.paths, tc.rel); got != tc.want {
				t.Fatalf("allowedByGrant(%v, %q) = %v, want %v", tc.paths, tc.rel, got, tc.want)
			}
		})
	}
}
