// The traversal corpus: every escape
// shape we know — dot-dot, absolute, backslash, and symlink aliases — must
// be refused by resolveUnder, and the paths-mode grant matcher must not be
// fooled by prefixes or un-cleaned input.

package toolsrv

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// TestUnderResolverAgreesWithResolveUnder: an underResolver answers every
// path of the traversal corpus as resolveUnder does, links in and out of
// the root included, with one resolver for the whole corpus, as a search
// uses one for all its targets.
func TestUnderResolverAgreesWithResolveUnder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	real := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"), []byte("s"))
	mustWrite(t, filepath.Join(outside, "deep", "secret"), []byte("s"))
	mustWrite(t, filepath.Join(real, "a", "b.txt"), []byte("x"))
	mustWrite(t, filepath.Join(real, "a", "c", "d.txt"), []byte("x"))
	for link, target := range map[string]string{
		"escape-dir":         outside,
		"escape-file":        filepath.Join(outside, "secret"),
		"inside-link":        filepath.Join(real, "a"),
		"inside-file":        filepath.Join(real, "a", "b.txt"),
		"a/c/up":             filepath.Join(real, "a"),
		"a/c/out":            filepath.Join(outside, "deep"),
		"a/dangling":         filepath.Join(real, "nothing-here"),
		"inside-link-to-out": filepath.Join(real, "escape-dir"),
	} {
		if err := os.Symlink(target, filepath.Join(real, link)); err != nil {
			t.Fatal(err)
		}
	}
	// The root itself behind a link, as /tmp is on macOS.
	root := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	rels := []string{
		"a/b.txt", ".", "a", "a/c", "a/c/d.txt", "a/./b.txt", "a/../a/b.txt", "new/dir/file.txt", "a/new.txt",
		"", "../x", "a/../../x", "/etc/passwd", `a\b`, "..",
		"escape-dir", "escape-dir/secret", "escape-file", "escape-dir/new/file",
		"inside-link", "inside-link/b.txt", "inside-link/c/d.txt", "inside-file",
		"a/c/up/b.txt", "a/c/up/c/up/c/d.txt", "a/c/out", "a/c/out/secret", "a/dangling", "a/dangling/x",
		"inside-link-to-out/secret", "inside-link/c/out/secret",
		"a/b.txt/x", "inside-file/x",
	}
	r := newUnderResolver(root+"/", osResolverCalls())
	for _, rel := range append(rels, rels...) {
		want, wantErr := resolveUnder(root, rel)
		got, err := r.resolve(rel)
		if (err == nil) != (wantErr == nil) || got != want {
			t.Errorf("%q: resolve = (%q, %v), resolveUnder = (%q, %v)", rel, got, err, want, wantErr)
		}
	}
	if _, err := newUnderResolver(filepath.Join(real, "no-root"), osResolverCalls()).resolve("a"); err == nil || !strings.Contains(err.Error(), "resolve tool root") {
		t.Errorf("a missing root: %v", err)
	}
}

// TestUnderResolverResolvesEachFolderOnce: a thousand files in a hundred
// folders cost one Lstat each, one each folder, and one root resolution.
func TestUnderResolverResolvesEachFolderOnce(t *testing.T) {
	root := t.TempDir()
	var rels []string
	for d := range 100 {
		for f := range 10 {
			rel := filepath.ToSlash(filepath.Join("pkg", strconv.Itoa(d), "file"+strconv.Itoa(f)+".go"))
			mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), []byte("x"))
			rels = append(rels, rel)
		}
	}
	calls, lstats, evals := countingCalls()
	r := newUnderResolver(root, calls)
	for _, rel := range rels {
		if _, err := r.resolve(rel); err != nil {
			t.Fatal(err)
		}
	}
	// 1000 files, 100 folders and "pkg".
	if *lstats != 1101 || *evals != 1 {
		t.Errorf("Lstat %d times and EvalSymlinks %d times, want 1101 and 1", *lstats, *evals)
	}
}

// countingCalls are the filesystem's calls, each counted.
func countingCalls() (calls resolverCalls, lstats, evals *int) {
	var l, e int
	real := osResolverCalls()
	return resolverCalls{
		lstat: func(p string) (os.FileInfo, error) { l++; return real.lstat(p) },
		eval:  func(p string) (string, error) { e++; return real.eval(p) },
	}, &l, &e
}
