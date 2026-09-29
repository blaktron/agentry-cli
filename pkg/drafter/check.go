package drafter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/draftcheck"
	corejudge "github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// AI Review's check of a finished draft (plan builder-simplify.md §5.2 step
// 4, D6; agentry-cli#418): the rendered package read with the release
// rubric as stage S5 will read it at publish, and with the draft question
// set (off_task, DEV-172) against the person's description. A hosted draft
// asks the registry's POST /api/v1/model-input/check with its job
// credential, which the runs extension admits at most twice a draft
// (agentry-enterprise#265); a draft on this computer asks the same thing
// through the core's pkg/draftcheck with the person's own Jev key. The
// verdicts are recorded in draft.json under aiReview.check.

// The check's verdicts, as the registry's route answers them.
const (
	CheckPass       = "pass"
	CheckFlagged    = "flagged"
	CheckNotScanned = "not_scanned"
)

// RubricIDUndeclared is the release rubric's C3: a capability the
// instructions use that the manifest does not declare. It is recorded and
// never sent to the repair turn. Until blaktron/agentry#731 the rubric read
// an instruction that reads its own inputs and writes its report that way,
// which editing the draft could not clear.
const RubricIDUndeclared = corejudge.RubricIDUndeclaredCapability

// Check is draft.json's record of the check, under aiReview.check: the
// verdicts of the last check the post-step asked, and how many it asked.
// A check with no answer (not asked, refused, or not answered) says why in
// Unanswered, in the CLI's own words, and has no verdicts.
type Check struct {
	Rubric *CheckRubric `json:"rubric,omitempty"`
	Draft  *CheckDraft  `json:"draft,omitempty"`
	// Model is the judge's pinned model.
	Model string `json:"model,omitempty"`
	// Requests and InputTokens are the judge's, over every check asked.
	Requests    int `json:"requests"`
	InputTokens int `json:"inputTokens"`
	// Checks is how many checks the post-step asked: one, or two when a
	// flag sent the draft to its repair turn and the second answer was
	// checked again.
	Checks     int    `json:"checks"`
	Unanswered string `json:"unanswered,omitempty"`
}

// CheckRubric is the release rubric's half: pass, flagged or not scanned,
// the six scores, and each question that scored 2 or more with the file
// whose chunk scored highest.
type CheckRubric struct {
	Verdict    string         `json:"verdict"`
	Scores     map[string]int `json:"scores,omitempty"`
	Findings   []CheckFinding `json:"findings"`
	RubricHash string         `json:"rubricHash"`
	Failure    string         `json:"failure,omitempty"`
}

// CheckFinding is one rubric question at 2 or more.
type CheckFinding struct {
	ID    string `json:"id"`
	Score int    `json:"score"`
	Path  string `json:"path"`
}

// CheckDraft is the draft question set's half.
type CheckDraft struct {
	Verdict     string         `json:"verdict"`
	Score       int            `json:"score"`
	Scores      map[string]int `json:"scores,omitempty"`
	Flagged     []string       `json:"flagged"`
	QuestionSet string         `json:"questionSet"`
	Failure     string         `json:"failure,omitempty"`
}

// CheckUnanswered is Word's for a check with no answer.
const CheckUnanswered = "unanswered"

// Word is the check as one word, for the envelope and the terminal: its
// Verdict, CheckUnanswered for a check with no answer, "" for none.
func (c *Check) Word() string {
	if c != nil && c.Unanswered != "" {
		return CheckUnanswered
	}
	return c.Verdict()
}

// Verdict is the check's as one word: flagged if either half flagged, else
// not scanned if either was, else pass; "" for a check with no verdicts.
func (c *Check) Verdict() string {
	switch {
	case c == nil || c.Rubric == nil || c.Draft == nil:
		return ""
	case c.Rubric.Verdict == CheckFlagged || c.Draft.Verdict == CheckFlagged:
		return CheckFlagged
	case c.Rubric.Verdict == CheckNotScanned || c.Draft.Verdict == CheckNotScanned:
		return CheckNotScanned
	}
	return CheckPass
}

// Repairable is what of a check the repair turn can act on, one line each
// in the CLI's own words (nothing of the judge's but ids, scores and the
// package's own paths): the draft question's flag, and each rubric finding
// but C3, which the draft cannot clear. Empty when there is nothing to
// repair.
func (c *Check) Repairable() []string {
	if c == nil || c.Rubric == nil || c.Draft == nil {
		return nil
	}
	var out []string
	if slices.Contains(c.Draft.Flagged, corejudge.DraftIDOffTask) {
		out = append(out, fmt.Sprintf("AI Review off_task, score %d: an agent that follows the instructions would not do the task the description asks for "+
			"(it does a different task, leaves out a part the description asks for, or returns a different kind of result).", c.Draft.Scores[corejudge.DraftIDOffTask]))
	}
	for _, f := range c.Rubric.Findings {
		if f.ID == RubricIDUndeclared {
			continue
		}
		out = append(out, fmt.Sprintf("AI Review release rubric %s, score %d, in %s: %s", f.ID, f.Score, f.Path, rubricMeaning[f.ID]))
	}
	return out
}

// rubricMeaning is what each release rubric question asks, in the CLI's
// words, for the repair turn and the notes.
var rubricMeaning = map[string]string{
	corejudge.RubricIDInjection:    "the text reads as instructions that would override an agent's task or rules.",
	corejudge.RubricIDExfiltration: "the text reads as sending data somewhere the task does not need.",
	RubricIDUndeclared:             "the text reads as needing a capability the manifest does not declare.",
	corejudge.RubricIDDeception:    "the text reads as misleading the person about what the agent does.",
	corejudge.RubricIDObfuscation:  "the text reads as hiding what it says (encoded or disguised content).",
	corejudge.RubricIDDestructive:  "the text reads as taking destructive actions the task does not need.",
}

// A Checker asks the check of a finished draft. Its error means the check
// was not asked, and says why in the CLI's own words; a judge that could
// not answer a half is a verdict of not scanned on that half.
type Checker interface {
	Check(ctx context.Context, p draftcheck.Package) (*Check, error)
}

// The check's refusals of a draft it will not ask about, before anything is
// sent, in the CLI's own words: none quotes a path or the core's error.
var (
	ErrCheckDescriptionTooLong = fmt.Errorf("the description is longer than the check reads (%d bytes)", corejudge.DraftMaxDescription)
	ErrCheckPackage            = errors.New("the package is not a draft the check reads")
	ErrCheckTooLarge           = fmt.Errorf("the package is past the check's bounds (%d files, %d bytes of text)", draftcheck.MaxFiles, draftcheck.MaxBytes)
	errCheckUnreadable         = errors.New("the package could not be read")
)

// checkRefusal is the CLI's word for why draftcheck will not read a package.
func checkRefusal(err error) error {
	switch {
	case errors.Is(err, draftcheck.ErrTooLarge):
		return ErrCheckTooLarge
	case errors.Is(err, draftcheck.ErrNotADraft):
		return ErrCheckPackage
	}
	return fmt.Errorf("%w: a path or a file it will not read", ErrCheckPackage)
}

// CheckPackage is the rendered package as the check reads it: the person's
// description, and exactly the text files publish.include takes from dir,
// the licence among them, as stage S5 will read them at publish.
func CheckPackage(description, dir string) (draftcheck.Package, error) {
	if len(description) > corejudge.DraftMaxDescription {
		return draftcheck.Package{}, ErrCheckDescriptionTooLong
	}
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml")) //nolint:gosec // the draft's own package folder
	if err != nil {
		return draftcheck.Package{}, fmt.Errorf("%w: it has no agent.yaml", ErrCheckPackage)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return draftcheck.Package{}, fmt.Errorf("%w: its agent.yaml does not parse", ErrCheckPackage)
	}
	files, err := archive.WalkWorkspace(dir)
	if err != nil {
		return draftcheck.Package{}, errCheckUnreadable
	}
	p := draftcheck.Package{Description: description}
	for _, fi := range manifest.ResolveInclude(m.Publish.Include, files) {
		if fi.Symlink {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(fi.Path))) //nolint:gosec // paths come from WalkWorkspace
		if err != nil {
			return draftcheck.Package{}, errCheckUnreadable
		}
		if !utf8.Valid(b) {
			continue
		}
		p.Files = append(p.Files, draftcheck.File{Path: fi.Path, Content: string(b)})
	}
	if _, err := draftcheck.Read(p); err != nil {
		return draftcheck.Package{}, checkRefusal(err)
	}
	return p, nil
}

// localCheckTimeout bounds a check with the person's own key: every chunk
// of a package of at most 64 KiB, up to three samples each.
const localCheckTimeout = 2 * time.Minute

// CoreCheck asks the check with the person's own Jev key through the core's
// own pkg/draftcheck, exactly as the registry asks it (D25).
func CoreCheck(key string) (Checker, error) {
	c, err := corejudge.New(corejudge.Config{APIKey: key})
	if err != nil {
		return nil, err
	}
	return LocalChecker(c), nil
}

// LocalChecker asks the check through a judge client of the caller's.
func LocalChecker(a corejudge.Asker) Checker { return localChecker{draftcheck.New(a)} }

type localChecker struct{ c *draftcheck.Checker }

func (l localChecker) Check(ctx context.Context, p draftcheck.Package) (*Check, error) {
	ctx, cancel := context.WithTimeout(ctx, localCheckTimeout)
	defer cancel()
	res, err := l.c.Check(ctx, p)
	if err != nil {
		// Only draftcheck.Read's, which CheckPackage has already passed.
		return nil, checkRefusal(err)
	}
	out := &Check{Model: res.Model, Requests: res.Usage.Requests, InputTokens: res.Usage.InputTokens,
		Rubric: &CheckRubric{Verdict: string(res.Rubric.Verdict), RubricHash: res.Rubric.Hash, Failure: res.Rubric.Failure, Findings: []CheckFinding{}},
		Draft: &CheckDraft{Verdict: string(res.Draft.Verdict), Score: res.Draft.Score, QuestionSet: res.Draft.QuestionSet,
			Failure: res.Draft.Failure, Flagged: append([]string{}, res.Draft.Flagged...)}}
	if res.Rubric.Verdict != corejudge.VerdictNotScanned {
		out.Rubric.Scores = res.Rubric.Scores
	}
	for _, f := range res.Rubric.Findings {
		out.Rubric.Findings = append(out.Rubric.Findings, CheckFinding{ID: f.ID, Score: f.Score, Path: f.Path})
	}
	if res.Draft.Verdict != corejudge.VerdictNotScanned {
		out.Draft.Scores = res.Draft.Scores
	}
	return out, nil
}

// RegistryChecker asks the registry's check with a hosted draft's job
// credential. It asks once and never again on a failure: the registry counts
// a check against the draft's cap of two as it admits it, before its checker
// answers, so a retry after a 503 would spend the check kept for after the
// repair turn (agentry-enterprise#265).
type RegistryChecker struct {
	// Base is the registry's root URL; JobToken the job's credential.
	Base     string
	JobToken string
	// HTTP is the client; nil is one that never follows a redirect.
	HTTP *http.Client
}

// registryCheckTimeout is past the route's own deadline (45 s).
const registryCheckTimeout = 60 * time.Second

// maxCheckAnswer is the most of the registry's answer read.
const maxCheckAnswer = 64 << 10

func (r *RegistryChecker) Check(ctx context.Context, p draftcheck.Package) (*Check, error) {
	type file struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	body := struct {
		Description string `json:"description"`
		Files       []file `json:"files"`
	}{Description: p.Description, Files: make([]file, len(p.Files))}
	for i, f := range p.Files {
		body.Files[i] = file{f.Path, f.Content}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.Base, "/")+"/api/v1/model-input/check", bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("the registry's address is not one the check can ask")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webscan.JobTokenHeader, r.JobToken)
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: registryCheckTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Never the transport's words: they can carry the address.
		return nil, errors.New("the registry did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCheckAnswer+1))
	if err != nil || len(raw) > maxCheckAnswer {
		return nil, errors.New("the registry's answer could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		var prob struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &prob)
		if slug := problemSlug(prob.Type); slug != "" {
			return nil, fmt.Errorf("the registry refused it (%d %s)", resp.StatusCode, slug)
		}
		return nil, fmt.Errorf("the registry refused it (%d)", resp.StatusCode)
	}
	var ans struct {
		Rubric CheckRubric `json:"rubric"`
		Draft  CheckDraft  `json:"draft"`
		Model  string      `json:"model"`
		Usage  struct {
			Requests    int `json:"requests"`
			InputTokens int `json:"inputTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, errNotACheck
	}
	c := &Check{Rubric: &ans.Rubric, Draft: &ans.Draft, Model: ans.Model, Requests: max(ans.Usage.Requests, 0), InputTokens: max(ans.Usage.InputTokens, 0)}
	if !c.validAnswer(p) {
		return nil, errNotACheck
	}
	return c, nil
}

// errNotACheck is a registry answer that is not of the route's shape.
var errNotACheck = errors.New("the registry's answer is not a check")

// validAnswer admits only an answer of the route's own shape: known verdicts,
// failures and question ids, scores of 0 to 3, finding paths of the package
// sent, short hashes and model. draft.json is read by clients as data, and
// the registry's answer is written into it.
func (c *Check) validAnswer(p draftcheck.Package) bool {
	verdict := func(v string) bool { return v == CheckPass || v == CheckFlagged || v == CheckNotScanned }
	failure := func(f string) bool {
		return f == "" || f == corejudge.FailureTimeout || f == corejudge.FailureJudgeError || f == corejudge.FailureTooLarge
	}
	score := func(n int) bool { return n >= 0 && n <= 3 }
	short := func(s string) bool {
		return len(s) <= 200 && utf8.ValidString(s) && !strings.ContainsFunc(s, isControl)
	}
	r, d := c.Rubric, c.Draft
	if !verdict(r.Verdict) || !verdict(d.Verdict) || !failure(r.Failure) || !failure(d.Failure) || !short(r.RubricHash) ||
		!short(d.QuestionSet) || !short(c.Model) || !score(d.Score) {
		return false
	}
	for id, n := range r.Scores {
		if _, ok := rubricMeaning[id]; !ok || !score(n) {
			return false
		}
	}
	paths := make(map[string]bool, len(p.Files))
	for _, f := range p.Files {
		paths[f.Path] = true
	}
	if r.Findings == nil {
		r.Findings = []CheckFinding{}
	}
	if len(r.Findings) > len(rubricMeaning) {
		return false
	}
	for _, f := range r.Findings {
		if _, ok := rubricMeaning[f.ID]; !ok || !score(f.Score) || (f.Path != "" && !paths[f.Path]) {
			return false
		}
	}
	for id, n := range d.Scores {
		if id != corejudge.DraftIDOffTask || !score(n) {
			return false
		}
	}
	if d.Flagged == nil {
		d.Flagged = []string{}
	}
	for _, id := range d.Flagged {
		if id != corejudge.DraftIDOffTask {
			return false
		}
	}
	return true
}

// problemSlug is a problem type's last segment when it is a plain slug.
func problemSlug(t string) string {
	s := t[strings.LastIndex(t, "/")+1:]
	if s == "" || len(s) > 64 || strings.ContainsFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && r != '-' }) {
		return ""
	}
	return s
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// RecordCheck writes the check into the draft's AI Review record, with how
// many checks were asked (c becomes the record's), and says in the notes what a person should know
// of it: nothing for a pass; for a flag, what it flagged and what it means
// at publish; for a half not scanned or a check not asked, that. The
// notes' words are the CLI's own.
func (d *Draft) RecordCheck(c *Check, checks int, unanswered error) {
	if d.AIReview == nil {
		d.AIReview = &AIReview{WithheldSources: []WithheldSource{}}
	}
	if c == nil {
		c = &Check{}
	}
	c.Checks = checks
	if unanswered != nil {
		c.Rubric, c.Draft, c.Unanswered = nil, nil, unanswered.Error()
	}
	d.AIReview.Check = c
	if s := checkNote(c); s != "" {
		d.composeNotes(s)
	}
}

func checkNote(c *Check) string {
	var lines []string
	switch {
	case c.Unanswered != "":
		lines = append(lines, "AI Review's check of the finished draft has no answer: "+c.Unanswered+". The pipeline reads the whole package at publish.")
	case c.Verdict() == CheckPass:
		return ""
	default:
		if slices.Contains(c.Draft.Flagged, corejudge.DraftIDOffTask) {
			lines = append(lines, "AI Review reads the instructions as not doing the task you described (off_task). Compare them with your description before you submit.")
		}
		for _, f := range c.Rubric.Findings {
			if f.ID == RubricIDUndeclared {
				lines = append(lines, fmt.Sprintf("The release rubric, asked as it will be at publish, reads %s as needing a capability the manifest does not declare (C3). "+
					"Check that the manifest declares every host, program, file access and secret the instructions use; at publish it sends the version to review, not to refusal.", f.Path))
				continue
			}
			lines = append(lines, fmt.Sprintf("The release rubric, asked as it will be at publish, flagged %s: %s At publish the pipeline records it as a finding and sends the version to review.",
				f.Path, rubricMeaning[f.ID]))
		}
		if c.Rubric.Verdict == CheckNotScanned {
			lines = append(lines, "The release rubric could not be asked ("+c.Rubric.Failure+"); the pipeline reads the whole package at publish.")
		}
		if c.Draft.Verdict == CheckNotScanned {
			lines = append(lines, "Whether the instructions do the task could not be asked ("+c.Draft.Failure+").")
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## AI Review's check\n\n- " + strings.Join(lines, "\n- ") + "\n"
}
