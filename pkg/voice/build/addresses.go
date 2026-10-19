package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// A build may take samples from web addresses the person gives: a blog
// post, a newsletter issue, an article. Each address is fetched before the
// run by the step a run's url input source takes (engine.SourceFetcher),
// then decoded with the other samples. AI Review reads the text of every
// fetched page before the run (the person's own key on this computer, the
// registry with the job's credential on a sandbox host), and a page it
// flags or cannot scan is withheld and named. A page whose text is drawn by
// scripts (thin HTML) is not staged: its address goes to the Voice Writer's
// web_read, which opens only the addresses given, renders such a page in a
// headless browser and has AI Review read it there before the model does.

// MaxURLs bounds the addresses one build takes.
const MaxURLs = 20

// ErrNoReviewKey refuses addresses with nothing to review them: the
// person's own TypeSafe key on this computer, the job's credential on a
// sandbox host:
// AI Review reads every page before the Voice Writer does.
var ErrNoReviewKey = errors.New("a sample from a web address is read by AI Review first, which takes your own TypeSafe key, and none was found; give the samples as files, or set the key")

// ParseURLs reads the addresses a build was given as url sources, each with
// the id url-<n>.
func ParseURLs(urls []string) ([]*sources.Source, error) {
	if len(urls) > MaxURLs {
		return nil, fmt.Errorf("a voice takes at most %d web addresses", MaxURLs)
	}
	kind, _ := sources.Lookup(manifest.IntakeURL)
	out := make([]*sources.Source, 0, len(urls))
	for i, raw := range urls {
		s, err := kind.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("--url %s: %w", raw, err)
		}
		// An address literal is held to the public rule here, before
		// anything runs; a name is held to it by the fetch step's proxy,
		// which resolves it.
		if a, err := netip.ParseAddr(s.Host); err == nil && !netaddr.Public(a) {
			return nil, fmt.Errorf("--url %s: %s is not a public address", raw, s.Host)
		}
		s.ID = fmt.Sprintf("url-%d", i+1)
		out = append(out, &s)
	}
	return out, nil
}

// Page is one address a build was given, and what became of it.
type Page struct {
	// URL is the address as shown (a query hidden).
	URL string `json:"url"`
	// Sample is the sample it was staged as, when its text was read before
	// the run; empty for a page read with web_read, or withheld.
	Sample string `json:"sample,omitempty"`
	// Rendered says the page's text is drawn by scripts, so the Voice
	// Writer reads it with web_read, rendered, AI Review reading it there.
	Rendered bool `json:"rendered,omitempty"`
	// Withheld says why AI Review did not pass its text, or why it could
	// not be read; the Voice Writer never saw it.
	Withheld string `json:"withheld,omitempty"`
	// WithheldMembers are the files of an archive the page was that the
	// Voice Writer was not given, each with why; the archive's other files
	// are samples.
	WithheldMembers []string `json:"withheldMembers,omitempty"`
	// Bytes is the file fetched.
	Bytes int64 `json:"bytes"`

	src     sources.Source
	fetched sources.Fetched
}

// fetchPages fetches each address into dir, where the samples are staged,
// under the file's own name, within what the samples' cap leaves. stagedAs
// is the samples staged so far, by name, and grows with each page.
func fetchPages(ctx context.Context, fetcher engine.SourceFetcher, srcs []*sources.Source, dir string, stagedAs map[string]string) ([]*Page, error) {
	kind, _ := sources.Lookup(manifest.IntakeURL)
	staged := dirBytes(dir)
	var pages []*Page
	for _, s := range srcs {
		if staged >= MaxSampleBytes {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s: the samples already come to their %d MB, so nothing more can be fetched", s.Shown(), MaxSampleBytes>>20))
		}
		dest := filepath.Join(dir, "."+s.ID)
		f, err := fetcher.Fetch(ctx, *s, kind.Caps.Within(1, MaxSampleBytes-staged), dest)
		if err != nil {
			return nil, err
		}
		name := safeName(f.Source.Name)
		switch {
		case name == decode.DocumentsFile:
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s: a sample may not be named %s; give the page another way", s.Shown(), decode.DocumentsFile))
		case stagedAs[name] != "":
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s and %s would both be the sample %s; give one", stagedAs[name], s.Shown(), name))
		}
		if err := os.Rename(filepath.Join(dest, f.Source.Name), filepath.Join(dir, name)); err != nil {
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("stage %s: %w", s.Shown(), err))
		}
		if err := os.Remove(dest); err != nil {
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("stage %s: %w", s.Shown(), err))
		}
		stagedAs[name] = s.Shown()
		staged += f.Source.Bytes
		pages = append(pages, &Page{URL: s.Shown(), Sample: name, Bytes: f.Source.Bytes, src: *s, fetched: f})
	}
	return pages, nil
}

// reviewPages decides, for each page fetched, what the Voice Writer gets:
// a page whose text is thin HTML goes to web_read (rendered there); any
// other page's text, an HTML page's as its text alone, is read by AI
// Review, and one it does not pass is withheld. It rewrites the staged
// inputs to match: the staged text of a reviewed HTML page becomes the text
// AI Review read, and a page not staged is dropped from documents.json,
// which keeps every other entry, an unread one with its reason. noRender,
// when set, is why this build cannot render a page drawn by scripts, and
// such a page is withheld with it. It answers the readable documents left
// and the addresses web_read may open.
func reviewPages(ctx context.Context, sc webscan.Scanner, dec *decode.Result, all []decode.Document, pages []*Page, inputsDir string, noRender string) ([]decode.Document, []string, error) {
	bySample := map[string]*Page{}
	for _, p := range pages {
		bySample[p.Sample] = p
	}
	mime := map[string]string{}
	for _, f := range dec.Report.Files {
		mime[f.Path] = f.MIME
	}
	var (
		listed []decode.Document
		toRead []string
	)
	for _, d := range all {
		p, member := bySample[d.Name], false
		if p == nil {
			// A member of an archive a page was is reviewed as its page
			// is, and decided on its own: it is staged or dropped, never
			// read with web_read, which opens the page's address alone.
			p, member = bySample[strings.SplitN(d.Name, "/", 2)[0]], true
		}
		if p == nil {
			// A file sample: listed as it was.
			listed = append(listed, d)
			continue
		}
		if d.Read == "" {
			// A page's file no decoder read is not listed: its name is the
			// host's or the archive's, which no review read.
			if member {
				p.WithheldMembers = append(p.WithheldMembers, d.Name+": no decoder could read it")
			} else {
				p.Sample, p.Withheld = "", "no decoder could read it"
			}
			continue
		}
		staged := filepath.Join(inputsDir, filepath.FromSlash(d.Read))
		raw, err := os.ReadFile(staged) //nolint:gosec // a file this build staged
		if err != nil {
			return nil, nil, engine.Classify(engine.ExitCLI, fmt.Errorf("review %s: %w", d.Name, err))
		}
		text := string(raw)
		if isHTML(mime[d.Name], d.Name) {
			// The address parsed when it was given; links in the page
			// resolve against it.
			base, err := url.Parse(p.src.URL)
			if err != nil {
				return nil, nil, engine.Classify(engine.ExitCLI, fmt.Errorf("review %s: %w", p.URL, err))
			}
			text = strings.TrimSpace(toolsrv.HTMLText(raw, base))
			if len(text) < toolsrv.RenderTextMin {
				if err := os.Remove(staged); err != nil {
					return nil, nil, engine.Classify(engine.ExitCLI, err)
				}
				if member {
					p.WithheldMembers = append(p.WithheldMembers, d.Name+": its text is drawn by scripts, and a file in an archive cannot be rendered")
					continue
				}
				if strings.Contains(p.src.URL, "?") {
					// A query can carry a signed download's credential, and
					// web_read's address is the model's to read.
					p.Withheld = "its text is drawn by scripts, and an address with a query is never handed to the Voice Writer to read"
					continue
				}
				if noRender != "" {
					p.Withheld = "its text is drawn by scripts, and " + noRender
					continue
				}
				p.Sample, p.Rendered = "", true
				toRead = append(toRead, p.src.URL)
				continue
			}
		}
		// The file's name is the host's or the archive's, and the model
		// reads it in the task and documents.json: AI Review reads it with
		// the text.
		if why := reviewText(ctx, sc, "File name: "+d.Name+"\n\n"+text); why != "" {
			if err := os.Remove(staged); err != nil {
				return nil, nil, engine.Classify(engine.ExitCLI, err)
			}
			if member {
				p.WithheldMembers = append(p.WithheldMembers, d.Name+": "+why)
				continue
			}
			p.Sample, p.Withheld = "", why
			continue
		}
		if err := os.WriteFile(staged, []byte(text), 0o644); err != nil { //nolint:gosec // an input the run reads
			return nil, nil, engine.Classify(engine.ExitCLI, err)
		}
		listed = append(listed, d)
	}
	if err := rewriteDocuments(inputsDir, listed); err != nil {
		return nil, nil, err
	}
	var readable []decode.Document
	for _, d := range listed {
		if d.Read != "" {
			readable = append(readable, d)
		}
	}
	return readable, toRead, nil
}

// reviewText is AI Review's read of a page's text before the run, in
// pieces of the most one scan takes: "" when every piece passed, else why
// it is withheld.
func reviewText(ctx context.Context, sc webscan.Scanner, text string) string {
	if strings.TrimSpace(text) == "" {
		return "it has no text"
	}
	for _, piece := range pieces(text, webscan.MaxText) {
		if res := sc.Scan(ctx, webscan.SourcePage, piece); !res.Passed() {
			return "AI Review did not pass it: " + res.Reason()
		}
	}
	return ""
}

// pieces cuts text into parts of at most n bytes, each at a rune boundary.
func pieces(text string, n int) []string {
	var out []string
	for len(text) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

// isHTML says whether a staged sample is an HTML page.
func isHTML(mime, name string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	if m == "text/html" || m == "application/xhtml+xml" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".html" || ext == ".htm" || ext == ".xhtml"
}

// rewriteDocuments writes documents.json again with the documents kept.
func rewriteDocuments(dir string, docs []decode.Document) error {
	if docs == nil {
		docs = []decode.Document{}
	}
	raw, err := json.MarshalIndent(map[string]any{"documents": docs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, decode.DocumentsFile), append(raw, '\n'), 0o644) //nolint:gosec // read by the run
}

// stagedInputs are the top-level entries of dir, as the run stages them.
func stagedInputs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	slices.Sort(out)
	return out, nil
}

// dirBytes is what the regular files under dir come to.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// pageLines are the receipt's lines for the pages, keyed source.<url-n>:
// the fetch in a run's words, and what AI Review made of its text.
func pageLines(pages []*Page, files []runnerdecode.File) map[string]string {
	if len(pages) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, p := range pages {
		line := sources.Line(p.src, p.fetched, sources.ChecksAsUploadNamed(p.fetched.Source.Name, files))
		switch {
		case p.Withheld != "":
			line += "; withheld from the Voice Writer: " + p.Withheld
		case p.Rendered:
			line += "; its text is drawn by scripts, so the Voice Writer reads it with web_read, rendered, AI Review reading it there first"
		default:
			line += "; AI Review read its text before the run and passed it"
		}
		if len(p.WithheldMembers) > 0 {
			line += "; withheld from the Voice Writer, of the archive's files: " + strings.Join(p.WithheldMembers, "; ")
		}
		out["source."+p.src.ID] = line
	}
	return out
}

// pagesTask is the task's paragraph on the pages: those to read with
// web_read, and those withheld.
func pagesTask(pages []*Page) string {
	var read, held []string
	for _, p := range pages {
		switch {
		case p.Rendered:
			read = append(read, p.src.URL)
		case p.Withheld != "":
			held = append(held, p.URL)
		}
		for _, m := range p.WithheldMembers {
			held = append(held, strings.SplitN(m, ":", 2)[0])
		}
	}
	var b bytes.Buffer
	if len(read) > 0 {
		b.WriteString("\nRead these pages with web_read, each a sample of the person's own writing (its text is drawn by scripts, so it was not staged as a file):\n")
		for _, u := range read {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	if len(held) > 0 {
		fmt.Fprintf(&b, "\nThese addresses were withheld before the run and are not yours to read: %s. Name them in your reply.\n", strings.Join(held, ", "))
	}
	return b.String()
}
