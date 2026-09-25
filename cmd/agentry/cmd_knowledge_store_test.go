package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/build"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// testKeychain is one machine's OS keychain, in memory.
type testKeychain struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *testKeychain) Get(a string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.m[a]; ok {
		return v, nil
	}
	return "", store.ErrNoEntry
}

func (k *testKeychain) Set(a, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[a] = v
	return nil
}

func (k *testKeychain) Delete(a string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, a)
	return nil
}

// newInstall points the runner home and the keychain at a fresh machine.
func newInstall(t *testing.T) (home, policyPath string) {
	t.Helper()
	home, policyPath = isolateRunnerHome(t)
	kc := &testKeychain{m: map[string]string{}}
	prev := knowledgeKeychain
	t.Cleanup(func() { knowledgeKeychain = prev })
	knowledgeKeychain = func() store.Keychain { return kc }
	return home, policyPath
}

func signIn(t *testing.T, reg *knowledgetest.Registry, owner string) {
	t.Helper()
	t.Setenv(envToken, reg.Token(owner))
}

func signOut(t *testing.T) { t.Helper(); t.Setenv(envToken, "") }

// storeDirs lists the store's directories, private ones included.
func storeDirs(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, store.Dir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestKnowledgeStoreCommands(t *testing.T) {
	isolateEnv(t)
	reg := knowledgetest.NewRegistry(t)
	r := []string{"--registry", reg.URL}
	kn := func(args ...string) (string, string, error) {
		return runCLI(t, strings.NewReader(""), append(append([]string{"knowledge"}, args...), r...)...)
	}
	newInstall(t)
	signIn(t, reg, "alice")
	pack := handMadePack(t, "runbooks")

	out, _, err := kn("import", pack)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "runbooks kept on this computer") || !strings.Contains(out, "which Agentry holds") {
		t.Fatalf("import said: %q", out)
	}
	if _, _, err := kn("import", pack); err == nil {
		t.Fatal("a pack was kept twice")
	} else {
		wantClassified(t, err, engine.ExitUsage)
	}

	t.Run("list", func(t *testing.T) {
		out, _, err := kn("list")
		if err != nil || !strings.Contains(out, "runbooks") || !strings.Contains(out, "this computer") {
			t.Fatalf("list: %q %v", out, err)
		}
		out, _, err = runCLI(t, nil, "--json", "knowledge", "list", "--registry", reg.URL)
		if err != nil {
			t.Fatal(err)
		}
		var got []knowledgeEntryJSON
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0].Name != "runbooks" || got[0].Wrapper != store.WrapperRegistry {
			t.Fatalf("list --json: %q %v", out, err)
		}
	})
	t.Run("export asks first, and writes only into a new or empty directory", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "out")
		_, _, err := kn("export", "runbooks", "--to", dst)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatal("an unconfirmed export wrote")
		}
		out, _, err := kn("export", "runbooks", "--to", dst, "--yes")
		if err != nil || !strings.Contains(out, "exported unencrypted") {
			t.Fatalf("export: %q %v", out, err)
		}
		if _, err := knowledge.Load(dst); err != nil {
			t.Fatalf("the export is not a pack: %v", err)
		}
		full := t.TempDir()
		if err := os.WriteFile(filepath.Join(full, "mine.txt"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := kn("export", "runbooks", "--to", full, "--yes"); err == nil {
			t.Fatal("an export wrote into a directory that was not empty")
		}
		if got, _ := os.ReadFile(filepath.Join(full, "mine.txt")); string(got) != "mine" {
			t.Fatal("the export touched a file it was not given")
		}
	})
	t.Run("upload, then download on a fresh install", func(t *testing.T) {
		out, _, err := kn("upload", "runbooks")
		if err != nil || !strings.Contains(out, "uploaded to "+reg.URL) {
			t.Fatalf("upload: %q %v", out, err)
		}
		if out, _, _ := kn("list"); !strings.Contains(out, "this computer and Agentry") {
			t.Fatalf("list after the upload: %q", out)
		}
		home, _ := newInstall(t)
		out, _, err = kn("download", "runbooks")
		if err != nil || !strings.Contains(out, "downloaded and kept") {
			t.Fatalf("download: %q %v", out, err)
		}
		if dirs := storeDirs(t, home); len(dirs) != 1 || !strings.HasPrefix(dirs[0], "kp_") {
			t.Fatalf("the fresh install's store holds %v", dirs)
		}
	})
	t.Run("delete", func(t *testing.T) {
		// Kept in Agentry too: no question.
		out, stderr, err := kn("delete", "runbooks")
		if err != nil || !strings.Contains(out, "deleted from this computer") || !strings.Contains(stderr, "Agentry still keeps a copy") {
			t.Fatalf("delete: %q %q %v", out, stderr, err)
		}
		if _, _, err := kn("download", "runbooks"); err != nil {
			t.Fatal(err)
		}
		// Everywhere: asked first.
		_, _, err = kn("delete", "runbooks", "--remote")
		wantClassified(t, err, engine.ExitUsage)
		if _, _, err := kn("delete", "runbooks", "--remote", "--yes"); err != nil {
			t.Fatal(err)
		}
		if reg.Packs("alice") != 0 {
			t.Fatal("--remote left Agentry's copy")
		}
		if _, _, err := kn("delete", "runbooks", "--yes"); err == nil {
			t.Fatal("a deleted pack was deleted again")
		} else {
			wantClassified(t, err, engine.ExitUsage)
		}
	})
}

func TestKnowledgeSignedOutIsKeptUnderTheMachineKeyUntilSignIn(t *testing.T) {
	isolateEnv(t)
	reg := knowledgetest.NewRegistry(t)
	newInstall(t)
	signOut(t)
	out, _, err := runCLI(t, nil, "knowledge", "import", handMadePack(t, "early"), "--registry", reg.URL)
	if err != nil || !strings.Contains(out, "this computer's key") {
		t.Fatalf("import signed out: %q %v", out, err)
	}
	if len(reg.Requests) != 0 {
		t.Fatalf("a signed-out import reached the registry: %v", reg.Requests)
	}
	signIn(t, reg, "alice")
	_, stderr, err := runCLI(t, nil, "knowledge", "import", handMadePack(t, "later"), "--registry", reg.URL)
	if err != nil || !strings.Contains(stderr, "knowledge pack early: its key is now wrapped to your account") {
		t.Fatalf("the next signed-in command did not rewrap: %q %v", stderr, err)
	}
}

func TestRunKnowledgeByName(t *testing.T) {
	isolateEnv(t)
	reg := knowledgetest.NewRegistry(t)
	home, policyPath := newInstall(t)
	signIn(t, reg, "alice")
	if _, _, err := runCLI(t, nil, "knowledge", "import", handMadePack(t, "runbooks"), "--registry", reg.URL); err != nil {
		t.Fatal(err)
	}
	pkg := repoPath(t, "testdata/runner/pure-text")
	dry := func(extra ...string) (string, string, error) {
		args := append([]string{"run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath, "--registry", reg.URL}, extra...)
		return runCLI(t, strings.NewReader(""), args...)
	}
	without, _, err := dry()
	if err != nil {
		t.Fatal(err)
	}
	with, stderr, err := dry("--knowledge", "runbooks")
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if with != without {
		t.Fatal("attaching a kept pack changed the printed plan")
	}
	if !strings.Contains(stderr, "knowledge pack runbooks: 1 page") {
		t.Fatalf("stderr: %q", stderr)
	}
	for _, d := range storeDirs(t, home) {
		if strings.HasPrefix(d, ".") {
			t.Fatalf("the run left the pack in the clear in %s", d)
		}
	}
	t.Run("a pack not kept is a plan-time refusal naming it", func(t *testing.T) {
		_, _, err := dry("--knowledge", "nosuch")
		wantClassified(t, err, engine.ExitPolicy)
		if !strings.Contains(err.Error(), "nosuch") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a pack that will not open is a plan-time refusal naming it", func(t *testing.T) {
		// Another machine, the files carried, signed out: no cached key,
		// no registry to unwrap it.
		src := filepath.Join(home, store.Dir)
		other, _ := newInstall(t)
		copyDir(t, src, filepath.Join(other, store.Dir))
		signOut(t)
		_, _, err := dry("--knowledge", "runbooks")
		wantClassified(t, err, engine.ExitPolicy)
		if !strings.Contains(err.Error(), "runbooks") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestKnowledgeCreateAndAddKeepThePackSealed(t *testing.T) {
	isolateEnv(t)
	reg := knowledgetest.NewRegistry(t)
	home, policyPath := newInstall(t)
	signIn(t, reg, "alice")
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var builtIn []string
	prev := knowledgeBuilder
	t.Cleanup(func() { knowledgeBuilder = prev })
	knowledgeBuilder = func(_ *cobra.Command, dir, name string, _ knowledgeBuildOpts) (*build.Result, string, error) {
		builtIn = append(builtIn, dir)
		files := map[string]string{
			"wiki/topics/runbook.md": "---\ntitle: Runbook\ntype: topic\nsummary: What to do\nsources: [doc:notes.md]\nupdated: 2026-09-25\n---\n\nRestart it [src: doc:notes.md].\n",
			"sources/notes.md":       "# notes\n",
		}
		id := ""
		if name == "" { // add: one more page over the pack as it stands, which keeps its id
			files["wiki/topics/more.md"] = "---\ntitle: More\ntype: topic\nsummary: More\nsources: [doc:notes.md]\nupdated: 2026-09-25\n---\n\nMore [src: doc:notes.md].\n"
			p, err := knowledge.Load(dir)
			if err != nil {
				return nil, "", err
			}
			name, id = p.Manifest.Name, p.Manifest.ID
		}
		made := knowledgetest.Make(t, name, files)
		if id != "" {
			made.Manifest.ID = id
			raw, _ := json.MarshalIndent(made.Manifest, "", "  ")
			if err := os.WriteFile(filepath.Join(made.Dir, knowledge.ManifestFile), raw, 0o600); err != nil {
				return nil, "", err
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, "", err
		}
		if err := copyTreeForTest(made.Dir, dir); err != nil {
			return nil, "", err
		}
		return &build.Result{Applied: &knowledge.Result{Manifest: made.Manifest}, Documents: []string{"notes.md"}}, t.TempDir(), nil
	}

	out, _, err := runCLI(t, nil, "knowledge", "create", "runbooks", "--file", doc, "--policy", policyPath, "--registry", reg.URL)
	if err != nil || !strings.Contains(out, "kept on this computer, encrypted to your account's key") {
		t.Fatalf("create: %q %v", out, err)
	}
	if len(builtIn) != 1 || !strings.HasPrefix(builtIn[0], filepath.Join(home, "tmp")) {
		t.Fatalf("built in %v, not a private directory under the runner home", builtIn)
	}
	if _, err := os.Stat(builtIn[0]); !os.IsNotExist(err) {
		t.Fatal("the plain pack directory outlived the create")
	}
	if _, _, err := runCLI(t, nil, "knowledge", "create", "runbooks", "--file", doc, "--policy", policyPath, "--registry", reg.URL); err == nil {
		t.Fatal("a second create of a kept name went through")
	} else {
		wantClassified(t, err, engine.ExitUsage)
	}
	if _, _, err := runCLI(t, nil, "knowledge", "upload", "runbooks", "--registry", reg.URL); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runCLI(t, nil, "knowledge", "add", "runbooks", "--file", doc, "--policy", policyPath, "--registry", reg.URL)
	if err != nil || !strings.Contains(out, "2 pages") || !strings.Contains(stderr, "upload runbooks") {
		t.Fatalf("add: %q %q %v", out, stderr, err)
	}
	if _, err := os.Stat(builtIn[1]); !os.IsNotExist(err) {
		t.Fatal("the opened pack outlived the add")
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyTreeForTest(src, dst); err != nil {
		t.Fatal(err)
	}
}

func copyTreeForTest(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}
