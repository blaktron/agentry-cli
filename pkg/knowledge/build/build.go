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
	"github.com/blaktron/agentry-cli/pkg/plainpath"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

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

	// The work directory is under the runner home, not the process's
	// temporary directory: the decode container binds from it, and the
	// daemon sees the runner home wherever the runner runs — a sandbox
	// host's runner is in a container whose /tmp the daemon cannot see.
	workRoot := filepath.Join(o.Home, "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	work, err := os.MkdirTemp(workRoot, "knowledge-build-")
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

	docs, inputs, err := dec.Stage(given, inputsDir)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	res.Skipped = dec.Undecodable()
	var readable []decode.Document
	for _, doc := range docs {
		if doc.Read != "" {
			readable = append(readable, doc)
			res.Documents = append(res.Documents, doc.Name)
		}
	}
	docs = readable
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
	n, err := collect(filepath.Join(o.OutDir, "reports"), dec, docs, applyDir, runErr)
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
		if rel == decode.DocumentsFile {
			return fmt.Errorf("%s: a document may not be named %s, the Creator's own list; rename it", from, decode.DocumentsFile)
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
		if !fi.IsDir() && fi.Size() > knowledge.MaxBytes {
			return nil, fmt.Errorf("document %s is %d bytes; a knowledge pack, its documents included, is at most 10 MB", p, fi.Size())
		}
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
			if info, err := e.Info(); err == nil && info.Size() > knowledge.MaxBytes {
				return fmt.Errorf("document %s is %d bytes; a knowledge pack, its documents included, is at most 10 MB", q, info.Size())
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

// task is the Creator's task: the pack, the date, the documents.
func task(name string, current *knowledge.Pack, docs []decode.Document, now time.Time) string {
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
// it read into applyDir with a changes.json: the pages it wrote, the page
// removals and the note its own changes.json asked for, and every document
// it read with its decoded text under sources/. Everything under reports/
// was written by the run, so it is read through an os.Root, regular files
// only, within the pack's cap. It returns how many pages the run wrote.
func collect(reports string, dec *decode.Result, docs []decode.Document, applyDir string, runErr error) (int, error) {
	ch := knowledge.Changes{Schema: knowledge.ChangesSchema}
	pages := 0
	if _, err := os.Lstat(filepath.Join(reports, knowledge.WikiDir)); err == nil {
		root, err := os.OpenRoot(reports)
		if err != nil {
			return 0, err
		}
		defer func() { _ = root.Close() }()
		// A link in the reports is never followed (agentry-cli#421): a
		// linked wiki/ fails the build, since there is no tree to take the
		// pages from; a linked page is skipped, and a linked changes.json
		// is not read, as if the Creator had not written them.
		if err := plainpath.Dir(root, knowledge.WikiDir); err != nil {
			return 0, fmt.Errorf("the Creator's reports: %w", err)
		}
		err = fs.WalkDir(root.FS(), knowledge.WikiDir, func(rel string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() || !strings.HasSuffix(rel, ".md") {
				return err
			}
			if b := path.Base(rel); b == knowledge.IndexFile || rel == knowledge.WikiDir+"/"+knowledge.LogFile {
				return nil // the apply step writes these
			}
			fi, err := plainpath.File(root, rel)
			if err != nil {
				return nil //nolint:nilerr // a link on the way: skipped, never followed
			}
			if fi.Size() > knowledge.MaxBytes {
				return fmt.Errorf("the Creator wrote %s past the pack's cap", rel)
			}
			body, err := fs.ReadFile(root.FS(), rel)
			if err != nil {
				return err
			}
			if len(body) > knowledge.MaxBytes {
				return fmt.Errorf("the Creator wrote %s past the pack's cap", rel)
			}
			pages++
			ch.Write = append(ch.Write, rel)
			return writeOut(filepath.Join(applyDir, filepath.FromSlash(rel)), body)
		})
		if err != nil {
			return 0, err
		}
		if own, ok := ownChanges(root); ok {
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
	if runErr != nil {
		// The log says so: the documents below were given to a run that
		// did not finish, and may be only partly in the pages.
		ch.Summary = strings.TrimSpace("The Creator run stopped before it finished (" + runErr.Error() + "); these documents may be only partly compiled, and a further add continues. " + ch.Summary)
	}
	for _, doc := range docs {
		ch.Documents = append(ch.Documents, doc.Name)
		src := knowledge.SourcesDir + "/" + doc.Name
		ch.Write = append(ch.Write, src)
		if err := dec.CopyOriginal(doc.Name, filepath.Join(applyDir, filepath.FromSlash(src))); err != nil {
			return 0, err
		}
		if doc.DecodedBy != "" {
			ch.Write = append(ch.Write, src+".txt")
			if err := dec.CopyText(doc.Name, filepath.Join(applyDir, filepath.FromSlash(src+".txt"))); err != nil {
				return 0, err
			}
		}
	}
	sort.Strings(ch.Write)
	raw, err := json.MarshalIndent(ch, "", "  ")
	if err != nil {
		return 0, err
	}
	return pages, writeOut(filepath.Join(applyDir, knowledge.ChangesFile), raw)
}

// ownChanges reads the changes.json the Creator wrote, if it wrote one as
// a plain file within the pack's bound; a link is not read (agentry-cli#421).
func ownChanges(root *os.Root) (knowledge.Changes, bool) {
	var own knowledge.Changes
	if _, err := plainpath.File(root, knowledge.ChangesFile); err != nil {
		return own, false
	}
	raw, err := fs.ReadFile(root.FS(), knowledge.ChangesFile)
	if err != nil || len(raw) > 1<<20 {
		return own, false
	}
	return own, json.Unmarshal(raw, &own) == nil
}

func writeOut(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644) //nolint:gosec // a page of the pack, or the apply step's input
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
