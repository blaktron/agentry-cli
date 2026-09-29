package localrun

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheBuiltinsLockHoldsEveryBuiltin (agentry-cli#470): every built-in is
// published to the registry from its embedded tree, and a registry version
// is never rebuilt, so a built-in whose tree changed must take a new
// version. builtins.lock records each version's archive digest; a tree that
// no longer packs to its version's digest fails here until the version moves.
func TestTheBuiltinsLockHoldsEveryBuiltin(t *testing.T) {
	lock := readBuiltinsLock(t)
	for _, b := range Builtins() {
		got, err := b.ArchiveDigest()
		if err != nil {
			t.Fatal(err)
		}
		want, ok := lock[b.Ref()]
		switch {
		case !ok:
			t.Errorf("%s is not in builtins.lock: append the line %q", b.Ref(), b.Ref()+" "+got)
		case want != got:
			t.Errorf("%s packs to %s, and builtins.lock holds %s for that version: its tree changed, so give it a new version (its agent.yaml and its package's Version constant) and append the new version's line; never edit a line", b.Ref(), got, want)
		}

		// The version the runner names is the one the manifest declares,
		// so the release the registry builds from the tree is this ref.
		dir := stageBuiltin(t, b)
		m, err := ManifestFrom(dir)
		if err != nil {
			t.Fatal(err)
		}
		if m.Namespace != b.Namespace() || m.Name != b.Name() || m.Version != b.Version() {
			t.Errorf("%s: its manifest declares %s/%s@%s", b.Ref(), m.Namespace, m.Name, m.Version)
		}
		// "The same bytes": the release carries every file the binary
		// embeds, so publish.include must leave none out.
		info, err := PackWorkspace(dir, m, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		packed := map[string]bool{}
		for _, f := range info.Files {
			packed[f.Path] = true
		}
		if err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !packed[p] {
				t.Errorf("%s embeds %s, and publish.include leaves it out of the release", b.Ref(), p)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// The registry's own rules for a public publish (V1-V9, V7's
		// LICENSE and README included), as `agentry validate` runs them: a
		// built-in the registry would refuse fails here, not at release.
		walked, err := pkgarchive.WalkWorkspace(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range manifest.ValidateInContext(m, manifest.Context{
			Namespace: b.Namespace(), Name: b.Name(), Public: true, Files: walked,
			ReadFile: func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))) },
		}) {
			if !manifest.IsWarning(issue) {
				t.Errorf("%s: the registry would refuse it: %v", b.Ref(), issue)
			}
		}

		// The README says what the application grants beyond the manifest,
		// and the directory shows it first: embedded and published both.
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil || !slices.Contains(m.Publish.Include, "README.md") {
			t.Errorf("%s: README.md is not both embedded and in publish.include (%v)", b.Ref(), err)
		}
	}
}

func stageBuiltin(t *testing.T, b Builtin) string {
	t.Helper()
	dir := t.TempDir()
	if err := b.Stage(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// readBuiltinsLock parses builtins.lock: "ref digest" lines, # comments,
// each ref once.
func readBuiltinsLock(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("builtins.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lock := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.Contains(fields[0], "@") || !strings.HasPrefix(fields[1], "sha256:") {
			t.Fatalf("builtins.lock:%d: %q is not \"ns/name@version sha256:<hex>\"", n, line)
		}
		if _, dup := lock[fields[0]]; dup {
			t.Fatalf("builtins.lock:%d: %s appears twice", n, fields[0])
		}
		lock[fields[0]] = fields[1]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lock
}
