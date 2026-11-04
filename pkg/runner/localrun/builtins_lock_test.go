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
	"testing/fstest"

	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheBuiltinsLockHoldsEveryBuiltin: every built-in is
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

		// The registry's own rules for a public publish (every manifest rule, V7's
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

	// A package whose publish.include matches no file is not packed, and one
	// whose include matches is.
	bare := writeTree(t, map[string]string{
		"agent.yaml": "schemaVersion: \"2\"\nname: x\nnamespace: acme\nversion: 1.0.0\nsummary: s\n",
		"README.md":  "r\n",
	})
	bm, err := ManifestFrom(bare)
	if err != nil {
		t.Fatal(err)
	}
	bm.Publish.Include = []string{"nothing-here.md"}
	if _, err := PackWorkspace(bare, bm, io.Discard); err == nil || !strings.Contains(err.Error(), "publish.include matches no files") {
		t.Errorf("an include that matches nothing: %v", err)
	}
	bm.Publish.Include = []string{"README.md"}
	if info, err := PackWorkspace(bare, bm, io.Discard); err != nil || len(info.Files) != 1 || info.Files[0].Path != "README.md" {
		t.Errorf("an include that matches README.md: %+v, %v", info.Files, err)
	}

	// A built-in is staged into an empty folder; one that holds its files already
	// is refused, naming the package.
	if err := Drafter().Stage(bare); err == nil || !strings.Contains(err.Error(), "stage "+Drafter().Ref()) {
		t.Errorf("staging into a folder that holds the package's files: %v", err)
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

// TestTheBuiltInSkillsAreSkills: each folder beneath a persistent
// built-in's skills/ is a skill of its own name that the bounds take, so
// none is left out of a roster for being malformed; the Coding agent and
// the Assistant have some, Main none.
func TestTheBuiltInSkillsAreSkills(t *testing.T) {
	for _, b := range PersistentBuiltins() {
		sub, err := fs.Sub(b.fsys, "skills")
		if err != nil {
			t.Fatal(err)
		}
		entries, _ := fs.ReadDir(sub, ".")
		if got := len(b.Skills()); got != len(entries) {
			t.Errorf("%s: %d of its %d skill folders are skills", b.Ref(), got, len(entries))
		}
		if (len(entries) > 0) != (b.Codes() || b.Declared().Declares("memory")) {
			t.Errorf("%s has %d skills", b.Ref(), len(entries))
		}
	}

	// A folder is a skill of its own name: a folder whose skill is named
	// otherwise, a folder with no skill file and a loose file are left out,
	// and the skills come in name order.
	skillFile := func(name string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: Use it when a test asks.\n---\nBody.\n")}
	}
	tree := fstest.MapFS{
		"skills/beta/SKILL.md":     skillFile("beta"),
		"skills/alpha/SKILL.md":    skillFile("alpha"),
		"skills/alpha/notes.md":    {Data: []byte("notes\n")},
		"skills/misnamed/SKILL.md": skillFile("other"),
		"skills/empty/notes.md":    {Data: []byte("notes\n")},
		"skills/loose.md":          {Data: []byte("loose\n")},
	}
	var names []string
	for _, s := range SkillsOf(tree) {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Errorf("the skills of the tree are %v, want alpha and beta", names)
	}
	if got := SkillsOf(fstest.MapFS{"agent.yaml": {Data: []byte("x")}}); len(got) != 0 {
		t.Errorf("a tree with no skills folder has skills %v", got)
	}
	if got := (Builtin{}).Skills(); len(got) != 0 {
		t.Errorf("the zero Builtin has skills %v", got)
	}
}
