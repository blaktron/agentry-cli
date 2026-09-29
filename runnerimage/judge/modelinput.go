package judge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Source is what kind of text a model-input scan reads. The list is closed
// and the value rides in the request's state beside the text, so the judge
// knows what it is looking at without anything from the text reaching a
// question.
type Source string

// The sources a model-input scan reads.
const (
	// SourceDescription is a person's description of what they want built,
	// read before the Drafter starts.
	SourceDescription Source = "description"
	// SourceSearchResult is one web search result: its title, address and
	// snippet.
	SourceSearchResult Source = "search_result"
	// SourcePage is a web page's text.
	SourcePage Source = "page"
	// SourceRegistry is an answer from the registry's public API.
	SourceRegistry Source = "registry"
	// SourceInputFile is a hosted run's input file, as decoded text.
	SourceInputFile Source = "input_file"
)

// Sources lists every Source.
func Sources() []Source {
	return []Source{SourceDescription, SourceSearchResult, SourcePage, SourceRegistry, SourceInputFile}
}

// ValidSource reports whether s is one of Sources.
func ValidSource(s Source) bool {
	for _, v := range Sources() {
		if v == s {
			return true
		}
	}
	return false
}

// Verdict is a model-input scan's answer.
type Verdict string

const (
	// VerdictPass is text no question scored at WithholdScore or above.
	VerdictPass Verdict = "pass"
	// VerdictWithheld is text a question scored at WithholdScore or above:
	// the model gets its address and the reason, never the text (D22).
	VerdictWithheld Verdict = "withheld"
	// VerdictNotScanned is text some part of which the judge could not
	// answer for — an error, a timeout, an answer out of shape. A caller
	// treats it as withheld (D22).
	VerdictNotScanned Verdict = "not_scanned"
)

// WithholdScore is the score at which text is withheld: 2, the gate's
// medium, a probability of 0.7 or more (D22).
const WithholdScore = 2

// Failure words a chunk that was not scanned carries. They are a closed
// vocabulary, never the vendor's text, so they can travel to a caller.
const (
	FailureTimeout    = "timeout"
	FailureJudgeError = "judge-error"
)

const (
	// modelInputParallel is how many of one text's chunks are asked at
	// once: a 32 KiB page (D24) is four.
	modelInputParallel = 4
	// The borderline band is the gate's (BandLow, BandHigh, Samples).
	modelInputBandLow  = BandLow
	modelInputBandHigh = BandHigh
	modelInputSamples  = Samples
)

// ChunkResult is one chunk's answer.
type ChunkResult struct {
	// Index is the chunk's place in the text, from 0; Bytes its length.
	Index int
	Bytes int
	// Verdict is pass, withheld or not scanned, for this chunk alone.
	Verdict Verdict
	// Scores are per question id, 0 to 3; Probabilities the highest
	// probability drawn per question. Both are empty when not scanned.
	Scores        map[string]int
	Probabilities map[string]float64
	// Failure is a Failure word when the chunk was not scanned.
	Failure string
	// Err is what went wrong, for the caller's own log; it never travels.
	Err error
}

// ScanResult is a model-input scan of one text.
type ScanResult struct {
	Verdict Verdict
	// Score is the highest score over every chunk and question.
	Score int
	// Scores is, per question id, the highest over the chunks scanned.
	Scores map[string]int
	// Flagged lists the question ids that scored WithholdScore or more,
	// sorted.
	Flagged []string
	Chunks  []ChunkResult
	// Model is the pinned model id; QuestionSet the hash of everything that
	// changes a verdict.
	Model       string
	QuestionSet string
	// Usage is what the scan cost in tokens and requests, for the record.
	// It is never a price (DEV-150: AI Review is 1 credit an operation).
	Usage Usage
}

// ModelInputScanner scans text a model is about to read with the
// model-input questions. It is safe for concurrent use.
type ModelInputScanner struct {
	asker    Asker
	parallel int
	resample bool
}

// NewModelInputScanner builds a scanner over an Asker, a *Client or a fake.
func NewModelInputScanner(a Asker) *ModelInputScanner {
	return &ModelInputScanner{asker: a, parallel: modelInputParallel, resample: true}
}

// Model is the pinned model id the scanner's answers come from.
func (s *ModelInputScanner) Model() string { return s.asker.Model() }

// QuestionSet is the hash of the model-input question set and everything
// else that changes a verdict: the model, the question ids and texts, the
// state's shape, the chunk size, the thresholds, the withhold line and the
// borderline band. Two scans with the same hash asked the same thing.
func (s *ModelInputScanner) QuestionSet() string {
	return modelInputHash(s.asker.Model(), s.resample)
}

func modelInputHash(model string, resample bool) string {
	return modelInputHashOf(model, resample, ModelInputQuestions())
}

// modelInputHashOf is the hash over a given set, so a test can show that a
// question's wording is in it (agentry#703).
func modelInputHashOf(model string, resample bool, qs []Question) string {
	parts := []string{"agentry/model-input/v1", "provider=typesafe", "model=" + model, "state=source,content"}
	for _, q := range qs {
		parts = append(parts, "question="+q.ID+"="+q.Text)
	}
	var srcs []string
	for _, src := range Sources() {
		srcs = append(srcs, string(src))
	}
	parts = append(parts, "sources="+strings.Join(srcs, ","),
		fmt.Sprintf("chunk=%d", ChunkSize), "thresholds=0.5/0.7/0.9", fmt.Sprintf("withhold=%d", WithholdScore))
	if resample {
		parts = append(parts, fmt.Sprintf("band=%.2f/%.2f", modelInputBandLow, modelInputBandHigh), fmt.Sprintf("samples=%d", modelInputSamples))
	} else {
		parts = append(parts, "samples=1")
	}
	return Hash(parts...)
}

// Scan asks the model-input questions about every chunk of text and
// answers with the whole text's verdict: withheld when any chunk scored
// WithholdScore or more, else not scanned when any chunk could not be
// answered, else pass. Text that is empty, or only white space, passes
// with no chunks: there is nothing for a model to read. The scan never
// returns an error; a failure is a verdict (D22).
func (s *ModelInputScanner) Scan(ctx context.Context, source Source, text string) ScanResult {
	res := ScanResult{Model: s.asker.Model(), QuestionSet: s.QuestionSet(), Scores: map[string]int{}}
	if !ValidSource(source) {
		res.Verdict = VerdictNotScanned
		res.Chunks = []ChunkResult{{Verdict: VerdictNotScanned, Failure: FailureJudgeError, Err: fmt.Errorf("judge: unknown source %q", source)}}
		return res
	}
	var pieces []string
	for _, c := range Chunk(text, ChunkSize) {
		if strings.TrimSpace(c) != "" {
			pieces = append(pieces, c)
		}
	}
	res.Chunks = make([]ChunkResult, len(pieces))
	usages := make([]Usage, len(pieces))
	sem := make(chan struct{}, max(1, s.parallel))
	var wg sync.WaitGroup
	for i, piece := range pieces {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res.Chunks[i], usages[i] = s.chunk(ctx, source, i, piece)
		}()
	}
	wg.Wait()

	res.Verdict = VerdictPass
	notScanned := false
	for i, c := range res.Chunks {
		res.Usage.Add(usages[i])
		if c.Verdict == VerdictNotScanned {
			notScanned = true
			continue
		}
		for id, sc := range c.Scores {
			if sc > res.Scores[id] {
				res.Scores[id] = sc
			}
			if sc > res.Score {
				res.Score = sc
			}
		}
	}
	for id, sc := range res.Scores {
		if sc >= WithholdScore {
			res.Flagged = append(res.Flagged, id)
		}
	}
	sort.Strings(res.Flagged)
	switch {
	case len(res.Flagged) > 0:
		res.Verdict = VerdictWithheld
	case notScanned:
		res.Verdict = VerdictNotScanned
	}
	return res
}

// chunk asks every question about one chunk, then, when resampling, asks
// the borderline ones twice more and keeps each question's highest answer.
func (s *ModelInputScanner) chunk(ctx context.Context, source Source, index int, piece string) (ChunkResult, Usage) {
	out := ChunkResult{Index: index, Bytes: len(piece)}
	samples := 1
	if s.resample {
		samples = modelInputSamples
	}
	qs := ModelInputQuestions()
	best, usage, err := askBanded(ctx, s.asker, map[string]string{"source": string(source), "content": piece}, qs, samples)
	if err != nil {
		return notScanned(ctx, out, err), usage
	}
	out.Verdict = VerdictPass
	out.Scores = make(map[string]int, len(qs))
	out.Probabilities = make(map[string]float64, len(qs))
	for _, q := range qs {
		p := best[q.ID]
		out.Probabilities[q.ID] = p
		out.Scores[q.ID] = ScoreFromProbability(p)
		if out.Scores[q.ID] >= WithholdScore {
			out.Verdict = VerdictWithheld
		}
	}
	return out, usage
}

func notScanned(ctx context.Context, c ChunkResult, err error) ChunkResult {
	c.Verdict = VerdictNotScanned
	c.Scores, c.Probabilities = nil, nil
	c.Err = err
	c.Failure = FailureOf(ctx, err)
	return c
}
