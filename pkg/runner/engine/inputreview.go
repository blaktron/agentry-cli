package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// AI Review of a hosted run's inputs (plan file-handling.md §8, M7;
// agentry-cli#321). Once the inputs are decoded and checked for malware,
// and before the agent starts, every text the model could read of them is
// pushed to the registry's model-input scan as `input_file` with the job's
// own credential: each input's text view, HTML's Markdown view, and the
// original as uploaded when that is text too, archive members included.
// A text AI Review flags, or one it could not read, refuses the run before
// the agent starts (the route's rule: not scanned is withheld), naming the
// file; in a session, a file added mid-run is refused alone. AI Review is
// one credit a run however many calls (DEV-150), which the sandbox host's
// runs extension settles, never this process. Local runs are not reviewed.

// MaxInputReviewText is the most text AI Review reads of one run's inputs,
// a session's added files included: a run past it is refused before any
// call, since what AI Review has not read never reaches the agent. The
// hosted upload cap is 10 MiB, so only an archive that unpacks to much
// more, or a session that adds a great deal, meets it.
const MaxInputReviewText = 16 << 20

// The shape of the review's calls. A call carries at most webscan.MaxText,
// several small texts packed into one, each headed by its path, as the pack
// scan does, at most reviewBatchTexts of them so a flagged call can be
// asked again a text at a time to name the file. reviewWorkers calls are in
// flight at once, started at least reviewPace apart, under the registry's
// budget for one job (limits.modelInputPerMin, 120 a minute).
const (
	reviewBatchTexts = 16
	reviewWorkers    = 4
	// reviewLabelMax bounds the path a text is headed by.
	reviewLabelMax = 256
)

// reviewPace is a variable so the engine's tests need not wait.
var reviewPace = 550 * time.Millisecond

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
}

// reviewCounts is the record of what AI Review read.
type reviewCounts struct {
	texts, calls, requests int
	bytes                  int64
}

// reviewRefusal is why the review refused: a receipt.Input* detail and the
// sentence that names the files.
type reviewRefusal struct {
	detail, why string
}

func newInputReview(sc webscan.Scanner) *inputReview {
	if sc == nil {
		return nil
	}
	return &inputReview{sc: sc}
}

// texts lists the texts the model could read of the decoded files, in path
// order: each read input's text view (the decoded text staged under
// inputs-text, or the input itself when it was read as it is), its Markdown
// view, and its original when that is a text other than the text view (an
// RTF or HTML document, a file no decoder took) — what read_input serves a
// binary original as, base64, is not text to steer a model with. in is
// where the decode read the inputs from.
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

// review reads texts with AI Review. It answers nil when every text passed,
// a refusal when one did not or the run's cap would be passed, and an error
// only when a staged text could not be read. added says the texts are a
// session's added file's.
func (r *inputReview) review(ctx context.Context, texts []reviewText, added bool) (*reviewRefusal, error) {
	if r == nil || len(texts) == 0 {
		return nil, nil
	}
	var size int64
	for _, t := range texts {
		fi, err := os.Lstat(t.file)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("the text of %s for AI Review: not a file this run staged", t.path)
		}
		size += fi.Size()
	}
	r.mu.Lock()
	spent := r.before.bytes + r.added.bytes
	r.mu.Unlock()
	if spent+size > MaxInputReviewText {
		return &reviewRefusal{detail: receipt.InputReviewLimit, why: fmt.Sprintf(
			"AI Review reads at most %d MiB of a run's text, and its inputs come to more (%d MiB): nothing it has not read reaches the agent",
			MaxInputReviewText>>20, (spent+size+(1<<20)-1)>>20)}, nil
	}
	pieces := make([][]string, len(texts))
	for i, t := range texts {
		b, err := os.ReadFile(t.file)
		if err != nil {
			return nil, fmt.Errorf("the text of %s for AI Review: %w", t.path, err)
		}
		pieces[i] = headedPieces(t.label, string(b))
	}
	batches := packBatches(pieces)

	var (
		mu       sync.Mutex
		counts   = reviewCounts{texts: len(texts), bytes: size}
		flagged  = map[int][]string{} // text → the questions it failed
		unread   = map[int]string{}   // text → why it could not be read
		failedAt [][]reviewPiece      // the flagged calls of several texts
	)
	call := func(ctx context.Context, body string) webscan.Result {
		res := r.sc.Scan(ctx, webscan.SourceInputFile, body)
		mu.Lock()
		counts.calls++
		counts.requests += max(res.Requests, 0)
		if res.Model != "" {
			r.mu.Lock()
			r.model, r.questionSet = res.Model, res.QuestionSet
			r.mu.Unlock()
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
				var b strings.Builder
				for _, p := range batch {
					b.WriteString(p.body)
				}
				res := call(run, b.String())
				if res.Passed() {
					continue
				}
				mu.Lock()
				// A call this review stopped is not a text it could not
				// read: the run is refused for the one that stopped it.
				if res.Verdict != webscan.Withheld && run.Err() != nil && ctx.Err() == nil {
					mu.Unlock()
					continue
				}
				switch {
				case res.Verdict == webscan.Withheld && distinctTexts(batch) > 1:
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
	// the refusal names the file; if none fails alone, each is named, as
	// they were read together.
	for _, batch := range failedAt {
		named := false
		for _, p := range batch {
			if _, done := flagged[p.text]; done {
				continue // a text in several parts is named once
			}
			if pace(ctx) != nil {
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

	r.mu.Lock()
	into := &r.before
	if added {
		into = &r.added
	}
	into.texts += counts.texts
	into.calls += counts.calls
	into.requests += counts.requests
	into.bytes += counts.bytes
	r.mu.Unlock()

	switch {
	case len(flagged) > 0:
		return &reviewRefusal{detail: receipt.InputFlagged, why: "AI Review flagged " + namedTexts(texts, flagged)}, nil
	case len(unread) > 0:
		return &reviewRefusal{detail: receipt.InputNotReviewed, why: "AI Review could not read " + namedUnread(texts, unread) +
			", and nothing it has not read reaches the agent"}, nil
	case ctx.Err() != nil:
		return &reviewRefusal{detail: receipt.InputNotReviewed, why: "the run stopped before AI Review had read its inputs"}, nil
	}
	return nil, nil
}

// headedPieces cuts a text into the parts one call can carry, each headed
// by its label (and its part's number, when there are several) on a line
// of its own.
func headedPieces(label, text string) []string {
	label = cutRunes(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, label), reviewLabelMax)
	// Room for the heading: "<label> (part n of m)\n".
	parts := splitText(text, webscan.MaxText-len(label)-40)
	out := make([]string, len(parts))
	for i, p := range parts {
		head := label
		if len(parts) > 1 {
			head = fmt.Sprintf("%s (part %d of %d)", label, i+1, len(parts))
		}
		out[i] = head + "\n" + p
	}
	return out
}

// splitText cuts s into parts of at most n bytes, at a line's end where
// one falls in the second half of the part, and never inside a character.
func splitText(s string, n int) []string {
	if s == "" {
		return []string{""}
	}
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

// packBatches packs the texts' pieces, in order, into calls of at most
// webscan.MaxText bytes and reviewBatchTexts texts.
func packBatches(pieces [][]string) [][]reviewPiece {
	var out [][]reviewPiece
	var cur []reviewPiece
	size, texts := 0, 0
	for i, ps := range pieces {
		for j, body := range ps {
			newText := j == 0
			if len(cur) > 0 && (size+len(body) > webscan.MaxText || (newText && texts == reviewBatchTexts)) {
				out = append(out, cur)
				cur, size, texts = nil, 0, 0
			}
			if newText || len(cur) == 0 {
				texts++
			}
			cur = append(cur, reviewPiece{text: i, body: body})
			size += len(body)
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// distinctTexts counts the texts a call carries.
func distinctTexts(batch []reviewPiece) int { return len(textsOf(batch)) }

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

// namedTexts names the inputs AI Review flagged, with the questions each
// failed: "a.docx (injection), b.pdf".
func namedTexts(texts []reviewText, flagged map[int][]string) string {
	byPath := map[string][]string{}
	for i, qs := range flagged {
		p := texts[i].path
		byPath[p] = append(byPath[p], qs...)
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		qs := uniqueSorted(byPath[p])
		if len(qs) > 0 {
			names = append(names, p+" ("+strings.Join(qs, ", ")+")")
		} else {
			names = append(names, p)
		}
	}
	return joinNamed(names)
}

// namedUnread names the inputs AI Review could not read, with why.
func namedUnread(texts []reviewText, unread map[int]string) string {
	byPath := map[string]string{}
	for i, why := range unread {
		byPath[texts[i].path] = why
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, p+" ("+strings.TrimPrefix(byPath[p], "it ")+")")
	}
	return joinNamed(names)
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
		t := time.NewTimer(time.Until(at))
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
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
	if r.before.calls == 0 && r.added.calls == 0 {
		return ""
	}
	what := "AI Review (the registry's model-input scan"
	if r.questionSet != "" {
		what += ", question set " + shortSet(r.questionSet)
	}
	if r.model != "" {
		what += ", " + r.model
	}
	what += ") read the inputs' text"
	var parts []string
	if r.before.calls > 0 {
		parts = append(parts, "before the agent started: "+r.before.say())
	}
	if r.added.calls > 0 {
		parts = append(parts, "the files added during the session, each before the agent was told of it: "+r.added.say())
	}
	return what + "; " + strings.Join(parts, "; ")
}

func (c reviewCounts) say() string {
	return fmt.Sprintf("%d text(s), %s, in %d call(s) and %d judge request(s)", c.texts, sizeWord(c.bytes), c.calls, c.requests)
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
	return Classify(ExitPolicy, inputRefused(rf.detail, fmt.Errorf("the run was refused before the agent started: %s", rf.why)))
}
