package knowledge

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The mechanical lint (plan knowledge.md §4.2): a Go port of the estate
// wiki's own lint (agentry-notes wiki/tools/wiki-lint.py) — its link,
// frontmatter, slug and index checks — made general for a pack, plus the
// check that every citation resolves to a document under sources/, since a
// pack keeps its sources so that its citations resolve (plan D8). The
// Creator's apply step drops a page that fails, never the whole build.

// Rule names what a finding is about.
type Rule string

// The lint's rules.
const (
	RuleFrontmatter Rule = "frontmatter"
	RuleSlug        Rule = "slug"
	RuleLink        Rule = "link"
	RuleCitation    Rule = "citation"
	RuleIndex       Rule = "index"
	RuleLayout      Rule = "layout"
)

// Finding is one lint failure.
type Finding struct {
	// Page is the slash path relative to the pack, e.g. wiki/topics/x.md.
	Page string
	// Line is the 1-based line, or 0 for the file as a whole.
	Line int
	Rule Rule
	// Target is, for a link or a citation, the slash path relative to the
	// pack that it names and that is missing; empty otherwise.
	Target  string
	Message string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s: %s", f.Page, f.Line, f.Rule, f.Message)
	}
	return fmt.Sprintf("%s: %s: %s", f.Page, f.Rule, f.Message)
}

// requiredKeys are the frontmatter keys every page carries: the estate
// schema's, with verified dropped (a pack's pages are compiled, not
// re-checked against a live system).
var requiredKeys = []string{"title", "type", "summary", "sources", "updated"}

var (
	slugRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	dateRE     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	linkRE     = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	citationRE = regexp.MustCompile(`\[src:\s*([^\]]+)\]`)
	fenceRE    = regexp.MustCompile("(?s)```.*?```")
	codeRE     = regexp.MustCompile("`[^`\n]*`")
	schemeRE   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// Lint checks the pack at dir, whose name the index is generated under.
// The error is for I/O alone; every problem with the pack is a Finding.
func Lint(dir, name string) ([]Finding, error) {
	findings, err := lintPages(dir)
	if err != nil {
		return nil, err
	}
	drift, err := indexDrift(dir, name)
	if err != nil {
		return nil, err
	}
	return append(findings, drift...), nil
}

// lintPages runs every check but the index's.
func lintPages(dir string) ([]Finding, error) {
	files, err := treeFiles(dir)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, f := range files {
		present[f] = true
	}
	var out []Finding
	slugs := map[string]string{}
	for _, rel := range files {
		if !strings.HasPrefix(rel, WikiDir+"/") {
			continue
		}
		if !strings.HasSuffix(rel, ".md") {
			out = append(out, Finding{Page: rel, Rule: RuleLayout, Message: "the wiki holds only markdown pages; put documents under sources/"})
			continue
		}
		if !isPage(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // a listed regular file of the pack
		if err != nil {
			return nil, err
		}
		out = append(out, lintPage(rel, string(b), present)...)
		parts := strings.Split(strings.TrimPrefix(rel, WikiDir+"/"), "/")
		if len(parts) != 2 {
			out = append(out, Finding{Page: rel, Rule: RuleSlug, Message: "a page lives at wiki/<section>/<slug>.md"})
			continue
		}
		section, slug := parts[0], strings.TrimSuffix(parts[1], ".md")
		if !slugRE.MatchString(section) || !slugRE.MatchString(slug) {
			out = append(out, Finding{Page: rel, Rule: RuleSlug, Message: "a section and a slug are lowercase letters, digits and '-', starting with a letter or digit"})
			continue
		}
		if prev, dup := slugs[slug]; dup {
			out = append(out, Finding{Page: rel, Rule: RuleSlug, Message: "slug " + slug + " is also " + prev})
			continue
		}
		slugs[slug] = rel
	}
	return out, nil
}

// lintPage checks one page's frontmatter, links and citations.
func lintPage(rel, text string, present map[string]bool) []Finding {
	var out []Finding
	fm, ok := parseFrontmatter(text)
	if !ok {
		return append(out, Finding{Page: rel, Line: 1, Rule: RuleFrontmatter, Message: "no frontmatter (a --- block of title, type, summary, sources, updated)"})
	}
	for _, k := range requiredKeys {
		if _, has := fm.values[k]; !has {
			out = append(out, Finding{Page: rel, Line: 1, Rule: RuleFrontmatter, Message: "missing " + k})
		}
	}
	if v, has := fm.values["updated"]; has && !dateRE.MatchString(fm.str("updated")) {
		out = append(out, Finding{Page: rel, Line: 1, Rule: RuleFrontmatter, Message: fmt.Sprintf("updated %v is not a YYYY-MM-DD date", v)})
	}
	if t := fm.str("type"); t != "" && !slugRE.MatchString(t) {
		out = append(out, Finding{Page: rel, Line: 1, Rule: RuleFrontmatter, Message: "type " + t + " is not a lowercase word"})
	}

	body := stripCode(text)
	lines := strings.Split(body, "\n")
	pageDir := path.Dir(rel)
	for i := fm.bodyLine; i < len(lines); i++ {
		for _, m := range linkRE.FindAllStringSubmatch(lines[i], -1) {
			target := m[1]
			if strings.HasPrefix(target, "#") || schemeRE.MatchString(target) {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			resolved := path.Clean(path.Join(pageDir, target))
			switch {
			case strings.HasPrefix(target, "/") || resolved == ".." || strings.HasPrefix(resolved, "../"):
				out = append(out, Finding{Page: rel, Line: i + 1, Rule: RuleLink, Message: "link " + m[1] + " points outside the pack"})
			case !present[resolved]:
				out = append(out, Finding{Page: rel, Line: i + 1, Rule: RuleLink, Target: resolved, Message: "link " + m[1] + " resolves to nothing in the pack"})
			}
		}
		for _, m := range citationRE.FindAllStringSubmatch(lines[i], -1) {
			for _, part := range strings.Split(m[1], ";") {
				fields := strings.Fields(part)
				if len(fields) == 0 {
					continue
				}
				key := fields[0]
				src, isDoc := strings.CutPrefix(key, citeSource)
				switch {
				case !isDoc:
					out = append(out, Finding{Page: rel, Line: i + 1, Rule: RuleCitation, Message: "citation key " + key + " is not doc:<path> under sources/"})
				case !validSourcePath(src):
					out = append(out, Finding{Page: rel, Line: i + 1, Rule: RuleCitation, Message: "citation " + key + " is not a path under sources/"})
				case !present[SourcesDir+"/"+src]:
					out = append(out, Finding{Page: rel, Line: i + 1, Rule: RuleCitation, Target: SourcesDir + "/" + src, Message: "citation " + key + " names no document under sources/"})
				}
			}
		}
	}
	return out
}

// citeSource is the citation key prefix for a source document.
const citeSource = "doc:"

func validSourcePath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

// stripCode blanks fenced blocks and code spans, keeping line numbers.
func stripCode(s string) string {
	s = fenceRE.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat("\n", strings.Count(m, "\n")) })
	return codeRE.ReplaceAllString(s, "``")
}

// frontmatterDoc is a page's parsed frontmatter: flat keys, with inline
// ([a, b]) and block (- a) lists, as wiki-lint.py reads it.
type frontmatterDoc struct {
	values   map[string]any
	bodyLine int // index of the first line after the block
}

func (f frontmatterDoc) str(k string) string {
	s, _ := f.values[k].(string)
	return s
}

func parseFrontmatter(text string) (frontmatterDoc, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return frontmatterDoc{}, false
	}
	doc := frontmatterDoc{values: map[string]any{}}
	key := ""
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "---" {
			doc.bodyLine = i + 1
			return doc, true
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if item, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), "- "); ok && key != "" && line != strings.TrimLeft(line, " \t") {
			list, _ := doc.values[key].([]string)
			doc.values[key] = append(list, unquote(item))
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch {
		case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
			var list []string
			for _, item := range strings.Split(v[1:len(v)-1], ",") {
				if item = strings.TrimSpace(item); item != "" {
					list = append(list, unquote(item))
				}
			}
			doc.values[key] = list
		case v == "":
			doc.values[key] = []string{}
		default:
			doc.values[key] = unquote(v)
		}
	}
	return frontmatterDoc{}, false
}

func unquote(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }

// pageMeta is what the index needs of a page.
type pageMeta struct {
	rel, section, title, summary string
}

// readPages reads every page's title and summary, in path order.
func readPages(dir string) ([]pageMeta, error) {
	files, err := treeFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []pageMeta
	for _, rel := range files {
		if !isPage(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // a listed regular file of the pack
		if err != nil {
			return nil, err
		}
		inWiki := strings.TrimPrefix(rel, WikiDir+"/")
		section, _, _ := strings.Cut(inWiki, "/")
		if !strings.Contains(inWiki, "/") {
			section = ""
		}
		fm, _ := parseFrontmatter(string(b))
		title := fm.str("title")
		if title == "" {
			title = strings.TrimSuffix(path.Base(rel), ".md")
		}
		out = append(out, pageMeta{rel: inWiki, section: section, title: title, summary: fm.str("summary")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}
