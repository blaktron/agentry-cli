package judge

import (
	"context"
	"fmt"
)

// The draft question set (plan builder-simplify.md D6, agentry#708): what
// AI Review asks about a finished draft beside the release rubric — does
// its instruction do the task the person described. The text is a
// constant, like the other two sets' texts: the person's description, the
// manifest and the instruction travel in the request's state, and nothing
// of them reaches a question (DEV-113's rule). It is phrased so that yes
// is the problem, as the rubric's are, so a score of WithholdScore or more
// is a flag.
//
// The set was benched on the Drafter's fixture drafts before anything used
// it (pkg/draftcheck TestDraftBench; agentry-notes
// measurements/draft-check-bench.md): this was the one question of three
// that flagged no good draft and every draft made for another task. It
// misses most drafts that leave out one part of the task.
const DraftOffTask = "The state holds a person's description of a task (description), the manifest of an AI agent " +
	"drafted for it (manifest) and the instruction that agent follows (instruction). Would an agent that " +
	"follows this instruction fail to do the task the description asks for: because it does a different " +
	"task, leaves out a part the description asks for, or returns a different kind of result? Choices the " +
	"instruction makes on its own that still do the described task do not count."

// DraftIDOffTask is the draft question's id, the key of a draft scan's
// scores.
const DraftIDOffTask = "off_task"

// DraftQuestions is the draft set. It returns a fresh slice, so no caller
// can change what another asks.
func DraftQuestions() []Question {
	return []Question{{ID: DraftIDOffTask, Text: DraftOffTask}}
}

// VerdictFlagged is a draft scan some question scored at WithholdScore or
// above: the draft has a problem to fix before it is submitted.
const VerdictFlagged Verdict = "flagged"

// FailureTooLarge is a draft scan not asked because a part of the draft
// is past its bound: it is not scanned, never cut.
const FailureTooLarge = "too-large"

// The bounds of a draft scan's state. A draft asked about is one request,
// under Jev's input window: a description the site admits, a manifest of
// a few kilobytes, and an instruction of about a screen.
const (
	DraftMaxDescription = 8 << 10
	DraftMaxManifest    = 16 << 10
	DraftMaxInstruction = 24 << 10
)

// DraftInput is what a draft scan reads: the person's description, the
// draft's canonical manifest JSON, and the text of its instruction file.
type DraftInput struct {
	Description  string
	ManifestJSON []byte
	Instruction  string
}

// DraftResult is a draft scan's answer.
type DraftResult struct {
	// Verdict is pass, flagged or not scanned.
	Verdict Verdict
	// Score is the highest score; Scores per question id, 0 to 3, and
	// Probabilities the highest probability drawn. Both are empty when not
	// scanned.
	Score         int
	Scores        map[string]int
	Probabilities map[string]float64
	// Flagged lists the question ids that scored WithholdScore or more,
	// sorted.
	Flagged []string
	// Failure is a Failure word when the draft was not scanned; Err what
	// went wrong, for the caller's own log. Err never travels.
	Failure string
	Err     error
	// Model is the pinned model id; QuestionSet the hash of everything
	// that changes a verdict.
	Model       string
	QuestionSet string
	Usage       Usage
}

// DraftScanner asks the draft set about one draft. It is safe for
// concurrent use.
type DraftScanner struct {
	asker Asker
}

// NewDraftScanner builds a scanner over an Asker, a *Client or a fake.
func NewDraftScanner(a Asker) *DraftScanner {
	return &DraftScanner{asker: a}
}

// Model is the pinned model id the scanner's answers come from.
func (s *DraftScanner) Model() string { return s.asker.Model() }

// QuestionSet is the hash of the draft set and everything else that
// changes a verdict: the model, the question ids and texts, the state's
// shape, its bounds, the thresholds, the flag line and the band.
func (s *DraftScanner) QuestionSet() string { return draftHash(s.asker.Model()) }

func draftHash(model string) string {
	return draftHashOf(model, DraftQuestions())
}

// draftHashOf is the hash over a given set, so a test can show that a
// question's wording is in it.
func draftHashOf(model string, qs []Question) string {
	parts := []string{"agentry/draft-check/v1", "provider=typesafe", "model=" + model, "state=description,manifest,instruction"}
	for _, q := range qs {
		parts = append(parts, "question="+q.ID+"="+q.Text)
	}
	parts = append(parts, fmt.Sprintf("bounds=%d/%d/%d", DraftMaxDescription, DraftMaxManifest, DraftMaxInstruction),
		"thresholds=0.5/0.7/0.9", fmt.Sprintf("flag=%d", WithholdScore),
		fmt.Sprintf("band=%.2f/%.2f", BandLow, BandHigh), fmt.Sprintf("samples=%d", Samples))
	return Hash(parts...)
}

// Scan asks the draft set about a draft in one request, then asks the
// borderline questions twice more and keeps each one's highest answer. A
// part past its bound is not scanned, and neither is a draft the judge
// could not answer for; the scan never returns an error, since a failure
// is a verdict.
func (s *DraftScanner) Scan(ctx context.Context, in DraftInput) DraftResult {
	res := DraftResult{Model: s.asker.Model(), QuestionSet: s.QuestionSet()}
	if len(in.Description) > DraftMaxDescription || len(in.ManifestJSON) > DraftMaxManifest || len(in.Instruction) > DraftMaxInstruction {
		res.Verdict, res.Failure = VerdictNotScanned, FailureTooLarge
		res.Err = fmt.Errorf("judge: a draft of %d/%d/%d bytes is past the bounds %d/%d/%d", len(in.Description), len(in.ManifestJSON),
			len(in.Instruction), DraftMaxDescription, DraftMaxManifest, DraftMaxInstruction)
		return res
	}
	qs := DraftQuestions()
	best, u, err := askBanded(ctx, s.asker, map[string]string{"description": in.Description, "manifest": string(in.ManifestJSON), "instruction": in.Instruction}, qs, Samples)
	res.Usage = u
	if err != nil {
		res.Verdict, res.Failure, res.Err = VerdictNotScanned, FailureOf(ctx, err), err
		return res
	}
	res.Verdict = VerdictPass
	res.Scores = make(map[string]int, len(qs))
	res.Probabilities = make(map[string]float64, len(qs))
	for _, q := range qs {
		sc := ScoreFromProbability(best[q.ID])
		res.Probabilities[q.ID], res.Scores[q.ID] = best[q.ID], sc
		res.Score = max(res.Score, sc)
		if sc >= WithholdScore {
			res.Flagged = append(res.Flagged, q.ID)
		}
	}
	if len(res.Flagged) > 0 {
		res.Verdict = VerdictFlagged
	}
	return res
}
