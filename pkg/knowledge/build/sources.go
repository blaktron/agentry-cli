package build

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
)

// A build may take a git repository or a file at a web address beside its
// files. Each is an input source of the run's kind (pkg/runner/sources),
// fetched before the decode by the same step a run's source takes
// (engine.SourceFetcher): the runner image's source role in the files
// container, on a network of its own behind a proxy to the source's host
// alone, the token on the step's stdin and nowhere else. What it fetches is
// staged as documents under names a citation carries, decoded as a run
// decodes it (a repository under the source rule, a file as an upload),
// counted against the pack's cap, and kept in the pack under sources/.

// MaxURLs bounds the addresses one build takes.
const MaxURLs = 20

// MaxRepos bounds the repositories one build takes.
const MaxRepos = 10

// ParseSources turns the repositories (an https address, with @ref or #ref
// for a branch, a tag or a commit other than the default branch) and the
// file addresses a build was given into sources, each with the id its
// documents are staged under: repo-<host>-<path> for a repository, which is
// also the folder the pack keeps it in, and url-<n> for a file, whose
// document is the file's own name once it is fetched. Tokens are not
// resolved here (sources.ResolveTokens).
func ParseSources(repos, urls []string) ([]*sources.Source, error) {
	if len(repos) > MaxRepos {
		return nil, fmt.Errorf("a build takes at most %d repositories", MaxRepos)
	}
	if len(urls) > MaxURLs {
		return nil, fmt.Errorf("a build takes at most %d addresses", MaxURLs)
	}
	var out []*sources.Source
	ids := map[string]string{}
	for _, raw := range repos {
		s, err := parseRepo(raw)
		if err != nil {
			return nil, fmt.Errorf("--repo %s: %w", raw, err)
		}
		if prev, dup := ids[s.ID]; dup {
			return nil, fmt.Errorf("--repo %s and --repo %s would both be the documents %s/; give one", prev, raw, s.ID)
		}
		ids[s.ID] = raw
		out = append(out, s)
	}
	kind, _ := sources.Lookup(manifest.IntakeURL)
	for i, raw := range urls {
		s, err := kind.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("--url %s: %w", raw, err)
		}
		s.ID = fmt.Sprintf("url-%d", i+1)
		out = append(out, &s)
	}
	return out, nil
}

// parseRepo reads a repository's address with its ref after @ (or #, as a
// run's intake answer carries one), and names its id after its host and
// path: https://github.com/o/r@main is repo-github.com-o-r. An @ is the ref
// only in the path, after the host: before it, it would be credentials,
// which the kind refuses.
func parseRepo(raw string) (*sources.Source, error) {
	raw = strings.TrimSpace(raw)
	answer := raw
	if u, err := url.Parse(raw); err == nil && u.Fragment == "" && !strings.Contains(raw, "#") {
		if i := strings.LastIndex(u.Path, "@"); i >= 0 {
			addr := *u
			addr.Path, addr.RawPath = u.Path[:i], ""
			answer = addr.String() + "#" + u.Path[i+1:]
		}
	}
	kind, _ := sources.Lookup(manifest.IntakeRepository)
	s, err := kind.Parse(answer)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	id := "repo-" + safeName(s.Host)
	if p != "" {
		id += "-" + safeName(strings.ReplaceAll(p, "/", "-"))
	}
	s.ID = id
	return &s, nil
}

// FetchedSource is one source a build fetched, as the build reports it. It
// never holds the token.
type FetchedSource struct {
	// Kind is repository or url.
	Kind manifest.IntakeType `json:"kind"`
	// URL is the address as shown (a query hidden); Ref the ref asked for,
	// Commit the commit a repository resolved to.
	URL    string `json:"url"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Document is what the pack cites it under: a repository's folder, or
	// a file's name.
	Document string `json:"document"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	// LeftOut counts what a repository's fetch left out, by reason.
	LeftOut map[string]int `json:"leftOut,omitempty"`
	// Token says where the token came from (flag, keychain, prompt),
	// empty for an anonymous fetch.
	Token string        `json:"token,omitempty"`
	Took  time.Duration `json:"-"`

	src     sources.Source
	fetched sources.Fetched
}

// file says whether it is one file from a web address rather than a
// repository.
func (f FetchedSource) file() bool { return f.Kind == manifest.IntakeURL }

// At is the ref a repository was fetched at, in words.
func (f FetchedSource) At() string {
	if f.Ref == "" {
		return "its default branch"
	}
	return fmt.Sprintf("%q", f.Ref)
}

// Words say what a build brought in: a repository with its ref and commit,
// or a file, each with the document the pack cites it under.
func (f FetchedSource) Words() string {
	if f.file() {
		return fmt.Sprintf("file %s as %s, %d bytes", f.URL, f.Document, f.Bytes)
	}
	return fmt.Sprintf("repository %s at %s, commit %s, %d files as %s", f.URL, f.At(), f.Commit, f.Files, f.Document)
}

// stageState is what the documents already spend of the pack while its
// sources are fetched: the files the pack holds, the documents staged by
// name, the bytes they come to, and the pack's cap.
type stageState struct {
	held   int
	given  map[string]string
	staged int64
	limit  int64
}

// PageReserve is the room in a pack's 2,000 files a build keeps for the
// pages it writes, its index and its log, when it fetches a source: a
// source is fetched within what is left after it and the documents.
const PageReserve = 300

// fetchSources fetches each source into docsDir, where the documents are
// staged, within the pack's cap: a repository under its id, its files
// renamed to names a citation carries, and a file under its own name. It
// answers what it fetched and the ids the decode holds to the source rule.
func fetchSources(ctx context.Context, o Options, fetcher engine.SourceFetcher, docsDir string, st stageState) ([]FetchedSource, []string, error) {
	var (
		out     []FetchedSource
		repoIDs []string
	)
	given, staged, limit := st.given, st.staged, st.limit
	files := st.held + len(given)
	for _, s := range o.Sources {
		kind, ok := sources.Lookup(s.Kind)
		if !ok {
			return nil, nil, engine.Classify(engine.ExitUsage, sources.Unfetched(s.ID, s.Kind))
		}
		room := knowledge.MaxFiles - PageReserve - files
		if room <= 0 || staged >= limit {
			return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s %s: the pack and the documents already come to its cap (%d files, %d of them kept for the pages; %s), so nothing more can be fetched", kind.Noun, s.Shown(), knowledge.MaxFiles, PageReserve, knowledge.FormatCap(limit)))
		}
		caps := kind.CapsFor(o.Hosted).Within(room, limit-staged)
		dest := filepath.Join(docsDir, s.ID)
		if _, err := os.Lstat(dest); err == nil || given[s.ID] != "" {
			return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s %s would be the documents %s/, which a document given already is; rename it", kind.Noun, s.Shown(), s.ID))
		}
		f, err := fetcher.Fetch(ctx, *s, caps, dest)
		if err != nil {
			return nil, nil, err
		}
		got := FetchedSource{Kind: s.Kind, URL: s.Shown(), Ref: s.Ref, Commit: f.Source.Commit, Files: f.Source.Files, Bytes: f.Source.Bytes,
			LeftOut: f.Source.Skipped, Token: s.TokenFrom, Took: f.Took, src: *s, fetched: f}
		switch {
		case kind.AsUpload:
			// The one file, under its own name, as a document given with
			// --file would be; the folder the step wrote goes.
			name := safeName(f.Source.Name)
			if name == decode.DocumentsFile {
				return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s: a document may not be named %s, the Creator's own list; give a file under another name", s.Shown(), decode.DocumentsFile))
			}
			if prev, dup := given[name]; dup {
				return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s and %s would both be the document %s; give one", prev, s.Shown(), name))
			}
			if err := os.Rename(filepath.Join(dest, f.Source.Name), filepath.Join(docsDir, name)); err != nil {
				return nil, nil, engine.Classify(engine.ExitCLI, fmt.Errorf("stage %s: %w", s.Shown(), err))
			}
			if err := os.Remove(dest); err != nil {
				return nil, nil, engine.Classify(engine.ExitCLI, fmt.Errorf("stage %s: %w", s.Shown(), err))
			}
			given[name] = s.Shown()
			got.Document = name
		default:
			// A repository's files keep its layout under the id, each
			// name made one a citation carries.
			if err := plainTree(dest); err != nil {
				return nil, nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s %s: %w", kind.Noun, s.Shown(), err))
			}
			given[s.ID] = s.Shown()
			got.Document = s.ID + "/"
			repoIDs = append(repoIDs, s.ID)
		}
		files += f.Source.Files
		staged += f.Source.Bytes
		out = append(out, got)
	}
	return out, repoIDs, nil
}

// plainTree renames every file and folder under root to the name a
// citation carries (safeName), as a directory given with --file is staged.
// Two names that would become one are refused rather than one hiding the
// other.
func plainTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != root {
				dirs = append(dirs, p)
			}
			return nil
		}
		return renamePlain(p)
	})
	if err != nil {
		return err
	}
	// Deepest first, so a folder's new name is not walked twice.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := renamePlain(dir); err != nil {
			return err
		}
	}
	return nil
}

func renamePlain(p string) error {
	base := filepath.Base(p)
	plain := safeName(base)
	if plain == base {
		return nil
	}
	dst := filepath.Join(filepath.Dir(p), plain)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s and %s would both be the document %s; the pack cites only letters, digits, '.', '-' and '_'", base, plain, plain)
	}
	return os.Rename(p, dst)
}

// sourceLines are the receipt's lines for the sources a build fetched,
// keyed source.<document>, each as a run's source line reads, with the
// malware check of its files from the decode.
func sourceLines(fetched []FetchedSource, dec *decode.Result) map[string]string {
	if len(fetched) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, f := range fetched {
		checks := sources.ChecksFor(f.src, dec.Report.Files)
		if f.file() {
			// Staged under its own name, not a folder of its id.
			checks = sources.ChecksAsUploadNamed(f.Document, dec.Report.Files)
		}
		out["source."+strings.TrimSuffix(f.Document, "/")] = sources.Line(f.src, f.fetched, checks)
	}
	return out
}

// sourcesTask is the task's paragraph on the sources: what each folder or
// file is, and how to read a repository.
func sourcesTask(fetched []FetchedSource) string {
	if len(fetched) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nBrought in for this build:\n")
	for _, f := range fetched {
		if f.file() {
			fmt.Fprintf(&b, "- %s is the file from %s.\n", f.Document, f.URL)
			continue
		}
		fmt.Fprintf(&b, "- %s holds the git repository %s at %s (commit %s), %d %s: its README and docs first, then its code for what the docs leave out, and no generated file.\n",
			f.Document, f.URL, f.At(), f.Commit, f.Files, plural(f.Files, "file", "files"))
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// heldFiles counts the files a pack holds, under wiki/ and sources/: what
// its 2,000 already spend before a build adds to it.
func heldFiles(packDir string) int {
	n := 0
	for _, dir := range []string{knowledge.WikiDir, knowledge.SourcesDir} {
		_ = filepath.WalkDir(filepath.Join(packDir, dir), func(_ string, e fs.DirEntry, err error) error {
			if err == nil && e.Type().IsRegular() {
				n++
			}
			return nil
		})
	}
	return n
}

// keptFilesWithin holds what the pack will keep to its 2,000 files, before
// anything runs: the files it holds, each document read (and the text
// beside a decoded one), and the reserve for the pages. The store refuses
// a pack past it when it seals one, after the whole build.
func keptFilesWithin(held int, docs []decode.Document) error {
	n := held + PageReserve
	for _, d := range docs {
		if d.Read == "" {
			continue
		}
		n++
		if d.DecodedBy != "" {
			n++
		}
	}
	if n > knowledge.MaxFiles {
		return fmt.Errorf("the documents read would bring the pack to %d files with room for its pages; a knowledge pack holds at most %d: give fewer documents, or a narrower repository", n, knowledge.MaxFiles)
	}
	return nil
}
