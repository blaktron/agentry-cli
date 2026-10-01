// Package webscan is AI Review of what the Drafter's web tools bring back,
// before its model reads any of it (plan builder-rework.md §7.3, D7, D20–D25;
// agentry-cli#345). A text is scanned as model input, with the core's
// model-input question set, in one of two places:
//
//   - Registry: a hosted draft pushes the text to the registry's
//     POST /api/v1/model-input/scan with the job's own credential, and
//     searches through POST /api/v1/model-input/search, which scans every
//     result before it answers (D20, D21). No sandbox host holds the
//     platform's Jev or search key.
//   - Jev: a local draft scans with the person's own Jev key (D25), through
//     this image's copy of the core's pkg/judge (runnerimage/judge, kept
//     byte for byte by scripts/sync-image-judge.sh), so a local scan asks
//     exactly what the registry asks.
//
// A text that is flagged, or that could not be scanned, is withheld: a caller
// gives the model its address and the reason, never the text (D22). A scan
// never fails with an error; a failure is a verdict.
//
// The package is part of the embedded runner-image build context and stays
// stdlib-only (runnerimage.TestEmbeddedSourcesStdlibOnly). The CLI uses it on
// the host too, for a hosted draft's scan of the description.
package webscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/judge"
	"github.com/blaktron/agentry-cli/runnerimage/websearch"
)

// Source is what kind of text a scan reads, the core's closed list.
type Source = judge.Source

// The sources the Drafter's scans read.
const (
	SourceDescription  = judge.SourceDescription
	SourceSearchResult = judge.SourceSearchResult
	SourcePage         = judge.SourcePage
	SourceRegistry     = judge.SourceRegistry
)

// Verdict is a scan's answer.
type Verdict = judge.Verdict

// The verdicts.
const (
	Pass       = judge.VerdictPass
	Withheld   = judge.VerdictWithheld
	NotScanned = judge.VerdictNotScanned
)

// MaxText is the most one scan request carries: the registry route's cap,
// eight of the judge's 8 KiB chunks.
const MaxText = 64 << 10

// Result is one text's scan.
type Result struct {
	Verdict Verdict `json:"verdict"`
	// Score is the highest over every chunk and question, 0 to 3.
	Score int `json:"score"`
	// Flagged lists the questions that scored 2 or more, sorted.
	Flagged []string `json:"flagged,omitempty"`
	// Failure is why a text was not scanned, a closed word
	// (judge.FailureTimeout, judge.FailureJudgeError, or "too-large",
	// "unavailable", "refused"); never a vendor's text.
	Failure string `json:"failure,omitempty"`
	// Model and QuestionSet name what answered and what was asked.
	Model       string `json:"model,omitempty"`
	QuestionSet string `json:"questionSet,omitempty"`
	// Requests and InputTokens are what the scan cost the judge, for the
	// record. Never a price: AI Review is 1 credit an operation (DEV-150).
	Requests    int   `json:"requests,omitempty"`
	InputTokens int64 `json:"inputTokens,omitempty"`
}

// Passed reports whether the text may reach a model.
func (r Result) Passed() bool { return r.Verdict == Pass }

// Reason is what the model, and the draft's notes, are told of a text that
// did not pass: which of AI Review's questions it failed, or that it could
// not be scanned. It is built from closed words only.
func (r Result) Reason() string {
	switch {
	case r.Verdict == Withheld && len(r.Flagged) > 0:
		return "AI Review flagged it (" + strings.Join(r.Flagged, ", ") + ")"
	case r.Verdict == Withheld:
		return "AI Review flagged it"
	case r.Verdict == Pass:
		return ""
	}
	if r.Failure != "" {
		return "it could not be scanned (" + r.Failure + ")"
	}
	return "it could not be scanned"
}

// Failure words of this package's own, beside the judge's.
const (
	FailureTooLarge    = "too-large"
	FailureUnavailable = "unavailable"
	FailureRefused     = "refused"
)

// Scanner scans a text as model input. Scan never returns an error.
type Scanner interface {
	Scan(ctx context.Context, source Source, text string) Result
}

// Hit is one web search result as a model may be shown it. A result that did
// not pass carries its origin alone in URL, and no title or snippet.
type Hit struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Result
}

// Searcher searches the web and scans every result before it answers.
type Searcher interface {
	// Name is the provider's name, for the record.
	Name() string
	Search(ctx context.Context, query string, count int) ([]Hit, error)
}

// ---- the registry's routes (hosted) ----

// JobTokenHeader carries a hosted job's credential (agentry#608).
const JobTokenHeader = "X-Agentry-Job-Token"

// Registry calls the registry's model-input routes with a hosted job's
// credential. The registry scans with the deployment's judge and searches
// with its own provider key; neither key leaves it.
type Registry struct {
	// Base is the registry's root, e.g. https://secureagentry.net; the
	// routes are under /api/v1.
	Base string
	// JobToken is the job's credential, presented in JobTokenHeader and
	// nowhere else.
	JobToken string
	// HTTP carries the requests; nil is a client with a 60-second timeout.
	// Redirects are never followed: the credential rides on every request.
	HTTP *http.Client
}

func (r *Registry) client() *http.Client {
	c := &http.Client{Timeout: 60 * time.Second}
	if r.HTTP != nil {
		copied := *r.HTTP
		c = &copied
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func (r *Registry) post(ctx context.Context, path string, body any, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.Base, "/")+"/api/v1"+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(JobTokenHeader, r.JobToken)
	resp, err := r.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("the registry answered %d%s", resp.StatusCode, problemCode(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("the registry's answer is not the route's shape: %w", err)
	}
	return resp.StatusCode, nil
}

// problemCode is the problem document's `code`, when it has one: a closed
// word of the registry's own, never text of the scanned input.
func problemCode(raw []byte) string {
	var p struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Code == "" || len(p.Code) > 64 {
		return ""
	}
	for _, c := range p.Code {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return ""
		}
	}
	return " (" + p.Code + ")"
}

// routeScan is the route's answer, the fields read of it.
type routeScan struct {
	Verdict     string         `json:"verdict"`
	Score       int            `json:"score"`
	Flagged     []string       `json:"flagged"`
	Model       string         `json:"model"`
	QuestionSet string         `json:"questionSet"`
	Chunks      []routeChunk   `json:"chunks"`
	Usage       routeUsage     `json:"usage"`
	Scores      map[string]int `json:"scores"`
}

type routeChunk struct {
	Failure string `json:"failure"`
}

type routeUsage struct {
	Requests    int   `json:"requests"`
	InputTokens int64 `json:"inputTokens"`
}

// Scan pushes text to POST /api/v1/model-input/scan. Anything but a 200 with
// a known verdict is not scanned.
func (r *Registry) Scan(ctx context.Context, source Source, text string) Result {
	if len(text) > MaxText {
		return Result{Verdict: NotScanned, Failure: FailureTooLarge}
	}
	var got routeScan
	status, err := r.post(ctx, "/model-input/scan", map[string]string{"source": string(source), "text": text}, &got)
	if err != nil {
		return Result{Verdict: NotScanned, Failure: routeFailure(ctx, status, err)}
	}
	res := Result{Verdict: Verdict(got.Verdict), Score: got.Score, Flagged: sortedCopy(got.Flagged), Model: got.Model,
		QuestionSet: got.QuestionSet, Requests: got.Usage.Requests, InputTokens: got.Usage.InputTokens}
	switch res.Verdict {
	case Pass, Withheld:
	case NotScanned:
		for _, c := range got.Chunks {
			if c.Failure != "" {
				res.Failure = closedFailure(c.Failure)
				break
			}
		}
	default:
		return Result{Verdict: NotScanned, Failure: judge.FailureJudgeError}
	}
	// A pass the route's own numbers contradict is not a pass.
	if res.Verdict == Pass && (res.Score >= judge.WithholdScore || len(res.Flagged) > 0) {
		res.Verdict = Withheld
	}
	return res
}

// routeFailure is the closed word for a route call that failed.
func routeFailure(ctx context.Context, status int, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return judge.FailureTimeout
	case status == http.StatusServiceUnavailable:
		return FailureUnavailable
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests ||
		status == http.StatusPaymentRequired || status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return FailureRefused
	}
	return judge.FailureJudgeError
}

func closedFailure(f string) string {
	switch f {
	case judge.FailureTimeout, judge.FailureJudgeError:
		return f
	}
	return judge.FailureJudgeError
}

// RegistrySearch is the registry's brokered search, as a Searcher.
type RegistrySearch struct{ *Registry }

// Name implements Searcher.
func (RegistrySearch) Name() string { return "registry" }

type routeHit struct {
	URL     string   `json:"url"`
	Title   *string  `json:"title"`
	Snippet *string  `json:"snippet"`
	Verdict string   `json:"verdict"`
	Score   int      `json:"score"`
	Flagged []string `json:"flagged"`
}

type routeSearch struct {
	Provider    string     `json:"provider"`
	Results     []routeHit `json:"results"`
	Model       string     `json:"model"`
	QuestionSet string     `json:"questionSet"`
	Usage       routeUsage `json:"usage"`
}

// Search asks POST /api/v1/model-input/search. The registry scanned every
// result; a result it did not pass comes back as its origin, and one whose
// verdict this client does not know is held to the same rule.
func (s RegistrySearch) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	var got routeSearch
	if _, err := s.post(ctx, "/model-input/search", map[string]any{"query": query, "count": count}, &got); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(got.Results))
	for i, h := range got.Results {
		res := Result{Verdict: Verdict(h.Verdict), Score: h.Score, Flagged: sortedCopy(h.Flagged), Model: got.Model, QuestionSet: got.QuestionSet}
		if i == 0 {
			// The search's cost is the whole answer's; it is counted once.
			res.Requests, res.InputTokens = got.Usage.Requests, got.Usage.InputTokens
		}
		hit := Hit{Result: res}
		if res.Verdict == Pass && res.Score < judge.WithholdScore && len(res.Flagged) == 0 && h.Title != nil && h.Snippet != nil {
			hit.URL, hit.Title, hit.Snippet = h.URL, *h.Title, *h.Snippet
		} else {
			if res.Verdict == Pass {
				res.Verdict = Withheld
				hit.Result = res
			}
			hit.URL = websearch.Origin(h.URL)
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// ---- the person's own keys (local) ----

// Jev scans with the person's own key through the core's judge.
type Jev struct {
	s *judge.ModelInputScanner
	// max is the most text one scan takes; zero is MaxText.
	max int
}

// NewJev builds a scanner over the person's Jev key; baseURL empty is
// TypeSafe's API.
func NewJev(key, baseURL string, hc *http.Client) (*Jev, error) {
	c, err := judge.New(judge.Config{APIKey: key, BaseURL: baseURL, HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	return &Jev{s: judge.NewModelInputScanner(c)}, nil
}

// WithMaxText is the same scanner taking up to n bytes a text: a review's
// input files (toolsrv's review grant) are scanned whole up to stage S1's
// text cap, where the Drafter's texts keep the registry route's MaxText.
func (j *Jev) WithMaxText(n int) *Jev {
	c := *j
	c.max = n
	return &c
}

// Scan implements Scanner.
func (j *Jev) Scan(ctx context.Context, source Source, text string) Result {
	limit := MaxText
	if j.max > 0 {
		limit = j.max
	}
	if len(text) > limit {
		return Result{Verdict: NotScanned, Failure: FailureTooLarge}
	}
	return fromJudge(j.s.Scan(ctx, source, text))
}

// fromJudge is a judge scan as a Result: the verdict, the flagged questions,
// the first chunk's failure word and the usage.
func fromJudge(r judge.ScanResult) Result {
	res := Result{Verdict: r.Verdict, Score: r.Score, Flagged: sortedCopy(r.Flagged), Model: r.Model,
		QuestionSet: r.QuestionSet, Requests: r.Usage.Requests, InputTokens: int64(r.Usage.InputTokens)}
	if res.Verdict == NotScanned {
		for _, c := range r.Chunks {
			if c.Failure != "" {
				res.Failure = closedFailure(c.Failure)
				break
			}
		}
	}
	return res
}

// ProviderSearch searches with the person's own search key and scans every
// result the way the registry's search route does: as a search result, its
// title, address and snippet together, a result that did not pass keeping
// its origin alone.
type ProviderSearch struct {
	Provider websearch.Provider
	Scanner  Scanner
}

// Name implements Searcher.
func (p ProviderSearch) Name() string { return p.Provider.Name() }

// Search implements Searcher.
func (p ProviderSearch) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	q, err := websearch.CleanQuery(query)
	if err != nil {
		return nil, err
	}
	count = websearch.ClampCount(count)
	found, err := p.Provider.Search(ctx, q, count)
	if err != nil {
		return nil, err
	}
	results := websearch.Clean(found, count)
	hits := make([]Hit, len(results))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, r := range results {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := p.Scanner.Scan(ctx, SourceSearchResult, searchResultText(r))
			if res.Passed() {
				hits[i] = Hit{URL: r.URL, Title: r.Title, Snippet: r.Snippet, Result: res}
			} else {
				hits[i] = Hit{URL: websearch.Origin(r.URL), Result: res}
			}
		}()
	}
	wg.Wait()
	return hits, nil
}

// searchResultText is what a search result is scanned as: everything of it
// a model is shown, its address included. It is the core's own form
// (internal/api's searchResultText), so a local scan of a result reads what
// the registry's does.
func searchResultText(r websearch.Result) string {
	return "Title: " + r.Title + "\nURL: " + r.URL + "\nSnippet: " + r.Snippet
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
