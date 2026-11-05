package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// PagesOptions configure ReadPages: the addresses a built-in other than the
// Voice Writer is given (the Report Designer's), read exactly as a voice's
// are.
type PagesOptions struct {
	URLs []*sources.Source
	// Review is AI Review, which reads every fetched page's text before
	// the run.
	Review webscan.Scanner
	// NoRender, when set, is why a page drawn by scripts cannot be read
	// with web_read in this build; such a page is withheld with it.
	NoRender string
	Home     string
	Policy   *policy.Policy
	RunID    string
	// OutDir receives the fetch step's receipts.
	OutDir string
	// Work is a folder of the caller's under the runner home, removed by
	// the caller: the pages are fetched, decoded and staged beneath it.
	Work   string
	Stderr io.Writer
}

// PagesResult is what ReadPages read.
type PagesResult struct {
	// Inputs is the folder holding the text of the pages AI Review passed
	// and documents.json, to be given to the run as one input; empty when
	// no page's text was staged.
	Inputs string
	// Docs are the documents staged there.
	Docs []decode.Document
	// Given are the addresses web_read may open: pages drawn by scripts.
	Given []string
	// Pages are the addresses given and what became of each.
	Pages []*Page
	// Decoded is the decode step's result, for the receipt.
	Decoded *decode.Result
}

// ReadPages fetches each address in the files container, checks what it
// brought with ClamAV and Hybrid Analysis, decodes it, and has AI Review
// read each page's text before the run, staging what passed under
// o.Work/pages as text inputs: the steps a voice's addresses take, for a
// build with addresses and no samples.
func ReadPages(ctx context.Context, d dockerx.Client, o PagesOptions) (*PagesResult, error) {
	if len(o.URLs) == 0 {
		return &PagesResult{}, nil
	}
	if o.Review == nil {
		return nil, engine.Classify(engine.ExitPolicy, ErrNoReviewKey)
	}
	logw := o.Stderr
	if logw == nil {
		logw = io.Discard
	}
	fetched, decodedDir, inputsDir := filepath.Join(o.Work, "fetched"), filepath.Join(o.Work, "decoded"), filepath.Join(o.Work, "pages")
	for _, dir := range []string{fetched, decodedDir, inputsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
	}
	runnerTag := o.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(o.Home, "image")
	img, err := decode.Ensure(ctx, d, runnerTag, o.Policy, ctxDir, logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	fetcher, err := engine.BuildSourceFetcher(ctx, d, runnerTag, img.Tag, o.Policy.Docker.Limits, o.RunID, filepath.Join(o.Work, "sources"), filepath.Join(o.OutDir, "receipts"), logw)
	if err != nil {
		return nil, err
	}
	res := &PagesResult{}
	given := map[string]string{}
	if res.Pages, err = fetchPages(ctx, fetcher, o.URLs, fetched, given); err != nil {
		return res, err
	}
	img.Signatures = ensureSignatures(ctx, o.Home, o.Policy.Images.ClamAVSignaturesRef(), logw)
	dec, err := decode.Run(ctx, d, img, fetched, decodedDir, o.Policy.Docker.Limits.WithDefaults(), logw)
	if err != nil {
		return res, engine.Classify(engine.ExitGateway, err)
	}
	lookup, err := hybridClient(o.Policy)
	if err != nil {
		return res, engine.Classify(engine.ExitPolicy, fmt.Errorf("the Hybrid Analysis key: %w", err))
	}
	dec.LookUp(ctx, lookup)
	if dec.Refused() {
		return res, engine.Classify(engine.ExitPolicy, fmt.Errorf("the build was refused before the agent started: %s", dec.Refusal()))
	}
	res.Decoded = dec
	all, _, err := dec.StageWith(given, inputsDir, decode.StageOptions{Members: true})
	if err != nil {
		return res, engine.Classify(engine.ExitUsage, err)
	}
	if res.Docs, res.Given, err = reviewPages(ctx, o.Review, dec, all, res.Pages, inputsDir, o.NoRender); err != nil {
		return res, err
	}
	if len(res.Docs) > 0 {
		res.Inputs = inputsDir
	}
	if len(res.Docs) == 0 && len(res.Given) == 0 {
		var unread []string
		for _, p := range res.Pages {
			if p.Withheld != "" {
				unread = append(unread, p.URL+" ("+p.Withheld+")")
			}
		}
		return res, engine.Classify(engine.ExitUsage, errors.Join(errors.New("no page could be read"), fmt.Errorf("%v", unread)))
	}
	return res, nil
}
