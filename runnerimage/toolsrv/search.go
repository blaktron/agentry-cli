package toolsrv

// grep and glob (plan ripgrep-search.md §4–§6, agentry-cli#912): search
// the files a run can already read by regular expression, and list them
// by path pattern, with ripgrep. Like the scratch folder they are a grant
// of the runner, never a line of the contract: they read only the areas
// the run's other tools read — its inputs (not under the review grant,
// where every file reaches the model through AI Review), its attached
// knowledge packs, its scratch folder, and its workspace when the
// contract grants filesystem.read — one area a call, so each answer keeps
// its area's rules: a search of the inputs answers as search_inputs does,
// the person's own files unchanged (DR-78), and every other area as a text
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

// The search tools (plan ripgrep-search.md §4), granted with Search.
const (
	ToolGrep = "grep"
	ToolGlob = "glob"
)

// RipgrepDir is where the runner image holds ripgrep and its licences,
// copied by digest from agentry-ripgrep (agentry-dockerimages#81). It is
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

// Search is grep and glob's grant: Enabled by the compiler with the
// scratch folder, for every package but a built-in (plan ripgrep-search.md
// D4). The tools exist only when the run also has an area to search.
type Search struct {
	Enabled bool `json:"enabled"`
}

// rgFixedArgs are the flags every ripgrep run takes (plan §5): JSON or
// NUL-separated output the server parses, no config file, ignore files not
// read and hidden files searched so nothing the person gave is skipped,
// .git never searched, caps on file size, line width, threads and what a
// pattern may compile to, and a stable order so offset pages are the same
// pages.
var rgFixedArgs = []string{
	"--no-config", "--no-ignore", "--hidden", "--glob=!.git",
	"--max-filesize=8M", "--threads=2", "--sort=path",
	"--regex-size-limit=16M", "--dfa-size-limit=64M",
}

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
		(s.KnowledgeDir != "" && len(s.Knowledge) > 0) || s.FSRead.Enabled
}

// grepDescription and globDescription name the areas this run has, so
// the model is never told of one it cannot search.
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
	if s.spec.FSRead.Enabled {
		areas = append(areas, s.spec.WorkspaceDir+" (the workspace)")
	}
	return strings.Join(areas, ", ")
}

func (s *server) grepDescription() string {
	return "Search the contents of files with a regular expression (ripgrep), in one area at a time: " + s.searchAreaNames() + ". " +
		`output "files" (the default) lists the files that match, "content" gives each matching line as path:line: text (with context lines as path-line- text), "count" the matching lines per file. ` +
		"Paths come back as this run sees them; read a file with the area's own tool: read_input with the path after /inputs/, knowledge_read with the path after /knowledge/<pack>/wiki/ (or doc:<the path after sources/>), scratch_read or fs_read with the path after /scratch/ or /workspace/. " +
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
	// personsInput: the answer is the person's own input (DR-78).
	personsInput bool
	// names are byName's paths as a glob is matched against them, each
	// relative to the folder the call named.
	names []string
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
		return folderScope(a.name, a.dir, rel)
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
		sc.names = append(sc.names, strings.TrimPrefix(strings.TrimPrefix(t.path, rel), "/"))
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
	if rel == "." && len(grants) > 0 {
		sc := searchScope{area: "workspace", dir: s.spec.WorkspaceDir, read: []string{s.spec.WorkspaceDir}}
		for _, g := range grants {
			abs, err := resolveUnder(s.spec.WorkspaceDir, g)
			if err != nil {
				continue
			}
			if _, err := os.Stat(abs); err == nil {
				sc.paths = append(sc.paths, path.Clean(g))
			}
		}
		if len(sc.paths) == 0 {
			return searchScope{}, errors.New("none of the workspace's granted paths exists")
		}
		return sc, nil
	}
	if !allowedByGrant(grants, rel) {
		return searchScope{}, fmt.Errorf("%s is not in the workspace paths this QuickAgent may read", path.Join(s.spec.WorkspaceDir, rel))
	}
	return folderScope("workspace", s.spec.WorkspaceDir, rel)
}

// searchConfinement is how ripgrep is held (plan §5), chosen once when the
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
	var all rgResult
	for start := 0; start < len(sc.paths); start += searchTargetsPerRun {
		chunk := sc.paths[start:min(len(sc.paths), start+searchTargetsPerRun)]
		r, failed := s.runRipgrepOnce(sc.dir, append(append(append([]string{}, args...), "--"), chunk...), sc.read)
		if failed != "" {
			s.callFault = true
			return rgResult{}, tool + ": the search could not be started confined: " + failed
		}
		all.stdout = append(all.stdout, r.stdout...)
		all.stderr += r.stderr
		all.timedOut = all.timedOut || r.timedOut
		all.cut = all.cut || r.cut
		if r.exit > all.exit {
			all.exit = r.exit
		}
		if all.timedOut || all.cut || r.exit == 2 && len(r.stdout) == 0 && rgRefusal(r.stderr) != "" {
			break
		}
	}
	if all.exit == 0 && len(all.stdout) == 0 {
		all.exit = 1
	}
	return all, ""
}

func (s *server) runRipgrepOnce(dir string, args, read []string) (rgResult, string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.searchTimeout)
	defer cancel()
	name, argv := s.search.argv(s.rgBin, read, append(append([]string{}, rgFixedArgs...), args...))
	cmd := exec.CommandContext(ctx, name, argv...)
	// An empty environment, not this server's: it holds the run's secrets.
	cmd.Env = []string{}
	cmd.Dir = dir
	if s.search.asSearch {
		runAs(cmd, searchID)
	}
	stdout, stderr := &capWriter{max: maxSearchOutput}, &capWriter{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	failed, err := s.search.run(cmd)
	if failed != "" {
		return rgResult{}, failed
	}
	r := rgResult{stdout: stdout.buf.Bytes(), stderr: stderr.text(), cut: stdout.total > int64(stdout.buf.Len())}
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

// searchFlags is grep's own options as ripgrep flags, from the closed set
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
	return f
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
	case strings.ContainsRune(a.Pattern, 0):
		return "grep: pattern holds a NUL", true
	case !slices.Contains(grepOutputs, a.Output):
		return fmt.Sprintf("grep: output %q is not one of %s", a.Output, strings.Join(grepOutputs, ", ")), true
	case a.Context < 0 || a.Context > maxGrepContext:
		return fmt.Sprintf("grep: context must be 0 to %d", maxGrepContext), true
	case a.Context > 0 && a.Output != "content":
		return `grep: context applies to output "content" alone`, true
	case a.Offset < 0:
		return "grep: offset must be >= 0", true
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
		return fmt.Sprintf("no file in %s to search", path.Clean("/"+strings.TrimPrefix(a.Path, "/"))), false
	}
	args := grepFlags(a.Glob, a.Type, a.IgnoreCase, a.FixedStrings)
	switch a.Output {
	case "files":
		args = append(args, "--files-with-matches", "--null")
	case "count":
		args = append(args, "--count", "--null")
	case "content":
		args = append(args, "--json")
		if a.Context > 0 {
			args = append(args, "--context="+strconv.Itoa(a.Context))
		}
	}
	args = append(args, "-e", a.Pattern)
	r, failed := s.runRipgrep("grep", args, sc)
	if failed != "" {
		return failed, true
	}
	if r.exit == 2 && len(r.stdout) == 0 {
		if why := rgRefusal(r.stderr); why != "" {
			return "grep: ripgrep refused the search: " + strings.TrimSpace(why), true
		}
	}
	var lines []string
	switch a.Output {
	case "files":
		for _, p := range splitNul(r.stdout) {
			lines = append(lines, sc.show(p))
		}
	case "count":
		lines = countLines(r.stdout, sc)
	case "content":
		lines = contentLines(r.stdout, sc, a.Context > 0)
	}
	s.callPersonsInput = sc.personsInput && !s.spec.Review.Enabled
	return s.searchAnswer("grep", lines, a.MaxResults, a.Offset, r, "no file in "+sc.area+" matches", "narrow path, glob or type"), false
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
	case a.Offset < 0:
		return "glob: offset must be >= 0", true
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
	args := []string{"--files", "--null", "--glob=" + a.Pattern}
	r, failed := s.runRipgrep("glob", args, sc)
	if failed != "" {
		return failed, true
	}
	if r.exit == 2 && len(r.stdout) == 0 {
		if why := rgRefusal(r.stderr); why != "" {
			return "glob: ripgrep refused the pattern: " + strings.TrimSpace(why), true
		}
	}
	var lines []string
	for _, p := range splitNul(r.stdout) {
		lines = append(lines, sc.show(p))
	}
	return s.searchAnswer("glob", lines, a.MaxResults, a.Offset, r, "no file in "+sc.area+" matches the pattern", "narrow path or pattern"), false
}

// searchAnswer is a search's result lines from offset, at most maxResults
// of them and within the result budget, with what it could not read or
// left out said after them.
func (s *server) searchAnswer(tool string, lines []string, maxResults, offset int, r rgResult, none, narrow string) string {
	var notes []string
	if n := unreadable(r.stderr); n > 0 {
		notes = append(notes, fmt.Sprintf("%d files could not be read by the search (only their owner may read them)", n))
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
	if maxResults <= 0 {
		maxResults = defaultGrepResults
	}
	maxResults = min(maxResults, maxGrepResults)
	total := len(lines)
	if offset >= total {
		return fmt.Sprintf("(offset %d is past the last of the %d results)", offset, total) + tail
	}
	end := min(total, offset+maxResults)
	b := s.spec.Limits.budget().less(tail)
	page := b.listPage("", lines[:end], offset, narrow)
	if end < total && !strings.Contains(page, "continue with offset") {
		page += fmt.Sprintf("\n[results %d–%d of %d; continue with offset %d, or %s]", offset+1, end, total, end, narrow)
	}
	return page + tail
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
		lines = append(lines, sc.show(string(p))+": "+string(n))
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

type rgText struct {
	Text  *string `json:"text"`
	Bytes *string `json:"bytes"`
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
