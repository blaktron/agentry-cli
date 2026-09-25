// Package build makes or extends a knowledge pack with the Knowledge
// Creator (plan knowledge.md §4.2; agentry-cli#318): it stages the
// documents, decodes them before any agent sees them (pkg/runner/decode),
// runs the built-in Creator over their text with the pack as it stands
// attached (pkg/runner/localrun, pkg/runner/engine), and applies the pages
// it wrote — with the documents themselves and their decoded text, which
// only this step, never the model, puts in the pack's sources/ — through
// pkg/knowledge's apply step. The CLI's `agentry knowledge create|add`
// drives it today; the desktop and the cloud build are its other callers.
//
// Locally the Creator runs against the model the runner policy names, for
// no credits, and nothing it reads or writes is scanned: the pack records
// "not scanned" (D19).
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// DocumentsFile is the Creator's list of what it was given, at the root of
// its inputs.
const DocumentsFile = "documents.json"

// Options configure one build.
type Options struct {
	// PackDir is the pack to extend, or where to make a new one.
	PackDir string
	// Name names a new pack; ignored when PackDir holds one.
	Name string
	// Documents are the files and directories to ingest.
	Documents []string

	Home       string
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	// Build is the client's build identity: the receipt's and the pack's
	// record of what made it.
	Build string
	// Now dates the pages and the log entry.
	Now time.Time
	// OutDir receives the run's reports and receipts.
	OutDir string
	// Budgets, when set, replace the Creator plan's.
	Budgets *spec.Budgets
	RunID   string
	Stderr  io.Writer
	OnUsage func(receipt.Usage)
}

// Result is what one build did.
type Result struct {
	// Envelope is the Creator run's receipt; nil when the run never began.
	Envelope *receipt.Envelope
	// Decoded is the decode step over the documents.
	Decoded *decode.Result
	// Applied is the apply step's account; nil when nothing was applied.
	Applied *knowledge.Result
	// Documents are the documents the Creator was given, by the name the
	// pack cites them under; Skipped are those no decoder could read.
	Documents, Skipped []string
	// RunErr is the Creator run's own failure when the build applied what
	// it wrote before it stopped (a budget, a timeout); the pack is valid,
	// and a further build with the same documents continues it.
	RunErr error
}

// Build makes or extends the pack.
func Build(ctx context.Context, d dockerx.Client, o Options) (*Result, error) {
	logw := o.Stderr
	if logw == nil {
		logw = io.Discard
	}
	res := &Result{}
	var current *knowledge.Pack
	if _, err := os.Stat(filepath.Join(o.PackDir, knowledge.ManifestFile)); err == nil {
		if current, err = knowledge.Load(o.PackDir); err != nil {
			return nil, engine.Classify(engine.ExitVerify, err)
		}
	} else {
		if err := knowledge.ValidName(o.Name); err != nil {
			return nil, engine.Classify(engine.ExitUsage, err)
		}
		if entries, err := os.ReadDir(o.PackDir); err == nil && len(entries) > 0 {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("%s exists and is not a knowledge pack; name a new directory or an existing pack", o.PackDir))
		}
	}
	name := o.Name
	if current != nil {
		name = current.Manifest.Name
	}

	work, err := os.MkdirTemp("", "agentry-knowledge-build-*")
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	docsDir, decodedDir, inputsDir, applyDir := filepath.Join(work, "docs"), filepath.Join(work, "decoded"), filepath.Join(work, "inputs"), filepath.Join(work, "apply")
	for _, dir := range []string{docsDir, decodedDir, inputsDir, applyDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
	}
	given, err := stageDocuments(o.Documents, docsDir)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}

	runnerTag := o.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(o.Home, "image")
	if err := gateway.EnsureInstalled(ctx, d, o.Policy, runnerTag, ctxDir, logw, true); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	img, err := decode.Ensure(ctx, d, runnerTag, o.Policy, ctxDir, logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	dec, err := decode.Run(ctx, d, img, docsDir, decodedDir, o.Policy.Docker.Limits.WithDefaults(), logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	res.Decoded = dec

	docs, inputs, err := creatorInputs(dec, given, docsDir, inputsDir)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	res.Skipped = dec.Undecodable()
	for _, doc := range docs {
		res.Documents = append(res.Documents, doc.Name)
	}
	if len(docs) == 0 {
		return res, engine.Classify(engine.ExitUsage, fmt.Errorf("no document could be read: %s", strings.Join(res.Skipped, ", ")))
	}

	var attached []engine.AttachedPack
	if current != nil {
		attached = []engine.AttachedPack{engine.AttachPack(current)}
	}
	env, runErr := localrun.Run{
		Builtin: localrun.KnowledgeCreator(), Home: o.Home, Policy: o.Policy, PolicySHA: o.PolicySHA,
		PolicyFile: o.PolicyFile, Build: o.Build, Task: strings.NewReader(task(name, current, docs, o.Now)),
		Inputs: inputs, Knowledge: attached, Decoded: dec, OutDir: o.OutDir, Budgets: o.Budgets,
		RunID: o.RunID, Stderr: logw, OnUsage: o.OnUsage,
	}.Execute(ctx, d)
	res.Envelope = env

	// Whatever the run wrote is applied, even when it stopped early: each
	// page is checked on its own, and a pack that took some documents'
	// pages is a valid pack a further build continues.
	n, err := collect(filepath.Join(o.OutDir, "reports"), docsDir, dec, docs, applyDir)
	if err != nil {
		return res, engine.Classify(engine.ExitCLI, err)
	}
	if n == 0 {
		if runErr != nil {
			return res, runErr
		}
		return res, engine.Classify(engine.ExitHarness, errors.New("the Knowledge Creator wrote no page"))
	}
	applied, err := knowledge.Apply(o.PackDir, applyDir, knowledge.ApplyOptions{
		Name: name, Now: o.Now, Creator: knowledge.Creator{CLI: o.Build, Model: o.Policy.Model.Name},
	})
	if err != nil {
		if errors.Is(err, knowledge.ErrTooLarge) {
			return res, engine.Classify(engine.ExitBudget, err)
		}
		return res, engine.Classify(engine.ExitCLI, err)
	}
	res.Applied = applied
	res.RunErr = runErr
	return res, nil
}

// stageDocuments copies the documents under dst by names a citation can
// carry — letters, digits, '.', '-' and '_', anything else a '-' — so
// "Q3 report.pdf" is cited as doc:Q3-report.pdf; a directory keeps its
// layout below its own name. Two documents of one name are refused.
func stageDocuments(paths []string, dst string) (map[string]string, error) {
	out := map[string]string{}
	add := func(rel, from string) error {
		if rel == DocumentsFile {
			return fmt.Errorf("%s: a document may not be named %s, the Creator's own list; rename it", from, DocumentsFile)
		}
		if prev, dup := out[rel]; dup {
			return fmt.Errorf("%s and %s would both be the document %s; rename one", prev, from, rel)
		}
		out[rel] = from
		return nil
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("document %s: %w", p, err)
		}
		base := safeName(filepath.Base(abs))
		if !fi.IsDir() {
			if err := add(base, p); err != nil {
				return nil, err
			}
			if err := copyFile(abs, filepath.Join(dst, base)); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(abs, func(q string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			if !e.Type().IsRegular() {
				return nil // a link is not a document
			}
			r, _ := filepath.Rel(abs, q)
			parts := strings.Split(filepath.ToSlash(r), "/")
			for i := range parts {
				parts[i] = safeName(parts[i])
			}
			rel := base + "/" + strings.Join(parts, "/")
			if err := add(rel, q); err != nil {
				return err
			}
			return copyFile(q, filepath.Join(dst, filepath.FromSlash(rel)))
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no document was given")
	}
	return out, nil
}

func safeName(s string) string {
	b := []rune(s)
	for i, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			b[i] = '-'
		}
	}
	out := strings.Trim(string(b), ".")
	if out == "" {
		out = "document"
	}
	return out
}

// document is one line of documents.json.
type document struct {
	// Name is the document as the pack cites it: [src: doc:<Name>].
	Name string `json:"name"`
	// GivenAs is the path it was given as.
	GivenAs string `json:"givenAs"`
	// Read is the file under the inputs to read: the document itself when
	// it is text, else the text decoded from it.
	Read   string `json:"read"`
	Family string `json:"family"`
	// DecodedBy names the decoder when the text was decoded.
	DecodedBy string `json:"decodedBy,omitempty"`
}

// creatorInputs lays out what the Creator reads: each readable document's
// text at the inputs root, and documents.json naming them; it returns the
// documents and the paths to stage under /inputs.
func creatorInputs(dec *decode.Result, givenAs map[string]string, docsDir, inputsDir string) ([]document, []string, error) {
	var docs []document
	// What the Creator reads, and what the pack keeps, must name one file
	// each: a text document called a.pdf.txt beside a.pdf, whose decoded
	// text is a.pdf.txt too, is refused rather than one hiding the other.
	claimed := map[string]string{}
	claim := func(name, by string) error {
		if prev, dup := claimed[name]; dup && prev != by {
			return fmt.Errorf("the documents %s and %s would both be read as %s; rename one", prev, by, name)
		}
		claimed[name] = by
		return nil
	}
	for _, f := range dec.Report.Files {
		if f.Outcome == runnerdecode.OutcomeText || f.Outcome == runnerdecode.OutcomeDecoded {
			if err := claim(f.Path, f.Path); err != nil {
				return nil, nil, err
			}
			if f.Outcome == runnerdecode.OutcomeDecoded {
				if err := claim(f.Output, f.Path); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	for _, f := range dec.Report.Files {
		doc := document{Name: f.Path, GivenAs: givenAs[f.Path], Family: f.Family}
		var src string
		switch f.Outcome {
		case runnerdecode.OutcomeText:
			doc.Read, src = f.Path, filepath.Join(docsDir, filepath.FromSlash(f.Path))
		case runnerdecode.OutcomeDecoded:
			doc.Read, doc.DecodedBy, src = f.Output, f.Tool, filepath.Join(dec.Out, filepath.FromSlash(f.Output))
		default:
			continue
		}
		if err := copyFile(src, filepath.Join(inputsDir, filepath.FromSlash(doc.Read))); err != nil {
			return nil, nil, err
		}
		docs = append(docs, doc)
	}
	raw, err := json.MarshalIndent(map[string]any{"documents": docs}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(inputsDir, DocumentsFile), append(raw, '\n'), 0o644); err != nil { //nolint:gosec // read by the run
		return nil, nil, err
	}
	entries, err := os.ReadDir(inputsDir)
	if err != nil {
		return nil, nil, err
	}
	var inputs []string
	for _, e := range entries {
		inputs = append(inputs, filepath.Join(inputsDir, e.Name()))
	}
	sort.Strings(inputs)
	return docs, inputs, nil
}

// task is the Creator's task: the pack, the date, the documents.
func task(name string, current *knowledge.Pack, docs []document, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Knowledge pack: %s. ", name)
	if current != nil {
		fmt.Fprintf(&b, "It exists, with %s: read its index with knowledge_list before you write, and extend it. ", knowledge.CountPages(current.Manifest.Pages))
	} else {
		b.WriteString("It is new: this run makes its first pages. ")
	}
	fmt.Fprintf(&b, "Today is %s.\n\nIngest these %d documents (documents.json names the file to read for each):\n", now.UTC().Format("2006-01-02"), len(docs))
	for _, d := range docs {
		fmt.Fprintf(&b, "- %s\n", d.Name)
	}
	return b.String()
}

// collect gathers what the Creator wrote under reports/ and the documents
// it read into applyDir with a changes.json: the pages it wrote, the
// removals and the note its own changes.json asked for, and every
// document it read with its decoded text under sources/. It returns how
// many pages the run wrote.
func collect(reports, docsDir string, dec *decode.Result, docs []document, applyDir string) (int, error) {
	ch := knowledge.Changes{Schema: knowledge.ChangesSchema}
	wiki := filepath.Join(reports, knowledge.WikiDir)
	pages := 0
	if _, err := os.Stat(wiki); err == nil {
		err := filepath.WalkDir(wiki, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
				return err
			}
			rel, _ := filepath.Rel(reports, p)
			rel = filepath.ToSlash(rel)
			if b := path.Base(rel); b == knowledge.IndexFile || rel == knowledge.WikiDir+"/"+knowledge.LogFile {
				return nil // the apply step writes these
			}
			pages++
			ch.Write = append(ch.Write, rel)
			return copyFile(p, filepath.Join(applyDir, filepath.FromSlash(rel)))
		})
		if err != nil {
			return 0, err
		}
	}
	if raw, err := os.ReadFile(filepath.Join(reports, knowledge.ChangesFile)); err == nil { //nolint:gosec // the run's own report
		var own knowledge.Changes
		if json.Unmarshal(raw, &own) == nil {
			// The model removes pages and nothing else: sources/ is the
			// build's, and holds the documents as given.
			for _, rel := range own.Delete {
				if strings.HasPrefix(rel, knowledge.WikiDir+"/") {
					ch.Delete = append(ch.Delete, rel)
				}
			}
			ch.Summary = own.Summary
		}
	}
	decoded := map[string]string{}
	for _, f := range dec.Report.Files {
		if f.Outcome == runnerdecode.OutcomeDecoded {
			decoded[f.Path] = f.Output
		}
	}
	for _, doc := range docs {
		ch.Documents = append(ch.Documents, doc.Name)
		src := knowledge.SourcesDir + "/" + doc.Name
		ch.Write = append(ch.Write, src)
		if err := copyFile(filepath.Join(docsDir, filepath.FromSlash(doc.Name)), filepath.Join(applyDir, filepath.FromSlash(src))); err != nil {
			return 0, err
		}
		if out, ok := decoded[doc.Name]; ok {
			ch.Write = append(ch.Write, src+".txt")
			if err := copyFile(filepath.Join(dec.Out, filepath.FromSlash(out)), filepath.Join(applyDir, filepath.FromSlash(src+".txt"))); err != nil {
				return 0, err
			}
		}
	}
	sort.Strings(ch.Write)
	raw, err := json.MarshalIndent(ch, "", "  ")
	if err != nil {
		return 0, err
	}
	return pages, os.WriteFile(filepath.Join(applyDir, knowledge.ChangesFile), raw, 0o644) //nolint:gosec // the apply step's input
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src) //nolint:gosec // a document or a file this build made
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644) //nolint:gosec // a document or a page of the pack
}
