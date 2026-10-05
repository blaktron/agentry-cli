// Package hybrid looks up a run's risky inputs on CrowdStrike's Hybrid
// Analysis by SHA-256 alone (plan file-handling.md §7, D6, D17;
// agentry-cli#564). The CLI process asks, never a container (invariant 2),
// and nothing of a file but its hash leaves the machine (invariant 5): the
// one call is GET /api/v2/overview/{sha256}, and no submission route is
// ever called. What the service answers is reduced to a fixed vocabulary
// (D14): malicious refuses the run, suspicious is noted with its threat
// score, anything else and an unseen hash are clean. A lookup that cannot
// answer is recorded and does not stop the run (D13).
package hybrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is Hybrid Analysis's origin.
const DefaultBase = "https://hybrid-analysis.com"

// userAgent is the one the API admits; it refuses another.
const userAgent = "Falcon Sandbox"

// The bounds on one lookup.
const (
	// MaxWait is the most a run waits in all on rate limits before the
	// rest of its lookups are not checked, quota (D13).
	MaxWait = time.Minute
	// requestTimeout bounds one request; maxAnswer one answer's body.
	requestTimeout = 20 * time.Second
	maxAnswer      = 1 << 20
	// defaultRetry is the wait on a 429 that names none, minRetry the
	// least any wait is, so a "Retry-After: 0" still spends the budget.
	defaultRetry = 5 * time.Second
	minRetry     = time.Second
)

// Outcome is what the lookup made of one hash, in the receipt's words.
type Outcome string

// The outcomes (plan §3 invariant 7).
const (
	Clean      Outcome = "clean"
	Noted      Outcome = "noted"
	Refused    Outcome = "refused"
	NotChecked Outcome = "not checked"
)

// Reason is why a hash was not checked.
type Reason string

// The reasons.
const (
	ReasonNoKey       Reason = "no key"
	ReasonQuota       Reason = "quota"
	ReasonUnavailable Reason = "unavailable"
	ReasonKeyRefused  Reason = "the key was refused"
	// ReasonNoDigest: the decode gave a risky file no digest, so it was
	// not asked about.
	ReasonNoDigest Reason = "no digest"
)

// Result is one hash's lookup. ThreatScore is the service's 0–100 score
// for a noted or refused hash, 0 when it gave none.
type Result struct {
	Outcome     Outcome
	Reason      Reason
	ThreatScore int
}

// String is the result in the receipt's words. The verdict is named from
// the outcome, never echoed from the service's answer.
func (r Result) String() string {
	switch r.Outcome {
	case Noted, Refused:
		return string(r.Outcome) + " (" + r.Verdict() + ")"
	case NotChecked:
		return "not checked: " + string(r.Reason)
	}
	return string(r.Outcome)
}

// Verdict names a noted or refused hash's verdict and threat score, as
// "malicious, threat score 100"; empty for any other outcome.
func (r Result) Verdict() string {
	if w := r.VerdictWord(); w != "" {
		return w + r.score()
	}
	return ""
}

// VerdictWord is the verdict alone, "malicious" or "suspicious", from the
// outcome and never from the service's answer; empty for any other.
func (r Result) VerdictWord() string {
	switch r.Outcome {
	case Noted:
		return "suspicious"
	case Refused:
		return "malicious"
	}
	return ""
}

func (r Result) score() string {
	if r.ThreatScore <= 0 {
		return ""
	}
	return ", threat score " + strconv.Itoa(r.ThreatScore)
}

// Client asks Hybrid Analysis with one key. A nil Client, or one with no
// key, asks nothing and answers every hash not checked, no key (D8).
type Client struct {
	Key  string
	Base string
	HTTP *http.Client
	// sleep waits on a rate limit; tests replace it.
	sleep func(context.Context, time.Duration) error
}

// sha256RE is a lower-case hex SHA-256, the only thing ever put in a URL.
var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 says whether s is a lower-case hex SHA-256.
func ValidSHA256(s string) bool { return sha256RE.MatchString(s) }

// errStop ends the lookups: every hash after it shares its result.
type errStop struct{ r Result }

func (e errStop) Error() string { return e.r.String() }

// Look asks for each distinct hash once, in order, and answers every one.
// A rate limit waits, up to MaxWait in all; past that, a refused key, an
// outage or an answer that is not the overview's ends the lookups, and the
// hash and those after it are not checked, with the reason. A string that
// is not a SHA-256 is never asked and is not checked, unavailable.
func (c *Client) Look(ctx context.Context, hashes []string) map[string]Result {
	out := make(map[string]Result, len(hashes))
	if c == nil || c.Key == "" {
		for _, h := range hashes {
			out[h] = Result{Outcome: NotChecked, Reason: ReasonNoKey}
		}
		return out
	}
	var stop *Result
	budget := MaxWait
	for _, h := range hashes {
		if _, done := out[h]; done {
			continue
		}
		if !ValidSHA256(h) {
			out[h] = Result{Outcome: NotChecked, Reason: ReasonUnavailable}
			continue
		}
		if stop != nil {
			out[h] = *stop
			continue
		}
		r, err := c.one(ctx, h, &budget)
		var s errStop
		if errors.As(err, &s) {
			stop = &s.r
			r = s.r
		}
		out[h] = r
	}
	return out
}

// one asks for one hash, waiting on rate limits from budget.
func (c *Client) one(ctx context.Context, h string, budget *time.Duration) (Result, error) {
	for {
		status, retry, body, err := c.get(ctx, h)
		if err != nil {
			return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonUnavailable}}
		}
		switch status {
		case http.StatusOK:
			return verdict(body)
		case http.StatusNotFound:
			return Result{Outcome: Clean}, nil
		case http.StatusTooManyRequests:
			if retry > *budget {
				return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonQuota}}
			}
			*budget -= retry
			if err := c.wait(ctx, retry); err != nil {
				return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonUnavailable}}
			}
		case http.StatusUnauthorized, http.StatusForbidden:
			return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonKeyRefused}}
		default:
			return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonUnavailable}}
		}
	}
}

// get makes the one request: GET, no body, the key in its header.
func (c *Client) get(ctx context.Context, h string) (int, time.Duration, []byte, error) {
	base := c.Base
	if base == "" {
		base = DefaultBase
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v2/overview/"+h, http.NoBody)
	if err != nil {
		return 0, 0, nil, err
	}
	req.Header.Set("api-key", c.Key)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	// A redirect is never followed: the key is in a header Go carries to
	// any host a redirect names, so a 3xx is an answer, unavailable.
	hc := &http.Client{Timeout: requestTimeout}
	if c.HTTP != nil {
		copied := *c.HTTP
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		// The error names the URL, which holds only the hash, never the key.
		return 0, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return 0, 0, nil, err
	}
	if len(body) > maxAnswer {
		return 0, 0, nil, fmt.Errorf("the overview is larger than %d bytes", maxAnswer)
	}
	return resp.StatusCode, retryAfter(resp.Header.Get("Retry-After")), body, nil
}

// retryAfter reads a 429's wait in seconds, else the default.
func retryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return max(time.Duration(n)*time.Second, minRetry)
	}
	return defaultRetry
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// verdict reduces an overview to the outcome D14 gives its verdict. Only
// the verdict and the threat score are read; nothing of the answer's text
// goes further.
func verdict(body []byte) (Result, error) {
	var o struct {
		Verdict     *string `json:"verdict"`
		ThreatScore *int    `json:"threat_score"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return Result{}, errStop{Result{Outcome: NotChecked, Reason: ReasonUnavailable}}
	}
	score := 0
	if o.ThreatScore != nil && *o.ThreatScore > 0 && *o.ThreatScore <= 100 {
		score = *o.ThreatScore
	}
	v := ""
	if o.Verdict != nil {
		v = strings.ToLower(strings.TrimSpace(*o.Verdict))
	}
	switch v {
	case "malicious":
		return Result{Outcome: Refused, ThreatScore: score}, nil
	case "suspicious":
		return Result{Outcome: Noted, ThreatScore: score}, nil
	}
	return Result{Outcome: Clean}, nil
}
