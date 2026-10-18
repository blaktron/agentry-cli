// Package build makes or extends a knowledge pack with the Knowledge
// Creator: it stages the documents, decodes them before any agent sees them
// (pkg/runner/decode), runs the built-in Creator over their text with the
// pack as it stands attached (pkg/runner/localrun, pkg/runner/engine), and
// applies the pages it wrote — with the documents themselves and their
// decoded text, which only this step, never the model, puts in the pack's
// sources/ — through pkg/knowledge's apply step. The CLI's `agentry
// knowledge create|add` drives it today; the desktop and the cloud build are
// its other callers.
//
// Locally the Creator runs against the model the runner policy names, for
// no credits, and neither the documents nor the pack are scanned: the pack
// records "not scanned". What the web tools bring back is read by AI
// Review before the model reads it.
//
// A build takes documents, a brief, or both: the brief is staged as one more
// text document (brief.md), so its claims are cited and kept like any
// document's. And a build may search the web, with the built-ins' web
// tools and every result read by AI Review first: Options.Web is that
// run's configuration, and a build without it runs the Creator without
// the tools.
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
	"github.com/blaktron/agentry-cli/pkg/removal"
	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// ensureSignatures is clamdb.Ensure, a variable so the tests use a
// stand-in bundle rather than the registry.
var ensureSignatures = clamdb.Ensure

// hybridClient is hybrid.FromPolicy, a variable so the tests look up
// against a fake service.
var hybridClient = hybrid.FromPolicy

// Options configure one build.
type Options struct {
	// PackDir is the pack to extend, or where to make a new one.
	PackDir string
	// Name names a new pack; ignored when PackDir holds one.
	Name string
	// Documents are the files and directories to ingest.
	Documents []string
	// Brief is what the pack should cover and for whom, in the person's
	// words, at most MaxBriefBytes: kept as sources/brief.md (or
	// brief-N.md when the pack holds one already) and read as a document.
	// A build takes documents, a brief, or both.
	Brief string
	// Web, when set, grants the Creator web_search and web_read for this
	// build, every result read by AI Review first (engine.WebRun); nil
	// runs it without them.
	Web *engine.WebRun

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
	// MaxBytes caps the pack, documents included: knowledge.MaxBytes when
	// zero, Agentry's cap, which a build In Agentry keeps; a build on this
	// computer passes knowledge.MaxLocalBytes.
	MaxBytes int64
	RunID    string
	Stderr   io.Writer
	OnUsage  func(receipt.Usage)
	// Hosted is a sandbox host's build, which never spends a model sign-in
	// (localrun.Run.Hosted).
	Hosted bool
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

// MaxBriefBytes is the most a brief may hold: 8 KiB.
const MaxBriefBytes = 8 << 10

// The brief's document name, and the stem of the names a further brief
// takes (brief-2.md, …), so that an earlier brief's citations still open
// what they cited.
const (
	briefStem = "brief"
	briefName = briefStem + ".md"
)

// CheckBrief refuses a brief past MaxBriefBytes, and a build given neither
// documents nor a brief; the clients ask it before anything runs.
func CheckBrief(documents []string, brief string) error {
	if len(brief) > MaxBriefBytes {
		return fmt.Errorf("the brief is %d bytes; a brief is at most %d (8 KiB)", len(brief), MaxBriefBytes)
	}
	if len(documents) == 0 && strings.TrimSpace(brief) == "" {
		return ErrNothingToBuild
	}
	return nil
}

// ErrNothingToBuild is CheckBrief's answer for a build given neither
// documents nor a brief.
var ErrNothingToBuild = errors.New("a knowledge build needs documents, a brief, or both")

// Build makes or extends the pack.
func Build(ctx context.Context, d dockerx.Client, o Options) (*Result, error) {
	logw := o.Stderr
	if logw == nil {
		logw = io.Discard
	}
	res := &Result{}
	o.Brief = strings.TrimSpace(o.Brief)
	if err := CheckBrief(o.Documents, o.Brief); err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
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
	// It holds the documents' copies, their decoded text and the pages in
	// the clear: a delete that fails even after removal's retry (Windows)
	// is named, never passed over.
	defer func() {
		if err := removeWork(work, true); err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err // the path is named already
			}
			_, _ = fmt.Fprintf(logw, "warning: could not delete %s (%v): it holds the documents and the pages in the clear; delete it by hand\n", work, err)
		}
	}()
	docsDir, decodedDir, inputsDir, applyDir := filepath.Join(work, "docs"), filepath.Join(work, "decoded"), filepath.Join(work, "inputs"), filepath.Join(work, "apply")
	for _, dir := range []string{docsDir, decodedDir, inputsDir, applyDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
	}
	limit := o.MaxBytes
	if limit <= 0 {
		limit = knowledge.MaxBytes
	}
	given, err := stageDocuments(o.Documents, docsDir, limit)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	brief := ""
	if o.Brief != "" {
		if brief, err = stageBrief(o.Brief, o.PackDir, current, given, docsDir); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
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
	// Every document is checked by ClamAV before it is decoded, and a
	// finding refuses the build.
	img.Signatures = ensureSignatures(ctx, o.Home, o.Policy.Images.ClamAVSignaturesRef(), logw)
	// A pack keeps every document as given, an archive's members too, so
	// the decode copies out each member it read.
	dec, err := decode.RunWith(ctx, d, img, docsDir, decodedDir, o.Policy.Docker.Limits.WithDefaults(), decode.Options{KeepMembers: true}, logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	// The risky documents are looked up on Hybrid Analysis by hash, and a
	// malicious verdict refuses the build too.
	lookup, err := hybridClient(o.Policy)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the Hybrid Analysis key: %w", err))
	}
	dec.LookUp(ctx, lookup)
	if dec.Refused() {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the build was refused before the agent started: %s", dec.Refusal()))
	}
	res.Decoded = dec

	docs, inputs, err := dec.Stage(given, inputsDir)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	res.Skipped = decode.Skipped(docs)
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
	// The web tools for this build alone: a build that does not search the
	// web runs the Creator without them, so its plan grants none.
	creator := localrun.KnowledgeCreator()
	if o.Web == nil {
		creator = creator.WithoutWebTools()
	}
	env, runErr := localrun.Run{
		Builtin: creator, Home: o.Home, Policy: o.Policy, PolicySHA: o.PolicySHA,
		PolicyFile: o.PolicyFile, Build: o.Build, Task: strings.NewReader(task(name, current, docs, brief, o.Web != nil, o.Now)),
		Inputs: inputs, Knowledge: attached, Decoded: dec, OutDir: o.OutDir, Budgets: o.Budgets,
		RunID: o.RunID, Stderr: logw, OnUsage: o.OnUsage, Web: o.Web, Hosted: o.Hosted,
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
		Name: name, Now: o.Now, Creator: knowledge.Creator{CLI: o.Build, Model: o.Policy.Model.Name}, MaxBytes: limit,
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
func stageDocuments(paths []string, dst string, limit int64) (map[string]string, error) {
	out := map[string]string{}
	// The documents together are held to the pack's cap before anything
	// runs: they are kept in the pack beside its pages.
	var total int64
	tooLarge := func(p string, size int64) error {
		if total += size; size > limit || total > limit {
			return fmt.Errorf("document %s brings the documents to %d bytes; a knowledge pack, its documents included, is at most %s", p, total, knowledge.FormatCap(limit))
		}
		return nil
	}
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
		if !fi.IsDir() {
			if err := tooLarge(p, fi.Size()); err != nil {
				return nil, err
			}
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
			if info, err := e.Info(); err == nil {
				if err := tooLarge(q, info.Size()); err != nil {
					return err
				}
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
	if len(out) == 0 && len(paths) > 0 {
		return nil, fmt.Errorf("no document was found in %s", strings.Join(paths, ", "))
	}
	return out, nil
}

// stageBrief writes the brief into dst as a text document and adds it to
// given, under brief.md, or brief-N.md when that name is a given document's
// or a source the pack keeps already: an earlier brief stays what its
// citations cite. It answers the name.
func stageBrief(brief, packDir string, current *knowledge.Pack, given map[string]string, dst string) (string, error) {
	used := map[string]bool{}
	for g := range given {
		used[strings.ToLower(g)] = true
	}
	name := briefName
	for n := 2; ; n++ {
		// A case-insensitive file system would make Brief.md and brief.md
		// one file in the stage.
		taken := used[strings.ToLower(name)]
		if !taken && current != nil {
			_, err := os.Lstat(filepath.Join(packDir, knowledge.SourcesDir, name))
			taken = err == nil
		}
		if !taken {
			break
		}
		name = fmt.Sprintf("%s-%d.md", briefStem, n)
	}
	if err := writeOut(filepath.Join(dst, name), []byte(brief+"\n")); err != nil {
		return "", err
	}
	given[name] = "the brief"
	return name, nil
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

// task is the Creator's task: the pack, the date, the documents, which of
// them is the brief, and whether this build may search the web.
func task(name string, current *knowledge.Pack, docs []decode.Document, brief string, web bool, now time.Time) string {
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
	if brief != "" {
		fmt.Fprintf(&b, "\n%s is the brief: what the person wants this pack to cover, and for whom. Read it first and build the pack it asks for.", brief)
		if len(docs) == 1 {
			b.WriteString(" It is the only document: research what it asks for")
			if web {
				b.WriteString(" with web_search and web_read, and from your own knowledge where the web does not settle a point.")
			} else {
				b.WriteString(" from your own knowledge.")
			}
		}
		b.WriteString("\n")
	}
	if web {
		b.WriteString("\nThis build may search the web (web_search, then web_read on a result's address). Search only for what the documents and the pack as it stands do not already hold.\n")
	} else {
		b.WriteString("\nThis build has no web tools.\n")
	}
	return b.String()
}

// collect gathers what the Creator wrote under reports/ and the documents
// it read into applyDir with a changes.json: the pages it wrote, the page
// removals and the note its own changes.json asked for, and every document
// it read with its decoded text under sources/. Everything under reports/
// was written by the run, so it is read through an os.Root, regular files
// only, within the pack's cap. It returns how many of the pages the run
// wrote it kept: a page it also deleted is not counted.
func collect(reports string, dec *decode.Result, docs []decode.Document, applyDir string, runErr error) (int, error) {
	ch := knowledge.Changes{Schema: knowledge.ChangesSchema}
	pages := 0
	if _, err := os.Lstat(filepath.Join(reports, knowledge.WikiDir)); err == nil {
		root, err := os.OpenRoot(reports)
		if err != nil {
			return 0, err
		}
		defer func() { _ = root.Close() }()
		// A link in the reports is never followed: a linked wiki/ fails
		// the build, since there is no tree to take the pages from; a
		// linked page is skipped, and a linked changes.json is not read,
		// as if the Creator had not written them.
		if err := plainpath.Dir(root, knowledge.WikiDir); err != nil {
			return 0, fmt.Errorf("the Creator's reports: %w", err)
		}
		// The model removes pages and nothing else: sources/ is the
		// build's, and holds the documents as given. A page it wrote and
		// then deleted in the same run is deleted, not written, and a
		// delete it named twice is one: the apply step refuses a list
		// that names a path twice.
		deleted := map[string]bool{}
		if own, ok := ownChanges(root); ok {
			for _, rel := range own.Delete {
				if strings.HasPrefix(rel, knowledge.WikiDir+"/") && !deleted[rel] {
					deleted[rel] = true
					ch.Delete = append(ch.Delete, rel)
				}
			}
			ch.Summary = own.Summary
		}
		err = fs.WalkDir(root.FS(), knowledge.WikiDir, func(rel string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() || !strings.HasSuffix(rel, ".md") {
				return err
			}
			if b := path.Base(rel); b == knowledge.IndexFile || rel == knowledge.WikiDir+"/"+knowledge.LogFile {
				return nil // the apply step writes these
			}
			if deleted[rel] {
				return nil // the Creator deleted it after writing it
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
				return fmt.Errorf("the Creator wrote %s past %s, more than a page can be", rel, knowledge.FormatCap(knowledge.MaxBytes))
			}
			pages++
			ch.Write = append(ch.Write, rel)
			return writeOut(filepath.Join(applyDir, filepath.FromSlash(rel)), body)
		})
		if err != nil {
			return 0, err
		}
	}
	if runErr != nil {
		// The log says so: the documents below were given to a run that
		// did not finish, and may be only partly in the pages.
		ch.Summary = strings.TrimSpace("The Creator run stopped before it finished (" + runErr.Error() + "); these documents may be only partly compiled, and a further add continues. " + ch.Summary)
	}
	for _, doc := range docs {
		if !dec.Keepable(doc.Name) {
			// A member that could not be kept as given: it was not
			// read, and the pack cannot hold it.
			continue
		}
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
// a plain file within the pack's bound; a link is not read.
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

// removeWork is removal.Remove, a variable so a test can hold the
// directory the way Docker Desktop can.
var removeWork = removal.Remove

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
