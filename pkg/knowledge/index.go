package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SectionIndexAbove is the page count past which the index lists sections
// and each section gets an index of its own (past a few hundred pages it
// lists sections whose own index.md lists their pages), so the file a query
// starts from stays small.
const SectionIndexAbove = 300

// GenerateIndex returns the index files the pack at dir should hold, by
// slash path relative to the pack: wiki/index.md, and each section's
// wiki/<section>/index.md once the pack is past SectionIndexAbove pages.
func GenerateIndex(dir, name string) (map[string][]byte, error) {
	return generateIndex(dir, name, SectionIndexAbove)
}

func generateIndex(dir, name string, splitAbove int) (map[string][]byte, error) {
	pages, err := readPages(dir)
	if err != nil {
		return nil, err
	}
	bySection := map[string][]pageMeta{}
	var sections []string
	for _, p := range pages {
		if _, ok := bySection[p.section]; !ok {
			sections = append(sections, p.section)
		}
		bySection[p.section] = append(bySection[p.section], p)
	}
	sort.Strings(sections)
	out := map[string][]byte{}

	if len(pages) <= splitAbove {
		var b bytes.Buffer
		b.WriteString(frontmatterBlock("Index", "Every page in "+name+", one line each, by section. Generated; do not edit by hand."))
		fmt.Fprintf(&b, "# %s\n\n%s.\n", name, count(len(pages), "page"))
		for _, s := range sections {
			fmt.Fprintf(&b, "\n## %s\n\n", sectionTitle(s))
			for _, p := range bySection[s] {
				b.WriteString(indexLine(p, p.rel))
			}
		}
		out[WikiDir+"/"+IndexFile] = b.Bytes()
		return out, nil
	}

	var b bytes.Buffer
	b.WriteString(frontmatterBlock("Index", "Every section of "+name+", with the pages each holds. Generated; do not edit by hand."))
	fmt.Fprintf(&b, "# %s\n\n%s in %s; each section's index lists its pages.\n\n", name, count(len(pages), "page"), count(len(sections), "section"))
	for _, s := range sections {
		fmt.Fprintf(&b, "- [%s](%s/%s) — %s\n", sectionTitle(s), s, IndexFile, count(len(bySection[s]), "page"))
		var sb bytes.Buffer
		sb.WriteString(frontmatterBlock(sectionTitle(s), "Every page in the "+sectionTitle(s)+" section of "+name+". Generated; do not edit by hand."))
		fmt.Fprintf(&sb, "# %s\n\n%s. [All sections](../%s).\n\n", sectionTitle(s), count(len(bySection[s]), "page"), IndexFile)
		for _, p := range bySection[s] {
			sb.WriteString(indexLine(p, strings.TrimPrefix(p.rel, s+"/")))
		}
		out[WikiDir+"/"+s+"/"+IndexFile] = sb.Bytes()
	}
	out[WikiDir+"/"+IndexFile] = b.Bytes()
	return out, nil
}

// WriteIndex writes GenerateIndex's files into the pack at dir and removes
// any section index it no longer calls for.
func WriteIndex(dir, name string) error {
	want, err := GenerateIndex(dir, name)
	if err != nil {
		return err
	}
	for _, stale := range sectionIndexes(dir) {
		if _, keep := want[stale]; !keep {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(stale))); err != nil {
				return err
			}
		}
	}
	for rel, b := range want {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil { //nolint:gosec // a pack's pages are read by the person and their runs
			return err
		}
	}
	return nil
}

// indexDrift reports index files that differ from what GenerateIndex
// would write, and section indexes it would not write at all.
func indexDrift(dir, name string) ([]Finding, error) {
	want, err := GenerateIndex(dir, name)
	if err != nil {
		return nil, err
	}
	var out []Finding
	rels := make([]string, 0, len(want))
	for rel := range want {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // inside the pack
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if !bytes.Equal(got, want[rel]) {
			out = append(out, Finding{Page: rel, Rule: RuleIndex, Message: "the index is not what the pages generate; regenerate it"})
		}
	}
	for _, rel := range sectionIndexes(dir) {
		if _, ok := want[rel]; !ok {
			out = append(out, Finding{Page: rel, Rule: RuleIndex, Message: "a section index the pack is too small to have; regenerate the index"})
		}
	}
	return out, nil
}

// sectionIndexes lists the wiki/<section>/index.md files present.
func sectionIndexes(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, WikiDir, "*", IndexFile))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if rel, err := filepath.Rel(dir, m); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	sort.Strings(out)
	return out
}

func frontmatterBlock(title, summary string) string {
	return "---\ntitle: " + title + "\ntype: index\nsummary: " + summary + "\n---\n\n"
}

func indexLine(p pageMeta, link string) string {
	line := "- [" + p.title + "](" + link + ")"
	if p.summary != "" {
		line += " — " + p.summary
	}
	return line + "\n"
}

// sectionTitle is a section's directory name as a heading: "entity-types"
// reads "Entity types".
func sectionTitle(s string) string {
	if s == "" {
		return "Pages"
	}
	s = strings.ReplaceAll(s, "-", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
