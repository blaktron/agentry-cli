// Package remote is the client of a registry's Writing Voice routes (the
// core's docs/voice.md): read the account's voice, put a new version for
// review, and set the account's always-attach flag, for the account a session
// token or a personal access token with voice:read and voice:write speaks
// for. The CLI and the desktop both use it.
//
// The text travels in the clear over TLS and the registry seals it at rest:
// unlike a knowledge pack, a voice is read by the registry to be reviewed
// before it is live, so a client seal would protect nothing the registry
// does not open.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/apiclient"

	"github.com/blaktron/agentry-cli/internal/origin"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/voice"
)

var (
	// ErrSignedOut is a registry with no credential, or one that refused
	// it: the knowledge store's own, so a caller tests one value for both.
	ErrSignedOut = store.ErrSignedOut
	// ErrNoAccountVoice is an account that keeps no Writing Voice (this
	// computer keeping none is voice.ErrNoVoice).
	ErrNoAccountVoice = errors.New("your account keeps no Writing Voice")
	// ErrUnconfigured is a registry that keeps no voices: no key to seal
	// them with, or no AI Review judge to review them.
	ErrUnconfigured = errors.New("this registry keeps no Writing Voices")
)

// Review is where a version is in its review.
type Review string

// The review states (the core's VoiceVersion.review).
const (
	Reviewing Review = "reviewing"
	Flagged   Review = "flagged"
	Passed    Review = "passed"
)

// Phrase is the review state in a sentence.
func (r Review) Phrase() string {
	switch r {
	case Reviewing:
		return "in review"
	case Flagged:
		return "flagged by AI Review"
	case Passed:
		return "passed by AI Review"
	}
	return string(r)
}

// VerdictIncomplete is a review report whose stage or judge did not
// finish: the version stays in review and the registry asks again.
const VerdictIncomplete = "incomplete"

// MaxScore is the top of the scale AI Review scores a question on (0–3).
const MaxScore = 3

// Finding is one thing AI Review found in a version: a rule stage's
// finding at a line, or a question of the voice set and its score. Flags
// says whether it is what keeps the version from going live.
type Finding struct {
	Scanner  string `json:"scanner"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Line     int    `json:"line,omitempty"`
	Question string `json:"question,omitempty"`
	Score    int    `json:"score,omitempty"`
	Flags    bool   `json:"flags"`
}

// Version is one version of the account's voice. Source is the
// registry's word (build, ai-edit, edit, upload, restore), not this
// computer's voice.Source.
type Version struct {
	ID          string     `json:"id"`
	Review      Review     `json:"review"`
	Source      string     `json:"source"`
	Digest      string     `json:"digest"`
	Bytes       int        `json:"bytes"`
	Text        string     `json:"-"`
	Verdict     string     `json:"verdict,omitempty"`
	Findings    []Finding  `json:"findings,omitempty"`
	Failure     string     `json:"failure,omitempty"`
	QuestionSet string     `json:"questionSet,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReviewedAt  *time.Time `json:"reviewedAt,omitempty"`
}

// Account is the account's voice: the always-attach flag, and the live,
// pending and previous versions, any of which may be missing.
type Account struct {
	AlwaysAttach bool     `json:"alwaysAttach"`
	Live         *Version `json:"live,omitempty"`
	Pending      *Version `json:"pending,omitempty"`
	Previous     *Version `json:"previous,omitempty"`
	QuestionSet  string   `json:"questionSet"`
}

// Client is one registry, as one account.
type Client struct {
	url string
	api *apiclient.ClientWithResponses
}

// New is the client of the registry at regURL (no trailing slash) with
// token as its bearer credential. hc carries every request (nil:
// http.DefaultClient, which has no timeout; callers pass their own).
func New(regURL, token string, hc *http.Client) (*Client, error) {
	regURL = strings.TrimRight(regURL, "/")
	if token == "" {
		return nil, fmt.Errorf("%s: %w", regURL, ErrSignedOut)
	}
	opts := []apiclient.ClientOption{apiclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})}
	if hc != nil {
		opts = append(opts, apiclient.WithHTTPClient(origin.Guard(hc)))
	}
	api, err := apiclient.NewClientWithResponses(regURL+"/api/v1", opts...)
	if err != nil {
		return nil, err
	}
	return &Client{url: regURL, api: api}, nil
}

// URL is the registry's base URL.
func (c *Client) URL() string { return c.url }

// Get is the account's voice: GET /voice. ErrNoAccountVoice when it keeps none.
func (c *Client) Get(ctx context.Context) (Account, error) {
	res, err := c.api.GetVoiceWithResponse(ctx)
	if err != nil {
		return Account{}, err
	}
	if res.JSON200 == nil {
		return Account{}, c.problem("read your Writing Voice", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return accountOf(*res.JSON200)
}

// Upload puts text as the account's pending version, source upload: PUT
// /voice. It goes live, replacing the live one, only once AI Review passes
// it; the answer is the account with the version in review.
func (c *Client) Upload(ctx context.Context, text []byte) (Account, error) {
	if err := voice.Check(text); err != nil {
		return Account{}, err
	}
	res, err := c.api.PutVoiceWithResponse(ctx, apiclient.PutVoiceRequest{Text: string(text), Source: apiclient.PutVoiceRequestSourceUpload})
	if err != nil {
		return Account{}, err
	}
	if res.JSON202 == nil {
		return Account{}, c.problem("submit your Writing Voice", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return accountOf(*res.JSON202)
}

// SetAlwaysAttach sets whether runs In Agentry attach the account's live
// voice by default: PUT /voice/settings.
func (c *Client) SetAlwaysAttach(ctx context.Context, on bool) (Account, error) {
	res, err := c.api.UpdateVoiceSettingsWithResponse(ctx, apiclient.VoiceSettingsRequest{AlwaysAttach: &on})
	if err != nil {
		return Account{}, err
	}
	if res.JSON200 == nil {
		return Account{}, c.problem("set your account's voice to attach", res.HTTPResponse, res.ApplicationproblemJSONDefault, res.Body)
	}
	return accountOf(*res.JSON200)
}

func accountOf(v apiclient.Voice) (Account, error) {
	out := Account{AlwaysAttach: v.AlwaysAttach, QuestionSet: v.QuestionSet}
	for _, s := range []struct {
		from *apiclient.VoiceVersion
		to   **Version
	}{{v.Live, &out.Live}, {v.Pending, &out.Pending}, {v.Previous, &out.Previous}} {
		if s.from == nil {
			continue
		}
		ver, err := versionOf(*s.from)
		if err != nil {
			return Account{}, err
		}
		*s.to = &ver
	}
	return out, nil
}

// versionOf holds a version to the format: the registry's text is checked
// as this computer's would be, and its digest must be the text's.
func versionOf(v apiclient.VoiceVersion) (Version, error) {
	text := []byte(v.Text)
	if err := voice.Check(text); err != nil {
		return Version{}, fmt.Errorf("the registry sent a voice that is not one: %w", err)
	}
	if voice.Digest(text) != v.Digest {
		return Version{}, errors.New("the registry sent a voice that is not the digest it names")
	}
	// A voice is Markdown: `voice show` prints it to a terminal, so one
	// carrying other control characters (an escape sequence) is not kept.
	if i := strings.IndexFunc(v.Text, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t' && r != '\r') || r == 0x7f || (r >= 0x80 && r < 0xa0)
	}); i >= 0 {
		return Version{}, errors.New("the registry sent a voice that holds a control character")
	}
	out := Version{
		ID: v.Id.String(), Review: Review(safe(string(v.Review), 20)), Source: safe(string(v.Source), 20), Digest: v.Digest, Bytes: v.Bytes,
		Text: v.Text, QuestionSet: safe(v.QuestionSet, 80), CreatedAt: v.CreatedAt, ReviewedAt: v.ReviewedAt,
	}
	if r := v.Report; r != nil {
		out.Verdict = string(r.Verdict)
		if r.AiReview.Failure != nil {
			out.Failure = safe(*r.AiReview.Failure, 80)
		}
		for _, f := range r.Findings {
			fd := Finding{Scanner: safe(f.Scanner, 80), Severity: safe(f.Severity, 20), Title: safe(f.Title, 200), Flags: f.Flags}
			if f.Line != nil {
				fd.Line = *f.Line
			}
			if f.Question != nil {
				fd.Question = safe(*f.Question, 40)
			}
			if f.Score != nil {
				fd.Score = *f.Score
			}
			out.Findings = append(out.Findings, fd)
		}
	}
	return out, nil
}

// problem turns a refusal into an error in the registry's words, with the
// package's sentinels where the caller acts on them.
func (c *Client) problem(op string, res *http.Response, p *apiclient.Problem, body []byte) error {
	status := 0
	if res != nil {
		status = res.StatusCode
	}
	if p == nil && len(body) > 0 {
		var parsed apiclient.Problem
		if json.Unmarshal(body, &parsed) == nil && parsed.Title != "" {
			p = &parsed
		}
	}
	slug, sentence := "", fmt.Sprintf("HTTP %d", status)
	if p != nil {
		if i := strings.LastIndex(p.Type, "/"); i >= 0 {
			slug = p.Type[i+1:]
		}
		sentence = safe(p.Title, 120)
		if p.Detail != nil && *p.Detail != "" {
			sentence += ": " + safe(*p.Detail, 400)
		}
	}
	var sentinel error
	switch {
	case status == http.StatusUnauthorized:
		sentinel = ErrSignedOut
		sentence += "; sign in again with `agentry login`"
	case slug == "voice-not-found":
		sentinel = ErrNoAccountVoice
	case slug == "voice-unconfigured":
		sentinel = ErrUnconfigured
	case status == http.StatusForbidden:
		sentence += "; sign in again with `agentry login` for a token that reaches your Writing Voice (voice:read, voice:write)"
	}
	if sentinel != nil {
		return fmt.Errorf("%s at %s: %s: %w", op, c.url, sentence, sentinel)
	}
	return fmt.Errorf("%s at %s: %s", op, c.url, sentence)
}

// safe is s made printable on one line and cut at n characters: a
// registry's words reach a terminal.
func safe(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}
