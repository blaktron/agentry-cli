package toolsrv

// grep and glob: search
// the files a run can already read by regular expression, and list them
// by path pattern, with ripgrep. Like the scratch folder they are a grant
// of the runner, never a line of the contract: they read only the areas
// the run's other tools read — its inputs (not under the review grant,
// where every file reaches the model through AI Review), its attached
// knowledge packs, its scratch folder, and its workspace when the
// contract grants filesystem.read — one area a call, so each answer keeps
// its area's rules: a search of the inputs answers as search_inputs does,
// the person's own files unchanged, and every other area as a text
// the gateway scans.
//
// ripgrep is the runner image's copy of agentry-ripgrep (copied by digest
// from agentry-dockerimages, signed by its workflow), started the way
// export_pdf starts Typst (pdf.go): through the confine role, with an
// empty environment, as a user of its own (searchID) that owns nothing
// and is neither root, which holds the run's secrets in its environment,
// nor nobody, which runs a declared command; with Landlock, it may read
// only the area it searches, may write nothing and may open no TCP
// connection; without (gVisor), its user is what holds it. Its argument
// list is fixed (rgFixedArgs) but for a closed set of the call's own
// options, the pattern after -e and the paths after --: nothing a call
// sends can become a flag, and no flag that makes ripgrep start another
// program (--pre, --search-zip) or follow a link (-L) is ever passed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// The search tools, granted with Search.
const (
	ToolGrep = "grep"
	ToolGlob = "glob"
)

// RipgrepDir is where the runner image holds ripgrep and its licences,
// copied by digest from agentry-ripgrep. It is
// outside every bin directory, so an exec image's minimize keeps it and no
// declared command finds it on a PATH.
const RipgrepDir = "/opt/agentry/ripgrep"

// RipgrepVersion is the ripgrep release in the pinned image, for the
// plan's enforcement text and the tools' errors.
const RipgrepVersion = "15.2.0"

// searchID is the uid and gid ripgrep runs as when the tools server is
// root: it owns nothing, and it is neither nobody (a declared command,
// confinement.go) nor typstID, so none can signal or trace another, and it
// cannot read the tools server's environment.
const searchID = 65532

// The bounds of one search.
const (
	// SearchMemory is ripgrep's address space.
	SearchMemory = 512 << 20
	// SearchTimeout is how long one search may run.
	SearchTimeout = 30 * time.Second
	// maxSearchOutput is the most of ripgrep's output the server reads; a
	// search past it answers what it read and says it stopped.
	maxSearchOutput = 16 << 20
	// defaultGrepResults and maxGrepResults bound grep's and glob's
	// result lines.
	defaultGrepResults = 100
	maxGrepResults     = 2000
	// maxGrepContext is the most lines of context around a match.
	maxGrepContext = 5
	// maxGrepPattern and maxGlobPattern bound a call's pattern and filter.
	maxGrepPattern = 1024
	maxGlobPattern = 256
	// grepLineBytes is the most of one matching line an answer shows.
	grepLineBytes = 400
	// searchTargetsPerRun is the most input files one ripgrep is given by
	// name; a larger set runs in several.
	searchTargetsPerRun = 2000
)

// Search is grep and glob's grant: Enabled by the compiler with the scratch
// folder, for every package but a built-in. The tools exist only when the run
// also has an area to search.
type Search struct {
	Enabled bool `json:"enabled"`
}

// rgFixedArgs are the flags every ripgrep run takes: no config
// file, ignore files not read and hidden files searched so nothing the
// person gave is skipped, caps on file size and on what a pattern may
// compile to, and a stable order so offset pages are the same pages
// (sorting also makes ripgrep search on one thread). The output flags are
// each tool's: --json for grep's content, --null paths otherwise.
var rgFixedArgs = []string{
	"--no-config", "--no-ignore", "--hidden",
	"--max-filesize=8M", "--sort=path",
	"--regex-size-limit=16M", "--dfa-size-limit=64M",
}

// rgSkipGit keeps .git out of every search. ripgrep lets the last glob that
// matches a path decide, so it goes after the call's own glob: a call's "*"
// would otherwise bring .git back.
const rgSkipGit = "--glob=!.git"

// grepOutputs are grep's answer shapes.
var grepOutputs = []string{"files", "content", "count"}

// rgTypeName is a ripgrep file type a call may name.
var rgTypeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_+-]{0,31}$`)

func grepSchema() map[string]any {
	return objSchema(map[string]any{
		"pattern":      strProp("a regular expression (ripgrep's syntax: Rust's regex, no look-around or back-references); with fixedStrings, the literal text"),
		"path":         strProp(`where to search, as this run sees it: "/inputs", "/knowledge/<pack>", "/scratch" or "/workspace" (when it is granted), or a folder or file beneath one`),
		"glob":         strProp(`search only files whose path matches this glob ("*.md", "wiki/**/*.md"); a leading ! leaves matching files out`),
		"type":         strProp(`search only files of this ripgrep type ("md", "py", "go", "json", "html")`),
		"ignoreCase":   boolProp("match without regard to case"),
		"fixedStrings": boolProp("match the pattern as literal text, not a regular expression"),
		"context":      intProp(fmt.Sprintf("lines of context before and after each match, 0 to %d (output content only)", maxGrepContext)),
		"output":       map[string]any{"type": "string", "enum": grepOutputs, "description": `"files" (the default) lists the files that match; "content" gives each matching line as path:line: text; "count" gives the matching lines per file`},
		"maxResults":   intProp(fmt.Sprintf("most result lines to return (default %d, at most %d)", defaultGrepResults, maxGrepResults)),
		"offset":       intProp("result line to start from, for the next page (default 0)"),
		"view":         viewProp(),
	}, "pattern", "path")
}

func globSchema() map[string]any {
	return objSchema(map[string]any{
		"pattern":    strProp(`the glob a file's path must match, relative to path ("**/*.md", "*.csv", "wiki/**/billing*")`),
		"path":       strProp(`where to list, as this run sees it: "/inputs", "/knowledge/<pack>", "/scratch" or "/workspace" (when it is granted), or a folder beneath one`),
		"maxResults": intProp(fmt.Sprintf("most paths to return (default %d, at most %d)", defaultGrepResults, maxGrepResults)),
		"offset":     intProp("path to start from, for the next page (default 0)"),
	}, "pattern", "path")
}

// searchAreas says whether spec gives grep and glob anything to search.
func (s *Spec) searchAreas() bool {
	return (s.InputsDir != "" && !s.Review.Enabled) || s.ScratchDir != "" ||
		(s.KnowledgeDir != "" && len(s.Knowledge) > 0) || (s.FSRead.Enabled && s.WorkspaceDir != "")
}

// searchAreaNames names the areas this run has, for the tools'
// descriptions and refusals, so the model is never told of one it cannot
// search.
func (s *server) searchAreaNames() string {
	var areas []string
	if s.spec.InputsDir != "" && !s.spec.Review.Enabled {
		areas = append(areas, s.spec.InputsDir+" (your inputs)")
	}
	if s.spec.KnowledgeDir != "" && len(s.spec.Knowledge) > 0 {
		areas = append(areas, s.spec.KnowledgeDir+"/<pack> (attached knowledge)")
	}
	if s.spec.ScratchDir != "" {
		areas = append(areas, s.spec.ScratchDir+" (the scratch folder)")
	}
	if s.spec.FSRead.Enabled && s.spec.WorkspaceDir != "" {
		name := "the workspace"
		if s.spec.WorkspaceName != "" {
			name = s.spec.WorkspaceName
		}
		areas = append(areas, s.spec.WorkspaceDir+" ("+name+")")
	}
	return strings.Join(areas, ", ")
}

func (s *server) grepDescription() string {
	return "Search the contents of files with a regular expression (ripgrep), in one area at a time: " + s.searchAreaNames() + ". " +
		`output "files" (the default) lists the files that match, "content" gives each matching line as path:line: text (with context lines as path-line- text), "count" the matching lines per file. ` +
		"Paths come back as this run sees them; read a file with the area's own tool: read_input with the path after /inputs/, knowledge_read with the path after /knowledge/<pack>/wiki/ (or doc:<the path after sources/>), scratch_read or fs_read with the path after /scratch/ or " + s.spec.WorkspaceDir + "/. " +
		"Hidden files and files an ignore file names are searched; .git is not. A long answer pages: pass offset to read on, or narrow path, glob or type."
}

func (s *server) globDescription() string {
	return "List the files whose path matches a glob (ripgrep's globs: * within a folder, ** across folders), in one area at a time: " + s.searchAreaNames() + ". " +
		"Paths come back sorted, as this run sees them, to read with the area's own tool. A long listing pages: pass offset to read on."
}

// searchScope is what one call searches.
type searchScope struct {
	// area is the area's name, for answers.
	area string
	// dir is ripgrep's working directory: the folder the call named, so a
	// glob with a slash in it is relative to that folder (ripgrep anchors
	// globs at its working directory).
	dir string
	// paths are what ripgrep is given, relative to dir; the inputs' are
	// files by name, absolute, which ripgrep searches whatever a glob or
	// type says (byName), so the server filters those itself.
	paths  []string
	byName bool
	// read are the directories Landlock lets ripgrep read beneath.
	read []string
	// display maps a path ripgrep answers with to the path the answer
	// shows; a path it does not hold is shown as it is.
	display map[string]string
	// personsInput: the answer is the person's own input.
	personsInput bool
	// names are byName's paths as a glob is matched against them, each
	// relative to the folder the call named.
	names []string
	// hidden are paths an answer leaves out: each pack's manifest.json,
	// which is the runner's record of the pack, not a page knowledge_read
	// opens.
	hidden map[string]bool
	// withheld, when set, says a shown path is left out of a grep's
	// answer: a file in a checked share folder that is not yet checked
	// (sharecheck.go). withheldN counts the files it left out.
	withheld  func(shown string) bool
	withheldN *map[string]bool
}

// keep says whether an answer shows the path shown, and counts a file a
// check withheld.
func (sc searchScope) keep(shown string) bool {
	if sc.hidden[shown] {
		return false
	}
	if sc.withheld != nil && sc.withheld(shown) {
		(*sc.withheldN)[shown] = true
		return false
	}
	return true
}

// scope resolves where a call searches: an area the run has, and a path
// beneath it, checked as every other tool checks a path (resolveUnder) and
// against the workspace's paths grant. view is the inputs' view, refused
// for any other area. grep searches the inputs' texts by name (texts);
// glob lists the inputs' folder as uploaded.
func (s *server) scope(tool, p, view string) (searchScope, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return searchScope{}, fmt.Errorf("path is required: one of %s", s.searchAreaNames())
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = path.Clean(p)
	type area struct {
		name, dir string
		ok        bool
	}
	areas := []area{
		{"inputs", s.spec.InputsDir, s.spec.InputsDir != "" && !s.spec.Review.Enabled},
		{"knowledge", s.spec.KnowledgeDir, s.spec.KnowledgeDir != "" && len(s.spec.Knowledge) > 0},
		{"scratch", s.spec.ScratchDir, s.spec.ScratchDir != ""},
		{"workspace", s.spec.WorkspaceDir, s.spec.FSRead.Enabled && s.spec.WorkspaceDir != ""},
	}
	for _, a := range areas {
		if !a.ok || (p != a.dir && !strings.HasPrefix(p, a.dir+"/")) {
			continue
		}
		rel := "."
		if p != a.dir {
			rel = strings.TrimPrefix(p, a.dir+"/")
		}
		if view != "" && a.name != "inputs" {
			return searchScope{}, fmt.Errorf("view applies to %s alone", s.spec.InputsDir)
		}
		switch {
		case a.name == "inputs" && tool == ToolGrep:
			return s.inputsScope(rel, view)
		case a.name == "workspace":
			return s.workspaceScope(rel)
		}
		sc, err := folderScope(a.name, a.dir, rel)
		if err == nil && a.name == "knowledge" {
			sc.hidden = map[string]bool{}
			for _, k := range s.spec.Knowledge {
				sc.hidden[path.Join(a.dir, k.Name, "manifest.json")] = true
			}
		}
		return sc, err
	}
	return searchScope{}, fmt.Errorf("%s is not an area this run can search; search one of %s", p, s.searchAreaNames())
}

// inputsScope is the inputs in view (each input's default when none is
// asked for), as search_inputs reads them: the text of each, by name, and
// shown by the input's own path.
func (s *server) inputsScope(rel, view string) (searchScope, error) {
	if err := validView(view); err != nil {
		return searchScope{}, err
	}
	if rel != "." {
		if _, err := resolveUnder(s.spec.InputsDir, rel); err != nil {
			return searchScope{}, err
		}
	}
	targets, err := s.searchTargets(rel, view)
	if err != nil {
		return searchScope{}, err
	}
	sc := searchScope{area: "inputs", dir: "/", byName: true, display: map[string]string{}, personsInput: true,
		read: []string{s.spec.InputsDir}}
	if s.spec.InputsTextDir != "" {
		sc.read = append(sc.read, s.spec.InputsTextDir)
	}
	for _, t := range targets {
		sc.paths = append(sc.paths, t.abs)
		sc.display[t.abs] = path.Join(s.spec.InputsDir, t.path)
		name := t.path
		if rel != "." {
			name = strings.TrimPrefix(t.path, rel+"/")
			if t.path == rel {
				name = path.Base(t.path)
			}
		}
		sc.names = append(sc.names, name)
	}
	return sc, nil
}

// folderScope is a folder or a file beneath an area's dir: ripgrep runs in
// the folder (a file's own folder) and is given ".", or the file's name.
func folderScope(area, dir, rel string) (searchScope, error) {
	abs, err := resolveUnder(dir, rel)
	if err != nil {
		return searchScope{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return searchScope{}, fmt.Errorf("nothing at %s", path.Join(dir, rel))
	}
	sc := searchScope{area: area, dir: abs, paths: []string{"."}, read: []string{dir}}
	if !fi.IsDir() {
		sc.dir, sc.paths = filepath.Dir(abs), []string{filepath.Base(abs)}
	}
	return sc, nil
}

// workspaceScope is a path in the workspace, held to the paths grant: the
// workspace root under a paths grant is each granted path that exists.
func (s *server) workspaceScope(rel string) (searchScope, error) {
	grants := s.spec.FSRead.Paths
	// Under a paths grant Landlock lets ripgrep read the granted paths
	// alone, so a link inside the workspace cannot take it to the rest.
	var granted []string
	for _, g := range grants {
		if abs, err := resolveUnder(s.spec.WorkspaceDir, g); err == nil {
			if _, err := os.Stat(abs); err == nil {
				granted = append(granted, abs)
			}
		}
	}
	if rel == "." && len(grants) > 0 {
		sc := searchScope{area: "workspace", dir: s.spec.WorkspaceDir, read: granted}
		for _, abs := range granted {
			sc.paths = append(sc.paths, strings.TrimPrefix(abs, s.spec.WorkspaceDir+"/"))
		}
		if len(sc.paths) == 0 {
			return searchScope{}, errors.New("none of the workspace's granted paths exists")
		}
		return sc, nil
	}
	if !allowedByGrant(grants, rel) {
		return searchScope{}, fmt.Errorf("%s is not in the workspace paths this QuickAgent may read", path.Join(s.spec.WorkspaceDir, rel))
	}
	sc, err := folderScope("workspace", s.spec.WorkspaceDir, rel)
	if err == nil && len(grants) > 0 {
		sc.read = granted
	}
	return sc, err
}

// searchConfinement is how ripgrep is held, chosen once when the
// server starts, as export_pdf's renderer is.
type searchConfinement struct {
	// landlock is the ABI when Landlock holds ripgrep's reads and writes,
	// 0 otherwise.
	landlock int
	// noNetwork says Landlock refuses ripgrep TCP (ABI 4 and later).
	noNetwork bool
	// limitsOnly runs the trampoline with no Landlock (gVisor): ripgrep is
	// held by its user alone.
	limitsOnly bool
	// asSearch starts ripgrep as searchID.
	asSearch bool
	// refused, when set, is why no search runs.
	refused string
	// none is the off-Linux debugging path.
	none bool
	// trampoline is the confine role's argv prefix.
	trampoline []string
}

func chooseSearchConfinement(goos string, abi int, abiErr error, euid int, exe string) searchConfinement {
	switch {
	case goos != "linux":
		return searchConfinement{none: true}
	case abiErr == nil:
		return searchConfinement{landlock: abi, noNetwork: confine.NetworkDenied(abi), asSearch: euid == 0, trampoline: []string{exe, ConfineRole}}
	case euid != 0:
		return searchConfinement{refused: fmt.Sprintf("this machine cannot hold the search to the area it reads (%v), and the tools server cannot start it as another user", abiErr)}
	default:
		return searchConfinement{limitsOnly: true, asSearch: true, trampoline: []string{exe, ConfineRole}}
	}
}

func newSearchConfinement() searchConfinement {
	abi, abiErr := confine.ABI()
	exe, err := os.Executable()
	if err != nil && abiErr == nil {
		abiErr = fmt.Errorf("this binary cannot find itself: %w", err)
	}
	return chooseSearchConfinement(runtime.GOOS, abi, abiErr, os.Geteuid(), exe)
}

// argv starts prog with args, held as c says: under Landlock it may read
// only beneath read, write nothing (but /dev/null) and, at ABI 4 or later,
// open no TCP connection.
func (c searchConfinement) argv(prog string, read, args []string) (string, []string) {
	if c.trampoline == nil {
		return prog, args
	}
	argv := append([]string{}, c.trampoline[1:]...)
	if c.limitsOnly {
		argv = append(argv, "--limits-only")
	} else {
		for _, d := range read {
			argv = append(argv, "--read", d)
		}
		if c.noNetwork {
			argv = append(argv, "--no-network")
		}
	}
	argv = append(argv, "--max-memory", strconv.Itoa(SearchMemory))
	return c.trampoline[0], append(append(argv, "--", prog), args...)
}

// describe is the sentence the server's log gives the search's holding.
func (c searchConfinement) describe() string {
	switch {
	case c.refused != "":
		return "no search runs: " + c.refused
	case c.limitsOnly:
		return fmt.Sprintf("ripgrep runs as uid %d, which owns nothing (no Landlock, so it may read what anyone may), in a %d MiB address space", searchID, SearchMemory>>20)
	case c.landlock > 0:
		net := "TCP is not refused (Landlock ABI below 4)"
		if c.noNetwork {
			net = "TCP refused"
		}
		return fmt.Sprintf("ripgrep may read only the area it searches and write nothing (Landlock ABI %d), %s, in a %d MiB address space", c.landlock, net, SearchMemory>>20)
	default:
		return "ripgrep is not confined (a debugging server off Linux)"
	}
}

func (c searchConfinement) run(cmd *exec.Cmd) (string, error) {
	if c.trampoline == nil {
		return "", cmd.Run()
	}
	return runTrampolined(cmd)
}

// rgResult is one ripgrep run's output.
type rgResult struct {
	stdout   []byte
	stderr   string
	exit     int
	timedOut bool
	cut      bool
}

// runRipgrep runs ripgrep with args over sc, as many times as sc's paths
// need, and joins what each answered. A failure of the machinery is the
// tool's error; ripgrep's own (a pattern it refuses) is in the result.
func (s *server) runRipgrep(tool string, args []string, sc searchScope) (rgResult, string) {
	if s.search.refused != "" {
		s.callFault = true
		return rgResult{}, tool + ": " + s.search.refused
	}
	if _, err := os.Stat(s.rgBin); err != nil {
		s.callFault = true
		return rgResult{}, fmt.Sprintf("%s: this runner image has no ripgrep (%v); the runner image must be rebuilt", tool, err)
	}
	if s.search.landlock > 0 && len(sc.read) == 0 {
		// confine without --read restricts no read: never start it so.
		s.callFault = true
		return rgResult{}, tool + ": the search has no area to hold its reads to"
	}
	// One deadline for the whole call, however many runs its paths need.
	ctx, cancel := context.WithTimeout(context.Background(), s.searchTimeout)
	defer cancel()
	var all rgResult
	for start := 0; start < len(sc.paths); start += searchTargetsPerRun {
		chunk := sc.paths[start:min(len(sc.paths), start+searchTargetsPerRun)]
		r, failed := s.runRipgrepOnce(ctx, sc.dir, append(append(append([]string{}, args...), "--"), chunk...), sc.read)
		if failed != "" {
			s.callFault = true
			return rgResult{}, tool + ": the search could not be started confined: " + failed
		}
		if room := maxSearchOutput - len(all.stdout); len(r.stdout) > room {
			r.stdout, r.cut = r.stdout[:room], true
		}
		all.stdout = append(all.stdout, r.stdout...)
		all.stderr += r.stderr
		all.timedOut = all.timedOut || r.timedOut
		all.cut = all.cut || r.cut
		if r.exit > all.exit {
			all.exit = r.exit
		}
		if all.timedOut || all.cut || (r.exit == 2 && len(r.stdout) == 0) {
			break
		}
	}
	return all, ""
}

// maxSearchStderr is the most of ripgrep's stderr the server keeps: its
// refusals and the files it could not read.
const maxSearchStderr = 64 << 10

func (s *server) runRipgrepOnce(ctx context.Context, dir string, args, read []string) (rgResult, string) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := s.ripgrepCommand(ctx, dir, args, read)
	stdout, stderr := &stopWriter{capWriter: capWriter{max: maxSearchOutput}, stop: stop}, &capWriter{max: maxSearchStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	failed, err := s.search.run(cmd)
	if failed != "" {
		return rgResult{}, failed
	}
	r := rgResult{stdout: stdout.buf.Bytes(), stderr: stderr.text(), cut: stdout.total > int64(stdout.buf.Len())}
	if r.cut {
		// Stopped by stopWriter, not by its time limit.
		return r, ""
	}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		r.timedOut = true
	case err == nil:
	case errors.As(err, &exitErr):
		r.exit = exitErr.ExitCode()
		if r.exit < 0 {
			r.exit = 2
		}
	default:
		return rgResult{}, err.Error()
	}
	return r, ""
}

// ripgrepCommand is ripgrep with args in dir, held as s.search says: an
// empty environment, not this server's, which holds the run's secrets, and
// as searchID where the server is root.
func (s *server) ripgrepCommand(ctx context.Context, dir string, args, read []string) *exec.Cmd {
	name, argv := s.search.argv(s.rgBin, read, append(append([]string{}, rgFixedArgs...), args...))
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Env = []string{}
	cmd.Dir = dir
	if s.search.asSearch {
		runAs(cmd, searchID)
	}
	return cmd
}

// stopWriter is a capWriter that stops ripgrep once its output passes the
// cap, rather than letting it run on to its time limit unread.
type stopWriter struct {
	capWriter
	stop context.CancelFunc
}

func (w *stopWriter) Write(p []byte) (int, error) {
	n, err := w.capWriter.Write(p)
	if w.total > w.max {
		w.stop()
	}
	return n, err
}

// rgFailed is a tool's answer when ripgrep ended in error with nothing to
// show: its refusal of the call's pattern or option, the agent's own to
// fix, or else a fault of the run's machinery (a search killed at its
// memory limit, say), with ripgrep's first line. "" when there is output or
// no error.
func (s *server) rgFailed(tool string, r rgResult) string {
	if r.exit != 2 || len(r.stdout) > 0 || r.timedOut {
		return ""
	}
	if why := rgRefusal(r.stderr); why != "" {
		return tool + ": ripgrep refused the search: " + strings.TrimSpace(why)
	}
	s.callFault = true
	first, _, _ := strings.Cut(strings.TrimSpace(r.stderr), "\n")
	if first == "" {
		first = "no message (stopped at its memory limit?)"
	}
	return tool + ": the search failed: " + first
}

// rgRefusal is ripgrep's own refusal of a call's pattern or option, from
// its stderr, or "" when it refused nothing.
func rgRefusal(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "regex parse error"), strings.Contains(line, "exceeds size limit"),
			strings.Contains(line, "unrecognized file type"), strings.Contains(line, "error parsing glob"):
			return strings.TrimPrefix(stderr, "rg: ")
		}
	}
	return ""
}

// unreadable counts the files ripgrep could not read (a workspace file
// only its owner may read, as ripgrep's user is not root).
func unreadable(stderr string) int {
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "Permission denied") || strings.Contains(line, "os error 13") {
			n++
		}
	}
	return n
}

// grepFlags is grep's own options as ripgrep flags, from the closed set
// a call may ask for; each value travels in the same argument as its flag,
// so none can be read as another flag.
func grepFlags(glob, typ string, ignoreCase, fixed bool) []string {
	var f []string
	if glob != "" {
		f = append(f, "--glob="+glob)
	}
	if typ != "" {
		f = append(f, "--type="+typ)
	}
	if ignoreCase {
		f = append(f, "--ignore-case")
	}
	if fixed {
		f = append(f, "--fixed-strings")
	}
	return append(f, rgSkipGit)
}

// checkGlob holds a call's glob to its bounds.
func checkGlob(name, glob string) error {
	switch {
	case len(glob) > maxGlobPattern:
		return fmt.Errorf("%s is longer than %d bytes", name, maxGlobPattern)
	case strings.ContainsAny(glob, "\x00\n"):
		return fmt.Errorf("%s holds a NUL or a newline", name)
	}
	return nil
}

func (s *server) toolGrep(raw json.RawMessage) (string, bool) {
	var a struct {
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Glob         string `json:"glob"`
		Type         string `json:"type"`
		IgnoreCase   bool   `json:"ignoreCase"`
		FixedStrings bool   `json:"fixedStrings"`
		Context      int    `json:"context"`
		Output       string `json:"output"`
		MaxResults   int    `json:"maxResults"`
		Offset       int    `json:"offset"`
		View         string `json:"view"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "grep: " + err.Error(), true
	}
	if a.Output == "" {
		a.Output = "files"
	}
	switch {
	case a.Pattern == "":
		return "grep: pattern is required", true
	case len(a.Pattern) > maxGrepPattern:
		return fmt.Sprintf("grep: pattern is longer than %d bytes", maxGrepPattern), true
	case strings.ContainsAny(a.Pattern, "\x00\n"):
		return "grep: pattern holds a NUL or a newline; a match is one line (use \\n nowhere)", true
	case !slices.Contains(grepOutputs, a.Output):
		return fmt.Sprintf("grep: output %q is not one of %s", a.Output, strings.Join(grepOutputs, ", ")), true
	case a.Context < 0 || a.Context > maxGrepContext:
		return fmt.Sprintf("grep: context must be 0 to %d", maxGrepContext), true
	case a.Context > 0 && a.Output != "content":
		return `grep: context applies to output "content" alone`, true
	case a.Offset < 0 || a.MaxResults < 0:
		return "grep: offset and maxResults must be >= 0", true
	case a.Type != "" && !rgTypeName.MatchString(a.Type):
		return fmt.Sprintf("grep: %q is not a file type name", a.Type), true
	}
	if err := checkGlob("glob", a.Glob); err != nil {
		return "grep: " + err.Error(), true
	}
	sc, err := s.scope(ToolGrep, a.Path, a.View)
	if err != nil {
		return s.failed("grep", err)
	}
	if sc.byName {
		// ripgrep searches a file given by name whatever --glob and --type
		// say, so the inputs' glob is the server's to apply.
		if a.Type != "" {
			return fmt.Sprintf("grep: type does not apply to %s; pass a glob (\"*.%s\")", s.spec.InputsDir, a.Type), true
		}
		if a.Glob != "" {
			keep, err := globMatcher(a.Glob)
			if err != nil {
				return "grep: " + err.Error(), true
			}
			var paths []string
			for i, p := range sc.paths {
				if keep(sc.names[i]) {
					paths = append(paths, p)
				}
			}
			sc.paths, a.Glob = paths, ""
		}
	}
	if len(sc.paths) == 0 {
		return "no file in " + sc.area + " to search", false
	}
	args := grepFlags(a.Glob, a.Type, a.IgnoreCase, a.FixedStrings)
	switch a.Output {
	case "files":
		args = append(args, "--files-with-matches", "--null")
	case "count":
		// --with-filename: given one file, ripgrep counts without its name.
		args = append(args, "--count", "--with-filename", "--null")
	case "content":
		args = append(args, "--json")
		if a.Context > 0 {
			args = append(args, "--context="+strconv.Itoa(a.Context))
		}
	}
	args = append(args, "-e", a.Pattern)
	if s.spec.ShareChecks != nil && sc.area == "workspace" {
		// A file in the share is searched only once checked: the rest are
		// left out of the answer, and the answer says how many.
		verdicts, err := s.shareVerdicts()
		if err != nil {
			return "grep: " + err.Error(), true
		}
		seen := map[string]bool{}
		sc.withheld, sc.withheldN = s.shareWithheld(verdicts), &seen
	}
	r, failed := s.runRipgrep("grep", args, sc)
	if failed != "" {
		return failed, true
	}
	if failed := s.rgFailed("grep", r); failed != "" {
		return failed, true
	}
	var lines []string
	switch a.Output {
	case "files":
		for _, p := range splitNul(r.stdout) {
			if p = sc.show(p); sc.keep(p) {
				lines = append(lines, p)
			}
		}
	case "count":
		lines = countLines(r.stdout, sc)
	case "content":
		lines = contentLines(r.stdout, sc, a.Context > 0)
	}
	s.callPersonsInput = sc.personsInput
	answer := s.searchAnswer(lines, a.MaxResults, a.Offset, r, "no file in "+sc.area+" matches", "narrow path, glob or type")
	if sc.withheldN != nil && len(*sc.withheldN) > 0 {
		answer += fmt.Sprintf("\n(matches in %d file(s) of the share were left out: they are not yet checked for malware, or ClamAV found something in them; a file is checked before your next turn)", len(*sc.withheldN))
	}
	return answer, false
}

func (s *server) toolGlob(raw json.RawMessage) (string, bool) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxResults int    `json:"maxResults"`
		Offset     int    `json:"offset"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "glob: " + err.Error(), true
	}
	switch {
	case a.Pattern == "":
		return "glob: pattern is required", true
	case a.Offset < 0 || a.MaxResults < 0:
		return "glob: offset and maxResults must be >= 0", true
	}
	if err := checkGlob("pattern", a.Pattern); err != nil {
		return "glob: " + err.Error(), true
	}
	sc, err := s.scope(ToolGlob, a.Path, "")
	if err != nil {
		return s.failed("glob", err)
	}
	if len(sc.paths) == 0 {
		return "no file to list", false
	}
	args := []string{"--files", "--null", "--glob=" + a.Pattern, rgSkipGit}
	r, failed := s.runRipgrep("glob", args, sc)
	if failed != "" {
		return failed, true
	}
	if failed := s.rgFailed("glob", r); failed != "" {
		return failed, true
	}
	var lines []string
	for _, p := range splitNul(r.stdout) {
		if p = sc.show(p); !sc.hidden[p] {
			lines = append(lines, p)
		}
	}
	return s.searchAnswer(lines, a.MaxResults, a.Offset, r, "no file in "+sc.area+" matches the pattern", "narrow path or pattern"), false
}

// searchAnswer is a search's result lines from offset: at most maxResults
// of them, and no more than fit the result budget, with what it left out
// and what it could not read said after them.
func (s *server) searchAnswer(lines []string, maxResults, offset int, r rgResult, none, narrow string) string {
	var notes []string
	if n := unreadable(r.stderr); n > 0 {
		notes = append(notes, fmt.Sprintf("files the search could not read, as only their owner may: %d", n))
	}
	if r.timedOut {
		notes = append(notes, fmt.Sprintf("the search stopped at its %s limit, so this is part of the answer; narrow it", s.searchTimeout))
	}
	if r.cut {
		notes = append(notes, "the search's output passed its limit, so this is part of the answer; narrow it")
	}
	tail := ""
	if len(notes) > 0 {
		tail = "\n[" + strings.Join(notes, "; ") + "]"
	}
	if len(lines) == 0 {
		return none + tail
	}
	if maxResults == 0 {
		maxResults = defaultGrepResults
	}
	maxResults = min(maxResults, maxGrepResults)
	total := len(lines)
	if offset >= total {
		return fmt.Sprintf("(offset %d is past the last of the %d results)", offset, total) + tail
	}
	room := s.spec.Limits.budget().less(tail).room()
	var out strings.Builder
	end := offset
	for end < total && end-offset < maxResults {
		add := len(lines[end])
		if out.Len() > 0 {
			add++
		}
		if int64(out.Len()+add) > room.bytes || end-offset+1 > room.lines {
			break
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(lines[end])
		end++
	}
	if offset > 0 || end < total {
		fmt.Fprintf(&out, "\n[results %d–%d of %d", offset+1, end, total)
		if end < total {
			fmt.Fprintf(&out, "; continue with offset %d, or %s", end, narrow)
		}
		out.WriteString("]")
	}
	return out.String() + tail
}

// show is the path an answer gives for p, a path ripgrep answered with.
func (sc searchScope) show(p string) string {
	if d, ok := sc.display[p]; ok {
		return d
	}
	return path.Join(sc.dir, p)
}

// splitNul splits ripgrep's paths, each ended by a NUL (--null with
// --files or --files-with-matches); what follows the last NUL, a path the
// output limit cut short, is left out.
func splitNul(out []byte) []string {
	parts := bytes.Split(out, []byte{0})
	// The last part is what follows the last NUL: empty, or a cut path.
	parts = parts[:len(parts)-1]
	paths := make([]string, 0, len(parts))
	for _, p := range parts {
		paths = append(paths, string(p))
	}
	return paths
}

// countLines is --count --null's answer, path NUL count a line, as
// "path: N".
func countLines(out []byte, sc searchScope) []string {
	var lines []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		p, n, ok := bytes.Cut(line, []byte{0})
		if !ok {
			continue
		}
		if shown := sc.show(string(p)); sc.keep(shown) {
			lines = append(lines, shown+": "+string(n))
		}
	}
	return lines
}

// rgEvent is the part of one --json event the answer needs.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

// rgText is ripgrep's text; a path or line that is not UTF-8 comes as
// base64 "bytes" instead, which the answer does not show.
type rgText struct {
	Text *string `json:"text"`
}

// contentLines is --json's matches and context as grep lines: a match as
// path:line: text, a context line as path-line- text, and, with context,
// "--" between groups that do not touch.
func contentLines(out []byte, sc searchScope, context bool) []string {
	var lines []string
	lastPath, lastLine := "", 0
	for _, raw := range bytes.Split(out, []byte("\n")) {
		var ev rgEvent
		if len(raw) == 0 || json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if ev.Type != "match" && ev.Type != "context" {
			continue
		}
		p := "?"
		if ev.Data.Path.Text != nil {
			p = sc.show(*ev.Data.Path.Text)
		}
		if !sc.keep(p) {
			continue
		}
		if context && lastPath != "" && (p != lastPath || ev.Data.LineNumber > lastLine+1) {
			lines = append(lines, "--")
		}
		lastPath, lastLine = p, ev.Data.LineNumber
		text := "[a line that is not UTF-8]"
		if ev.Data.Lines.Text != nil {
			at := 0
			if len(ev.Data.Submatches) > 0 {
				at = ev.Data.Submatches[0].Start
			}
			text = grepLine(*ev.Data.Lines.Text, at)
		}
		sep := ":"
		if ev.Type == "context" {
			sep = "-"
		}
		lines = append(lines, p+sep+strconv.Itoa(ev.Data.LineNumber)+sep+" "+text)
	}
	return lines
}

// grepLine is a matching line for an answer: its line ending dropped, tabs
// as spaces, and at most grepLineBytes of it around the match at byte at.
func grepLine(line string, at int) string {
	line = strings.TrimRight(line, "\r\n")
	if len(line) <= grepLineBytes {
		return strings.ReplaceAll(line, "\t", " ")
	}
	start := max(0, min(at, len(line))-grepLineBytes/3)
	for start > 0 && !utf8.RuneStart(line[start]) {
		start--
	}
	end := min(len(line), start+grepLineBytes)
	for end < len(line) && !utf8.RuneStart(line[end]) {
		end--
	}
	out := strings.ReplaceAll(line[start:end], "\t", " ")
	if start > 0 {
		out = "…" + out
	}
	if end < len(line) {
		out += "…"
	}
	return out
}

// ripgrepPath is ripgrep's binary in the runner image.
func ripgrepPath() string { return filepath.Join(RipgrepDir, "rg") }

// globMatcher matches a path, relative to the folder a call named, as
// ripgrep's --glob would: * and ? within one folder, ** across folders,
// [...] and {a,b}; a glob with no slash matches a file's name at any
// depth; a leading ! keeps what does not match. For the inputs, which
// ripgrep is given by name and so filters by no glob of its own.
func globMatcher(glob string) (func(string) bool, error) {
	neg := strings.HasPrefix(glob, "!")
	g := strings.TrimPrefix(glob, "!")
	anyDepth := !strings.Contains(g, "/")
	g = strings.TrimPrefix(g, "/")
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case strings.HasPrefix(g[i:], "**/"):
			re.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			re.WriteString(".*")
			i++
		case c == '*':
			re.WriteString("[^/]*")
		case c == '?':
			re.WriteString("[^/]")
		case c == '[':
			end := strings.IndexByte(g[i+1:], ']')
			if end < 0 {
				return nil, fmt.Errorf("glob %q has an unclosed [", glob)
			}
			class := g[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			re.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case c == '{':
			end := strings.IndexByte(g[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("glob %q has an unclosed {", glob)
			}
			alts := strings.Split(g[i+1:i+1+end], ",")
			for k, alt := range alts {
				alts[k] = regexp.QuoteMeta(alt)
			}
			re.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end + 1
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")
	m, err := regexp.Compile(re.String())
	if err != nil {
		return nil, fmt.Errorf("glob %q: %v", glob, err)
	}
	return func(rel string) bool {
		if anyDepth {
			rel = path.Base(rel)
		}
		return m.MatchString(rel) != neg
	}, nil
}
