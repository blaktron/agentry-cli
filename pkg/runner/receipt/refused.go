package receipt

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// RefusedInput is one input a run was refused for, in typed fields: the
// file's path as the decode reported it, which check refused it, and what
// that check found, in its own fixed words. A driver that keeps no text the
// run could have written (a hosted run's host) can keep these and say which
// file it was, which Reason, holding no path, cannot.
type RefusedInput struct {
	// Path is the decode's path for the file: an input's name as it was
	// given, or <input>/<member> for a member of an archive.
	Path string `json:"path"`
	// By is the check that refused it (RefusedBy*).
	By string `json:"by"`
	// Signature is ClamAV's name for what it found (By clamav).
	Signature string `json:"signature,omitempty"`
	// Verdict and ThreatScore are Hybrid Analysis's outcome (By
	// hybrid-analysis): "malicious", and its score when it gave one.
	Verdict     string `json:"verdict,omitempty"`
	ThreatScore int    `json:"threatScore,omitempty"`
	// Flagged are the questions AI Review's read of the file's text failed
	// (By ai-review). None when AI Review could not read it.
	Flagged []string `json:"flagged,omitempty"`
}

// The checks that refuse an input.
const (
	RefusedByClamAV         = "clamav"
	RefusedByHybridAnalysis = "hybrid-analysis"
	RefusedByAIReview       = "ai-review"
)

// MaxRefusedInputs is how many refused inputs an envelope names; it counts
// the rest (RefusedInputsMore).
const MaxRefusedInputs = 5

// maxRefusedPath bounds a path, as the decode's own name rules do.
const maxRefusedPath = 1024

var (
	// reviewQuestionRE is an AI Review question's word.
	reviewQuestionRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// clean is in held to its check's fields and grammars, or false.
func (in RefusedInput) clean() (RefusedInput, bool) {
	if !validRefusedPath(in.Path) {
		return RefusedInput{}, false
	}
	out := RefusedInput{Path: in.Path, By: in.By}
	switch in.By {
	case RefusedByClamAV:
		if runnerdecode.ValidSignature(in.Signature) {
			out.Signature = in.Signature
		}
	case RefusedByHybridAnalysis:
		if in.Verdict != "malicious" {
			return RefusedInput{}, false
		}
		out.Verdict = in.Verdict
		if in.ThreatScore > 0 && in.ThreatScore <= 100 {
			out.ThreatScore = in.ThreatScore
		}
	case RefusedByAIReview:
		for _, q := range in.Flagged {
			if reviewQuestionRE.MatchString(q) && !slices.Contains(out.Flagged, q) {
				out.Flagged = append(out.Flagged, q)
			}
		}
	default:
		return RefusedInput{}, false
	}
	return out, true
}

// validRefusedPath holds a path to the decode's shape: relative, slash
// separated, no empty, "." or ".." segment, printable UTF-8, bounded.
func validRefusedPath(p string) bool {
	if p == "" || len(p) > maxRefusedPath || !utf8.ValidString(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// refusedInputsError carries the inputs a refusal names beside the error,
// as reasonError carries its Reason.
type refusedInputsError struct {
	Inputs []RefusedInput
	Err    error
}

func (e *refusedInputsError) Error() string { return e.Err.Error() }
func (e *refusedInputsError) Unwrap() error { return e.Err }

// WithRefusedInputs attaches the inputs a refusal is for to err; nil stays
// nil, and so does an empty list.
func WithRefusedInputs(err error, inputs []RefusedInput) error {
	if err == nil || len(inputs) == 0 {
		return err
	}
	return &refusedInputsError{Inputs: inputs, Err: err}
}

// RefusedInputsOf is the inputs err's refusal names, each held to its
// grammar, at most MaxRefusedInputs of them, and how many more there were.
// Only a refusal whose reason is input-refused names any.
func RefusedInputsOf(err error) (inputs []RefusedInput, more int) {
	if r := ReasonOf(err); r == nil || r.Code != ReasonInputRefused {
		return nil, 0
	}
	var re *refusedInputsError
	if !errors.As(err, &re) {
		return nil, 0
	}
	return CleanRefusedInputs(re.Inputs)
}

// CleanRefusedInputs holds a list to the grammars, keeps the first
// MaxRefusedInputs entries that pass them and counts the rest, an entry
// that fails them included, so a refusal never reads as naming every file
// it was for. A repeated entry is named once and not counted.
func CleanRefusedInputs(in []RefusedInput) (out []RefusedInput, more int) {
	seen := map[[2]string]bool{}
	for _, i := range in {
		c, ok := i.clean()
		if !ok {
			more++
			continue
		}
		if seen[[2]string{c.Path, c.By}] {
			continue
		}
		seen[[2]string{c.Path, c.By}] = true
		if len(out) == MaxRefusedInputs {
			more++
			continue
		}
		out = append(out, c)
	}
	return out, more
}
