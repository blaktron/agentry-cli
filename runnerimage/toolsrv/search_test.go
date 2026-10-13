package toolsrv

// grep and glob (plan ripgrep-search.md, agentry-cli#912). The grant,
// scope and argument-list tests hold the server's half anywhere. The ones
// that search run the real ripgrep from the pinned image when
// AGENTRY_RIPGREP_BIN names it, as the ripgrep-image CI job does;
// AGENTRY_RIPGREP_REQUIRED=1 makes its absence a failure there.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// realRipgrep is the pinned ripgrep, or the test is skipped.
func realRipgrep(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("AGENTRY_RIPGREP_BIN")
	if bin == "" {
		if os.Getenv("AGENTRY_RIPGREP_REQUIRED") == "1" {
			t.Fatal("AGENTRY_RIPGREP_BIN is not set, and this job must search with the real ripgrep")
		}
		t.Skip("AGENTRY_RIPGREP_BIN is not set: the real ripgrep is the ripgrep-image CI job's")
	}
	return bin
}

// searchServer is a server for spec with grep and glob granted, running
// bin directly (the confine role is its own package's to test).
func searchServer(t *testing.T, spec *Spec, bin string) *server {
	t.Helper()
	spec.Search.Enabled = true
	s := newTestServer(t, spec)
	s.rgBin, s.search = bin, searchConfinement{}
	return s
}

func grepCall(t *testing.T, s *server, args map[string]any) (string, bool) {
	t.Helper()
	return s.toolGrep(rawArgs(t, args))
}

// TestSearchToolsFollowTheirGrant: grep and glob exist only with the grant
// and an area to search, after the scratch tools.
func TestSearchToolsFollowTheirGrant(t *testing.T) {
	has := func(spec *Spec) bool {
		got := spec.GrantedTools()
		return slices.Contains(got, ToolGrep) && slices.Contains(got, ToolGlob)
	}
	spec := scratchSpec(t)
	if has(spec) {
		t.Fatal("granted without Search")
	}
	spec.Search.Enabled = true
	if !has(spec) {
		t.Fatalf("not granted with Search and a scratch folder: %v", spec.GrantedTools())
	}
	got := spec.GrantedTools()
	if slices.Index(got, ToolGrep) != slices.Index(got, ToolScratchSearch)+1 {
		t.Errorf("grep does not follow the scratch tools: %v", got)
	}
	bare := baseSpec(t)
	bare.Search.Enabled = true
	if has(bare) {
		t.Errorf("granted with no area to search: %v", bare.GrantedTools())
	}
	review := inputsSpec(t)
	review.Search.Enabled, review.Review.Enabled = true, true
	if has(review) {
		t.Errorf("granted when the inputs are under the review grant and nothing else is searchable: %v", review.GrantedTools())
	}
	for name, shape := range map[string]func(*Spec){
		"inputs": func(s *Spec) { s.InputsDir = t.TempDir() },
		"knowledge": func(s *Spec) {
			s.KnowledgeDir, s.Knowledge = t.TempDir(), []KnowledgePack{{Name: "p", Digest: "sha256:aa"}}
		},
		"workspace": func(s *Spec) { s.FSRead = Access{Enabled: true} },
	} {
		s := baseSpec(t)
		s.Search.Enabled = true
		shape(s)
		if !has(s) {
			t.Errorf("%s alone: not granted: %v", name, s.GrantedTools())
		}
	}
}

// TestSearchScopeHoldsEachArea: a call names one area the run has; the
// workspace needs filesystem.read and keeps to its paths grant; view is
// the inputs' alone; nothing escapes an area by .. or a link.
func TestSearchScopeHoldsEachArea(t *testing.T) {
	spec := scratchSpec(t)
	spec.Search.Enabled = true
	s := newTestServer(t, spec)
	mustWrite(t, filepath.Join(spec.ScratchDir, "notes.md"), []byte("x\n"))
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("secret\n"))
	if err := os.Symlink(outside, filepath.Join(spec.ScratchDir, "out")); err != nil {
		t.Fatal(err)
	}
	if sc, err := s.scope(ToolGrep, spec.ScratchDir, ""); err != nil || sc.area != "scratch" || sc.dir != spec.ScratchDir || !slices.Equal(sc.paths, []string{"."}) || sc.read[0] != spec.ScratchDir {
		t.Fatalf("the scratch folder: %+v, %v", sc, err)
	}
	for _, p := range []string{
		spec.WorkspaceDir,                   // no filesystem.read
		spec.ScratchDir + "/../",            // .. out of the area
		spec.ScratchDir + "/out/secret.txt", // a link out of the area
		"/etc",                              // no area at all
		"",                                  // no path
		spec.ScratchDir + "/missing",        // nothing there
	} {
		if sc, err := s.scope(ToolGrep, p, ""); err == nil {
			t.Errorf("scope(%q) = %+v, want a refusal", p, sc)
		}
	}
	if _, err := s.scope(ToolGrep, spec.ScratchDir, ViewText); err == nil || !strings.Contains(err.Error(), "view applies to") {
		t.Errorf("a view outside the inputs: %v", err)
	}

	ws := baseSpec(t)
	ws.FSRead = Access{Enabled: true, Paths: []string{"docs"}}
	ws.Search.Enabled = true
	mustWrite(t, filepath.Join(ws.WorkspaceDir, "docs", "a.md"), []byte("a\n"))
	mustWrite(t, filepath.Join(ws.WorkspaceDir, "private", "b.md"), []byte("b\n"))
	w := newTestServer(t, ws)
	if sc, err := w.scope(ToolGrep, ws.WorkspaceDir, ""); err != nil || sc.dir != ws.WorkspaceDir || !slices.Equal(sc.paths, []string{"docs"}) {
		t.Errorf("the workspace root under a paths grant is the granted paths: %+v, %v", sc, err)
	}
	if _, err := w.scope(ToolGrep, ws.WorkspaceDir+"/private", ""); err == nil {
		t.Error("a workspace path outside the paths grant was searched")
	}
}

// TestRipgrepsArgumentsAreFixed: whatever a call sends, ripgrep gets the
// fixed flags, the call's own options each in one argument, the pattern
// after -e and the paths after --, and never a flag that starts another
// program or follows a link.
func TestRipgrepsArgumentsAreFixed(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "rg")
	mustWrite(t, fake, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+argvFile+"\nenv > "+argvFile+".env\nexit 1\n"))
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRY_SECRET_PROBE", "hunter2")
	s := searchServer(t, scratchSpec(t), fake)
	got, isErr := grepCall(t, s, map[string]any{
		"pattern": "--pre=sh", "path": s.spec.ScratchDir, "glob": "--search-zip", "type": "md",
		"ignoreCase": true, "output": "content", "context": 2,
	})
	if isErr {
		t.Fatalf("grep: %q", got)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := append(append([]string{}, rgFixedArgs...),
		"--glob=--search-zip", "--type=md", "--ignore-case", "--json", "--context=2", "-e", "--pre=sh", "--", ".")
	if !slices.Equal(argv, want) {
		t.Fatalf("ripgrep's arguments:\n got %q\nwant %q", argv, want)
	}
	for _, a := range argv[:len(argv)-4] {
		for _, banned := range []string{"--pre", "--search-zip", "-z", "-L", "--follow", "--pre-glob"} {
			if a == banned || strings.HasPrefix(a, banned+"=") {
				t.Errorf("ripgrep was given %q", a)
			}
		}
	}
	env, _ := os.ReadFile(argvFile + ".env")
	if strings.Contains(string(env), "hunter2") {
		t.Error("ripgrep was given the tools server's environment")
	}
	for _, args := range []map[string]any{
		{"pattern": "", "path": s.spec.ScratchDir},
		{"pattern": "a", "path": s.spec.ScratchDir, "type": "--pre"},
		{"pattern": "a", "path": s.spec.ScratchDir, "output": "lines"},
		{"pattern": "a", "path": s.spec.ScratchDir, "context": 6, "output": "content"},
		{"pattern": "a", "path": s.spec.ScratchDir, "context": 1},
		{"pattern": "a", "path": s.spec.ScratchDir, "glob": "a\nb"},
		{"pattern": strings.Repeat("a", maxGrepPattern+1), "path": s.spec.ScratchDir},
	} {
		if got, isErr := grepCall(t, s, args); !isErr || !strings.HasPrefix(got, "grep: ") {
			t.Errorf("%v: (%q, %v), want a refusal", args, got, isErr)
		}
	}
}

// TestSearchConfinementIsChosenByTheMachine: as root with Landlock,
// ripgrep reads only its area, writes nothing and, at ABI 4, opens no TCP,
// as its own user; without Landlock it runs as that user under the limits
// alone; not as root and without Landlock, no search runs.
func TestSearchConfinementIsChosenByTheMachine(t *testing.T) {
	ll := chooseSearchConfinement("linux", 4, nil, 0, "/r")
	name, argv := ll.argv("/opt/agentry/ripgrep/rg", []string{"/scratch"}, []string{"-e", "x"})
	want := []string{ConfineRole, "--read", "/scratch", "--no-network", "--max-memory", "536870912", "--", "/opt/agentry/ripgrep/rg", "-e", "x"}
	if name != "/r" || !slices.Equal(argv, want) || !ll.asSearch {
		t.Errorf("Landlock as root: %s %q (asSearch %v), want /r %q", name, argv, ll.asSearch, want)
	}
	gv := chooseSearchConfinement("linux", 0, errors.New("ENOSYS"), 0, "/r")
	_, argv = gv.argv("/rg", []string{"/scratch"}, []string{"-e", "x"})
	if !gv.asSearch || !slices.Equal(argv, []string{ConfineRole, "--limits-only", "--max-memory", "536870912", "--", "/rg", "-e", "x"}) {
		t.Errorf("gVisor: %q (asSearch %v)", argv, gv.asSearch)
	}
	if c := chooseSearchConfinement("linux", 0, errors.New("ENOSYS"), 1000, "/r"); c.refused == "" {
		t.Error("not root and no Landlock: a search would run unheld")
	}
	if searchID == unprivilegedID || searchID == typstID || searchID == 0 {
		t.Errorf("searchID %d is shared with another holder", searchID)
	}
}

// TestGrepSearchesTheScratchFolder: the three outputs, context, hidden
// files and files an ignore file names searched, .git not, and paging.
func TestGrepSearchesTheScratchFolder(t *testing.T) {
	s := searchServer(t, scratchSpec(t), realRipgrep(t))
	d := s.spec.ScratchDir
	mustWrite(t, filepath.Join(d, "a.md"), []byte("one\nInvoice 7\nthree\nfour\ninvoice 9\n"))
	mustWrite(t, filepath.Join(d, "b.go"), []byte("func InvoiceTotal() {}\n"))
	mustWrite(t, filepath.Join(d, ".hidden", "c.txt"), []byte("invoice hidden\n"))
	mustWrite(t, filepath.Join(d, ".gitignore"), []byte("*.go\n"))
	mustWrite(t, filepath.Join(d, ".git", "HEAD"), []byte("invoice in git\n"))

	got, isErr := grepCall(t, s, map[string]any{"pattern": "invoice", "path": d, "ignoreCase": true})
	want := strings.Join([]string{d + "/.hidden/c.txt", d + "/a.md", d + "/b.go"}, "\n")
	if isErr || got != want {
		t.Fatalf("files:\n got %q\nwant %q", got, want)
	}
	if got, _ := grepCall(t, s, map[string]any{"pattern": "invoice", "path": d, "ignoreCase": true, "output": "count"}); !strings.Contains(got, d+"/a.md: 2") {
		t.Errorf("count: %q", got)
	}
	got, _ = grepCall(t, s, map[string]any{"pattern": `[Ii]nvoice \d`, "path": d + "/a.md", "output": "content", "context": 1})
	want = strings.Join([]string{
		d + "/a.md-1- one", d + "/a.md:2: Invoice 7", d + "/a.md-3- three",
		d + "/a.md-4- four", d + "/a.md:5: invoice 9",
	}, "\n")
	if got != want {
		t.Errorf("content with context:\n got %q\nwant %q", got, want)
	}
	got, _ = grepCall(t, s, map[string]any{"pattern": "invoice", "path": d, "ignoreCase": true, "maxResults": 1})
	if !strings.HasPrefix(got, d+"/.hidden/c.txt\n") || !strings.Contains(got, "continue with offset 1") {
		t.Errorf("first page: %q", got)
	}
	if got, _ := grepCall(t, s, map[string]any{"pattern": "invoice", "path": d, "ignoreCase": true, "maxResults": 1, "offset": 1}); !strings.HasPrefix(got, d+"/a.md\n") {
		t.Errorf("second page: %q", got)
	}
	if got, isErr := grepCall(t, s, map[string]any{"pattern": "zebra", "path": d}); isErr || got != "no file in scratch matches" {
		t.Errorf("no match: (%q, %v)", got, isErr)
	}
	if got, isErr := grepCall(t, s, map[string]any{"pattern": "a(", "path": d}); !isErr || !strings.Contains(got, "regex parse error") {
		t.Errorf("a bad pattern: (%q, %v)", got, isErr)
	}
	if got, isErr := grepCall(t, s, map[string]any{"pattern": "a{1000}{1000}", "path": d}); !isErr || !strings.Contains(got, "size limit") {
		t.Errorf("a pattern past the size limit: (%q, %v)", got, isErr)
	}
}

// TestGrepNeverFollowsALinkOut: a link inside an area is not followed.
func TestGrepNeverFollowsALinkOut(t *testing.T) {
	s := searchServer(t, scratchSpec(t), realRipgrep(t))
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("invoice outside\n"))
	if err := os.Symlink(outside, filepath.Join(s.spec.ScratchDir, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(s.spec.ScratchDir, "file-link")); err != nil {
		t.Fatal(err)
	}
	if got, isErr := grepCall(t, s, map[string]any{"pattern": "invoice", "path": s.spec.ScratchDir}); isErr || got != "no file in scratch matches" {
		t.Errorf("a link was followed: (%q, %v)", got, isErr)
	}
}

// TestGrepSearchesTheInputsAsSearchInputsDoes: each input in its view, by
// its own path, answered as the person's own input.
func TestGrepSearchesTheInputsAsSearchInputsDoes(t *testing.T) {
	spec := decodedSpec(t)
	s := searchServer(t, spec, realRipgrep(t))
	got, isErr := grepCall(t, s, map[string]any{"pattern": "quarterly review", "path": spec.InputsDir, "ignoreCase": true, "output": "content"})
	want := spec.InputsDir + "/notes.txt:1: Remember the Quarterly Review.\n" + spec.InputsDir + "/report.docx:3: The quarterly review is on Friday."
	if isErr || got != want {
		t.Fatalf("inputs:\n got %q\nwant %q", got, want)
	}
	if !s.callPersonsInput {
		t.Error("a search of the inputs is not answered as the person's own input")
	}
	if got, _ := grepCall(t, s, map[string]any{"pattern": "binary zip", "path": spec.InputsDir}); !strings.HasPrefix(got, "no file in inputs") {
		t.Errorf("a decoded document's binary original was searched: %q", got)
	}
	if got, _ := grepCall(t, s, map[string]any{"pattern": `\d{4}`, "path": spec.InputsDir + "/scans"}); got != spec.InputsDir+"/scans/page1.png" {
		t.Errorf("a folder of the inputs: %q", got)
	}
}

// TestGrepSearchesAKnowledgePack: a pack's wiki and sources, by the paths
// knowledge_read takes after the pack's folder.
func TestGrepSearchesAKnowledgePack(t *testing.T) {
	spec := knowledgeSpec(t)
	s := searchServer(t, spec, realRipgrep(t))
	pack := spec.KnowledgeDir + "/aws-docs"
	got, isErr := grepCall(t, s, map[string]any{"pattern": `\[src: doc:[^\]]+\]`, "path": pack, "output": "content"})
	if isErr || got != pack+"/wiki/topics/s3-buckets.md:9: Versioning keeps every object version [src: doc:s3-guide.txt]." {
		t.Fatalf("a citation: (%q, %v)", got, isErr)
	}
	if s.callPersonsInput {
		t.Error("a pack's search was answered as the person's input")
	}
	got, _ = s.toolGlob(rawArgs(t, map[string]any{"pattern": "wiki/topics/*.md", "path": pack}))
	if got != pack+"/wiki/topics/iam.md\n"+pack+"/wiki/topics/s3-buckets.md" {
		t.Errorf("glob: %q", got)
	}
}

// TestGlobMatcherIsRipgrepsGlob: the inputs' glob, which the server applies
// because ripgrep filters no file it is given by name.
func TestGlobMatcherIsRipgrepsGlob(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"*.md", "a.md", true},
		{"*.md", "wiki/topics/a.md", true}, // no slash: the name, at any depth
		{"*.md", "a.mdx", false},
		{"wiki/*.md", "wiki/a.md", true},
		{"wiki/*.md", "wiki/topics/a.md", false},
		{"wiki/**/*.md", "wiki/topics/deep/a.md", true},
		{"**/billing*", "x/y/billing-2026.csv", true},
		{"*.{csv,tsv}", "data/a.tsv", true},
		{"[ab].txt", "b.txt", true},
		{"[!ab].txt", "b.txt", false},
		{"!*.md", "a.md", false},
		{"!*.md", "a.txt", true},
		{"a.b", "axb", false},
	} {
		m, err := globMatcher(c.glob)
		if err != nil {
			t.Fatalf("%q: %v", c.glob, err)
		}
		if got := m(c.path); got != c.want {
			t.Errorf("%q on %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
	if _, err := globMatcher("[ab"); err == nil {
		t.Error("an unclosed class was accepted")
	}
}

// TestGrepFiltersTheInputsByGlob: ripgrep would search every input it is
// given by name, so the server keeps only the ones the glob matches, and
// refuses a type there.
func TestGrepFiltersTheInputsByGlob(t *testing.T) {
	spec := decodedSpec(t)
	s := searchServer(t, spec, realRipgrep(t))
	got, isErr := grepCall(t, s, map[string]any{"pattern": "review", "path": spec.InputsDir, "ignoreCase": true, "glob": "*.txt"})
	if isErr || got != spec.InputsDir+"/notes.txt" {
		t.Errorf("glob over the inputs: (%q, %v)", got, isErr)
	}
	if got, isErr := grepCall(t, s, map[string]any{"pattern": "review", "path": spec.InputsDir, "type": "txt"}); !isErr || !strings.Contains(got, "pass a glob") {
		t.Errorf("type over the inputs: (%q, %v)", got, isErr)
	}
}

// TestAGlobIsRelativeToTheNamedFolder: ripgrep runs in the folder a call
// names, so a glob with a slash is relative to it.
func TestAGlobIsRelativeToTheNamedFolder(t *testing.T) {
	s := searchServer(t, scratchSpec(t), realRipgrep(t))
	d := s.spec.ScratchDir
	mustWrite(t, filepath.Join(d, "notes", "wiki", "a.md"), []byte("x\n"))
	mustWrite(t, filepath.Join(d, "notes", "b.md"), []byte("x\n"))
	got, _ := s.toolGlob(rawArgs(t, map[string]any{"pattern": "wiki/*.md", "path": d + "/notes"}))
	if got != d+"/notes/wiki/a.md" {
		t.Errorf("glob relative to the named folder: %q", got)
	}
	got, _ = grepCall(t, s, map[string]any{"pattern": "x", "path": d + "/notes/b.md"})
	if got != d+"/notes/b.md" {
		t.Errorf("a single file: %q", got)
	}
}

// TestAnInputsSearchReachesTheAgentUnchanged: on the wire, a grep of the
// inputs answers as read_input does, an embedded resource the gateway's
// secret block does not rewrite (DR-78); the next call, of the scratch
// folder, is a text item again.
func TestAnInputsSearchReachesTheAgentUnchanged(t *testing.T) {
	spec := decodedSpec(t)
	spec.ScratchDir = t.TempDir()
	s := searchServer(t, spec, realRipgrep(t))
	mustWrite(t, filepath.Join(spec.ScratchDir, "n.md"), []byte("review\n"))
	var out bytes.Buffer
	s.out = &out
	call := func(id int, args map[string]any) map[string]any {
		t.Helper()
		out.Reset()
		params, _ := json.Marshal(map[string]any{"name": ToolGrep, "arguments": args})
		s.handleToolsCall(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: "tools/call", Params: params})
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("response %q: %v", out.String(), err)
		}
		return resultOf(t, m)["content"].([]any)[0].(map[string]any)
	}
	if c := call(1, map[string]any{"pattern": "review", "path": spec.InputsDir, "ignoreCase": true}); c["type"] != "resource" {
		t.Errorf("a grep of the inputs is %v, want an embedded resource", c["type"])
	}
	if c := call(2, map[string]any{"pattern": "review", "path": spec.ScratchDir}); c["type"] != "text" {
		t.Errorf("a grep of the scratch folder after it is %v, want a text item", c["type"])
	}
}
