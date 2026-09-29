package drafter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	corejudge "github.com/blaktron/agentry/pkg/judge"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// maxTask is the longest task a draft is given: the description and its
// hints together, as the model reads them. It is half the scan route's cap,
// so the description's scan is one request of at most four chunks.
const maxTask = 32 << 10

// Task is the Drafter's task message: the person's description and hints,
// framed. It is exactly what the description's scan reads (Prescan) and
// what the model is given.
func Task(description string, hints []string) (string, error) {
	description = strings.TrimSpace(description)
	if description == "" {
		return "", errors.New("describe what the QuickAgent should do")
	}
	var b strings.Builder
	b.WriteString("Draft a QuickAgent for the person's description below. AI Review read the description and the hints before you did.\n\n")
	b.WriteString("## Description\n\n")
	b.WriteString(description)
	b.WriteString("\n")
	var kept []string
	for _, h := range hints {
		if h = strings.TrimSpace(h); h != "" {
			kept = append(kept, h)
		}
	}
	if len(kept) > 0 {
		b.WriteString("\n## Hints\n\n")
		for _, h := range kept {
			b.WriteString("- " + strings.ReplaceAll(h, "\n", " ") + "\n")
		}
	}
	task := b.String()
	if !utf8.ValidString(task) {
		return "", errors.New("the description and hints must be text (UTF-8)")
	}
	if len(task) > maxTask {
		return "", fmt.Errorf("the description and hints are %d KiB; a draft takes at most %d KiB of them", len(task)>>10, maxTask>>10)
	}
	return task, nil
}

// Prescan is the step before the Drafter starts (plan §7.3, D22): AI Review
// reads the task as a description, and one that is flagged, or cannot be
// scanned, refuses the draft (exit 11) before anything else happens.
func Prescan(ctx context.Context, sc webscan.Scanner, task string) (webscan.Result, error) {
	res := sc.Scan(ctx, webscan.SourceDescription, task)
	if !res.Passed() {
		return res, engine.Classify(engine.ExitPolicy, fmt.Errorf(
			"AI Review did not pass the description, so the Drafter did not start: %s. Nothing was searched or read; reword the description and try again", res.Reason()))
	}
	return res, nil
}

// CoreJev scans with the person's own Jev key through the core's own
// pkg/judge, the registry's client and question set (D25). It is the
// description's scanner for a local draft; the tools server in the run
// asks the same questions through the image's byte-for-byte copy.
func CoreJev(key string) (webscan.Scanner, error) {
	c, err := corejudge.New(corejudge.Config{APIKey: key})
	if err != nil {
		return nil, err
	}
	return coreJev{corejudge.NewModelInputScanner(c)}, nil
}

type coreJev struct{ s *corejudge.ModelInputScanner }

func (j coreJev) Scan(ctx context.Context, source webscan.Source, text string) webscan.Result {
	if len(text) > webscan.MaxText {
		return webscan.Result{Verdict: webscan.NotScanned, Failure: webscan.FailureTooLarge}
	}
	r := j.s.Scan(ctx, corejudge.Source(source), text)
	flagged := append([]string(nil), r.Flagged...)
	sort.Strings(flagged)
	res := webscan.Result{Verdict: webscan.Verdict(r.Verdict), Score: r.Score, Flagged: flagged, Model: r.Model,
		QuestionSet: r.QuestionSet, Requests: r.Usage.Requests, InputTokens: int64(r.Usage.InputTokens)}
	if res.Verdict == webscan.NotScanned {
		for _, c := range r.Chunks {
			if c.Failure != "" {
				res.Failure = c.Failure
				break
			}
		}
	}
	return res
}
