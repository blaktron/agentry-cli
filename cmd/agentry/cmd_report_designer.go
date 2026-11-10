package main

// agentry report-design design | cover: the built-in Report Designer on
// this computer. Design writes a whole report design from a brief; Cover
// writes a custom cover page in Typst for one of the person's designs. What
// it makes is a new design: written to the run's output folder, and with
// --save added to the account's library under a name of its own, never as
// the default and never in the place of a design there.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/reportdesigner/build"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	voicebuild "github.com/blaktron/agentry-cli/pkg/voice/build"
)

// designerRun is the run itself (pkg/reportdesigner/build); a seam for the
// commands' tests, which check what is saved and said around it.
var designerRun = build.Run

// designerOpts are the Report Designer commands' flags.
type designerOpts struct {
	brief, from, logo string
	documents         []string
	images, urls      []string
	save              bool
	out               string
	policyPath        string
	budgetFlags
	outputFormat string
	stage        string
	onUsage      func(receipt.Usage)
}

// maxDesignerImageBytes caps an image file as given, before the core's own
// caps after decoding.
const maxDesignerImageBytes = 4 << 20

func designerRunFlags(cmd *cobra.Command, o *designerOpts) {
	f := cmd.Flags()
	f.StringVar(&o.brief, "brief", "", "what the reports are and how they should look (required)")
	f.BoolVar(&o.save, "save", false, "add the result to your account's report designs, under a new name and never as the default")
	f.StringVar(&o.out, "out", "", "output folder for the run's reports, receipts and the design it makes (default: ./agentry-out/report-designer)")
	f.StringVar(&o.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.IntVar(&o.timeout, "timeout", 0, "the run's wall-clock budget in seconds (default: the policy's or 1800, whichever is longer)")
	f.IntVar(&o.maxTurns, "max-turns", -1, "the run's tool-call budget, 0 = unlimited (default: the policy's)")
	f.Float64Var(&o.maxCost, "max-cost", -1, "stop the run once the harness reports this much cost in USD (default: the policy's)")
	f.StringVar(&o.outputFormat, "output-format", "text", "stdout format: text | json (json = the run's result envelope with the design under reportDesign)")
}

func newReportDesignDesignCmd() *cobra.Command {
	var o designerOpts
	cmd := &cobra.Command{
		Use:   "design --brief TEXT [--document FILE] [--from NAME] [--logo FILE] [--image FILE]… [--url URL]… [--save]",
		Short: "Design a report design from a brief, with the built-in Report Designer",
		Long: "Run the Report Designer on this computer, against the model your runner policy names, to write a whole\n" +
			"report design from your brief: colours checked for contrast, faces, the title block and headings, a\n" +
			"header and footer, the brand block and a cover style. --from starts from one of your designs, a built-in\n" +
			"theme or a design file; --logo gives it your logo; --image gives it up to four reference images it looks\n" +
			"at and never keeps; --url gives it up to two web pages, such as your own site, read first by AI Review\n" +
			"with your own TypeSafe key. --document gives it one document of yours, a PDF or a Word document (.docx)\n" +
			"up to 20 MB, to build the design around: it looks at the first twelve pages of a PDF and the pictures\n" +
			"they draw, reads a Word document's theme, header, footer and pictures (no Word layout is rendered), and\n" +
			"can take your logo and your cover page's artwork from it, with a custom cover modelled on your cover\n" +
			"page; the document is checked like any input, read in full and kept only for the run, and no whole page\n" +
			"and none of its text is kept. The design is written to the output folder, and with --save added to your\n" +
			"account's designs under a new name. It is never your default: pick it for a run with --report-design, or\n" +
			"make it the default with `agentry report-design use`.",
		Args: cobra.NoArgs,
		RunE: designerRunE(&o, func(cmd *cobra.Command) error { return runReportDesigner(cmd, build.Design, &o) }),
	}
	designerRunFlags(cmd, &o)
	f := cmd.Flags()
	f.StringVar(&o.from, "from", "", "start from this design: one of your account's (NAME or ID), a built-in theme, or a design FILE")
	f.StringArrayVar(&o.documents, "document", nil, "a document FILE to build the design around: one PDF or Word document (.docx), up to 20 MB")
	f.StringVar(&o.logo, "logo", "", "your logo, a PNG or a JPEG FILE")
	f.StringArrayVar(&o.images, "image", nil, "a reference image FILE, PNG or JPEG, for its look (repeatable, at most 4)")
	f.StringArrayVar(&o.urls, "url", nil, "the https address of a page about you or your brand, such as your site (repeatable, at most 2)")
	return cmd
}

func newReportDesignCoverCmd() *cobra.Command {
	var o designerOpts
	cmd := &cobra.Command{
		Use:   "cover --brief TEXT --design NAME [--image FILE]… [--save]",
		Short: "Write a custom cover page for one of your designs, with the built-in Report Designer",
		Long: "Run the Report Designer on this computer, against the model your runner policy names, to write a custom\n" +
			"cover page for a design in Typst code. It renders the cover, looks at it and refines it, up to twelve\n" +
			"renders. --image gives it pictures the cover may draw (with the design's own, at most six). The result is\n" +
			"the design with that cover, written to the output folder, and with --save added to your account's designs\n" +
			"under a new name; the design you named is left as it is. A PDF export draws the cover as its first page,\n" +
			"and passes it over for the built-in band cover if it ever fails to render.",
		Args: cobra.NoArgs,
		RunE: designerRunE(&o, func(cmd *cobra.Command) error { return runReportDesigner(cmd, build.Cover, &o) }),
	}
	designerRunFlags(cmd, &o)
	f := cmd.Flags()
	f.StringVar(&o.from, "design", "", "the design the cover is for: one of your account's (NAME or ID), a built-in theme, or a design FILE (required)")
	f.StringArrayVar(&o.images, "image", nil, "an image FILE the cover may draw, PNG or JPEG (repeatable)")
	return cmd
}

// designerRunE wraps a Report Designer command as voiceRunE does: the
// output format is checked first, and in its json form a run that failed
// before the run wrote an envelope still writes one.
func designerRunE(o *designerOpts, work func(cmd *cobra.Command) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if o.outputFormat != "text" && o.outputFormat != "json" {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("unknown --output-format %q (want text or json)", o.outputFormat))
		}
		o.stage = receipt.StagePolicy
		o.onUsage = usageReporter(o.outputFormat, cmd.ErrOrStderr())
		err := work(cmd)
		var printed *envelopePrinted
		if err != nil && o.outputFormat == "json" && !errors.As(err, &printed) {
			printBuiltinFailure(cmd, localrun.ReportDesigner().Ref(), o.stage, err)
		}
		if errors.As(err, &printed) {
			return printed.err
		}
		return err
	}
}

// documentLine says what a Design run took from the person's document.
func documentLine(res *build.Result, name string) string {
	var took []string
	if res.LogoFromDocument {
		took = append(took, "its logo")
	}
	if res.CoverImageFromDocument {
		took = append(took, "a cover image")
	}
	switch {
	case res.CoverFromDocument && res.CoverPicturesFromDocument == 1:
		took = append(took, "a custom cover modelled on its cover page, drawing 1 picture of it")
	case res.CoverFromDocument && res.CoverPicturesFromDocument > 1:
		took = append(took, fmt.Sprintf("a custom cover modelled on its cover page, drawing %d pictures of it", res.CoverPicturesFromDocument))
	case res.CoverFromDocument:
		took = append(took, "a custom cover modelled on its cover page")
	}
	line := fmt.Sprintf("It looked at %d image(s) of %s", res.DocumentPages, name)
	switch len(took) {
	case 0:
		return line + " and took no picture of it."
	case 1:
		return line + " and took " + took[0] + " from it."
	}
	return line + " and took " + strings.Join(took[:len(took)-1], ", ") + " and " + took[len(took)-1] + " from it."
}

// designerReport is the json report of a Report Designer command.
type designerReport struct {
	Job       string `json:"job"`
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Cover     string `json:"cover"`
	Looked    bool   `json:"looked"`
	Thumbnail bool   `json:"thumbnail,omitempty"`
	Previews  int    `json:"previews"`
	// DocumentPages is how many images of --document the Designer looked
	// at; LogoFromDocument says its logo is a crop of the document,
	// CoverImageFromDocument that its cover image is a picture of it, and
	// CoverFromDocument that its custom cover is modelled on it, drawing
	// CoverPicturesFromDocument pictures of it.
	DocumentPages             int                `json:"documentPages,omitempty"`
	LogoFromDocument          bool               `json:"logoFromDocument,omitempty"`
	CoverImageFromDocument    bool               `json:"coverImageFromDocument,omitempty"`
	CoverFromDocument         bool               `json:"coverFromDocument,omitempty"`
	CoverPicturesFromDocument int                `json:"coverPicturesFromDocument,omitempty"`
	Dir                       string             `json:"dir"`
	Saved                     *savedDesign       `json:"saved,omitempty"`
	Pages                     []*voicebuild.Page `json:"pages,omitempty"`
}

// savedDesign is a design --save added to the library.
type savedDesign struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Registry string `json:"registry"`
}

// readImageFile reads an image a flag named, within its cap.
func readImageFile(flag, file string) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // a file the person named
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxDesignerImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", flag, file, err)
	}
	if len(b) > maxDesignerImageBytes {
		return nil, fmt.Errorf("%s %s is over %d MB", flag, file, maxDesignerImageBytes>>20)
	}
	return b, nil
}

// readDocumentFile reads --document within its cap: one regular file.
func readDocumentFile(file string) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // a file the person named
	if err != nil {
		return nil, fmt.Errorf("--document: %w", err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("--document %s is not a file", file)
	}
	b, err := io.ReadAll(io.LimitReader(f, build.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("--document %s: %w", file, err)
	}
	if len(b) > build.MaxDocumentBytes {
		return nil, fmt.Errorf("--document %s is over %d MB", file, build.MaxDocumentBytes>>20)
	}
	return b, nil
}

// designerFrom is the design a --from or --design value names: a design
// file, one of the account's designs when someone is signed in, or a
// built-in theme.
func designerFrom(ctx context.Context, cmd *cobra.Command, ref string) (*reportdesign.Design, error) {
	if designIsFile(ref) {
		doc, err := readDesignFile(ref)
		if err != nil {
			return nil, &exitError{code: 2, err: fmt.Errorf("%s: %w", ref, err)}
		}
		d, err := reportdesign.Parse(doc)
		if err != nil {
			return nil, &exitError{code: 2, err: fmt.Errorf("%s: %w", ref, err)}
		}
		return &d, nil
	}
	reg := registryURL()
	if token, err := credentialFor(reg); err == nil && token != "" {
		lib := &designLibrary{reg: reg, token: token}
		doc, err := lib.Named(ctx, ref)
		if err == nil {
			d, err := reportdesign.Parse(doc)
			if err != nil {
				return nil, fmt.Errorf("the registry answered a report design this CLI cannot read (%w); update agentry", err)
			}
			return &d, nil
		}
		if errors.Is(err, errNoLibrary) || isLibraryProblem(err) {
			return nil, err
		}
	}
	for _, id := range reportdesign.BuiltinIDs() {
		if d, ok := reportdesign.Builtin(id); ok && (strings.EqualFold(id, ref) || strings.EqualFold(d.Name, ref)) {
			return &d, nil
		}
	}
	return nil, &exitError{code: 2, err: fmt.Errorf("no report design named %q in your account's library or among the built-in themes (%s), and no such file", ref, strings.Join(reportdesign.BuiltinIDs(), ", "))}
}

// isLibraryProblem says whether err is the registry's failure rather than
// a name it does not hold.
func isLibraryProblem(err error) bool {
	var problem *exitError
	return errors.As(err, &problem)
}

func runReportDesigner(cmd *cobra.Command, job build.Job, o *designerOpts) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	if job == build.Cover && o.from == "" {
		return engine.Classify(engine.ExitUsage, errors.New("a cover is written for one of your designs: name it with --design"))
	}
	var from *reportdesign.Design
	if o.from != "" {
		var err error
		if from, err = designerFrom(ctx, cmd, o.from); err != nil {
			return err
		}
	}
	var logo []byte
	if o.logo != "" {
		var err error
		if logo, err = readImageFile("--logo", o.logo); err != nil {
			return engine.Classify(engine.ExitUsage, err)
		}
	}
	var doc *build.Document
	switch len(o.documents) {
	case 0:
	case 1:
		b, err := readDocumentFile(o.documents[0])
		if err != nil {
			return engine.Classify(engine.ExitUsage, err)
		}
		doc = &build.Document{Name: filepath.Base(o.documents[0]), Data: b}
		if _, err := build.DocumentKind(doc.Name, doc.Data); err != nil {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--document %s: %w", o.documents[0], err))
		}
	default:
		return engine.Classify(engine.ExitUsage, errors.New("a design is built around one document: give --document once"))
	}
	var images [][]byte
	for _, file := range o.images {
		b, err := readImageFile("--image", file)
		if err != nil {
			return engine.Classify(engine.ExitUsage, err)
		}
		images = append(images, b)
	}
	if len(o.urls) > build.MaxURLs {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a design takes at most %d web addresses", build.MaxURLs))
	}
	urls, err := voicebuild.ParseURLs(o.urls)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	// --save needs a library: asked before the run, so a run is never
	// spent on a design that has nowhere to go.
	var lib *designLibrary
	var reg string
	if o.save {
		if lib, reg, err = signedInLibrary(cmd, "or leave out --save to keep the design as a file"); err != nil {
			return err
		}
	}

	home, err := runnerHome()
	if err != nil {
		return err
	}
	policyFile, err := policy.Resolve(o.policyPath)
	if err != nil {
		return err
	}
	pol, polSHA, err := policy.EnsureDefault(policyFile.Path)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	o.stage = receipt.StagePrepare
	model := strings.TrimSpace(pol.Model.Provider + " " + pol.Model.Name)
	_, _ = fmt.Fprintf(errOut, "The Report Designer runs on this machine against the model your runner policy names (%s).\n"+
		"The model's use costs no credits. It looks at its renders and your references when the model reads images.\n", model)
	pay, endOps := creditsPay(errOut)
	defer endOps()
	review, web, err := voicePageReview(cmd.Context(), pol, urls, errOut, managed.PurposeRun, pay)
	if err != nil {
		return err
	}
	runID, err := engine.NewRunID()
	if err != nil {
		return err
	}
	outDir := o.out
	if outDir == "" {
		outDir = filepath.Join("agentry-out", "report-designer")
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	budgets := o.over(localrun.ReportDesigner().LocalBudgets(pol.Budgets))
	res, err := designerRun(ctx, knowledgeDocker(), build.Options{
		Job: job, Brief: o.brief, From: from, Logo: logo, Images: images, URLs: urls, Document: doc, Review: review, Web: web,
		Home: home, Policy: pol, PolicySHA: polSHA, PolicyFile: &policyFile, Build: version.Build(),
		OutDir: outDir, Budgets: &budgets, RunID: runID, Stderr: errOut, OnUsage: o.onUsage, Credits: creditsClient(pol),
	})
	report := designerReport{Job: string(job)}
	if res != nil {
		report.Pages = res.Pages
		for _, p := range res.Pages {
			if p.Withheld != "" {
				_, _ = fmt.Fprintf(errOut, "withheld: %s (%s)\n", p.URL, p.Withheld)
			}
		}
	}
	if err != nil {
		if o.outputFormat == "json" && res != nil && res.Envelope != nil {
			printBuiltinEnvelope(ctx, cmd.OutOrStdout(), *res.Envelope, "reportDesign", report, err)
			return &envelopePrinted{err: err}
		}
		return err
	}
	d := res.Design
	report.Name, report.SHA256, report.Cover, report.Looked, report.Thumbnail, report.Previews, report.Dir = d.Name, d.SHA256(), d.Cover.Style, res.Looked, res.Thumbnail, res.Previews, res.Dir
	report.DocumentPages, report.LogoFromDocument = res.DocumentPages, res.LogoFromDocument
	report.CoverImageFromDocument, report.CoverFromDocument, report.CoverPicturesFromDocument = res.CoverImageFromDocument, res.CoverFromDocument, res.CoverPicturesFromDocument
	if o.save {
		saved, err := saveDesignerResult(ctx, lib, reg, d)
		if err != nil {
			err = fmt.Errorf("the design was written to %s but not saved to your account: %w", res.Dir, err)
			if o.outputFormat == "json" && res.Envelope != nil {
				printBuiltinEnvelope(ctx, cmd.OutOrStdout(), *res.Envelope, "reportDesign", report, err)
				return &envelopePrinted{err: err}
			}
			return err
		}
		report.Saved = saved
	}
	if o.outputFormat == "json" {
		printBuiltinEnvelope(ctx, cmd.OutOrStdout(), *res.Envelope, "reportDesign", report, nil)
		return nil
	}
	out := cmd.OutOrStdout()
	if !res.Looked {
		_, _ = fmt.Fprintln(out, "The Report Designer says it could not look at its renders or your references, so it designed by the checks alone: use a model that reads images to have it look.")
	}
	what := "design"
	if job == build.Cover {
		what = "design with its custom cover"
	}
	_, _ = fmt.Fprintf(out, "The Report Designer made the %s %q (sha256 %s, %d renders), written to %s.\n", what, d.Name, report.SHA256, res.Previews, res.Dir)
	if doc != nil {
		_, _ = fmt.Fprintf(out, "%s\n", documentLine(res, doc.Name))
	}
	if job == build.Cover && !res.Thumbnail {
		_, _ = fmt.Fprintln(out, "Its last render was not of the cover it kept, so the design has no thumbnail of it.")
	}
	if report.Saved != nil {
		_, _ = fmt.Fprintf(out, "Added it to your account's report designs at %s as %q (%s). It is not your default: a run takes it with --report-design %s, or make it the default with `agentry report-design use`.\n",
			reg, report.Saved.Name, report.Saved.ID, quoteIfSpaced(report.Saved.Name))
	} else {
		_, _ = fmt.Fprintf(out, "Add it to your account with `agentry report-design import %s`, or run again with --save.\n", quoteIfSpaced(filepath.Join(res.Dir, "design.json")))
	}
	return nil
}

// saveDesignerResult adds the design to the library as a new design: under
// its own name, or that name with a number when the library has one of
// that name, and never as the default.
func saveDesignerResult(ctx context.Context, lib *designLibrary, reg string, d reportdesign.Design) (*savedDesign, error) {
	list, err := lib.list(ctx)
	if err != nil {
		return nil, err
	}
	if len(list.Designs) >= reportdesign.MaxLibrary {
		return nil, &exitError{code: 2, err: fmt.Errorf("your account's library at %s holds %d report designs, the most it keeps; delete one, then `agentry report-design import` the file", reg, len(list.Designs))}
	}
	d.Name = freeName(list, d.Name)
	if err := d.Validate(); err != nil {
		return nil, err
	}
	rec, err := lib.create(ctx, d.Canonical(), false)
	if err != nil {
		return nil, err
	}
	if rec.Default {
		// The registry never makes a design the default unasked; a
		// record that says so is not one this CLI keeps quiet about.
		return nil, fmt.Errorf("the registry saved %s (%s) as your default, which was not asked; pick your default with `agentry report-design use`", d.Name, rec.ID)
	}
	return &savedDesign{ID: rec.ID, Name: d.Name, Registry: reg}, nil
}

// freeName is name, or name with the first number from 2 that no design of
// the library has, within the name's cap.
func freeName(list libraryList, name string) string {
	taken := func(n string) bool {
		for _, d := range list.Designs {
			if strings.EqualFold(d.Name, n) || strings.EqualFold(d.ID, n) {
				return true
			}
		}
		return false
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		base := []rune(name)
		if room := reportdesign.MaxNameRunes - utf8.RuneCountInString(suffix); len(base) > room {
			base = base[:room]
		}
		if n := strings.TrimSpace(string(base)) + suffix; !taken(n) {
			return n
		}
	}
}
