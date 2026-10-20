package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/ship"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// shipFixture is a signed-in machine, a fake registry and a QuickAgent
// package in the namespace acme.
type shipFixture struct {
	reg *knowledgetest.Registry
	dir string
	kn  func(args ...string) (string, string, error)
}

func newShipFixture(t *testing.T, user string) *shipFixture {
	t.Helper()
	isolateEnv(t)
	reg := knowledgetest.NewRegistry(t)
	newInstall(t)
	signIn(t, reg, user)
	f := &shipFixture{reg: reg, dir: writeTree(t, skillFiles("demo", ""))}
	f.kn = func(args ...string) (string, string, error) {
		return runCLI(t, strings.NewReader(""), append(append([]string{"knowledge"}, args...), "--registry", reg.URL)...)
	}
	return f
}

// ship runs `knowledge ship` against the fixture's package.
func (f *shipFixture) ship(args ...string) (string, string, error) {
	return f.kn(append([]string{"ship", "--dir", f.dir}, args...)...)
}

// keep imports a hand-made pack under name and, with upload, keeps a copy
// in Agentry and lets its scan end.
func (f *shipFixture) keep(t *testing.T, name string, files map[string]string, upload bool) {
	t.Helper()
	dir := knowledgetest.MakeCapped(t, name, files, knowledge.MaxLocalBytes).Dir
	if _, _, err := f.kn("import", dir); err != nil {
		t.Fatal(err)
	}
	if upload {
		if _, _, err := f.kn("upload", name); err != nil {
			t.Fatal(err)
		}
		f.reg.Finish()
	}
}

// agentamlIssues are the problems the registry's rules find in the
// package: the manifest, the include set and the shipped packs' files.
func registryRefuses(t *testing.T, dir string) []error {
	t.Helper()
	m, err := readWorkspaceManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	walked, err := pkgarchive.WalkWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []error
	for _, issue := range manifest.ValidateInContext(m, manifest.Context{
		Namespace: m.Namespace, Name: m.Name, Files: walked,
		ReadFile: func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))) },
	}) {
		if !manifest.IsWarning(issue) {
			out = append(out, issue)
		}
	}
	return out
}

func shippedBy(t *testing.T, dir string) []string {
	t.Helper()
	m, err := readWorkspaceManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m.ShippedPacks()
}

func recordOf(t *testing.T, dir string) ship.Record {
	t.Helper()
	r, err := ship.ReadRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestKnowledgeShipLaysAStoredPackThatTheRegistryAccepts(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "runbooks", nil, true)

	stdout, stderr, err := f.ship("runbooks")
	if err != nil {
		t.Fatalf("%v\n%s%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "knowledge pack runbooks laid under knowledge/runbooks") {
		t.Errorf("stdout %q", stdout)
	}
	if !strings.Contains(stderr, "asks you to confirm each pack") || !strings.Contains(stderr, "agent.yaml: added runbooks to knowledge.ships") {
		t.Errorf("stderr %q", stderr)
	}
	if got := shippedBy(t, f.dir); !slices.Equal(got, []string{"runbooks"}) {
		t.Fatalf("ships %v", got)
	}
	if issues := registryRefuses(t, f.dir); len(issues) > 0 {
		t.Fatalf("the registry would refuse the package: %v", issues)
	}
	// The origin is the pack's copy in Agentry, and lives in the sidecar,
	// never in agent.yaml.
	out, _, err := runCLI(t, nil, "--json", "knowledge", "list", "--registry", f.reg.URL)
	var kept []knowledgeEntryJSON
	if err != nil || json.Unmarshal([]byte(out), &kept) != nil || len(kept) != 1 || kept[0].Agentry == nil {
		t.Fatalf("list: %q %v", out, err)
	}
	e, ok := recordOf(t, f.dir).Find("runbooks")
	if !ok || e.Origin == nil || *e.Origin != (ship.Origin{Kind: ship.KindOwn, PackID: kept[0].Agentry.ID}) || e.Registry != f.reg.URL {
		t.Errorf("sidecar %+v", e)
	}
	yml, _ := os.ReadFile(filepath.Join(f.dir, "agent.yaml"))
	if strings.Contains(string(yml), kept[0].Agentry.ID) || strings.Contains(string(yml), "origin") {
		t.Errorf("the origin reached agent.yaml:\n%s", yml)
	}
}

func TestKnowledgeShipRefreshesAShippedPack(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "runbooks", nil, false)
	if _, _, err := f.ship("runbooks"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(ship.Dir(f.dir, "runbooks"), "manifest.json"))

	if _, _, err := f.kn("delete", "runbooks", "--yes"); err != nil {
		t.Fatal(err)
	}
	f.keep(t, "runbooks", map[string]string{
		"wiki/topics/new.md": "---\ntitle: New\ntype: topic\nsummary: New facts\nsources: [doc:n.txt]\nupdated: 2026-09-25\n---\n\nNew [src: doc:n.txt].\n",
		"sources/n.txt":      "n\n",
	}, false)
	if _, stderr, err := f.ship("runbooks"); err != nil || strings.Contains(stderr, "added runbooks to knowledge.ships") {
		t.Fatalf("a second run: %v %s", err, stderr)
	}
	second, _ := os.ReadFile(filepath.Join(ship.Dir(f.dir, "runbooks"), "manifest.json"))
	if string(first) == string(second) {
		t.Error("the shipped pack is still the earlier content")
	}
	if _, err := os.Stat(filepath.Join(ship.Dir(f.dir, "runbooks"), "wiki", "topics", "new.md")); err != nil {
		t.Errorf("the refreshed pack lacks the new page: %v", err)
	}
	if got := shippedBy(t, f.dir); !slices.Equal(got, []string{"runbooks"}) {
		t.Errorf("ships %v", got)
	}
	if issues := registryRefuses(t, f.dir); len(issues) > 0 {
		t.Fatalf("the registry would refuse the package: %v", issues)
	}
}

func TestKnowledgeShipRecordsNoOriginWhereThereIsNoCopyInAgentry(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "local-only", nil, false)
	_, stderr, err := f.ship("local-only")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "kept only on this computer") {
		t.Errorf("stderr %q", stderr)
	}
	if e, ok := recordOf(t, f.dir).Find("local-only"); !ok || e.Origin != nil {
		t.Errorf("sidecar %+v %v", e, ok)
	}
	// Changed here since its upload: no claim either.
	f.keep(t, "uploaded", nil, true)
	if _, _, err := f.ship("uploaded"); err != nil {
		t.Fatal(err)
	}
	if e, _ := recordOf(t, f.dir).Find("uploaded"); e.Origin == nil {
		t.Fatalf("an unchanged copy in Agentry has no origin: %+v", e)
	}
}

func TestKnowledgeShipDownloadsYourPackFromAgentry(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "runbooks", nil, true)
	if _, _, err := f.kn("delete", "runbooks", "--yes"); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := f.ship("runbooks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "downloading your copy") {
		t.Errorf("stderr %q", stderr)
	}
	if e, ok := recordOf(t, f.dir).Find("runbooks"); !ok || e.Origin == nil || e.Origin.Kind != ship.KindOwn {
		t.Errorf("sidecar %+v", e)
	}
	// A name neither kept nor in Agentry is a usage error that says so.
	if _, _, err := f.ship("nothing"); err == nil || !strings.Contains(err.Error(), "kept here or in Agentry") {
		t.Fatalf("a pack that does not exist: %v", err)
	} else {
		wantClassified(t, err, engine.ExitUsage)
	}
}

func TestKnowledgeShipRefusals(t *testing.T) {
	f := newShipFixture(t, "alice")

	t.Run("a sixth pack, while a shipped pack can still be refreshed", func(t *testing.T) {
		for i := 0; i < ship.MaxPacks; i++ {
			name := "pack" + string(rune('a'+i))
			f.keep(t, name, nil, false)
			if _, _, err := f.ship(name); err != nil {
				t.Fatal(err)
			}
		}
		f.keep(t, "sixth", nil, false)
		_, _, err := f.ship("sixth")
		if err == nil || !strings.Contains(err.Error(), "at most") && !strings.Contains(err.Error(), "the most a release ships") {
			t.Fatalf("a sixth pack: %v", err)
		}
		wantClassified(t, err, engine.ExitUsage)
		if _, statErr := os.Stat(ship.Dir(f.dir, "sixth")); !os.IsNotExist(statErr) {
			t.Error("the refused pack was laid")
		}
		if got := shippedBy(t, f.dir); len(got) != ship.MaxPacks {
			t.Errorf("ships %v", got)
		}
		if _, _, err := f.ship("packa"); err != nil {
			t.Errorf("refreshing a shipped pack at the cap: %v", err)
		}
	})

	// A pack kept on this computer may hold up to 1 GB, so one past what a
	// release ships can be kept; its files are bounded by the store too, so
	// the 2,000-file cap is held in the ship package's own tests.
	t.Run("a pack past 10 MB", func(t *testing.T) {
		g := newShipFixture(t, "alice")
		g.keep(t, "huge", map[string]string{
			"wiki/topics/p.md":  knowledgetest.DefaultFiles["wiki/topics/buckets.md"],
			"sources/guide.txt": strings.Repeat("0123456789abcdef", 11<<16),
		}, false)
		_, _, err := g.ship("huge")
		if err == nil || !strings.Contains(err.Error(), "a release ships at most 10.0 MiB") {
			t.Fatalf("an 11 MiB pack: %v", err)
		}
		wantClassified(t, err, engine.ExitUsage)
		if _, statErr := os.Stat(filepath.Join(g.dir, "knowledge")); !os.IsNotExist(statErr) {
			t.Error("a refused pack left knowledge/ behind")
		}
	})

	t.Run("a directory that is no package", func(t *testing.T) {
		if _, _, err := f.kn("ship", "runbooks", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no QuickAgent package") {
			t.Fatalf("err = %v", err)
		} else {
			wantClassified(t, err, engine.ExitUsage)
		}
	})
	t.Run("a name that is no pack name", func(t *testing.T) {
		if _, _, err := f.ship("../x"); err == nil {
			t.Fatal("a path was taken for a pack")
		}
	})
}

func TestKnowledgeShipOrganizationPacks(t *testing.T) {
	f := newShipFixture(t, "alice")
	id := f.reg.SeedShared(t, "acme", "handbook", map[string]string{
		"sources/handbook.md": "# Handbook\nThe rota changes on Mondays.\n",
		"wiki/index.md":       "# Index\n\n- [Rota](topics/rota.md)\n",
		"wiki/log.md":         "# Log\n",
		"wiki/topics/rota.md": "---\ntitle: Rota\n---\nThe rota changes on Mondays. [src: doc:handbook.md]\n",
	})

	t.Run("a member who is not an administrator", func(t *testing.T) {
		f.reg.Join("acme", "alice", "member")
		_, _, err := f.ship("handbook", "--org", "acme")
		if err == nil || !strings.Contains(err.Error(), "only an administrator of acme") {
			t.Fatalf("err = %v", err)
		}
		wantClassified(t, err, engine.ExitUsage)
		// A member may even export it once an administrator marks it so;
		// that is not the right to ship it.
		f.reg.SetExportable("acme", id, true)
		if _, _, err := f.ship("handbook", "--org", "acme"); err == nil || !strings.Contains(err.Error(), "only an administrator of acme") {
			t.Fatalf("an exportable pack: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(f.dir, "knowledge")); !os.IsNotExist(statErr) {
			t.Error("a pack was laid for a member")
		}
	})

	t.Run("an administrator, into a package in another namespace", func(t *testing.T) {
		f.reg.Join("acme", "alice", "admin")
		other := writeTree(t, skillFiles("demo", ""))
		yml, _ := os.ReadFile(filepath.Join(other, "agent.yaml"))
		if err := os.WriteFile(filepath.Join(other, "agent.yaml"), []byte(strings.Replace(string(yml), "namespace: acme", "namespace: alice", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := f.kn("ship", "handbook", "--org", "acme", "--dir", other)
		if err == nil || !strings.Contains(err.Error(), "own namespace") {
			t.Fatalf("err = %v", err)
		}
		wantClassified(t, err, engine.ExitUsage)
		if _, statErr := os.Stat(filepath.Join(other, "knowledge")); !os.IsNotExist(statErr) {
			t.Error("a pack was laid into another namespace")
		}
	})

	t.Run("a pack the organization does not share", func(t *testing.T) {
		if _, _, err := f.ship("nothing", "--org", "acme"); err == nil || !strings.Contains(err.Error(), "shares no knowledge pack named nothing") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an administrator, into the organization's namespace", func(t *testing.T) {
		stdout, _, err := f.ship("handbook", "--org", "acme")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "knowledge pack handbook laid") {
			t.Errorf("stdout %q", stdout)
		}
		if issues := registryRefuses(t, f.dir); len(issues) > 0 {
			t.Fatalf("the registry would refuse the package: %v", issues)
		}
		e, ok := recordOf(t, f.dir).Find("handbook")
		if !ok || e.Origin == nil || *e.Origin != (ship.Origin{Kind: ship.KindOrg, Org: "acme", PackID: id}) {
			t.Errorf("sidecar %+v", e)
		}
	})

	t.Run("a kept copy of the organization's pack takes the same road", func(t *testing.T) {
		other := writeTree(t, skillFiles("demo", ""))
		f.reg.Join("acme", "alice", "member")
		if _, _, err := f.kn("download", "--org", "acme", "handbook"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.kn("ship", "handbook", "--dir", other); err == nil || !strings.Contains(err.Error(), "only an administrator of acme") {
			t.Fatalf("a member shipping a kept copy: %v", err)
		}
		f.reg.Join("acme", "alice", "admin")
		if _, _, err := f.kn("ship", "handbook", "--dir", other); err != nil {
			t.Fatal(err)
		}
		if e, _ := recordOf(t, other).Find("handbook"); e.Origin == nil || e.Origin.Kind != ship.KindOrg {
			t.Errorf("sidecar %+v", e)
		}
	})
}

func TestKnowledgeShipRemove(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "one", nil, true)
	f.keep(t, "two", nil, false)
	for _, n := range []string{"one", "two"} {
		if _, _, err := f.ship(n); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := f.ship("--remove", "one")
	if err != nil || !strings.Contains(out, "knowledge pack one taken out") {
		t.Fatalf("%q %v", out, err)
	}
	if got := shippedBy(t, f.dir); !slices.Equal(got, []string{"two"}) {
		t.Errorf("ships %v", got)
	}
	if _, err := os.Stat(ship.Dir(f.dir, "one")); !os.IsNotExist(err) {
		t.Error("the folder stayed")
	}
	if _, ok := recordOf(t, f.dir).Find("one"); ok {
		t.Error("the sidecar still records it")
	}
	if issues := registryRefuses(t, f.dir); len(issues) > 0 {
		t.Fatalf("%v", issues)
	}
	if _, _, err := f.ship("--remove", "two"); err != nil {
		t.Fatal(err)
	}
	yml, _ := os.ReadFile(filepath.Join(f.dir, "agent.yaml"))
	if strings.Contains(string(yml), "knowledge") {
		t.Errorf("agent.yaml still mentions knowledge:\n%s", yml)
	}
	for _, p := range []string{"knowledge", ".agentry"} {
		if _, err := os.Stat(filepath.Join(f.dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s stayed", p)
		}
	}
	if _, _, err := f.ship("--remove", "two"); err == nil || !strings.Contains(err.Error(), "does not ship") {
		t.Errorf("removing a pack that is not shipped: %v", err)
	}
	if _, _, err := f.ship("--remove", "two", "--org", "acme"); err == nil {
		t.Error("--org with --remove")
	}
}

func TestKnowledgeShipJSON(t *testing.T) {
	f := newShipFixture(t, "alice")
	f.keep(t, "runbooks", nil, true)
	out, _, err := runCLI(t, nil, "--json", "knowledge", "ship", "runbooks", "--dir", f.dir, "--registry", f.reg.URL)
	if err != nil {
		t.Fatal(err)
	}
	var got shipResult
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Name != "runbooks" || got.Origin == nil || got.Pages != 1 || !strings.HasPrefix(got.SHA256, "sha256:") {
		t.Fatalf("%q %+v %v", out, got, err)
	}
}
