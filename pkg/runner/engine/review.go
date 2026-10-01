// The review grant at run time (plan desktop-review-key-and-run-cards.md
// §6.2, D18, D27, D28; agentry-cli#503). The compiled plan grants it
// (spec.CapSummary.ReviewTools, toolsrv.Review.Enabled) and gives the tools
// container TypeSafe's API as its one host; the run brings the person's key.
// The engine refuses a run of such a plan without the key before anything
// starts, writes the key into the run's gateway secrets file for the tools
// container alone, stamps the ledger into the run's copy of the tools spec,
// and counts the ledger into the receipt.
package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// ReviewRun is a review-grant run's key: the person's own TypeSafe key. It
// reaches the tools container alone, through the run's gateway secrets
// file, which is deleted at teardown; the receipts are masked of it.
type ReviewRun struct {
	JevKey string
}

// ReviewLedgerFile is the review ledger in the receipts directory.
const ReviewLedgerFile = "review.ndjson"

// ErrNoReviewKey is the refusal of a review-grant run without the person's
// key (D28, as a draft on this computer is refused without one, DR-44).
var ErrNoReviewKey = errors.New("a review runs only with your own TypeSafe key, and this run has none")

// checkReview holds a run to its plan: a plan with the review grant runs
// only with the person's key, and a plan without it takes none.
func checkReview(p Params) error {
	has := p.Plan.Capabilities.ReviewTools
	switch {
	case !has && p.Review != nil:
		return Classify(ExitCLI, errors.New("this plan has no review grant, so a run cannot be given a review key"))
	case !has:
		return nil
	case p.Review == nil || p.Review.JevKey == "":
		return Classify(ExitPolicy, ErrNoReviewKey)
	}
	return nil
}

// reviewSecrets is the review key as the gateway secrets file names it.
func reviewSecrets(p Params) map[string]string {
	if p.Review == nil {
		return nil
	}
	return map[string]string{compile.SecretKey(toolsrv.EnvReviewJevKey): p.Review.JevKey}
}

// grantReview stamps the run's ledger into the rendered tools spec.
func grantReview(gatewayDir string, r *ReviewRun) error {
	if r == nil {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if !ts.Review.Enabled {
		return errors.New("this run's rendered tools spec has no review grant; remove the plan directory to recompile it with this CLI")
	}
	ts.Review.Ledger = path.Join(spec.OutMount, "receipts", ReviewLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// ReadReviewLedger reads a run's review ledger (receipts/review.ndjson): its
// entries, and how many lines did not decode. A missing ledger is none.
func ReadReviewLedger(path string) ([]toolsrv.ReviewEntry, int, error) {
	f, err := os.Open(path) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.ReviewEntry
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e toolsrv.ReviewEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			bad++
			continue
		}
		out = append(out, e)
	}
	return out, bad, sc.Err()
}

// reviewRecord counts the run's review ledger into the receipt; nil for a
// run without the review grant. A line that does not decode is counted as
// refused, never as passed.
func reviewRecord(receiptsDir string, r *ReviewRun) *receipt.Review {
	if r == nil {
		return nil
	}
	rec := &receipt.Review{}
	entries, bad, _ := ReadReviewLedger(filepath.Join(receiptsDir, ReviewLedgerFile))
	rec.Refused = bad
	for _, e := range entries {
		if e.Refused != "" {
			rec.Refused++
			continue
		}
		rec.ScanRequests += e.Requests
		rec.ScanInputTokens += e.InputTokens
		if e.Model != "" {
			rec.Model = e.Model
		}
		if e.Tool == toolsrv.ToolAIReview {
			if e.Verdict == webscan.NotScanned {
				rec.Failed++
				continue
			}
			rec.Reviews++
			rec.Findings += e.Findings
			rec.RubricHash = e.RubricHash
			continue
		}
		rec.Read++
		switch e.Verdict {
		case webscan.Pass:
			rec.Passed++
		case webscan.Withheld:
			rec.Withheld++
		default:
			rec.NotScanned++
		}
		if e.QuestionSet != "" {
			rec.QuestionSet = e.QuestionSet
		}
	}
	return rec
}

// reviewLine is the receipt's enforcement line for the counts and the
// network, which only the per-run docker filter holds at the daemon too.
func reviewLine(rec *receipt.Review) string {
	line := fmt.Sprintf("AI Review read every input file read_input returned before the model, with the person's own TypeSafe key: %d read, %d passed, %d withheld as flagged, %d withheld as not scanned; %d refused before anything was scanned; "+
		"%d rubric review(s) answered with %d finding(s) at score 2 or more, %d did not finish",
		rec.Read, rec.Passed, rec.Withheld, rec.NotScanned, rec.Refused, rec.Reviews, rec.Findings, rec.Failed)
	if rec.Model != "" {
		line += "; judge " + rec.Model
	}
	if rec.RubricHash != "" {
		line += ", rubric " + rec.RubricHash
	}
	line += "; each file and verdict is in receipts/" + ReviewLedgerFile + " and gateway.log. The tools container reaches " + toolsrv.ReviewHost + ":443 alone, through the gateway's proxy"
	if filterSupported {
		return line + ", and the run's docker filter admits that proxy toward no other host"
	}
	return line + "; the per-run docker filter, which would also hold the proxy to that host at the daemon, runs on Linux only (Docker Desktop, agentry-cli#322)"
}
