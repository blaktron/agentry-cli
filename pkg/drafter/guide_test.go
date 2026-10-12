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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
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
// it. The fetch tool's response cap and timeout are the tools server's.
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
	b.WriteString("## Tool results\n\n")
	fmt.Fprintf(&b, "- Every result of the runner's own tools is held to %d KiB and %s lines, the harness's limit less a margin: a file is read a page at a time from the offset asked, each page saying where the next begins; a listing or a search stops at it and says what it left out.\n\n", toolsrv.DefaultMaxResultBytes>>10, thousands(toolsrv.DefaultMaxResultLines))
	b.WriteString("## The fetch tool\n\n")
	fmt.Fprintf(&b, "- `fetch` reads at most %d KiB of a response's body, and gives up on a request after %d seconds.\n", toolsrv.DefaultMaxFetchBytes>>10, int(toolsrv.FetchTimeout.Seconds()))
	b.WriteString("- A body larger than one tool result is saved in the scratch folder, under `fetch/`, and answered with its first page and where it was saved.\n")
	b.WriteString("- `saveTo` saves the whole body at a path in the scratch folder, whatever its type, binary included, and the answer says what was saved instead of returning it.\n\n")
	b.WriteString("## Input sources\n\n")
	fmt.Fprintf(&b, "- A `repository` question's repository is fetched by the runner before the run: at most %s files and %s on the person's computer, %s files and %s In Agentry.\n",
		thousands(sources.LocalCaps.Files), binarySize(sources.LocalCaps.Bytes), thousands(sources.HostedRepositoryCaps.Files), binarySize(sources.HostedRepositoryCaps.Bytes))
	fmt.Fprintf(&b, "- A `url` question's file is fetched the same way: one file of at most %s on the person's computer, %s In Agentry.\n\n", binarySize(sources.URLCaps.Bytes), binarySize(sources.HostedURLCaps.Bytes))
	b.WriteString("## Reports, PDFs and the scratch folder\n\n")
	b.WriteString("- `write_report` writes a text file into `reports/`, which the person receives. It refuses a PDF.\n")
	fmt.Fprintf(&b, "- `export_pdf` turns a Markdown file from `reports/` or the scratch folder, of at most %d KiB, into a PDF in `reports/`, rendered by Typst %s within %d seconds, at most %d MiB. It draws at most %d local PNG or JPEG images of up to %d MiB each; any other image is drawn as its alt text. The PDF's look is the person's report theme, which the run cannot choose.\n",
		toolsrv.MaxPDFSourceBytes>>10, toolsrv.TypstVersion, int(toolsrv.PDFRenderTimeout.Seconds()), toolsrv.MaxPDFBytes>>20, toolsrv.MaxPDFImages, toolsrv.MaxPDFImageBytes>>20)
	fmt.Fprintf(&b, "- The scratch folder, `%s`, is the run's own working space through `scratch_write`, `scratch_list`, `scratch_read` and `scratch_search`: empty at the start, at most %d MiB on Linux, never part of what the person receives, and deleted with the run.\n\n", spec.ScratchMount, spec.ToolsTmpfsBytes>>20)
	b.WriteString("## Each container's limits\n\n")
	fmt.Fprintf(&b, "- Memory %s and %d CPU; on Linux, also at most %d processes.\n\n", policy.DefaultMemory, policy.DefaultCPUs, policy.DefaultPids)
	b.WriteString("## The base commands\n\n")
	b.WriteString("Every runner provides these to a QuickAgent that declares them in `exec`, with no entry in the operator's catalogue:\n\n")
	b.WriteString(strings.Join(policy.BaseExecCommands(), ", ") + ".\n")
	return b.Bytes()
}

// serverListPages are the guide's two list pages, rendered from
// ServerListFile (plan drafter-knowledge.md §4.4; agentry-cli#658): each
// entry with what was observed, on which runtime, on which date, and
// never a word of safety or quality.
func serverListPages(t *testing.T) map[string][]byte {
	t.Helper()
	l, err := LoadServerList(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	byList := map[string][]ServerEntry{}
	for _, s := range l.Servers {
		byList[s.List] = append(byList[s.List], s)
	}
	pages := map[string][]byte{}
	for _, p := range []struct {
		list, file, title, summary, intro string
		deps                              bool
	}{
		{RunsHere, "runs-here.md", "Runs here",
			"Declared MCP servers seen to install, start and list their tools in the runner, each with the versions, the runtime, the date and what a QuickAgent must declare for it. Generated from servers.yaml; do not edit by hand.",
			"These servers were seen to install with the runner's own builder, start with no arguments through the runner's gateway, and list their tools, on the runtime and the date each entry gives. The survey's gateway ran outside a run, with the daemon's default runtime and without a run's Docker filter, so a run can still refuse what it started. That is all an entry says: it is not a judgement of a server's safety or quality, and a later version can behave differently. The list is one thing to look at when you choose a server, not a gate. A server on neither list may be pinned after the checks on [Declared MCP servers](mcp-servers.md), and a server here still needs them: its keys, its hosts, and whether its tools fit the task. An entry speaks for the version it gives; another version of the server is on neither list.", false},
		{DoesNotRunHere, "does-not-run-here.md", "Does not run here",
			"Declared MCP servers seen to fail in the runner, and packages whose presence as a direct dependency means a server cannot run, each with the reason, the runtime and the date. Generated from servers.yaml; do not edit by hand.",
			"These servers were seen to fail in the runner, or cannot run here for the reason each entry gives, on the runtime and the date it gives. Do not pin them: no setting of the QuickAgent makes them run. An entry comes off the list when the runner gains what the server lacked. A server not listed here can still fail; check it against [Declared MCP servers](mcp-servers.md) before you pin it. An entry speaks for every version of its server, unless its reason is " + orList(codeSpans(VersionBoundReasons())) + ", which hold for the version seen. The check after you answer sends a pin this page covers, or a server with a dependency listed at its end, to a repair turn.", true},
	} {
		entries := byList[p.list]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Ecosystem+":"+entries[i].Package < entries[j].Ecosystem+":"+entries[j].Package
		})
		var updated time.Time
		for _, e := range entries {
			if e.Date.After(updated) {
				updated = e.Date
			}
		}
		if p.deps {
			for _, d := range l.Dependencies {
				if d.Date.After(updated) {
					updated = d.Date
				}
			}
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "---\ntitle: %s\ntype: list\nsummary: %s\nsources: [doc:servers.yaml]\nupdated: %s\n---\n\n", p.title, p.summary, updated.Format(time.DateOnly))
		fmt.Fprintf(&b, "# %s\n\n%s [src: doc:servers.yaml]\n\n%d servers.\n", p.title, p.intro, len(entries))
		for _, e := range entries {
			fmt.Fprintf(&b, "\n## %s `%s`\n\n", ecosystemName(e.Ecosystem), e.Package)
			fmt.Fprintf(&b, "- **Versions:** %s, on %s.\n", e.Versions, e.Runtime)
			if e.Reason != "" {
				fmt.Fprintf(&b, "- **Why it does not run:** it %s (`%s`).\n", mustReason(t, e.Reason), e.Reason)
			}
			fmt.Fprintf(&b, "- **Seen:** %s [src: doc:servers.yaml]\n", e.Observed)
			if len(e.Tools) > 0 {
				fmt.Fprintf(&b, "- **Tools listed:** %s.\n", strings.Join(codeSpans(e.Tools), ", "))
			}
			for _, c := range e.Conditions {
				fmt.Fprintf(&b, "- **Needs:** %s\n", c)
			}
			fmt.Fprintf(&b, "- **Evidence:** %s.\n", evidenceOn(e.Evidence, e.Date))
		}
		if p.deps && len(l.Dependencies) > 0 {
			deps := slices.Clone(l.Dependencies)
			sort.Slice(deps, func(i, j int) bool {
				return deps[i].Ecosystem+":"+deps[i].Package < deps[j].Ecosystem+":"+deps[j].Package
			})
			b.WriteString("\n## Dependencies that mean a server does not run here\n\n")
			b.WriteString("A server whose own direct dependencies (npm's `dependencies`, PyPI's `requires_dist`) include one of these packages does not run here, whatever list the server itself is on [src: doc:servers.yaml].\n\n")
			for _, d := range deps {
				fmt.Fprintf(&b, "- %s `%s`: it %s (`%s`). %s (%s.)\n", ecosystemName(d.Ecosystem), d.Package, mustReason(t, d.Reason), d.Reason, d.Observed, evidenceOn(d.Evidence, d.Date))
			}
		}
		pages[knowledge.WikiDir+"/servers/"+p.file] = b.Bytes()
	}
	pages[knowledge.WikiDir+"/servers/sign-ins.md"] = signInsPage(t, l)
	return pages
}

// signInsPage renders the list's remote servers and package sign-ins
// (agentry-cli#667): what each vendor's server asks of whoever runs it,
// and the package sign-ins a draft may copy.
func signInsPage(t *testing.T, l *ServerList) []byte {
	t.Helper()
	var updated time.Time
	for _, r := range l.Remote {
		if r.Date.After(updated) {
			updated = r.Date
		}
	}
	for _, si := range l.SignIns {
		if si.Date.After(updated) {
			updated = si.Date
		}
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "---\ntitle: Remote servers and sign-ins\ntype: list\nsummary: Vendors' own remote MCP servers, by address and how each signs in, and the package servers' OAuth sign-ins seen working, which a draft copies exactly. Generated from servers.yaml; do not edit by hand.\nsources: [doc:servers.yaml]\nupdated: %s\n---\n\n", updated.Format(time.DateOnly))
	b.WriteString("# Remote servers and sign-ins\n\n")
	b.WriteString("An entry says what was seen, or what the vendor's own metadata said on its date. It is not a judgement of a vendor's safety or quality. How a remote server or a sign-in runs, and when to choose one, is on [Declared MCP servers](mcp-servers.md#remote-servers) [src: doc:servers.yaml].\n\n")
	fmt.Fprintf(&b, "## Remote servers\n\n%s. *Agentry signs in as itself* means the vendor lets Agentry register its own client; *the person brings their own client* means it does not, so whoever runs the QuickAgent makes an OAuth client at the vendor once, and the run setup shows how [src: doc:servers.yaml].\n", entriesWord(len(l.Remote)))
	remote := slices.Clone(l.Remote)
	sort.SliceStable(remote, func(i, j int) bool {
		return remote[i].Vendor+remote[i].URL+string(remote[i].Auth) < remote[j].Vendor+remote[j].URL+string(remote[j].Auth)
	})
	for _, r := range remote {
		fmt.Fprintf(&b, "\n### %s, `%s`\n\n", r.Vendor, r.URL)
		fmt.Fprintf(&b, "- **Signs in with:** `auth: %s`", r.Auth)
		switch r.Client {
		case ClientAgentry:
			b.WriteString("; Agentry signs in as itself.\n")
		case ClientOwn:
			b.WriteString("; the person brings their own client.\n")
		default:
			b.WriteString(".\n")
		}
		if len(r.Scopes) > 0 {
			fmt.Fprintf(&b, "- **Scopes it offers:** %s.\n", strings.Join(codeSpans(r.Scopes), ", "))
		}
		if r.Seen {
			fmt.Fprintf(&b, "- **Seen to run here:** %s [src: doc:servers.yaml]\n", r.Observed)
		} else {
			fmt.Fprintf(&b, "- **Not seen to run here.** %s [src: doc:servers.yaml]\n", r.Observed)
		}
		for _, c := range r.Conditions {
			fmt.Fprintf(&b, "- **Needs:** %s\n", c)
		}
		fmt.Fprintf(&b, "- **Evidence:** %s.\n", evidenceOn(r.Evidence, r.Date))
	}
	fmt.Fprintf(&b, "\n## Package sign-ins\n\n%s. A package's `oauth` block is written only by copying one of these exactly, with the egress and settings it lists: the check after you answer sends any other to a repair turn [src: doc:servers.yaml].\n", entriesWord(len(l.SignIns)))
	for _, si := range l.SignIns {
		fmt.Fprintf(&b, "\n### %s `%s` %s\n\n", ecosystemName(si.Ecosystem), si.Package, si.Versions)
		if si.Client == ClientOwn {
			b.WriteString("- **Its client:** the person brings their own client.\n")
		} else {
			b.WriteString("- **Its client:** Agentry signs in as itself.\n")
		}
		fmt.Fprintf(&b, "- **Seen:** %s [src: doc:servers.yaml]\n", si.Observed)
		fmt.Fprintf(&b, "- **Evidence:** %s.\n\n", evidenceOn(si.Evidence, si.Date))
		b.WriteString("Copy this entry into `capabilities.mcpServers`, with the server's tools, exactly as it stands:\n\n```yaml\n")
		// The manifest's own order: the package, its version, its hosts, its
		// settings, then the sign-in.
		entry := struct {
			NPM     string            `yaml:"npm,omitempty"`
			PyPI    string            `yaml:"pypi,omitempty"`
			Version string            `yaml:"version"`
			Egress  []string          `yaml:"egress,flow"`
			Env     map[string]string `yaml:"env,omitempty"`
			OAuth   manifest.MCPOAuth `yaml:"oauth"`
		}{Version: si.Versions, Egress: si.Egress, Env: si.Env, OAuth: si.OAuth}
		if si.Ecosystem == "npm" {
			entry.NPM = si.Package
		} else {
			entry.PyPI = si.Package
		}
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode([]any{entry}); err != nil {
			t.Fatal(err)
		}
		b.WriteString("```\n")
	}
	return b.Bytes()
}

func mustReason(t *testing.T, class string) string {
	t.Helper()
	s, ok := NotRunReason(class)
	if !ok {
		t.Fatalf("reason %q is not in the closed set", class)
	}
	return s
}

func ecosystemName(e string) string {
	if e == "pypi" {
		return "PyPI"
	}
	return e
}

func codeSpans(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
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
	for rel, b := range serverListPages(t) {
		check(rel, b)
	}
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
	if want := "Up to " + numberWord(manifest.MaxMCPServers) + " the manifest declares"; !strings.Contains(string(run), want) {
		t.Errorf("how a run works does not say %q", want)
	}
}

// instructionFile is the Drafter's own instruction, by its path in the
// package's tree; heldText reads any other path as a page of the guide.
const instructionFile = "AGENT.md"

// guideFile is a file the tests hold to the code (plan drafter-knowledge.md
// D6, D11; agentry-cli#659): the instruction, or a page of the guide by its
// path under the wiki (manifest/manifest.md), as the package embeds it.
func guideFile(t *testing.T, rel string) []byte {
	t.Helper()
	p := rel
	if rel != instructionFile {
		p = guideDir + "/" + knowledge.WikiDir + "/" + rel
	}
	b, err := fs.ReadFile(FS(), p)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return b
}

// heldText is guideFile with its whitespace collapsed, so that a held
// phrase does not depend on where a line wraps.
func heldText(t *testing.T, rel string) string {
	t.Helper()
	return strings.Join(strings.Fields(string(guideFile(t, rel))), " ")
}

// guidePages are the guide's pages by their path under the wiki, the index
// and the log left out.
func guidePages(t *testing.T) []string {
	t.Helper()
	var out []string
	root := guideDir + "/" + knowledge.WikiDir
	if err := fs.WalkDir(FS(), root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		if strings.Contains(rel, "/") {
			out = append(out, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// mustSay fails for every phrase (by the file that must carry it) that its
// file does not say.
func mustSay(t *testing.T, want map[string][]string) {
	t.Helper()
	for file, phrases := range want {
		text := heldText(t, file)
		for _, p := range phrases {
			if !strings.Contains(text, p) {
				t.Errorf("%s does not say %q", file, p)
			}
		}
	}
}

// binarySize is a cap in whole GiB where it is one, else in MiB.
func binarySize(n int64) string {
	if n >= 1<<30 && n%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

// numberWord is n as the guide writes a small number.
func numberWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

// TestTheInstructionNamesTheGuide: AGENT.md names the pack, sends the
// Drafter to the servers page before it pins a server and to the
// manifest's bounds before it returns (plan drafter-knowledge.md §4.3,
// D11), and every page it or the repair turn's framing names is a page of
// the guide, so the map cannot point at nothing.
func TestTheInstructionNamesTheGuide(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"`" + GuideName + "`",
			"**Before you declare any MCP server, read `servers/mcp-servers.md`**",
			"`servers/sign-ins.md`",
			"**Before you return, check the manifest against the bounds in `manifest/manifest.md`.**",
		},
	})
	pages := guidePages(t)
	// A page as the text names it: a section and a slug, starting a word or
	// a code span (pack:runbooks/topics/rollback.md, a citation of the
	// person's pack, is not one).
	page := regexp.MustCompile("(?:^|[\\s`(])([a-z0-9-]+/[a-z0-9-]+\\.md)\\b")
	for from, text := range map[string]string{instructionFile: heldText(t, instructionFile), "the repair turn's framing": repairFraming} {
		named := page.FindAllStringSubmatch(text, -1)
		if len(named) == 0 {
			t.Errorf("%s names no page of the guide", from)
		}
		for _, m := range named {
			if p := m[1]; !slices.Contains(pages, p) {
				t.Errorf("%s names %s, which is no page of the guide", from, p)
			}
		}
	}
	// The repair turn is told where the bounds are.
	if !strings.Contains(repairFraming, "manifest/manifest.md") {
		t.Error("the repair turn is not told where the bounds are")
	}
}

// TestEveryPageIsOnTheMap: the guide's start page links every other page,
// so a Drafter that reads it can reach the whole guide.
func TestEveryPageIsOnTheMap(t *testing.T) {
	start := heldText(t, "guide/start-here.md")
	for _, p := range guidePages(t) {
		if p != "guide/start-here.md" && !strings.Contains(start, "(../"+p) {
			t.Errorf("guide/start-here.md does not link %s", p)
		}
	}
}

// stubPages are the pages that state today's facts until the milestone
// that changes them rewrites them (plan drafter-knowledge.md §4.3, D6):
// what holds publishing (plan quickagents-that-run.md M8, agentry#893). The
// input cases were rewritten by agentry-cli#634 (M7), and the keys page by
// optional secrets M6 (agentry-cli#652). The pages are public text and
// name the change, not the plan.
var stubPages = []string{"draft/review.md"}

// TestTheStubPagesSayTheyAreTodaysFacts: each stub says it states today's
// facts and when it is rewritten, so a reader does not take it for the
// settled guide.
func TestTheStubPagesSayTheyAreTodaysFacts(t *testing.T) {
	for _, p := range stubPages {
		text := heldText(t, p)
		if !strings.Contains(text, "This page states") || !strings.Contains(text, "It is rewritten when") {
			t.Errorf("%s does not say it states today's facts and when it is rewritten", p)
		}
	}
}

// TestTheGuidesPagesStateTheRules: the rules that moved off AGENT.md are
// stated on the pages that now carry them, each where a draft reads it.
func TestTheGuidesPagesStateTheRules(t *testing.T) {
	mustSay(t, map[string][]string{
		"manifest/egress-and-fetch.md":  {"**Never `\"*\"`.**", "A host an MCP server reaches goes in that server's `egress`, not here.", "port 443"},
		"manifest/intake-and-guide.md":  {"**A `choice` question always carries its `choices`**", "**Leave out a field that has nothing to say.**"},
		"manifest/knowledge-accepts.md": {"**Never declare `ships`.**", "a field of the manifest itself and never under `capabilities`"},
		"manifest/run-modes.md":         {"Add `interactive` only when the description asks for a conversation"},
		"manifest/manifest.md":          {"`license`: `\"Apache-2.0\"` unless the description names another SPDX licence", "`publish`: `{\"include\": [\"agent.yaml\", \"AGENT.md\", \"README.md\", \"LICENSE\"]}`"},
		"draft/instructions.md":         {"never copy a page's text", "Use these six headings, in this order"},
		"servers/mcp-servers.md":        {"**An exact version**", "never a range and never `latest`"},
	})
	facts, err := fs.ReadFile(FS(), guideDir+"/"+knowledge.SourcesDir+"/runtime-facts.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("at most %d KiB of a response's body", toolsrv.DefaultMaxFetchBytes>>10); !strings.Contains(string(facts), want) {
		t.Errorf("the runtime facts do not say %q", want)
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

// orList joins items as prose: "a", "a or b", "a, b or c".
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// entriesWord is "1 entry" or "n entries".
func entriesWord(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// TestTheGuideTeachesInputSources (input sources M8, agentry-cli#835;
// plan input-sources.md §5, §6): the instruction and the guide's pages ask
// for a codebase by a repository question and a file at an address by a
// url question, declare no egress and no key to fetch either, never advise
// a .zip for a repository, and teach fetch's saveTo, the scratch tools and
// that a tool result comes a page at a time.
func TestTheGuideTeachesInputSources(t *testing.T) {
	mustSay(t, map[string][]string{
		instructionFile: {
			"comes by a `repository` or a `url` intake question",
			"**the source itself needs no egress and no key**",
			"is still the QuickAgent's own `fetch` to its exact host",
			"**never as a `.zip`**",
			"(`saveTo`)",
			"`scratch_search`",
			"Tool results come a page at a time",
		},
		"manifest/inputs.md": {
			"**A codebase is asked for by a `repository` question**, never as a `.zip`",
			"**A file at a web address is asked for by a `url` question**",
			"never declare `GITHUB_TOKEN` or any other key to clone it",
			"under `/inputs/<id>/`",
			"AI Review reads a repository's Markdown files",
		},
		"manifest/intake-and-guide.md": {
			"`repository`: a git repository's https address",
			"`url`: the https address of one file",
		},
		"manifest/egress-and-fetch.md": {
			"**`saveTo` keeps the whole body.**",
			"`scratch_search`",
			"**A repository or a file the person gives at run time needs no host here.**",
			"declares that database's API host (`api.osv.dev`) for its own `fetch`",
		},
		"runtime/how-a-run-works.md": {
			"**Results come a page at a time.**",
			"`scratch_search`",
			"`repository` or `url` question is fetched by the runner",
		},
	})
	for _, p := range append(guidePages(t), instructionFile) {
		text := heldText(t, p)
		for _, stale := range []string{"repository as a .zip", "a codebase as it is (an archive"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says %q", p, stale)
			}
		}
	}
}
