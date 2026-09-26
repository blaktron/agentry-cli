// Package judge is the estate's one client for TypeSafe AI's Jev and the
// question sets it is asked (plan builder-rework.md D20, D23, D25;
// agentry#608). Jev is a typed-decision model behind POST /v1/systemone: a
// request carries a `state` (the data a question is about) and a set of
// yes/no questions ("Nouls"), and each question is answered with a
// probability.
//
// Two question sets exist, both constants:
//   - the rubric's six, which stage S5 of the publication pipeline asks
//     about a release (RubricQuestions);
//   - the model-input four, which a scan of text a model is about to read
//     asks (ModelInputQuestions), through ModelInputScanner.
//
// Nothing a caller scans is ever rendered into a question: the text travels
// in the request's state, the data the questions are asked about, so a
// hostile page can no more rewrite the question than a hostile SKILL.md can
// (DEV-113's rule). The registry asks with the deployment's key; the CLI and
// the desktop ask with the person's own key through this same package, so a
// local scan asks exactly what the registry asks (D25).
package judge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// The borderline band both question sets share: an answer inside it is
// asked twice more and the highest of the Samples kept, never the mean
// (disagreement escalates). Part of every hash that names it.
const (
	BandLow  = 0.30
	BandHigh = 0.70
	Samples  = 3
)

const (
	// DefaultBaseURL is TypeSafe's API.
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultModel is the pinned Jev version the thresholds are calibrated
	// against. A moving alias is refused (PinnedModel).
	DefaultModel = "jev-1.13.0"
	// Path is the endpoint every question is posted to.
	Path = "/v1/systemone"
	// ChunkSize is the most text one request carries: 8 KiB.
	ChunkSize = 8 << 10
	// defaultRetryWait is the pause before the one retry on 429 or 529.
	defaultRetryWait = 2 * time.Second
	// maxResponse bounds what is read of one answer.
	maxResponse = 1 << 20
)

// Config builds a Client.
type Config struct {
	// BaseURL is the API root; empty is DefaultBaseURL.
	BaseURL string
	// APIKey is the bearer key. It is required, never logged, and redacted
	// from every error this package returns.
	APIKey string
	// Model is the pinned version id; empty is DefaultModel.
	Model string
	// HTTPClient carries the requests; nil is a client with a 60-second
	// timeout.
	HTTPClient *http.Client
	// RetryWait is the pause before the one retry on 429 or 529. Zero is
	// two seconds; a negative value retries at once (tests).
	RetryWait time.Duration
}

// Client asks Jev questions. It is safe for concurrent use.
type Client struct {
	endpoint  string
	apiKey    string
	model     string
	http      *http.Client
	retryWait time.Duration
}

// ErrNoKey is New's answer to a Config with no key.
var ErrNoKey = errors.New("judge: a Jev key is required")

// New builds a Client, refusing a missing key and an unpinned model.
func New(cfg Config) (*Client, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, ErrNoKey
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultModel
	}
	if !PinnedModel(model) {
		return nil, fmt.Errorf("judge: model %q is not a pinned version id; use one such as %s", model, DefaultModel)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		hc = &copied
	}
	// A redirect is never followed: the key rides on every request, and an
	// answer from anywhere but the endpoint is not an answer.
	hc.CheckRedirect = refuseRedirect
	wait := cfg.RetryWait
	switch {
	case wait == 0:
		wait = defaultRetryWait
	case wait < 0:
		wait = 0
	}
	return &Client{endpoint: base + Path, apiKey: key, model: model, http: hc, retryWait: wait}, nil
}

// PinnedModel reports whether a model id names one version — it carries a
// dotted number — rather than a moving alias (`jev-latest`, `jev-preview`,
// a bare family name). Thresholds are calibrated per version and the
// vendor's aliases move without a changelog, so New refuses anything else.
func PinnedModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || strings.HasSuffix(m, "-latest") || strings.HasSuffix(m, "-preview") {
		return false
	}
	for i := 0; i+2 < len(m); i++ {
		if m[i] >= '0' && m[i] <= '9' && m[i+1] == '.' && m[i+2] >= '0' && m[i+2] <= '9' {
			return true
		}
	}
	return false
}

// Model is the pinned model id every answer must name.
func (c *Client) Model() string { return c.model }

// Endpoint is the URL questions are posted to.
func (c *Client) Endpoint() string { return c.endpoint }

// Question is one yes/no question: an id the answer is keyed by, and its
// text. The text is a constant of this package; see RubricQuestions and
// ModelInputQuestions.
type Question struct {
	ID   string
	Text string
}

// Usage is the token count Jev reported for one or more requests. Output is
// free at Jev's price; input is what a scan costs.
type Usage struct {
	InputTokens  int
	OutputTokens int
	// Requests is how many requests the count covers.
	Requests int
}

// Add sums another reading into u.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.Requests += o.Requests
}

// Asker is what asks one request's questions: *Client, or a fake in a test.
type Asker interface {
	// Ask posts one request and returns each question's probability by id.
	Ask(ctx context.Context, state map[string]string, questions []Question) (map[string]float64, Usage, error)
	// Model is the pinned model id the answers came from.
	Model() string
}

// Ask posts one request of the given questions over the given state and
// returns each question's probability, keyed by the question's id. One
// retry on 429 or 529; every other failure, a missing answer, a probability
// outside [0,1] or a response naming a model other than the pinned one is
// an error, and the caller fails closed on it.
func (c *Client) Ask(ctx context.Context, state map[string]string, questions []Question) (map[string]float64, Usage, error) {
	if len(questions) == 0 {
		return nil, Usage{}, errors.New("judge: no questions to ask")
	}
	qs := make(map[string]any, len(questions))
	for _, q := range questions {
		qs[q.ID] = map[string]any{"type": "noul", "instructions": q.Text}
	}
	payload, err := json.Marshal(map[string]any{"model": c.model, "state": state, "questions": qs})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("judge: payload: %w", err)
	}
	raw, err := c.post(ctx, payload)
	if err != nil {
		return nil, Usage{}, err
	}
	var resp struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, Usage{}, fmt.Errorf("judge: typesafe response is not the answer shape: %w", err)
	}
	if resp.Model != c.model {
		return nil, Usage{}, fmt.Errorf("judge: typesafe served model %q, not the pinned %q", resp.Model, c.model)
	}
	out := make(map[string]float64, len(questions))
	for _, q := range questions {
		a, ok := resp.Answers[q.ID]
		if !ok || a.Noul == nil {
			return nil, Usage{}, fmt.Errorf("judge: typesafe answered without the %s question", q.ID)
		}
		if *a.Noul < 0 || *a.Noul > 1 {
			return nil, Usage{}, fmt.Errorf("judge: typesafe probability %v for %s out of range", *a.Noul, q.ID)
		}
		out[q.ID] = *a.Noul
	}
	u := Usage{Requests: 1}
	if resp.Usage != nil {
		u.InputTokens, u.OutputTokens = resp.Usage.InputTokens, resp.Usage.OutputTokens
	}
	return out, u, nil
}

// post sends the payload, retrying once on 429/529 after retryWait. The
// error carries a bounded snippet of the vendor's body with the key
// redacted, since an error reaches a stage log and a report.
func (c *Client) post(ctx context.Context, payload []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		raw, status, err := c.once(ctx, payload)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			return raw, nil
		}
		// Redact before cutting: a key that straddled the cut would keep
		// its first half.
		snippet := strings.ReplaceAll(string(raw), c.apiKey, "[key]")
		if len(snippet) > 200 {
			snippet = strings.ToValidUTF8(snippet[:200], "")
		}
		retryable := status == http.StatusTooManyRequests || status == 529
		if !retryable || attempt >= 1 {
			return nil, &StatusError{Status: status, Snippet: snippet}
		}
		if c.retryWait > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("judge: typesafe request: %w", ctx.Err())
			case <-time.After(c.retryWait):
			}
		}
	}
}

// StatusError is a non-200 answer: the status and a bounded snippet of the
// body with the key redacted. The snippet can echo what was asked, so a
// caller that must not log scanned text logs Status alone.
type StatusError struct {
	Status  int
	Snippet string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("judge: typesafe endpoint returned %d: %s", e.Status, e.Snippet)
}

func (c *Client) once(ctx context.Context, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("judge: typesafe request: %w", redact(err, c.apiKey))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("judge: typesafe request: %w", redact(err, c.apiKey))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, 0, fmt.Errorf("judge: typesafe response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// refuseRedirect makes a 3xx the answer itself, which Ask then refuses as
// a non-200 status.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// redact strips the key from an error's text (a URL error can echo the
// request), keeping the error otherwise intact.
func redact(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
}

// runeCut is the largest cut at or below n, and above zero, that starts a
// character, looking back at most one character's length; with none there
// (invalid UTF-8, or a character longer than n) it cuts at n, so every piece
// is non-empty and the split always ends.
func runeCut(s string, n int) int {
	for b := n; b > 0 && b > n-utf8.UTFMax; b-- {
		if utf8.RuneStart(s[b]) {
			return b
		}
	}
	return n
}

// Hash is the hash a question set is recorded under: SHA-256 over the
// parts, each followed by a NUL, as "sha256:<hex>". It is the construction
// the publication pipeline's rubric hash uses.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ScoreFromProbability maps a positive-class probability onto the rubric's
// 0–3 scale: 0.5/0.7/0.9 → 1/2/3. A score of 2 is the gate's medium, and
// for a model-input scan the line at which text is withheld (D22).
func ScoreFromProbability(p float64) int {
	switch {
	case p >= 0.9:
		return 3
	case p >= 0.7:
		return 2
	case p >= 0.5:
		return 1
	}
	return 0
}

// Chunk splits s into pieces of at most n bytes, cutting at the last line
// end in a piece when one falls in its second half, and otherwise at the
// last whole character, so no piece ends inside a UTF-8 sequence. Every
// byte of s is in exactly one piece, in order.
func Chunk(s string, n int) []string {
	if n <= 0 || len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > n {
		cut := strings.LastIndexByte(s[:n], '\n')
		if cut <= 0 || cut < n/2 {
			cut = runeCut(s, n)
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}
