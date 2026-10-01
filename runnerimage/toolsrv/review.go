// The review grant (plan desktop-review-key-and-run-cards.md §6.2, D18, D24,
// D27; agentry-cli#503): a built-in that reviews the QuickAgent handed to it
// as its inputs, with the person's own TypeSafe key. Two things change in
// its tools container:
//
//   - ai_review asks AI Review's release rubric, the six questions stage S5
//     asks, over the manifest's canonical JSON (staged by the host, since
//     this image has no YAML parser) and every text file of the package,
//     through this image's copy of the core's pkg/judge, and answers with
//     each question's score and the file that drew it, the model and the
//     rubric hash. It never answers with the package's text: a file
//     read_input withheld would reach the model through an excerpt.
//   - read_input returns a file only after AI Review's model-input questions
//     pass it. A file flagged, or one that could not be scanned, is withheld:
//     the model gets its path and the reason, never the text.
//
// The key reaches this container alone, through the run's gateway secrets
// file, and every answer is scrubbed of it. The container's network reaches
// TypeSafe's API and nowhere else: the compiler gives it ReviewHost as its
// one allowed host, through the gateway's proxy, and a scan or a question
// goes nowhere else. Every file scanned and every ai_review call is a line
// of the run's review ledger, which the engine counts into the receipt.

package toolsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/judge"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// Review configures the review grant. Enabled is compiled (a built-in the
// CLI grants it to); Ledger is the run's, stamped by the engine.
type Review struct {
	Enabled bool `json:"enabled"`
	// Ledger is the container path the review ledger is appended to, one
	// JSON line a file scanned or a call answered.
	Ledger string `json:"ledger,omitempty"`
}

// EnvReviewJevKey carries the person's own TypeSafe key into this container
// alone, from the run's secrets file. It never reaches the agent container.
const EnvReviewJevKey = "AGENTRY_REVIEW_JEV_KEY"

// ReviewSecretEnv is every key the review grant may be given.
func ReviewSecretEnv() []string { return []string{EnvReviewJevKey} }

// ReviewHost is the one host a review's tools container reaches: TypeSafe's
// API, judge.DefaultBaseURL's host (a test holds the two equal).
const ReviewHost = "api.typesafe.ai"

// The review's bounds.
const (
	// ReviewFileBytes is the most of one file a scan or the rubric reads:
	// stage S1's text cap, past which the registry reports the file as not
	// fully scanned.
	ReviewFileBytes = 1 << 20
	// ReviewTotalBytes is the most text one ai_review call sends.
	ReviewTotalBytes = 4 << 20
	// ReviewMaxFiles is the most files one ai_review call reads.
	ReviewMaxFiles = 500
	// DefaultMaxReviewCalls bounds ai_review calls a run.
	DefaultMaxReviewCalls = 3
	// reviewScanTimeout bounds one file's scan, up to ReviewFileBytes of
	// it, 8 KiB a request; reviewRubricTimeout one ai_review call.
	reviewScanTimeout   = 5 * time.Minute
	reviewRubricTimeout = 15 * time.Minute
	// reviewSniffBytes is the window S1's text sniff reads.
	reviewSniffBytes = 512
)

// ReviewEntry is one line of the review ledger: a file read_input scanned or
// refused, or one ai_review call.
type ReviewEntry struct {
	Tool string `json:"tool"`
	// Path is the input file, inputs-relative; for ai_review, the package
	// directory it read (dir, cleaned; "." is the inputs root).
	Path string `json:"path,omitempty"`
	// Manifest is the manifest JSON an ai_review call read, inputs-relative.
	Manifest string `json:"manifest,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	// Refused says why a call read nothing, in the words the model was
	// given.
	Refused string `json:"refused,omitempty"`
	// Files and NotRead are what an ai_review call sent and left out.
	Files   int `json:"files,omitempty"`
	NotRead int `json:"notRead,omitempty"`
	// RubricHash names what the rubric asked; Findings counts the questions
	// that scored 2 or more.
	RubricHash string `json:"rubricHash,omitempty"`
	Findings   int    `json:"findings,omitempty"`
	// Axes are an answered ai_review call's six scores, as the model was
	// given them: the record a host post-step reads the review from, so
	// that what it reports never rests on what the model says the tool
	// answered (agentry-cli#504).
	Axes []ReviewAxis `json:"axes,omitempty"`
	webscan.Result
}

// reviewState is the review grant's per-run state.
type reviewState struct {
	mu sync.Mutex
	// scanner reads an input file as model input; asker asks the rubric.
	// Either nil means no key: every file is withheld as not scanned and
	// ai_review answers that it cannot run.
	scanner webscan.Scanner
	asker   judge.Asker
	// verdicts are read_input's scans, one a file: a file is scanned once
	// and every page of it is answered from that verdict. A scan that
	// failed is not asked again in the run: the file stays withheld, and
	// the model cannot spend the person's key retrying it. Requests are
	// handled one at a time (mcp.go), so no two reads scan a file at once.
	verdicts map[string]reviewVerdict
	calls    int
	ledger   *os.File
}

type reviewVerdict struct {
	size    int64
	modTime time.Time
	res     webscan.Result
	// binary is a file that is not UTF-8 text: read_input never returns
	// one, under the grant or not.
	binary bool
}

// newReviewState builds the scanner and the asker from the key in this
// container's environment.
func (s *server) newReviewState() *reviewState {
	r := &reviewState{verdicts: map[string]reviewVerdict{}}
	key := os.Getenv(EnvReviewJevKey)
	if key == "" {
		_, _ = fmt.Fprintln(s.logw, "review: no AI Review key: every input file will be withheld")
		return r
	}
	if j, err := webscan.NewJev(key, "", nil); err == nil {
		r.scanner = j.WithMaxText(ReviewFileBytes)
	}
	if c, err := judge.New(judge.Config{APIKey: key}); err == nil {
		r.asker = c
	}
	return r
}

// review returns the run's review state, built on first use.
func (s *server) review() *reviewState {
	s.reviewOnce.Do(func() {
		if s.reviewState == nil {
			s.reviewState = s.newReviewState()
		}
	})
	return s.reviewState
}

// recordReview logs an entry and appends it to the review ledger.
func (s *server) recordReview(e ReviewEntry) {
	line := fmt.Sprintf("review: %s %q", e.Tool, e.Path)
	switch {
	case e.Refused != "":
		line += " refused: " + e.Refused
	default:
		line += fmt.Sprintf(" %s score=%d", e.Verdict, e.Score)
		if len(e.Flagged) > 0 {
			line += " flagged=" + strings.Join(e.Flagged, ",")
		}
		if e.Failure != "" {
			line += " failure=" + e.Failure
		}
	}
	_, _ = fmt.Fprintln(s.logw, line)
	if s.spec.Review.Ledger == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	r := s.review()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ledger == nil {
		if err := os.MkdirAll(filepath.Dir(s.spec.Review.Ledger), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(s.spec.Review.Ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
		if err != nil {
			_, _ = fmt.Fprintf(s.logw, "review: open ledger: %v\n", err)
			return
		}
		s.own(s.spec.Review.Ledger)
		r.ledger = f
	}
	_, _ = r.ledger.Write(append(b, '\n'))
}

// ---- read_input, scanned ----

// reviewedInput answers whether read_input may page out the file at abs:
// ok when it may, and otherwise what the model is told instead and whether
// that is a tool error. The whole file is scanned once; a file past
// ReviewFileBytes is withheld as too large, and one that is not UTF-8 text
// is refused as read_input refuses binary without the grant. (ai_review
// picks its files by stage S1's sniff instead, as the registry reads a
// release; read_input keeps its own rule, which never returns binary.)
func (s *server) reviewedInput(rel, abs string, fi fs.FileInfo) (instead string, isErr, ok bool) {
	r := s.review()
	r.mu.Lock()
	v, cached := r.verdicts[rel]
	r.mu.Unlock()
	if cached && v.size == fi.Size() && v.modTime.Equal(fi.ModTime()) {
		return v.answer(rel)
	}
	v = reviewVerdict{size: fi.Size(), modTime: fi.ModTime()}
	switch data, err := readCapped(abs, ReviewFileBytes); {
	case errors.Is(err, errTooLarge):
		v.res = webscan.Result{Verdict: webscan.NotScanned, Failure: webscan.FailureTooLarge}
		s.recordReview(ReviewEntry{Tool: ToolReadInput, Path: rel, Bytes: ReviewFileBytes + 1, Result: v.res})
	case err != nil:
		return "read_input: " + err.Error(), true, false
	case !utf8.Valid(data):
		v.binary = true
		s.recordReview(ReviewEntry{Tool: ToolReadInput, Path: rel, Bytes: len(data), Refused: "binary file"})
	default:
		v.res = s.scanInput(string(data))
		s.recordReview(ReviewEntry{Tool: ToolReadInput, Path: rel, Bytes: len(data), Result: v.res})
	}
	r.mu.Lock()
	r.verdicts[rel] = v
	r.mu.Unlock()
	return v.answer(rel)
}

// answer is what read_input says for a verdict.
func (v reviewVerdict) answer(rel string) (instead string, isErr, ok bool) {
	switch {
	case v.binary:
		return fmt.Sprintf("binary file; %d bytes", v.size), true, false
	case v.res.Passed():
		return "", false, true
	}
	return withheldInput(rel, v.res), false, false
}

// scanInput scans one input file's text, bounded in time.
func (s *server) scanInput(text string) webscan.Result {
	r := s.review()
	if r.scanner == nil {
		return webscan.Result{Verdict: webscan.NotScanned, Failure: webscan.FailureUnavailable}
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewScanTimeout)
	defer cancel()
	return r.scanner.Scan(ctx, webscan.SourceInputFile, text)
}

// withheldInput is what the model gets of an input file that did not pass.
func withheldInput(rel string, res webscan.Result) string {
	return fmt.Sprintf("withheld: the input file %s was not given to you, because %s.\nName it as withheld in what you write, and do not guess at what it says.",
		rel, res.Reason())
}

var errTooLarge = errors.New("too large")

// readCapped reads a whole file of at most limit bytes.
func readCapped(abs string, limit int64) ([]byte, error) {
	f, err := os.Open(abs) //nolint:gosec // resolved under a tool root by the caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// ---- ai_review ----

func aiReviewSchema() map[string]any {
	return objSchema(map[string]any{
		"manifest": strProp("inputs-relative path of the manifest's canonical JSON, as the host staged it"),
		"dir":      strProp("inputs-relative directory of the package whose files are reviewed; empty or \".\" is the inputs root"),
	}, "manifest")
}

const aiReviewDescription = "Ask AI Review's release rubric, the six questions the registry's stage S5 asks, about a QuickAgent package in your inputs: its manifest's canonical JSON and every text file under dir. " +
	"Returns each question's score (0 to 3) and the file that drew it, the findings (the questions that scored 2 or more), the files left out and why, the model and the rubric hash. " +
	"It never returns the package's text. It uses the person's own key; a run has at most 3 calls."

// ReviewAxis is one rubric question's answer over the package.
type ReviewAxis struct {
	ID string `json:"id"`
	// Score is 0 to 3, as the registry scores it (judge.ScoreFromProbability).
	Score       int       `json:"score"`
	Probability float64   `json:"probability"`
	Samples     []float64 `json:"samples,omitempty"`
	// File is the package path of the chunk that drew the highest
	// probability ("agent.yaml" is the manifest).
	File string `json:"file,omitempty"`
}

// ReviewNotRead is a file ai_review left out, and why.
type ReviewNotRead struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ReviewAnswer is ai_review's answer.
type ReviewAnswer struct {
	Model      string `json:"model"`
	RubricHash string `json:"rubricHash"`
	// Axes are all six questions, in the rubric's order; Findings those of
	// them that scored 2 or more.
	Axes     []ReviewAxis    `json:"axes"`
	Findings []ReviewAxis    `json:"findings"`
	Files    []string        `json:"files"`
	NotRead  []ReviewNotRead `json:"notRead,omitempty"`
	// Requests and InputTokens are what the call cost the judge, for the
	// record; AI Review is 1 credit an operation (DEV-150).
	Requests    int   `json:"requests"`
	InputTokens int64 `json:"inputTokens"`
}

// The reasons a file is left out, as ai_review says them.
const (
	notReadBinary     = "not text"
	notReadTooLarge   = "past the 1 MiB a file the registry scans as text"
	notReadShipped    = "a shipped knowledge pack's text, which the registry reads in its own stage"
	notReadOverBudget = "past what one review sends (4 MiB, 500 files)"
	notReadLink       = "a link, which a package archive does not carry"
)

func (s *server) toolAIReview(raw json.RawMessage) (string, bool) {
	text, isErr := s.aiReview(raw)
	return s.scrubSecrets(text), isErr
}

func (s *server) aiReview(raw json.RawMessage) (string, bool) {
	var a struct {
		Manifest string `json:"manifest"`
		Dir      string `json:"dir"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "ai_review: " + err.Error(), true
	}
	if a.Manifest == "" {
		return "ai_review: manifest is required", true
	}
	if s.spec.InputsDir == "" {
		return "ai_review: no inputs channel configured", true
	}
	r := s.review()
	if r.asker == nil {
		s.recordReview(ReviewEntry{Tool: ToolAIReview, Refused: "no AI Review key"})
		return "ai_review: this run holds no AI Review key, so the review cannot run", true
	}
	manifestAbs, err := resolveUnder(s.spec.InputsDir, path.Clean(a.Manifest))
	if err != nil {
		return "ai_review: manifest: " + err.Error(), true
	}
	manifestJSON, err := readCapped(manifestAbs, ReviewFileBytes)
	if err != nil {
		return "ai_review: manifest: " + err.Error(), true
	}
	if t := bytes.TrimSpace(manifestJSON); len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return "ai_review: manifest is not a JSON object; pass the canonical JSON the host staged, not agent.yaml", true
	}
	dir := a.Dir
	if dir == "" {
		dir = "."
	}
	root, err := resolveUnder(s.spec.InputsDir, path.Clean(dir))
	if err != nil {
		return "ai_review: dir: " + err.Error(), true
	}
	ships := ShippedPacks(manifestJSON)
	files, notRead, err := reviewFiles(root, manifestAbs, ships)
	if err != nil {
		return "ai_review: " + err.Error(), true
	}

	// A call counts once it asks the judge: a refused argument spends none.
	r.mu.Lock()
	if r.calls >= DefaultMaxReviewCalls {
		r.mu.Unlock()
		s.recordReview(ReviewEntry{Tool: ToolAIReview, Refused: "the run's calls are spent"})
		return fmt.Sprintf("ai_review: this run's %d calls are spent; write the report with what you have", DefaultMaxReviewCalls), true
	}
	r.calls++
	r.mu.Unlock()
	sc := judge.NewRubricScanner(r.asker)
	ctx, cancel := context.WithTimeout(context.Background(), reviewRubricTimeout)
	defer cancel()
	axes, usage, err := sc.Scan(ctx, manifestJSON, ships, files)
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	entry := ReviewEntry{Tool: ToolAIReview, Path: path.Clean(dir), Manifest: path.Clean(a.Manifest), Files: len(files), NotRead: len(notRead), RubricHash: sc.Hash(),
		Result: webscan.Result{Model: sc.Model(), Requests: usage.Requests, InputTokens: int64(usage.InputTokens)}}
	if err != nil {
		entry.Verdict, entry.Failure = webscan.NotScanned, judge.FailureOf(ctx, err)
		s.recordReview(entry)
		return "ai_review: AI Review could not finish the review (" + entry.Failure + "); a rubric half asked is not an answer", true
	}
	ans := ReviewAnswer{Model: sc.Model(), RubricHash: sc.Hash(), Files: paths, NotRead: notRead,
		Requests: usage.Requests, InputTokens: int64(usage.InputTokens), Axes: []ReviewAxis{}, Findings: []ReviewAxis{}}
	for _, q := range judge.RubricQuestions() {
		res := axes[q.ID]
		ax := ReviewAxis{ID: q.ID, Score: judge.ScoreFromProbability(res.Best), Probability: res.Best, Samples: res.Samples, File: res.Path}
		ans.Axes = append(ans.Axes, ax)
		if ax.Score >= judge.WithholdScore {
			ans.Findings = append(ans.Findings, ax)
			if ax.Score > entry.Score {
				entry.Score = ax.Score
			}
			entry.Flagged = append(entry.Flagged, ax.ID)
		}
	}
	entry.Axes = ans.Axes
	entry.Findings, entry.Verdict = len(ans.Findings), webscan.Pass
	if len(ans.Findings) > 0 {
		entry.Verdict = webscan.Withheld
	}
	s.recordReview(entry)
	out, err := json.MarshalIndent(ans, "", "  ")
	if err != nil {
		return "ai_review: " + err.Error(), true
	}
	return string(out), false
}

// reviewFiles is every text file under root, as stage S5 reads a release:
// package paths relative to root, sorted, the manifest's own staged JSON
// left out, and a shipped pack's tree left to its own stage. What is left
// out is named.
func reviewFiles(root, manifestAbs string, ships []string) ([]judge.RubricFile, []ReviewNotRead, error) {
	var files []judge.RubricFile
	var notRead []ReviewNotRead
	total := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == manifestAbs {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular():
			notRead = append(notRead, ReviewNotRead{Path: rel, Reason: notReadLink})
			return nil
		case shippedFile(ships, rel):
			notRead = append(notRead, ReviewNotRead{Path: rel, Reason: notReadShipped})
			return nil
		}
		data, err := readCapped(p, ReviewFileBytes)
		switch {
		case errors.Is(err, errTooLarge):
			notRead = append(notRead, ReviewNotRead{Path: rel, Reason: notReadTooLarge})
			return nil
		case err != nil:
			return err
		case !LooksText(data[:min(len(data), reviewSniffBytes)]):
			notRead = append(notRead, ReviewNotRead{Path: rel, Reason: notReadBinary})
			return nil
		case len(files) >= ReviewMaxFiles || total+len(data) > ReviewTotalBytes:
			notRead = append(notRead, ReviewNotRead{Path: rel, Reason: notReadOverBudget})
			return nil
		}
		total += len(data)
		files = append(files, judge.RubricFile{Path: rel, Content: string(data)})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Slice(notRead, func(i, j int) bool { return notRead[i].Path < notRead[j].Path })
	return files, notRead, nil
}

// LooksText is stage S1's text sniff (the core's pkg/archive LooksText,
// which this stdlib-only image cannot import; a test holds the two equal):
// no NUL byte, and under 5% control characters, over the first 512 bytes.
func LooksText(head []byte) bool {
	if len(head) == 0 {
		return true
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	nonPrint := 0
	for _, b := range head {
		if b < 0x09 || (b > 0x0d && b < 0x20) {
			nonPrint++
		}
	}
	return nonPrint*100/len(head) < 5
}

// ShippedPacks are the knowledge packs a manifest's canonical JSON ships,
// as the core's manifest.ShippedPacksOf reads them (a test holds the two
// equal).
func ShippedPacks(manifestJSON []byte) []string {
	var m struct {
		Knowledge struct {
			Ships []struct {
				Name string `json:"name"`
			} `json:"ships"`
		} `json:"knowledge"`
	}
	if json.Unmarshal(manifestJSON, &m) != nil {
		return nil
	}
	var out []string
	for _, s := range m.Knowledge.Ships {
		if validPackName(s.Name) {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// shippedFile reports whether a package path is in a shipped pack's tree,
// knowledge/<name>/.
func shippedFile(ships []string, rel string) bool {
	for _, name := range ships {
		if strings.HasPrefix(rel, "knowledge/"+name+"/") {
			return true
		}
	}
	return false
}
