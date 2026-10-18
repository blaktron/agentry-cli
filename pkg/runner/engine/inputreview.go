package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// AI Review of a hosted run's inputs. Once the inputs are decoded and
// checked for malware, and before the agent starts, every text the model
// could read of them is pushed to the registry's model-input scan as
// `input_file` with the job's own credential: each input's text view,
// HTML's Markdown view, and the original as uploaded when that is text too,
// archive members included. A text AI Review flags, or one it could not
// read, refuses the run before the agent starts (the route's rule: not
// scanned is withheld), naming the file; in a session, a file added mid-run
// is refused alone. A file past what AI Review reads of a run is withheld,
// never refused: it is listed as not read and none of its views is served,
// and the run goes on with the rest (input is scanned, not
// blocked). AI Review is one credit a run however
// many calls, which the sandbox host's runs extension settles, never this
// process. Local runs are not reviewed.

// MaxInputReviewText is the most text AI Review reads of one run's inputs,
// a session's added files included, so that one credit cannot buy an
// unbounded number of judge requests. A file whose texts do not fit in
// what is left is withheld, unasked: no text AI Review has not read
// reaches the agent. The hosted upload cap is 10 MiB, so only an archive
// that unpacks to much more, or a session that adds a great deal, meets it.
const MaxInputReviewText = 16 << 20

// MaxInputReviewCalls is the most calls AI Review makes of one run's
// inputs, a session's added files, the calls asked again and those that
// name a flagged file included: the registry's cap of scans a run (the
// runs extension's runInputScans), which refuses a call past it and so
// the run. reviewRetryBudget of them are kept for asking
// again, and reviewCalls read the texts and name a flagged file. A file
// whose texts would take the calls past what is left, keeping room to
// name a flagged file, is withheld, unasked, as a file past
// MaxInputReviewText is. Packed as packer packs them, 16 MiB of text
// takes about 260 to 275 calls whatever the sizes of its texts, so a run
// meets this only with very many small texts (2,000 small HTML files,
// read as uploaded, as text and as Markdown, are 375 calls) or a session
// that adds a great many files.
const MaxInputReviewCalls = 400

// reviewCalls is how many of MaxInputReviewCalls read the texts and name a
// flagged file, and reviewNaming the most calls one review keeps for
// naming: the texts of the flagged calls in flight, asked one at a time.
const (
	reviewCalls  = MaxInputReviewCalls - reviewRetryBudget
	reviewNaming = reviewWorkers * reviewBatchTexts
)

// The shape of the review's calls. A call carries at most webscan.MaxText,
// texts packed one after another, each headed by its path, as the pack scan
// does, at most reviewBatchTexts of them so a flagged call can be asked
// again a text at a time to name the file. A text that does not fit in what
// is left of a call is cut to fill it, when at least reviewMinPiece is
// left, and goes on in the next call, which starts reviewOverlap bytes
// before the cut, so a phrase no longer than that is read whole in one call
// wherever the cut falls. reviewWorkers calls are in flight at once,
// started at least reviewPace apart, under the registry's budget for one
// job (limits.modelInputPerMin, 120 a minute).
const (
	reviewBatchTexts = 16
	reviewWorkers    = 4
	// reviewLabelMax bounds the path a text is headed by.
	reviewLabelMax = 256
	reviewMinPiece = 4 << 10
	reviewOverlap  = 1 << 10
)

// reviewPace is a variable so the engine's tests need not wait.
var reviewPace = 550 * time.Millisecond

// A call answered not scanned for a failure that may pass (transient) is
// asked again, at most reviewRetries times, the nth after n times
// reviewBackoff and then its turn under reviewPace, holding its place
// among the reviewWorkers. Only then is the run refused as not
// reviewed: one judge error must not refuse a person's run, and a text AI
// Review has not read still never reaches the agent. A run asks again at
// most reviewRetryBudget times in all, a session's added files included,
// so a judge that keeps failing cannot spend the run's
// MaxInputReviewCalls on asking again.
const (
	reviewRetries     = 2
	reviewRetryBudget = 24
)

// reviewBackoff is a variable so the engine's tests need not wait.
var reviewBackoff = 2 * time.Second

// reviewLeft is how much text AI Review has left to read of the run, a
// variable so a test can spend it without staging 16 MiB.
var reviewLeft = func(r *inputReview) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return MaxInputReviewText - r.before.bytes - r.added.bytes
}

// reviewText is one text of an input as the model could read it: the
// input's path, what the review calls it, and the file that holds it.
type reviewText struct {
	path, label, file string
}

// reviewPiece is a text, or one part of a long one, as a call carries it.
type reviewPiece struct {
	text int // the index of the text it is of
	body string
}

// inputReview is the run's AI Review of its inputs, kept across a session's
// added files so the cap and the receipt cover them all.
type inputReview struct {
	sc webscan.Scanner

	mu sync.Mutex
	// before and added count what was read before the agent started and
	// what a session added.
	before, added      reviewCounts
	model, questionSet string
	// retriesUsed is how many calls the run has asked again, of
	// reviewRetryBudget, and callsUsed how many it has made to read and to
	// name, of reviewCalls.
	retriesUsed, callsUsed int
}

// reviewCounts is the record of what AI Review read.
type reviewCounts struct {
	// calls counts every call, retries counts those asked again.
	texts, calls, retries, requests int
	bytes                           int64
	// withheld are the inputs past MaxInputReviewText or
	// MaxInputReviewCalls, not asked about.
	withheld []string
}

// reviewRefusal is why the review refused: a receipt.Input* detail and the
// sentence that names the files.
type reviewRefusal struct {
	detail, why string
	// inputs names the files, typed, for the envelope.
	inputs []receipt.RefusedInput
}

// notReviewed is the unread reason list_inputs gives a file withheld past
// MaxInputReviewText or MaxInputReviewCalls: fixed words, as every unread
// reason.
const notReviewed = "not read by AI Review, past what it reads of a run (at most 16 MiB of text, in at most 400 calls), so it is not given to the agent"

func newInputReview(sc webscan.Scanner) *inputReview {
	if sc == nil {
		return nil
	}
	return &inputReview{sc: sc}
}

// reviewTexts lists the texts the model could read of the decoded files, in path
// order: each read input's text view (the decoded text staged under
// inputs-text, or the input itself when it was read as it is), its Markdown
// view, and its original when that is a text other than the text view (an
// RTF or HTML document, a file no decoder took) — what read_input serves a
// binary original as, base64, is not text to steer a model with.
// in is the directory the decode read the inputs from.
func reviewTexts(entries []toolsrv.InputEntry, in, textRoot, convertedRoot string) []reviewText {
	var out []reviewText
	for _, e := range entries {
		rel := filepath.FromSlash(e.Path)
		textView := ""
		if e.Unread == "" && !e.Unpacked {
			textView = filepath.Join(textRoot, rel)
			if _, err := os.Lstat(textView); err != nil {
				// Read as it is and not copied: the input itself.
				textView = filepath.Join(in, rel)
			}
			out = append(out, reviewText{path: e.Path, label: e.Path, file: textView})
		}
		if e.Converted {
			out = append(out, reviewText{path: e.Path, label: e.Path + " (as Markdown)", file: filepath.Join(convertedRoot, rel)})
		}
		if e.InArchive == "" && (e.Decoded || e.Original) {
			orig := filepath.Join(in, rel)
			if orig != textView && isTextFile(orig) {
				out = append(out, reviewText{path: e.Path, label: e.Path + " (as uploaded)", file: orig})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// isTextFile is read_input's own test of an original (toolsrv's isBinary):
// its first 64 KiB hold no NUL and are UTF-8, a character cut at the end
// aside.
func isTextFile(path string) bool {
	f, err := os.Open(path) //nolint:gosec // a file this run staged
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64<<10)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	data := buf[:n]
	if n == len(buf) {
		for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	return n > 0 && !strings.ContainsRune(string(data), 0) && utf8.Valid(data)
}

// review reads texts with AI Review. It answers the inputs withheld, each
// a path whose texts did not fit in what the run has left of
// MaxInputReviewText and were not asked about; a refusal when a text it
// read did not pass; and an error only when a staged text is missing.
// added says the texts are a session's added file's.
func (r *inputReview) review(ctx context.Context, all []reviewText, added bool) (withheld []string, _ *reviewRefusal, _ error) {
	if r == nil || len(all) == 0 {
		return nil, nil, nil
	}
	// An input's texts are read together or not at all, in path order.
	sizes := map[string]int64{}
	byPath := map[string][]reviewText{}
	var order []string
	for _, t := range all {
		fi, err := os.Lstat(t.file)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("the staged text of %s is missing", t.path)
		}
		if _, ok := sizes[t.path]; !ok {
			order = append(order, t.path)
		}
		sizes[t.path] += fi.Size()
		byPath[t.path] = append(byPath[t.path], t)
	}
	left, callsLeft := reviewLeft(r), r.callsLeft()
	var (
		texts []reviewText
		pk    packer
		size  int64
	)
	for _, p := range order {
		if size+sizes[p] > left {
			withheld = append(withheld, p)
			continue
		}
		mark, n := pk.mark(), len(texts)
		for _, t := range byPath[p] {
			b, err := os.ReadFile(t.file)
			if err != nil {
				return nil, nil, fmt.Errorf("the staged text of %s: %w", t.path, err)
			}
			pk.add(len(texts), t.label, string(b))
			texts = append(texts, t)
		}
		if pk.needs() > callsLeft {
			pk.reset(mark)
			texts = texts[:n]
			withheld = append(withheld, p)
			continue
		}
		size += sizes[p]
	}
	if len(texts) == 0 {
		r.record(reviewCounts{withheld: withheld}, "", "", added)
		return withheld, nil, nil
	}
	batches := pk.done()

	var (
		mu       sync.Mutex
		counts   = reviewCounts{texts: len(texts), bytes: size, withheld: withheld}
		flagged  = map[int][]string{} // text → the questions it failed
		unread   = map[int]string{}   // text → why it could not be read
		failedAt [][]reviewPiece      // the flagged calls of several texts
		model    string
		set      string
	)
	call := func(ctx context.Context, body string) webscan.Result {
		res := r.sc.Scan(ctx, webscan.SourceInputFile, body)
		mu.Lock()
		counts.calls++
		counts.requests += max(res.Requests, 0)
		if res.Model != "" {
			model, set = res.Model, res.QuestionSet
		}
		mu.Unlock()
		return res
	}
	run, stop := context.WithCancel(ctx)
	defer stop()
	pace := pacer(reviewPace)
	jobs := make(chan []reviewPiece)
	var wg sync.WaitGroup
	for range reviewWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range jobs {
				if pace(run) != nil {
					continue
				}
				if !r.takeCall() {
					// review withholds a file whose texts do not fit in the
					// run's calls, so this is not met; were it, the texts
					// would go unread, and unread texts refuse the run.
					mu.Lock()
					for _, i := range textsOf(batch) {
						unread[i] = "past the calls AI Review makes of a run"
					}
					mu.Unlock()
					stop()
					continue
				}
				var b strings.Builder
				for _, p := range batch {
					b.WriteString(p.body)
				}
				res := call(run, b.String())
				for n := 1; n <= reviewRetries && res.Transient() && run.Err() == nil && r.takeRetry(); n++ {
					if sleepCtx(run, time.Duration(n)*reviewBackoff) != nil || pace(run) != nil {
						break
					}
					mu.Lock()
					counts.retries++
					mu.Unlock()
					res = call(run, b.String())
				}
				if res.Passed() {
					continue
				}
				mu.Lock()
				// A call this review stopped is not a text it could not
				// read: the run is refused for the one that stopped it,
				// or, when the run itself stopped (in a retry's wait,
				// say), as stopped.
				if res.Verdict != webscan.Withheld && run.Err() != nil {
					mu.Unlock()
					continue
				}
				switch {
				case res.Verdict == webscan.Withheld && len(textsOf(batch)) > 1:
					failedAt = append(failedAt, batch)
				case res.Verdict == webscan.Withheld:
					flagged[batch[0].text] = res.Flagged
				default:
					for _, i := range textsOf(batch) {
						unread[i] = res.Reason()
					}
				}
				mu.Unlock()
				stop()
			}
		}()
	}
	for _, b := range batches {
		if run.Err() != nil {
			break
		}
		jobs <- b
	}
	close(jobs)
	wg.Wait()

	// A flagged call of several texts is asked again a text at a time, so
	// the refusal names the file; if none fails alone, or the run's calls
	// are spent first, each is named, as they were read together.
	for _, batch := range failedAt {
		named := false
		for _, p := range batch {
			if _, done := flagged[p.text]; done {
				continue // a text in several parts is named once
			}
			if pace(ctx) != nil || !r.takeCall() {
				break
			}
			if res := call(ctx, p.body); res.Verdict == webscan.Withheld {
				flagged[p.text] = res.Flagged
				named = true
			}
		}
		if !named {
			for _, i := range textsOf(batch) {
				flagged[i] = nil
			}
		}
	}

	r.record(counts, model, set, added)

	switch {
	case len(flagged) > 0:
		said := map[int]string{}
		for i, q := range flagged {
			said[i] = strings.Join(q, ", ")
		}
		return withheld, &reviewRefusal{detail: receipt.InputFlagged, why: "AI Review flagged " + namedTexts(texts, said), inputs: reviewRefused(texts, flagged)}, nil
	case len(unread) > 0:
		said := map[int]string{}
		for i, w := range unread {
			said[i] = strings.TrimPrefix(w, "it ")
		}
		unreadAt := make(map[int][]string, len(unread)) // no questions: it was not read
		for i := range unread {
			unreadAt[i] = nil
		}
		return withheld, &reviewRefusal{detail: receipt.InputNotReviewed, why: "AI Review could not read " + namedTexts(texts, said) +
			", and no text it has not read reaches the agent", inputs: reviewRefused(texts, unreadAt)}, nil
	case ctx.Err() != nil:
		return withheld, &reviewRefusal{detail: receipt.InputNotReviewed, why: "the run stopped before AI Review had read its inputs"}, nil
	}
	return withheld, nil, nil
}

// takeRetry takes one of the run's reviewRetryBudget, or answers false
// when they are spent.
func (r *inputReview) takeRetry() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retriesUsed >= reviewRetryBudget {
		return false
	}
	r.retriesUsed++
	return true
}

// takeCall takes one of the run's reviewCalls, or answers false when they
// are spent.
func (r *inputReview) takeCall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.callsUsed >= reviewCalls {
		return false
	}
	r.callsUsed++
	return true
}

// callsLeft is how many of the run's reviewCalls are left.
func (r *inputReview) callsLeft() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reviewCalls - r.callsUsed
}

// sleepCtx waits d, or answers the context's error.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// record adds one review's counts to the run's, before the agent started
// or for a session's added file.
func (r *inputReview) record(c reviewCounts, model, set string, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if model != "" {
		r.model, r.questionSet = model, set
	}
	into := &r.before
	if added {
		into = &r.added
	}
	into.texts += c.texts
	into.calls += c.calls
	into.retries += c.retries
	into.requests += c.requests
	into.bytes += c.bytes
	into.withheld = append(into.withheld, c.withheld...)
}

// packer packs texts, in order, into the review's calls of at most
// webscan.MaxText bytes and reviewBatchTexts texts, each text headed by
// its label (and its part's number, when it is cut) on a line of its own.
// A text goes whole into the call when it fits in what is left of it.
// When it does not, and at least reviewMinPiece is left, it is cut to fill
// the call and goes on in the next, each part after the first starting
// reviewOverlap bytes before the cut; with less left, the text starts the
// next call. So a call that closed for size carries more than
// webscan.MaxText less reviewMinPiece and a heading, and 16 MiB of text
// packs into about ⌈16 MiB / 64 KiB⌉ calls, plus one for every
// reviewBatchTexts small texts.
type packer struct {
	out         [][]reviewPiece
	cur         []reviewPiece
	size, texts int
	// pieces counts the pieces packed, of every call.
	pieces int
}

// packMark is a packer as it was, to take it back to.
type packMark struct {
	out                 int
	cur                 []reviewPiece
	size, texts, pieces int
}

func (p *packer) mark() packMark {
	return packMark{out: len(p.out), cur: slices.Clone(p.cur), size: p.size, texts: p.texts, pieces: p.pieces}
}

func (p *packer) reset(m packMark) {
	p.out, p.cur, p.size, p.texts, p.pieces = p.out[:m.out], m.cur, m.size, m.texts, m.pieces
}

// calls is how many calls what is packed takes.
func (p *packer) calls() int {
	if len(p.cur) > 0 {
		return len(p.out) + 1
	}
	return len(p.out)
}

// needs is how many of the run's calls what is packed needs: its calls,
// and those kept to name a flagged file.
func (p *packer) needs() int { return p.calls() + min(reviewNaming, p.pieces) }

// done answers the calls.
func (p *packer) done() [][]reviewPiece {
	p.flush()
	return p.out
}

func (p *packer) flush() {
	if len(p.cur) > 0 {
		p.out = append(p.out, p.cur)
		p.cur, p.size, p.texts = nil, 0, 0
	}
}

func (p *packer) room() int { return webscan.MaxText - p.size }

func (p *packer) put(text int, body string) {
	if len(p.cur) == 0 || p.cur[len(p.cur)-1].text != text {
		p.texts++
	}
	p.cur = append(p.cur, reviewPiece{text: text, body: body})
	p.size += len(body)
	p.pieces++
}

// add packs the text-th text, headed by label.
func (p *packer) add(text int, label, body string) {
	label = cutRunes(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, label), reviewLabelMax)
	whole := len(label) + 1 + len(body)
	// Room for a part's heading: "<label> (part n of m)\n".
	head := len(label) + 40
	if len(p.cur) > 0 && (p.texts == reviewBatchTexts || whole > p.room() && p.room()-head < reviewMinPiece) {
		p.flush()
	}
	if whole <= p.room() {
		p.put(text, label+"\n"+body)
		return
	}
	var parts []string
	for start, n := 0, p.room()-head; ; n = webscan.MaxText - head {
		if len(body)-start <= n {
			parts = append(parts, body[start:])
			break
		}
		cut := cutAt(body, start+n)
		parts = append(parts, body[start:cut])
		start = runeStart(body, cut-reviewOverlap)
	}
	for i, part := range parts {
		if i > 0 {
			p.flush()
		}
		p.put(text, fmt.Sprintf("%s (part %d of %d)\n", label, i+1, len(parts))+part)
	}
}

// cutAt is where a part that may run to end is cut: after the last line
// in its last reviewOverlap bytes, or else at end, never inside a
// character.
func cutAt(s string, end int) int {
	if i := strings.LastIndexByte(s[end-reviewOverlap:end], '\n'); i >= 0 {
		return end - reviewOverlap + i + 1
	}
	return runeStart(s, end)
}

// runeStart is i, or the start of the character i is inside; i as it is
// when s is not UTF-8 there.
func runeStart(s string, i int) int {
	for j := i; j > i-utf8.UTFMax && j > 0; j-- {
		if utf8.RuneStart(s[j]) {
			return j
		}
	}
	return i
}

// cutRunes cuts s to at most n bytes, never inside a character.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// textsOf is the texts a call carries, in order.
func textsOf(batch []reviewPiece) []int {
	var out []int
	for _, p := range batch {
		if len(out) == 0 || out[len(out)-1] != p.text {
			out = append(out, p.text)
		}
	}
	return out
}

// namedTexts names the inputs a review refused, in path order, each with
// what was said of its texts: the questions they failed, or why they could
// not be read ("a.docx (injection), b.pdf").
func namedTexts(texts []reviewText, said map[int]string) string {
	byPath := map[string][]string{}
	for i, w := range said {
		p := texts[i].path
		byPath[p] = append(byPath[p], w)
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		var words []string
		for _, w := range byPath[p] {
			if w != "" {
				words = append(words, strings.Split(w, ", ")...)
			}
		}
		if words = uniqueSorted(words); len(words) > 0 {
			names = append(names, p+" ("+strings.Join(words, ", ")+")")
		} else {
			names = append(names, p)
		}
	}
	return joinNamed(names)
}

// reviewRefused names the inputs a review refused, typed, in path order,
// each with the questions its texts failed (none for a file AI Review
// could not read).
func reviewRefused(texts []reviewText, at map[int][]string) []receipt.RefusedInput {
	byPath := map[string][]string{}
	for i, q := range at {
		p := texts[i].path
		byPath[p] = append(byPath[p], q...)
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]receipt.RefusedInput, 0, len(paths))
	for _, p := range paths {
		out = append(out, receipt.RefusedInput{Path: p, By: receipt.RefusedByAIReview, Flagged: uniqueSorted(byPath[p])})
	}
	return out
}

// joinNamed joins at most eight names, and counts the rest.
func joinNamed(names []string) string {
	const most = 8
	if len(names) <= most {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:most], ", ") + fmt.Sprintf(" and %d more", len(names)-most)
}

func uniqueSorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	n := 0
	for i, v := range out {
		if i == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

// pacer answers a wait that starts the calls at least every apart, or the
// context's error.
func pacer(every time.Duration) func(context.Context) error {
	var mu sync.Mutex
	var next time.Time
	return func(ctx context.Context) error {
		mu.Lock()
		at := next
		if now := time.Now(); at.Before(now) {
			at = now
		}
		next = at.Add(every)
		mu.Unlock()
		return sleepCtx(ctx, time.Until(at))
	}
}

// line is the receipt's inputReview line, empty when AI Review read
// nothing (a local run, a run with no inputs).
func (r *inputReview) line() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.before.calls == 0 && r.added.calls == 0 && len(r.before.withheld) == 0 && len(r.added.withheld) == 0 {
		return ""
	}
	what := "AI Review (the registry's model-input scan"
	if r.questionSet != "" {
		what += ", question set " + shortSet(r.questionSet)
	}
	if r.model != "" {
		what += ", " + r.model
	}
	what += ") was asked about the inputs' text"
	var parts []string
	if r.before.calls > 0 || len(r.before.withheld) > 0 {
		parts = append(parts, "before the agent started: "+r.before.say())
	}
	if r.added.calls > 0 || len(r.added.withheld) > 0 {
		parts = append(parts, "the files added during the session, each before the agent was told of it: "+r.added.say())
	}
	return what + "; " + strings.Join(parts, "; ")
}

func (c reviewCounts) say() string {
	calls := fmt.Sprintf("%d call(s)", c.calls)
	if c.retries > 0 {
		calls = fmt.Sprintf("%d call(s), %d of them asked again after AI Review could not answer", c.calls, c.retries)
	}
	out := fmt.Sprintf("%d text(s), %s, in %s and %d judge request(s), none flagged", c.texts, sizeWord(c.bytes), calls, c.requests)
	if len(c.withheld) > 0 {
		out += fmt.Sprintf("; %d file(s) withheld unread, past what AI Review reads of a run (at most %d MiB of text, in at most %d calls): %s",
			len(c.withheld), MaxInputReviewText>>20, MaxInputReviewCalls, joinNamed(c.withheld))
	}
	return out
}

// sizeWord is a size in KiB, at least 1.
func sizeWord(n int64) string { return fmt.Sprintf("%d KiB", max((n+1023)>>10, 1)) }

// shortSet is a question set's hash as the receipt names it.
func shortSet(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	return cutRunes(h, 12)
}

// refusedRun is the error of a run AI Review refused before the agent
// started, with the policy class, so a hosted run is not charged for the
// agent it never ran (the runs extension settles AI Review's credit alone
// when the review flagged an input).
func (rf *reviewRefusal) refusedRun() error {
	return Classify(ExitPolicy, receipt.WithRefusedInputs(inputRefused(rf.detail, fmt.Errorf("the run was refused before the agent started: %s", rf.why)), rf.inputs))
}
