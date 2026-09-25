// The knowledge tools (plan knowledge.md §4.5, agentry-cli#317): a run
// searches the knowledge packs the person attached — LLM wikis of at most
// 10 MB, mounted read-only under KnowledgeDir, one directory per pack —
// through three tools that exist only while a pack is attached. Every path
// funnels through resolveUnder, rooted at the pack's wiki/ or sources/,
// and a pack is reached only by a name the spec lists, never by a path.
//
// A pack's layout (knowledge/v1, pkg/knowledge in the CLI, which this
// stdlib-only package does not import): manifest.json; wiki/ holding
// index.md, log.md and the compiled pages, each with YAML frontmatter
// (title, summary) and citations written [src: doc:<path>]; sources/
// holding the documents the pages were compiled from. knowledge_read takes
// a page as the index writes it, relative to wiki/, or a citation's
// doc:<path> for a source, so the agent can follow a citation as written.

package toolsrv

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// defaultSearchResults and maxSearchResults bound knowledge_search.
	defaultSearchResults = 20
	maxSearchResults     = 50
	// maxMatchLine caps the matched line quoted in a search result.
	maxMatchLine = 200
	// maxPackName bounds a pack name, which is also a directory name.
	maxPackName = 64
	// citeSource is the citation key prefix that names a source document.
	citeSource = "doc:"
)

// validateKnowledge holds the knowledge half of validateSpec: packs need
// an absolute mount, and each name must be one directory component the
// tools can join under it without a path check of its own.
func validateKnowledge(spec *Spec) error {
	if spec.KnowledgeDir != "" && !filepath.IsAbs(spec.KnowledgeDir) {
		return fmt.Errorf("knowledgeDir %q must be an absolute container path when set", spec.KnowledgeDir)
	}
	if len(spec.Knowledge) == 0 {
		return nil
	}
	if spec.KnowledgeDir == "" {
		return fmt.Errorf("knowledgeDir must be set when knowledge packs are attached")
	}
	seen := map[string]bool{}
	for _, p := range spec.Knowledge {
		if !validPackName(p.Name) {
			return fmt.Errorf("knowledge pack name %q must be 1-%d lowercase letters, digits, '.', '-' or '_', starting with a letter or digit", p.Name, maxPackName)
		}
		if seen[p.Name] {
			return fmt.Errorf("knowledge pack %q is attached twice", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// validPackName is pkg/knowledge's name rule, restated here because this
// package imports nothing outside the standard library.
func validPackName(name string) bool {
	if name == "" || len(name) > maxPackName {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '-' || r == '_'):
		default:
			return false
		}
	}
	return true
}

func knowledgeListSchema() map[string]any {
	return objSchema(map[string]any{
		"pack":    strProp("the pack to list; may be omitted when one pack is attached (omit it with several to list the packs)"),
		"section": strProp("a section of the pack's wiki to list (a directory under wiki/, e.g. \"topics\"); omit for the pack's index"),
	})
}

func knowledgeSearchSchema() map[string]any {
	return objSchema(map[string]any{
		"pack":  strProp("the pack to search; may be omitted when one pack is attached"),
		"query": strProp("a word or a phrase; case does not matter, and a page holding every word matches when the phrase is nowhere"),
		"scope": strProp("wiki (the compiled pages, the default), sources (the documents they cite), or all"),
		"limit": intProp(fmt.Sprintf("the most results to return (default %d, at most %d)", defaultSearchResults, maxSearchResults)),
	}, "query")
}

func knowledgeReadSchema() map[string]any {
	return objSchema(map[string]any{
		"pack":     strProp("the pack to read from; may be omitted when one pack is attached"),
		"path":     strProp("a page as the index writes it (relative to the pack's wiki, e.g. topics/billing.md), or a citation's doc:<path> for a source document"),
		"offset":   intProp("byte offset to start reading from (default 0)"),
		"maxBytes": intProp("cap on returned bytes; the server limit still applies"),
	}, "path")
}

// pack resolves the pack a call names: the one attached pack when name is
// empty, else the listed pack of that name. The error is the model's text.
func (s *server) pack(name string) (string, string, error) {
	names := make([]string, 0, len(s.spec.Knowledge))
	for _, p := range s.spec.Knowledge {
		names = append(names, p.Name)
	}
	if name == "" {
		if len(names) == 1 {
			return names[0], filepath.Join(s.spec.KnowledgeDir, names[0]), nil
		}
		return "", "", fmt.Errorf("several packs are attached; pass pack (one of: %s)", strings.Join(names, ", "))
	}
	for _, n := range names {
		if n == name {
			return n, filepath.Join(s.spec.KnowledgeDir, n), nil
		}
	}
	return "", "", fmt.Errorf("no pack named %q is attached (attached: %s)", name, strings.Join(names, ", "))
}

// ---- knowledge_list ----

func (s *server) toolKnowledgeList(raw json.RawMessage) (string, bool) {
	var a struct {
		Pack    string `json:"pack"`
		Section string `json:"section"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "knowledge_list: " + err.Error(), true
	}
	if a.Pack == "" && a.Section == "" && len(s.spec.Knowledge) > 1 {
		return s.listPacks(), false
	}
	name, root, err := s.pack(a.Pack)
	if err != nil {
		return "knowledge_list: " + err.Error(), true
	}
	wiki := filepath.Join(root, "wiki")
	rel := path.Clean(a.Section)
	if a.Section == "" || rel == "." {
		text, isErr, err := readPaged(filepath.Join(wiki, "index.md"), 0, 0, s.spec.Limits.readCap())
		if err != nil {
			return "knowledge_list: " + name + " has no readable index: " + err.Error(), true
		}
		return text, isErr
	}
	dir, err := resolveUnder(wiki, rel)
	if err != nil {
		return "knowledge_list: " + err.Error(), true
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return fmt.Sprintf("knowledge_list: no section %q in %s", rel, name), true
	}
	if idx := filepath.Join(dir, "index.md"); isRegular(idx) {
		text, isErr, err := readPaged(idx, 0, 0, s.spec.Limits.readCap())
		if err != nil {
			return "knowledge_list: " + err.Error(), true
		}
		if isErr {
			return text, true
		}
		// A section's index links its pages by name, as markdown does
		// beside them; say how the tools reach them.
		return fmt.Sprintf("(section %s of %s: read a page with knowledge_read, path %s/<page> or the page's name alone)\n\n", rel, name, rel) + text, false
	}
	var lines []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		relPage, _ := filepath.Rel(wiki, p)
		b, err := os.ReadFile(p) //nolint:gosec // under the pack's wiki, a regular file
		if err != nil {
			return nil
		}
		title, summary := frontmatter(string(b))
		line := filepath.ToSlash(relPage)
		switch {
		case title != "" && summary != "":
			line += "\t" + title + " — " + summary
		case title != "":
			line += "\t" + title
		}
		lines = append(lines, line)
		return nil
	})
	if len(lines) == 0 {
		return fmt.Sprintf("(no pages in section %q of %s)", rel, name), false
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), false
}

// listPacks names each attached pack with its page count, read from the
// pack's own manifest.json.
func (s *server) listPacks() string {
	var b strings.Builder
	b.WriteString("Knowledge packs attached to this run (pass pack to choose one):")
	for _, p := range s.spec.Knowledge {
		b.WriteString("\n" + p.Name)
		var m struct {
			Pages int `json:"pages"`
		}
		if raw, err := os.ReadFile(filepath.Join(s.spec.KnowledgeDir, p.Name, "manifest.json")); err == nil && json.Unmarshal(raw, &m) == nil {
			b.WriteString("\t" + strconv.Itoa(m.Pages) + " pages")
		}
	}
	return b.String()
}

// ---- knowledge_search ----

// A match's rank, by where it was found: the phrase as written above every
// word apart, and a title above a summary above a body line.
const (
	rankWordsBody = iota + 1
	rankWordsSummary
	rankWordsTitle
	rankPhraseBody
	rankPhraseSummary
	rankPhraseTitle
)

type searchHit struct {
	path, title, where string
	rank               int
}

func (s *server) toolKnowledgeSearch(raw json.RawMessage) (string, bool) {
	var a struct {
		Pack  string `json:"pack"`
		Query string `json:"query"`
		Scope string `json:"scope"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "knowledge_search: " + err.Error(), true
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "knowledge_search: query is required", true
	}
	var dirs []string
	switch a.Scope {
	case "", "wiki":
		dirs = []string{"wiki"}
	case "sources":
		dirs = []string{"sources"}
	case "all":
		dirs = []string{"wiki", "sources"}
	default:
		return "knowledge_search: scope must be wiki, sources or all", true
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultSearchResults
	}
	limit = min(limit, maxSearchResults)
	name, root, err := s.pack(a.Pack)
	if err != nil {
		return "knowledge_search: " + err.Error(), true
	}

	phrase := strings.ToLower(query)
	words := strings.Fields(phrase)
	var hits []searchHit
	for _, d := range dirs {
		base := filepath.Join(root, d)
		_ = filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			// Regular files only: a link, planted or copied, is never
			// followed out of the pack.
			if err != nil || !e.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			rel = filepath.ToSlash(rel)
			shown := rel
			if d == "wiki" {
				if !strings.HasSuffix(rel, ".md") || path.Base(rel) == "index.md" || rel == "log.md" {
					return nil
				}
			} else {
				shown = citeSource + rel
			}
			b, err := os.ReadFile(p) //nolint:gosec // under the pack, a regular file
			if err != nil || !utf8.Valid(b) {
				return nil
			}
			if h, ok := matchPage(string(b), phrase, words); ok {
				h.path = shown
				hits = append(hits, h)
			}
			return nil
		})
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no match for %q in %s", query, name), false
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank > hits[j].rank
		}
		return hits[i].path < hits[j].path
	})
	noun := "matches"
	if len(hits) == 1 {
		noun = "match"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s for %q in %s", len(hits), noun, query, name)
	if len(hits) > limit {
		fmt.Fprintf(&b, " (showing %d; raise limit or narrow the query)", limit)
		hits = hits[:limit]
	}
	b.WriteString(":")
	for _, h := range hits {
		b.WriteString("\n" + h.path + "\t" + h.title + "\t" + h.where)
	}
	return b.String(), false
}

// matchPage finds query in one page: the phrase in its title, its summary
// or a body line, else every word somewhere in the page, placed where they
// all appear together. Line numbers count from the top of the file.
func matchPage(text, phrase string, words []string) (searchHit, bool) {
	title, summary := frontmatter(text)
	lines := strings.Split(text, "\n")
	first := bodyStart(lines)
	lt, ls := strings.ToLower(title), strings.ToLower(summary)
	h := searchHit{title: title}

	switch {
	case strings.Contains(lt, phrase):
		h.rank, h.where = rankPhraseTitle, "title"
		return h, true
	case strings.Contains(ls, phrase):
		h.rank, h.where = rankPhraseSummary, "summary"
		return h, true
	}
	for i := first; i < len(lines); i++ {
		if strings.Contains(strings.ToLower(lines[i]), phrase) {
			h.rank, h.where = rankPhraseBody, lineRef(i, lines[i])
			return h, true
		}
	}
	if len(words) < 2 || !containsAll(strings.ToLower(text), words) {
		return h, false
	}
	switch {
	case containsAll(lt, words):
		h.rank, h.where = rankWordsTitle, "title"
	case containsAll(ls, words):
		h.rank, h.where = rankWordsSummary, "summary"
	default:
		h.rank, h.where = rankWordsBody, ""
		for i := first; i < len(lines); i++ {
			if containsAll(strings.ToLower(lines[i]), words) {
				h.where = lineRef(i, lines[i])
				break
			}
		}
		if h.where == "" {
			h.where = "across the page"
		}
	}
	return h, true
}

func containsAll(s string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

func lineRef(i int, line string) string {
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > maxMatchLine {
		line = string([]rune(line)[:maxMatchLine]) + "…"
	}
	return "line " + strconv.Itoa(i+1) + ": " + line
}

// frontmatter reads a page's title and summary from its YAML frontmatter:
// the flat "key: value" lines between a leading "---" and the next, which
// is all the knowledge/v1 page shape puts there that a search reads.
// pkg/knowledge parses the same block more fully for its lint; this is the
// part the tools need, restated because this package imports nothing
// outside the standard library.
func frontmatter(text string) (title, summary string) {
	lines := strings.Split(text, "\n")
	end := bodyStart(lines)
	for _, l := range lines[1:max(end-1, 1)] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "title":
			title = v
		case "summary":
			summary = v
		}
	}
	return title, summary
}

// bodyStart is the index of the first line after the frontmatter block,
// or 0 when the page has none.
func bodyStart(lines []string) int {
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return i + 1
		}
	}
	return 0
}

// ---- knowledge_read ----

func (s *server) toolKnowledgeRead(raw json.RawMessage) (string, bool) {
	var a struct {
		Pack     string `json:"pack"`
		Path     string `json:"path"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"maxBytes"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "knowledge_read: " + err.Error(), true
	}
	if a.Path == "" {
		return "knowledge_read: path is required", true
	}
	if a.Offset < 0 {
		return "knowledge_read: offset must be >= 0", true
	}
	_, root, err := s.pack(a.Pack)
	if err != nil {
		return "knowledge_read: " + err.Error(), true
	}
	base, rel := filepath.Join(root, "wiki"), a.Path
	if src, ok := strings.CutPrefix(a.Path, citeSource); ok {
		base, rel = filepath.Join(root, "sources"), src
	}
	abs, err := resolveUnder(base, rel)
	if err != nil {
		return "knowledge_read: " + err.Error(), true
	}
	if _, err := os.Lstat(abs); err != nil && base == filepath.Join(root, "wiki") && !strings.Contains(rel, "/") {
		// A section's index links its pages by name alone. A page's name is
		// unique in its pack (the format's lint), so a bare name that is not
		// at the top of the wiki is found where it is.
		found, ferr := findPage(base, rel)
		if ferr != nil {
			return "knowledge_read: " + ferr.Error(), true
		}
		if found != "" {
			abs = found
		}
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return fmt.Sprintf("knowledge_read: %q is a directory (use knowledge_list)", path.Clean(a.Path)), true
	}
	text, isErr, err := readPaged(abs, a.Offset, a.MaxBytes, s.spec.Limits.readCap())
	if err != nil {
		return "knowledge_read: " + err.Error(), true
	}
	return text, isErr
}

// findPage finds the regular file named name under wiki, or "" when there is
// none; two of that name is an error naming both.
func findPage(wiki, name string) (string, error) {
	var hits []string
	_ = filepath.WalkDir(wiki, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && d.Name() == name {
			hits = append(hits, p)
		}
		return nil
	})
	switch len(hits) {
	case 0:
		return "", nil
	case 1:
		return hits[0], nil
	}
	rels := make([]string, 0, len(hits))
	for _, h := range hits {
		r, _ := filepath.Rel(wiki, h)
		rels = append(rels, filepath.ToSlash(r))
	}
	sort.Strings(rels)
	return "", fmt.Errorf("%q names several pages (%s); pass the one you mean", name, strings.Join(rels, ", "))
}

func isRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}
