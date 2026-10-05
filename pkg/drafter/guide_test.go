package drafter

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

// guideDir is the drafting guide in this package's tree, the directory the
// embed reads.
var guideDir = filepath.Join(manifest.KnowledgeDir, GuideName)

// update rewrites the guide's generated files, as the repo's golden tests
// do (-update).
var update = flag.Bool("update", false, "rewrite the drafting guide's generated files")

// updateGuide is the command that rewrites the guide's generated files.
const updateGuide = "go test ./pkg/drafter -run TestTheDraftingGuideIsCurrent -update"

// runtimeFacts is sources/runtime-facts.md: the defaults the guide's pages
// state (plan drafter-knowledge.md D6; agentry-cli#657). The values the
// code holds as exported constants (the runtime images and versions, the
// server cap, the container limits, the base commands) are read from them,
// so those cannot drift from the runner. The install flags, port 443 and
// musl are written here as the runner's code writes them inline; a change
// there must change this text, and the page tests below hold the pages to
// it.
func runtimeFacts() []byte {
	var b bytes.Buffer
	b.WriteString("# Runtime facts\n\n")
	b.WriteString("Generated from the Agentry CLI's code by its tests; do not edit by hand. These are the defaults a runner uses unless its operator's policy names others.\n\n")
	b.WriteString("## Declared MCP servers\n\n")
	fmt.Fprintf(&b, "- npm servers install and run on `%s`: Node %s.\n", policy.DefaultNodeRuntime, policy.DefaultNodeVersion)
	fmt.Fprintf(&b, "- PyPI servers install and run on `%s`: Python %s.\n", policy.DefaultPythonRuntime, policy.DefaultPythonVersion)
	b.WriteString("- Both images are Alpine Linux, whose C library is musl.\n")
	b.WriteString("- npm installs run with `--ignore-scripts`; PyPI installs take wheels only (`--only-binary=:all:`), for the server and every dependency.\n")
	fmt.Fprintf(&b, "- A QuickAgent declares at most %d MCP servers.\n", manifest.MaxMCPServers)
	b.WriteString("- A server's declared hosts are reached on TCP port 443 only; with none declared it has no network.\n\n")
	b.WriteString("## Each container's limits\n\n")
	fmt.Fprintf(&b, "- Memory %s and %d CPU; on Linux, also at most %d processes.\n\n", policy.DefaultMemory, policy.DefaultCPUs, policy.DefaultPids)
	b.WriteString("## The base commands\n\n")
	b.WriteString("Every runner provides these to a QuickAgent that declares them in `exec`, with no entry in the operator's catalogue:\n\n")
	b.WriteString(strings.Join(policy.BaseExecCommands(), ", ") + ".\n")
	return b.Bytes()
}

// guideManifest is the manifest.json the guide's tree makes.
func guideManifest(t *testing.T, dir string) []byte {
	t.Helper()
	d, err := knowledge.TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	pages, err := knowledge.Pages(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(knowledge.Manifest{Schema: knowledge.Schema, Name: GuideName, Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestTheDraftingGuideIsCurrent holds the guide's generated files to what
// its pages and the code make: the runtime facts, the index, and
// manifest.json, in that order, since each feeds the next. With -update it
// writes them instead.
func TestTheDraftingGuideIsCurrent(t *testing.T) {
	check := func(rel string, want []byte) {
		t.Helper()
		p := filepath.Join(guideDir, filepath.FromSlash(rel))
		if *update {
			if err := os.WriteFile(p, want, 0o644); err != nil { //nolint:gosec // a page of the guide
				t.Fatal(err)
			}
			return
		}
		got, err := os.ReadFile(p) //nolint:gosec // inside the package
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s is stale or missing; run: %s", rel, updateGuide)
		}
	}
	check(knowledge.SourcesDir+"/runtime-facts.md", runtimeFacts())
	index, err := knowledge.GenerateIndex(guideDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range index {
		check(rel, b)
	}
	check(knowledge.ManifestFile, guideManifest(t, guideDir))
}

// TestTheDraftingGuideLoadsFromTheEmbeddedTree: the bytes the CLI embeds,
// laid down as a runner lays a built-in down, load as a pack of the CLI's
// format under its name, within the cap a shipped pack is held to (V8),
// and pass the pack lint (frontmatter, links, citations, the index).
func TestTheDraftingGuideLoadsFromTheEmbeddedTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, FS()); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(dir, guideDir)
	p, err := knowledge.Load(packDir)
	if err != nil {
		t.Fatalf("the embedded guide does not load: %v (run: %s)", err, updateGuide)
	}
	if p.Manifest.Name != GuideName || p.Manifest.Pages < 3 {
		t.Fatalf("manifest %+v", p.Manifest)
	}
	raw, err := os.ReadFile(filepath.Join(packDir, knowledge.ManifestFile)) //nolint:gosec // the test's own copy
	if err != nil {
		t.Fatal(err)
	}
	if size := p.Manifest.Bytes + int64(len(raw)); size > knowledge.MaxBytes {
		t.Fatalf("the guide is %d bytes; a shipped pack is at most %d", size, knowledge.MaxBytes)
	}
	findings, err := knowledge.Lint(packDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("lint: %s", f)
	}
}

// TestTheDrafterShipsTheGuide: the manifest declares the pack, publish
// takes every file of it, and it is the package's only shipped pack.
func TestTheDrafterShipsTheGuide(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ShippedPacks(); len(got) != 1 || got[0] != GuideName {
		t.Fatalf("ships %v, want [%s]", got, GuideName)
	}
	var files []manifest.FileInfo
	if err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, manifest.FileInfo{Path: p})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	included := map[string]bool{}
	for _, f := range manifest.ResolveInclude(m.Publish.Include, files) {
		included[f.Path] = true
	}
	for _, f := range files {
		if !included[f.Path] {
			t.Errorf("%s is embedded but publish.include leaves it out", f.Path)
		}
	}
	if !included[guideDir+"/"+knowledge.ManifestFile] {
		t.Fatalf("publish.include leaves the guide's manifest out")
	}
}

// TestTheDraftingGuideStatesTheRuntime: the servers page states the
// values the code owns, and the guide's pages name the pack's own pages
// by the paths they have.
func TestTheDraftingGuideStatesTheRuntime(t *testing.T) {
	page, err := fs.ReadFile(FS(), guideDir+"/wiki/servers/mcp-servers.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Node " + policy.DefaultNodeVersion,
		"Python " + policy.DefaultPythonVersion,
		"Node " + policy.DefaultNodeVersion + " or Python " + policy.DefaultPythonVersion,
		"port 443",
		"--ignore-scripts",
		"musllinux",
		"At most " + numberWord(manifest.MaxMCPServers) + " servers",
	} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the servers page does not say %q", want)
		}
	}
	run, err := fs.ReadFile(FS(), guideDir+"/wiki/runtime/how-a-run-works.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Up to " + numberWord(manifest.MaxMCPServers) + " npm or PyPI packages"; !strings.Contains(string(run), want) {
		t.Errorf("how a run works does not say %q", want)
	}
}

// numberWord is n as the guide writes a small number.
func numberWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

// TestTheInstructionNamesTheGuide: AGENT.md names the pack and sends the
// Drafter to the servers page before it pins a server.
func TestTheInstructionNamesTheGuide(t *testing.T) {
	b, err := fs.ReadFile(FS(), "AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`" + GuideName + "`", "servers/mcp-servers.md"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("AGENT.md does not name %q", want)
		}
	}
}

// TestAPersonsPackNamedAsTheGuideIsRefusedFirst: CheckKnowledgeSize, which
// every client calls before anything is scanned, refuses a person's pack
// that takes the guide's name, so no AI Review request is spent on a draft
// the runner would refuse at mount.
func TestAPersonsPackNamedAsTheGuideIsRefusedFirst(t *testing.T) {
	ok := knowledgetest.Make(t, "runbooks", nil)
	if err := CheckKnowledgeSize([]*knowledge.Pack{ok}); err != nil {
		t.Fatalf("an ordinary pack: %v", err)
	}
	impostor := knowledgetest.Make(t, GuideName, nil)
	err := CheckKnowledgeSize([]*knowledge.Pack{ok, impostor})
	var ce *engine.ClassifiedError
	if !errors.Is(err, ErrKnowledgeNameTaken) || !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
		t.Fatalf("a pack named %s: %v", GuideName, err)
	}
}

// TestADraftRecordsTheGuideItHad: draft.json's drafter block names the
// guide with its manifest's digest, apart from the person's packs, whether
// the answer was a draft (Finish) or not (Unparsed).
func TestADraftRecordsTheGuideItHad(t *testing.T) {
	raw, err := fs.ReadFile(FS(), guideDir+"/"+knowledge.ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	var m knowledge.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := GuideRead{Name: GuideName, Digest: m.SHA256, Pages: m.Pages, Bytes: m.Bytes}
	g := guideRead()
	if g == nil || *g != want || !strings.HasPrefix(g.Digest, "sha256:") {
		t.Fatalf("guide %+v, want %+v", g, want)
	}
	env := &receipt.Envelope{RunID: "r1"}
	d := Unparsed(env, t.TempDir(), webscan.Result{}, "agentry/drafter@"+Version, "test", errors.New("not json"))
	if d.Drafter == nil || d.Drafter.Guide == nil || *d.Drafter.Guide != want || len(d.Knowledge) != 0 {
		t.Fatalf("drafter %+v knowledge %v", d.Drafter, d.Knowledge)
	}
	out, err := json.Marshal(d.Drafter)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"guide":{"name":"`+GuideName+`","digest":"`+m.SHA256+`"`) {
		t.Fatalf("draft.json's drafter block %s", out)
	}
}
