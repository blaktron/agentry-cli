package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// The input sources' fetch step, before the decode. Each source the intake
// answered is fetched by the runner image's `source` role in the files
// container (the decode image), on a network of its own whose only route out is
// a CONNECT proxy to the source's host on port 443, at public addresses alone.
// The token, when there is one, reaches the step on its stdin: never in an
// environment, an argument or a file. Then the proxy and the network are
// removed, the tree the step wrote is held to the source's rules on the host
// (regular files and folders only, no .git, within the caps, the counts the
// report gives), and it moves to /inputs/<id>/, where the decode treats it as
// it treats uploads: a repository with its own cap and malware rule, a url
// source's one file as an upload. The agent container never sees a token, a
// source mount of its own or a route to the source's host.

// sourceRun is one source fetched: the source, what its step reported, and
// how long it took.
type sourceRun struct {
	src     sources.Source
	fetched sources.Fetched
}

// fetchSources fetches every input source the run was given into
// /inputs/<id>/, and rewrites intake.json with what each resolved to. A
// run with no source does nothing here.
func fetchSources(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) ([]sourceRun, error) {
	srcs := p.Intake.SourceList()
	if len(srcs) == 0 {
		return nil, nil
	}
	if p.Policy == nil {
		return nil, Classify(ExitCLI, errors.New("the input sources cannot be fetched: no policy names the files image"))
	}
	img, err := ensureDecode(ctx, d, p.Plan.Proxy.Image, p.Policy, filepath.Join(rd.root, "image"), p.Stderr)
	if err != nil {
		return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the input sources could not be fetched: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeImage))
	}
	// Each source's staging folder goes as its fetch ends; their parent
	// with the last of them.
	work := filepath.Join(rd.root, "sources")
	defer func() { _ = os.Remove(work) }()
	fetcher := SourceFetcher{
		Docker: d, ProxyImage: p.Plan.Proxy.Image, FilesImage: img.Tag, Limits: p.limits(),
		User: rd.personUser(), RunID: runID, Work: work, Receipts: rd.receiptsDir, Stderr: p.Stderr,
		Later:   td.add,
		Network: func(name string) { td.networks = append(td.networks, name) },
	}
	var runs []sourceRun
	for _, s := range srcs {
		f, err := fetchSource(ctx, fetcher, p, rd, *s)
		if err != nil {
			return nil, err
		}
		runs = append(runs, sourceRun{src: *s, fetched: f})
	}
	if p.Intake.Fetched == nil {
		p.Intake.Fetched = map[string]sources.Fetched{}
	}
	for _, r := range runs {
		p.Intake.Fetched[r.src.ID] = r.fetched
	}
	if err := writeIntake(p, rd); err != nil {
		return nil, err
	}
	return runs, nil
}

// fetchSource fetches one source into /inputs/<id>/ with the run's caps:
// the kind's own, or its hosted caps on a hosted run (the one with AI
// Review, which then reads the source's Markdown in the decode).
func fetchSource(ctx context.Context, fetcher SourceFetcher, p Params, rd *runDirs, s sources.Source) (sources.Fetched, error) {
	kind, ok := sources.Lookup(s.Kind)
	if !ok {
		return sources.Fetched{}, Classify(ExitCLI, sources.Unfetched(s.ID, s.Kind))
	}
	dest := filepath.Join(rd.inputs, s.ID)
	if _, err := os.Lstat(dest); err == nil {
		return sources.Fetched{}, Classify(ExitCLI, inputRefused(receipt.InputNameClash, fmt.Errorf("the input source %s collides with an input of the same name under %s", s.ID, spec.InputsMount)))
	}
	f, err := fetcher.Fetch(ctx, s, kind.CapsFor(p.InputReview != nil), dest)
	if err != nil {
		return sources.Fetched{}, err
	}
	relabelForToolContainers(context.WithoutCancel(ctx), p.Stderr, relabelTarget{dir: dest, recursive: true})
	return f, nil
}

// sourceLines puts each source's receipt line into enforced, under
// source.<id>, with the malware check of its files from the decode.
func sourceLines(enforced map[string]string, runs []sourceRun, dec *inputsDecoder) {
	var (
		files    []runnerdecode.File
		reviewed map[string]int
		hosted   bool
	)
	if dec != nil {
		dec.mu.Lock()
		files = dec.before.Report.Files
		reviewed, hosted = maps.Clone(dec.sourceReviewed), dec.review != nil
		dec.mu.Unlock()
	}
	for _, r := range runs {
		line := sources.Line(r.src, r.fetched, sources.ChecksFor(r.src, files))
		if hosted {
			line += "; " + reviewedWords(r.src, reviewed[r.src.ID])
		}
		enforced["source."+r.src.ID] = line
	}
}

// reviewedWords says what AI Review read of a source on a hosted run: a
// repository's Markdown files, a url source's file whole.
func reviewedWords(s sources.Source, markdown int) string {
	if k, ok := sources.Lookup(s.Kind); ok && k.AsUpload {
		return "AI Review reads it as it reads an upload"
	}
	switch markdown {
	case 0:
		return "AI Review read no file of it: it has no Markdown file"
	case 1:
		return "AI Review read its 1 Markdown file, not its code"
	}
	return fmt.Sprintf("AI Review read its %d Markdown files, not its code", markdown)
}

// sourceIDs are the folders under /inputs that the decode holds to the
// source rule. A kind decoded as an upload (a url source) is not among
// them: its file gets every check and every decoder, and counts against
// the uploads' cap.
func sourceIDs(runs []sourceRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		if k, ok := sources.Lookup(r.src.Kind); ok && k.AsUpload {
			continue
		}
		out = append(out, r.src.ID)
	}
	return out
}

// writeIntake rewrites intake.json, in /inputs and the receipts, once the
// sources resolved.
func writeIntake(p Params, rd *runDirs) error {
	return stageIntakeDoc(p.Intake, rd)
}
