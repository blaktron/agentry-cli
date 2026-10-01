package drafter

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/archive"
	corejudge "github.com/blaktron/agentry/pkg/judge"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// A draft that reads the person's knowledge (plan knowledge-and-drafter.md
// §6, D3, D4, D14; agentry-cli#522). The Drafter may be given the person's
// packs, up to MaxKnowledgeBytes in all, and nothing of a pack reaches its
// model unscanned (builder-rework D7):
//
//   - A pack Agentry scanned is not scanned again. Its scan stands in for
//     scanning each read (DR-44's addendum) when its record is confirmed
//     with the registry that keeps it (store.AgentryScan), or laid down by
//     a sandbox host from Agentry's store for a hosted draft, and is
//     complete: every stage finished and AI Review read every chunk.
//     Every file its findings place is withheld, and every file it could
//     not read as text.
//   - Any other pack is scanned first, before the draft starts, with the
//     draft's own scanner (the person's AI Review key on this computer):
//     every file of its tree, as model input, in pieces of at most
//     webscan.MaxText. A file that is flagged, could not be scanned, or is
//     not text is withheld.
//
// What is withheld is cut from a private copy of the pack, whose index is
// written again from the pages that remain, so the Drafter's knowledge
// tools never serve it; the draft's notes name each file under "Withheld
// by AI Review", with why, never its text.

// MaxKnowledgeBytes is the most a draft's packs may hold in all (D3),
// whatever a pack kept on this computer may hold (DR-48): Agentry's own
// cap on one pack.
const MaxKnowledgeBytes = knowledge.MaxBytes

// MaxKnowledgePacks is the most packs one draft takes: a hosted run's
// bound on attached packs.
const MaxKnowledgePacks = 5

// The two ways a pack a draft reads was scanned, as draft.json records it.
const (
	// ScannedInAgentry: the pack's scan in Agentry stood in.
	ScannedInAgentry = "agentry"
	// ScannedForDraft: AI Review scanned it before this draft started.
	ScannedForDraft = "draft"
)

// KnowledgeIn is one pack a draft is given.
type KnowledgeIn struct {
	Pack *knowledge.Pack
	// Agentry is the scan record of Agentry's copy of this pack's tree,
	// confirmed with the registry or laid down by a sandbox host from
	// Agentry's store; nil when there is none, and the pack is scanned
	// first.
	Agentry json.RawMessage
	// Unconfirmed is why a kept pack's record could not be confirmed, for
	// what the person is told; "" when there was nothing to confirm.
	Unconfirmed string
}

// PackRead is draft.json's record of one pack the Drafter was given.
type PackRead struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Pages  int    `json:"pages"`
	Bytes  int64  `json:"bytes"`
	// Scan is how the pack was scanned: ScannedInAgentry or
	// ScannedForDraft.
	Scan string `json:"scan"`
	// Withheld is every file of the pack the Drafter was not given.
	Withheld []WithheldFile `json:"withheld"`
	// Requests counts AI Review's requests for a pack scanned for the
	// draft, for the record (never a price: one credit an operation,
	// DEV-150).
	Requests int `json:"requests,omitempty"`
}

// WithheldFile is one file of a pack the Drafter was not given.
type WithheldFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// KnowledgeRead is what a draft reads of the person's knowledge: the packs
// to attach to its run, cut of what was withheld, and the record of each.
type KnowledgeRead struct {
	Packs []engine.AttachedPack
	Read  []PackRead
}

// Names are the packs' names, in the order given.
func (k *KnowledgeRead) Names() []string {
	if k == nil {
		return nil
	}
	out := make([]string, 0, len(k.Read))
	for _, r := range k.Read {
		out = append(out, r.Name)
	}
	return out
}

// ErrKnowledgeTooLarge is a draft's packs past MaxKnowledgeBytes, or more
// than MaxKnowledgePacks of them.
var ErrKnowledgeTooLarge = errors.New("a draft reads at most " + knowledge.FormatCap(MaxKnowledgeBytes) + " of knowledge in all")

// ErrKnowledgeUnscanned is a pack that must be scanned first with no
// scanner to scan it.
var ErrKnowledgeUnscanned = errors.New("the pack is not scanned, and AI Review must scan it before the Drafter reads it")

// CheckKnowledgeSize holds a draft's packs to MaxKnowledgePacks and
// MaxKnowledgeBytes, counted by each pack's files and its manifest: what a
// client checks before it scans anything.
func CheckKnowledgeSize(packs []*knowledge.Pack) error {
	if len(packs) > MaxKnowledgePacks {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%d knowledge packs: a draft reads at most %d: %w", len(packs), MaxKnowledgePacks, ErrKnowledgeTooLarge))
	}
	var total int64
	for _, p := range packs {
		total += p.Manifest.Bytes + manifestSize(p.Dir)
	}
	if total > MaxKnowledgeBytes {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("the knowledge packs hold %s: %w", sizeOf(total), ErrKnowledgeTooLarge))
	}
	return nil
}

func manifestSize(dir string) int64 {
	if fi, err := os.Stat(filepath.Join(dir, knowledge.ManifestFile)); err == nil {
		return fi.Size()
	}
	return 0
}

func sizeOf(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// ReadyKnowledge makes the packs a draft reads (see the top of this file):
// held to the size bounds, each one's withheld files decided by its scan in
// Agentry or by sc, and those files cut from a private copy under scratch,
// which the caller removes after the run. sc may be nil when every pack
// carries a record that stands in; a pack that needs scanning is then
// refused (ErrKnowledgeUnscanned, exit 11). log takes a line a person
// reads as each pack is scanned; nil discards them.
func ReadyKnowledge(ctx context.Context, in []KnowledgeIn, sc webscan.Scanner, scratch string, log io.Writer) (*KnowledgeRead, error) {
	if log == nil {
		log = io.Discard
	}
	packs := make([]*knowledge.Pack, 0, len(in))
	for _, k := range in {
		packs = append(packs, k.Pack)
	}
	if err := CheckKnowledgeSize(packs); err != nil {
		return nil, err
	}
	out := &KnowledgeRead{}
	for _, k := range in {
		m := k.Pack.Manifest
		rec := PackRead{Name: m.Name, Digest: m.SHA256, Pages: m.Pages, Bytes: m.Bytes, Withheld: []WithheldFile{}}
		// Everything below reads a private copy held to the digest the pack
		// was loaded with, so what is scanned, what a record is matched
		// against and what the Drafter is given are the same bytes.
		own, err := privateCopy(k.Pack, filepath.Join(scratch, m.Name))
		if err != nil {
			return nil, err
		}
		files, err := knowledge.TreeFiles(own.Dir)
		if err != nil {
			return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("knowledge pack %s: %w", m.Name, err))
		}
		withheld, why := fromAgentryScan(own.Dir, files, k.Agentry)
		if withheld != nil {
			rec.Scan = ScannedInAgentry
		} else {
			if why == "" {
				why = cmp.Or(k.Unconfirmed, "it has not been scanned")
			}
			if sc == nil {
				return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("knowledge pack %s: %s: %w", m.Name, why, ErrKnowledgeUnscanned))
			}
			_, _ = fmt.Fprintf(log, "Knowledge pack %s: %s; AI Review is scanning it now (%d files, %s).\n", m.Name, why, len(files), sizeOf(m.Bytes))
			rec.Scan = ScannedForDraft
			if withheld, rec.Requests, err = scanPack(ctx, sc, own.Dir, files); err != nil {
				return nil, err
			}
			_, _ = fmt.Fprintf(log, "Knowledge pack %s: AI Review read it; %d of %d files withheld.\n", m.Name, len(withheld), len(files))
		}
		for _, f := range files {
			if r, ok := withheld[f]; ok {
				rec.Withheld = append(rec.Withheld, WithheldFile{Path: f, Reason: r})
			}
		}
		out.Read = append(out.Read, rec)
		if len(withheld) == len(files) {
			// Nothing of it is left to read: it is not attached.
			continue
		}
		p := own
		if len(withheld) > 0 {
			if p, err = withholdFrom(own, withheld); err != nil {
				return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("knowledge pack %s: withhold what AI Review flagged: %w", m.Name, err))
			}
		}
		out.Packs = append(out.Packs, engine.AttachPack(p))
	}
	return out, nil
}

// privateCopy copies the pack into dst, under the caller's private scratch,
// and holds the copy to the digest the pack was loaded with: a pack
// changed since is refused rather than read.
func privateCopy(p *knowledge.Pack, dst string) (*knowledge.Pack, error) {
	name := p.Manifest.Name
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	if err := knowledge.CopyPack(p.Dir, dst); err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("knowledge pack %s: %w", name, err))
	}
	own, err := knowledge.Load(dst)
	if err != nil {
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("knowledge pack %s changed after it was checked: %w", name, err))
	}
	if own.Manifest.SHA256 != p.Manifest.SHA256 {
		return nil, engine.Classify(engine.ExitVerify, fmt.Errorf("knowledge pack %s changed after it was checked (checked %s, copied %s)", name, p.Manifest.SHA256, own.Manifest.SHA256))
	}
	return own, nil
}

// ---- a scan in Agentry ----

// agentrySchema is the core's pack scan report schema (scan.PackReportSchema).
const agentrySchema = "agentry.knowledge-scan/v1"

// agentryReport is what a draft reads of the registry's pack scan report
// (the core's scan.PackReport, agentry.knowledge-scan/v1).
type agentryReport struct {
	Schema string `json:"schema"`
	Stages []struct {
		ID     string `json:"id"`
		Failed bool   `json:"failed"`
	} `json:"stages"`
	Read struct {
		Sources int      `json:"sources"`
		Wiki    int      `json:"wiki"`
		NotRead []string `json:"notRead"`
	} `json:"read"`
	Review struct {
		Configured   bool `json:"configured"`
		ChunkBytes   int  `json:"chunkBytes"`
		SourceChunks int  `json:"sourceChunks"`
		WikiChunks   int  `json:"wikiChunks"`
		Failed       int  `json:"failed"`
	} `json:"review"`
	Findings []struct {
		Scanner  string `json:"scanner"`
		Severity string `json:"severity"`
		Part     string `json:"part"`
		File     string `json:"file"`
		Chunk    int    `json:"chunk"`
	} `json:"findings"`
}

// AgentryScanStandsIn says whether raw, a pack's scan record in Agentry,
// stands in for scanning the pack for a draft (D14), and why not: it must
// be the core's report, every stage must have finished, AI Review must have
// read every chunk, and it must account for every file of the pack's tree.
// A client says so before anything is scanned (D3); ReadyKnowledge asks
// the same.
func AgentryScanStandsIn(p *knowledge.Pack, raw json.RawMessage) (bool, string) {
	files, err := knowledge.TreeFiles(p.Dir)
	if err != nil {
		return false, "its files could not be listed"
	}
	_, why := agentryRecord(raw, len(files))
	return why == "", why
}

// agentryRecord parses raw and holds it to what AgentryScanStandsIn
// requires of it: the report, or why it does not stand in ("" and nil for
// no record).
func agentryRecord(raw json.RawMessage, files int) (*agentryReport, string) {
	if len(raw) == 0 {
		return nil, "it has not been scanned"
	}
	var r agentryReport
	if err := json.Unmarshal(raw, &r); err != nil || r.Schema != agentrySchema {
		return nil, "its scan record in Agentry could not be read"
	}
	for _, st := range r.Stages {
		if st.Failed {
			return nil, "its scan in Agentry did not finish"
		}
	}
	switch {
	case !r.Review.Configured:
		return nil, "its scan in Agentry did not include AI Review"
	case r.Review.Failed > 0:
		return nil, "AI Review did not read all of it in Agentry"
	case r.Read.Sources+r.Read.Wiki+len(r.Read.NotRead) != files:
		return nil, "its scan record in Agentry does not account for every file of it"
	}
	return &r, ""
}

// fromAgentryScan is the files of the pack at dir a scan in Agentry
// withholds, by path, with why; nil, and why not, when the record cannot
// stand in for a scan (agentryRecord, D14). Beside what the record places,
// any file this client does not read as text is withheld too.
func fromAgentryScan(dir string, files []string, raw json.RawMessage) (map[string]string, string) {
	r, why := agentryRecord(raw, len(files))
	if r == nil {
		if len(raw) == 0 {
			return nil, ""
		}
		return nil, why
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	out := map[string]string{}
	add := func(f, why string) {
		if _, ok := out[f]; !ok {
			out[f] = why
		}
	}
	half := func(part, why string) {
		for _, f := range files {
			if part == "" || strings.HasPrefix(f, part+"/") {
				add(f, why)
			}
		}
	}
	for _, f := range r.Read.NotRead {
		if have[f] {
			add(f, "Agentry's scan could not read it as text")
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f))) //nolint:gosec // a file of the pack
		if err != nil || !isText(b) {
			add(f, notText)
		}
	}
	for _, f := range r.Findings {
		why := "Agentry's scan flagged it (" + closedWord(f.Scanner) + ")"
		part := f.Part
		if part != knowledge.WikiDir && part != knowledge.SourcesDir {
			part = ""
		}
		switch {
		case f.File != "" && have[f.File]:
			add(f.File, why)
		case f.File == "" && f.Chunk > 0 && part != "":
			placed, ok := reviewChunk(dir, files, part, r.Review.ChunkBytes, f.Chunk, r.chunks(part))
			if !ok {
				half(part, why)
				continue
			}
			for _, p := range placed {
				add(p, why)
			}
		default:
			// A finding this client cannot place withholds all it may be
			// in: its half, or the whole pack.
			half(part, why)
		}
	}
	return out, ""
}

func (r agentryReport) chunks(part string) int {
	if part == knowledge.WikiDir {
		return r.Review.WikiChunks
	}
	return r.Review.SourceChunks
}

// closedWord keeps a scanner's id to the closed words the notes may carry:
// letters and digits, at most 16.
func closedWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == 16 {
			break
		}
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "a finding"
	}
	return b.String()
}

// reviewChunk is the files AI Review's chunk number (1-based) of a half
// read in Agentry, rebuilt as the core cuts them (internal/scan/pack.go,
// judgeChunks): the half's text files in path order, packed into chunks of
// at most limit bytes, a longer file in pieces of whole lines. ok is false
// when the rebuilt half does not have the chunk count the report records,
// and the caller withholds the whole half.
func reviewChunk(dir string, files []string, part string, limit, chunk, want int) ([]string, bool) {
	if limit <= 0 || chunk > want {
		return nil, false
	}
	type piece struct {
		from string
		n    int
	}
	var chunks [][]piece
	var cur []piece
	size := 0
	for _, f := range files {
		if !strings.HasPrefix(f, part+"/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f))) //nolint:gosec // a file of the pack
		if err != nil {
			return nil, false
		}
		if !isText(b) {
			continue
		}
		for _, p := range coreSplitLines(string(b), limit) {
			if size+len(p) > limit && len(cur) > 0 {
				chunks = append(chunks, cur)
				cur, size = nil, 0
			}
			cur = append(cur, piece{from: f, n: len(p)})
			size += len(p)
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	if len(chunks) != want {
		return nil, false
	}
	var out []string
	for _, p := range chunks[chunk-1] {
		if len(out) == 0 || out[len(out)-1] != p.from {
			out = append(out, p.from)
		}
	}
	return out, true
}

// coreSplitLines is the core's splitLines (internal/scan/pack.go): s cut
// into pieces of at most n bytes, each ending at a line's end where one
// falls in its second half, else at a rune boundary.
func coreSplitLines(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := strings.LastIndexByte(s[:n], '\n') + 1
		if cut < n/2 {
			cut = n
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

// isText is the core's test of a file a pack's scan reads as text.
func isText(b []byte) bool {
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	return archive.LooksText(head) && utf8.Valid(b)
}

// ---- a scan for the draft ----

// notText is why a file that is not text is withheld unread.
const notText = "it is not text, so AI Review could not read it"

// packScanWorkers is how many of a pack's chunks are scanned at once.
const packScanWorkers = 4

// scanEntry is one file's piece in a chunk: the path it is from, and the
// text a scan reads, the path heading the piece as the core's pack scan
// gives it to the judge, so a name is read as well as what it holds.
type scanEntry struct {
	from string
	text string
}

// packChunks cuts the pack's text files, in path order, into chunks of at
// most limit bytes as the core's pack scan does (judgeChunks), each piece
// headed by its path, so a pack of many small pages takes a request per
// chunk rather than per page. Files that are not text are answered apart.
func packChunks(dir string, files []string, limit int) ([][]scanEntry, map[string]string, error) {
	var chunks [][]scanEntry
	var cur []scanEntry
	size := 0
	notRead := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f))) //nolint:gosec // a file of the pack
		if err != nil {
			return nil, nil, err
		}
		if !isText(b) {
			notRead[f] = notText
			continue
		}
		// Room for the heading: "<path> (part n of m)\n".
		pieces := coreSplitLines(string(b), max(limit-len(f)-32, 1024))
		for i, p := range pieces {
			head := f
			if len(pieces) > 1 {
				head = fmt.Sprintf("%s (part %d of %d)", f, i+1, len(pieces))
			}
			e := scanEntry{from: f, text: head + "\n" + p}
			if size+len(e.text) > limit && len(cur) > 0 {
				chunks = append(chunks, cur)
				cur, size = nil, 0
			}
			cur = append(cur, e)
			size += len(e.text)
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, notRead, nil
}

// scanPack scans the pack's text as model input, in the pack scan's chunks,
// and answers the files withheld, with why, and the requests it took. A
// chunk of several files that does not pass is scanned again a file at a
// time, so only the file AI Review flags is withheld; if each passes alone,
// the whole chunk is withheld, as it was read. A file that is not text is
// withheld unread.
func scanPack(ctx context.Context, sc webscan.Scanner, dir string, files []string) (map[string]string, int, error) {
	chunks, out, err := packChunks(dir, files, corejudge.ChunkSize)
	if err != nil {
		return nil, 0, engine.Classify(engine.ExitCLI, err)
	}
	var mu sync.Mutex
	requests := 0
	scan := func(text string) webscan.Result {
		res := sc.Scan(ctx, webscan.SourceInputFile, text)
		mu.Lock()
		requests += max(res.Requests, 0)
		mu.Unlock()
		return res
	}
	withhold := func(f, why string) {
		mu.Lock()
		if _, ok := out[f]; !ok {
			out[f] = why
		}
		mu.Unlock()
	}
	jobs := make(chan []scanEntry)
	var wg sync.WaitGroup
	for range packScanWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range jobs {
				var b strings.Builder
				byFile := map[string][]string{}
				var order []string
				for _, e := range chunk {
					b.WriteString(e.text)
					if _, ok := byFile[e.from]; !ok {
						order = append(order, e.from)
					}
					byFile[e.from] = append(byFile[e.from], e.text)
				}
				res := scan(b.String())
				if res.Passed() {
					continue
				}
				if len(order) == 1 || ctx.Err() != nil {
					for _, f := range order {
						withhold(f, res.Reason())
					}
					continue
				}
				placed := false
				for _, f := range order {
					one := scan(strings.Join(byFile[f], ""))
					if !one.Passed() {
						withhold(f, one.Reason())
						placed = true
					}
				}
				if !placed {
					for _, f := range order {
						withhold(f, res.Reason())
					}
				}
			}
		}()
	}
	for _, c := range chunks {
		if ctx.Err() != nil {
			break
		}
		jobs <- c
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, requests, engine.Classify(engine.ExitCLI, fmt.Errorf("the knowledge scan was interrupted: %w", ctx.Err()))
	}
	return out, requests, nil
}

// ---- the copy the draft reads ----

// withholdFrom removes the withheld files from the private copy p and
// answers it, loaded and verified, with its scan record the pack's own.
// Nothing is written that was not read: an index that was not withheld
// loses only the lines that link a withheld page (each the page's summary),
// keeping every other byte the scan read; a withheld index or log is
// replaced by a fixed line.
func withholdFrom(p *knowledge.Pack, withheld map[string]string) (*knowledge.Pack, error) {
	dst := p.Dir
	var pages []string
	for f := range withheld {
		if err := os.Remove(filepath.Join(dst, filepath.FromSlash(f))); err != nil {
			return nil, err
		}
		if rel, ok := strings.CutPrefix(f, knowledge.WikiDir+"/"); ok {
			pages = append(pages, rel)
		}
	}
	files, err := knowledge.TreeFiles(dst)
	if err != nil {
		return nil, err
	}
	for gone := range withheld {
		switch {
		case gone == knowledge.WikiDir+"/"+knowledge.LogFile:
			if err := os.WriteFile(filepath.Join(dst, filepath.FromSlash(gone)), []byte("# Log\n\nWithheld by AI Review.\n"), 0o600); err != nil {
				return nil, err
			}
		case isIndex(gone):
			stub := "# Index\n\nThis index was withheld by AI Review; list a section to see its pages.\n"
			if err := os.WriteFile(filepath.Join(dst, filepath.FromSlash(gone)), []byte(stub), 0o600); err != nil {
				return nil, err
			}
		}
	}
	if len(pages) > 0 {
		for _, f := range files {
			if !isIndex(f) {
				continue
			}
			if err := dropIndexLines(filepath.Join(dst, filepath.FromSlash(f)), strings.TrimPrefix(slashpath.Dir(f), knowledge.WikiDir), pages); err != nil {
				return nil, err
			}
		}
	}
	d, err := knowledge.TreeDigest(dst)
	if err != nil {
		return nil, err
	}
	n, err := knowledge.Pages(dst)
	if err != nil {
		return nil, err
	}
	m := p.Manifest
	m.Pages, m.Bytes, m.SHA256 = n, d.Bytes, d.SHA256
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dst, knowledge.ManifestFile), append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	return knowledge.Load(dst)
}

// isIndex says whether a pack path is one of its wiki's index files.
func isIndex(f string) bool {
	return strings.HasPrefix(f, knowledge.WikiDir+"/") && slashpath.Base(f) == knowledge.IndexFile
}

// dropIndexLines removes from the index at p, in the wiki directory dir
// ("" for the wiki itself, "/topics" for a section), every line that links
// one of the withheld pages (paths relative to the wiki).
func dropIndexLines(p, dir string, pages []string) error {
	b, err := os.ReadFile(p) //nolint:gosec // a file of the private copy
	if err != nil {
		return err
	}
	dir = strings.TrimPrefix(dir, "/")
	var links []string
	for _, pg := range pages {
		rel := pg
		if dir != "" {
			r, ok := strings.CutPrefix(pg, dir+"/")
			if !ok {
				continue
			}
			rel = r
		}
		links = append(links, "]("+rel+")")
	}
	if len(links) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	kept := lines[:0]
	for _, l := range lines {
		drop := false
		for _, link := range links {
			if strings.Contains(l, link) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, l)
		}
	}
	return os.WriteFile(p, []byte(strings.Join(kept, "")), 0o600)
}

// ---- the draft's record ----

// readKnowledge records the packs the Drafter was given in the draft, and
// each withheld file among AI Review's withheld sources, as
// pack:<name>/<path>, so the notes' "Withheld by AI Review" names it.
func (d *Draft) readKnowledge(k *KnowledgeRead) {
	if k == nil || len(k.Read) == 0 {
		return
	}
	defer d.composeNotes("")
	d.Knowledge = append([]PackRead(nil), k.Read...)
	if d.AIReview == nil {
		d.AIReview = &AIReview{WithheldSources: []WithheldSource{}}
	}
	for _, r := range k.Read {
		for _, w := range r.Withheld {
			d.AIReview.WithheldSources = append(d.AIReview.WithheldSources,
				WithheldSource{Tool: "knowledge", URL: PackSource(r.Name, w.Path), Reason: w.Reason})
		}
	}
}

// PackSource is how a draft names a file of a pack it read, in its
// sources and its notes (plan §6, D4): pack:<name>/<path>, the path
// relative to the pack's wiki for a page, else to the pack.
func PackSource(name, file string) string {
	if rel, ok := strings.CutPrefix(file, knowledge.WikiDir+"/"); ok {
		return "pack:" + name + "/" + rel
	}
	return "pack:" + name + "/" + file
}
