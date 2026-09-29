package localrun

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
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
		m, err := ManifestFrom(stageBuiltin(t, b))
		if err != nil {
			t.Fatal(err)
		}
		if m.Namespace != b.Namespace() || m.Name != b.Name() || m.Version != b.Version() {
			t.Errorf("%s: its manifest declares %s/%s@%s", b.Ref(), m.Namespace, m.Name, m.Version)
		}
		if !slices.Contains(m.Publish.Include, "README.md") {
			t.Errorf("%s: publish.include leaves out README.md, which the directory shows first", b.Ref())
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
