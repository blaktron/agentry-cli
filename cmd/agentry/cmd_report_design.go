package main

// agentry report-design: the person's report design library from the
// command line, so a script or a person without the site or the desktop
// can see which design their PDFs take, keep one as a file, put one on
// their account, pick the default and tidy the library. show asks exactly
// what a run asks (chooseReportTheme), so what it prints is what the next
// run draws. Every document this command writes or sends is the design's
// canonical form (pkg/reportdesign), so export then import is byte for
// byte. The images travel in the design: export writes them, import sends
// them, and show says which there are.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

func newReportDesignCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report-design",
		Short: "List, show, pick, export, import, delete or design the report designs your PDFs take",
		Long: "A report design is how a run's PDFs look: the paper, three faces, an accent\n" +
			"and a secondary colour, a brand block, the title block, the headings, the\n" +
			"tables, the quotes, a running header, a footer of three slots and a cover\n" +
			"page. It is yours, never the agent's. Your account keeps a library of up to\n" +
			"ten named designs with one of them the default. A run takes the first that\n" +
			"is set: --report-design NAME|ID|FILE or --report-theme NAME, your account's\n" +
			"default design when you are signed in, report.design or report.theme in\n" +
			"the config, then Plain.\n" +
			"The built-in themes are " + strings.Join(toolsrv.BuiltinThemes(), ", ") + ".\n" +
			"`design` and `cover` run the built-in Report Designer, which writes a new design from a brief or a\n" +
			"custom cover page for one of yours.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newReportDesignListCmd(), newReportDesignShowCmd(), newReportDesignUseCmd(), newReportDesignDeleteCmd(),
		newReportDesignExportCmd(), newReportDesignImportCmd(), newReportDesignDesignCmd(), newReportDesignCoverCmd())
	return cmd
}

// signedInLibrary is the account's library at the registry, or a usage
// error when no one is signed in there; it gates on the registry's
// minimum version first.
func signedInLibrary(cmd *cobra.Command, orElse string) (*designLibrary, string, error) {
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil {
		return nil, reg, err
	}
	if token == "" {
		msg := fmt.Sprintf("not signed in at %s: run `agentry login`", reg)
		if orElse != "" {
			msg += ", or " + orElse
		}
		return nil, reg, &exitError{code: 2, err: errors.New(msg)}
	}
	if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), reg, token); err != nil {
		return nil, reg, err
	}
	return &designLibrary{reg: reg, token: token}, reg, nil
}

// libraryNamed is the library's design a name or an id names, as a usage
// error when there is none.
func libraryNamed(ctx context.Context, lib *designLibrary, ref string) (librarySummary, error) {
	d, err := lib.find(ctx, ref)
	if err != nil {
		if errors.Is(err, errNoLibrary) {
			return librarySummary{}, err
		}
		var problem *exitError
		if errors.As(err, &problem) {
			return librarySummary{}, err
		}
		return librarySummary{}, &exitError{code: 2, err: err}
	}
	return d, nil
}

func newReportDesignListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the report designs of your account's library",
		Long: "Lists your account's report designs at the registry, oldest first, with\n" +
			"the built-in each started from, its revision, which is the default, and\n" +
			"which images each carries. --json prints the library.",
		Args: cobra.NoArgs,
		RunE: runReportDesignList,
	}
}

func runReportDesignList(cmd *cobra.Command, _ []string) error {
	lib, reg, err := signedInLibrary(cmd, "")
	if err != nil {
		return err
	}
	list, err := lib.list(cmd.Context())
	if err != nil {
		return err
	}
	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"registry": reg, "default": list.Default, "designs": list.Designs})
	}
	out := cmd.OutOrStdout()
	if len(list.Designs) == 0 {
		_, _ = fmt.Fprintf(out, "Your account at %s has no report design; your runs take report.design or report.theme in the config, or Plain. Add one with `agentry report-design import FILE`.\n", reg)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tBASED ON\tVERSION\tDEFAULT\tIMAGES\tUPDATED\tID")
	for _, d := range list.Designs {
		dflt := ""
		if d.Default {
			dflt = "default"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", d.Name, d.BasedOn, d.Version, dflt, imagesLabel(d.HasLogo, d.HasCoverImage, d.HasCustomCover), d.UpdatedAt.UTC().Format("2006-01-02"), d.ID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if list.Default == "" {
		_, _ = fmt.Fprintf(out, "\nNone is the default: your runs take report.design or report.theme in the config, or Plain. Pick one with `agentry report-design use NAME`.\n")
	}
	return nil
}

// imagesLabel says which images a design carries, in a few words.
func imagesLabel(logo, coverImage, customCover bool) string {
	var s []string
	if logo {
		s = append(s, "logo")
	}
	if coverImage {
		s = append(s, "cover image")
	}
	if customCover {
		s = append(s, "custom cover")
	}
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func newReportDesignShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [NAME|ID]",
		Short: "Print the report design a run would take now, or one of your library",
		Long: "Without a name, prints the design the next run's PDFs would take, chosen as\n" +
			"a run chooses it without flags: your account's default design at the\n" +
			"registry when you are signed in, then report.design or report.theme in the\n" +
			"config, then Plain. With a name or an id, prints that design of your\n" +
			"account's library. --json prints the design's document beside what chose it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runReportDesignShowNamed(cmd, args[0])
			}
			return runReportDesignShow(cmd)
		},
	}
}

func runReportDesignShow(cmd *cobra.Command) error {
	reg := registryURL()
	cfg, err := loadCLIConfig()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; its report settings are not read\n", err)
	}
	// The account's answer is kept so the reason can tell an account that
	// has no default from one that could not be read or holds a design
	// this CLI refuses; the chooser warns on both and takes the next
	// choice.
	acct := accountNotAsked
	if os.Getenv(envRunGrant) != "" {
		acct = accountDriver
	}
	var account accountDesigns
	if read := readAccountTheme(reg); read != nil {
		account = &watchedAccount{accountDesigns: read, state: &acct}
	}
	choice, err := chooseReportTheme(cmd.Context(), reportFlags{}, cfg, account, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	d, err := designOfChoice(choice)
	if err != nil {
		return err
	}
	return printDesign(cmd, reg, d, string(choice.From), whyChosen(choice, reg, acct))
}

// watchedAccount notes what the account answered for the default.
type watchedAccount struct {
	accountDesigns
	state *accountState
}

func (w *watchedAccount) Default(ctx context.Context) ([]byte, error) {
	doc, err := w.accountDesigns.Default(ctx)
	switch {
	case err != nil:
		*w.state = accountUnread
	case doc == nil:
		*w.state = accountNone
	default:
		*w.state = accountHasDesign
	}
	return doc, err
}

func runReportDesignShowNamed(cmd *cobra.Command, ref string) error {
	lib, reg, err := signedInLibrary(cmd, "")
	if err != nil {
		return err
	}
	sum, err := libraryNamed(cmd.Context(), lib, ref)
	if err != nil {
		return err
	}
	rec, err := lib.get(cmd.Context(), sum.ID, true)
	if err != nil {
		return err
	}
	doc, err := designOfRecord(rec)
	if err != nil {
		return err
	}
	d, err := reportdesign.Parse(doc)
	if err != nil {
		return fmt.Errorf("your account's report design %q is not one this CLI can read (%w); update agentry", sum.Name, err)
	}
	why := fmt.Sprintf("the design %q (%s) of your account's library at %s", sum.Name, sum.ID, reg)
	if sum.Default {
		why += ", the default your runs take"
	} else {
		why += ", which a run takes with --report-design " + quoteIfSpaced(sum.Name)
	}
	return printDesign(cmd, reg, d, "the library", why)
}

// quoteIfSpaced quotes a name for a shell when it has a space.
func quoteIfSpaced(name string) string {
	if strings.ContainsAny(name, " \t\"'") {
		return fmt.Sprintf("%q", name)
	}
	return name
}

// printDesign prints a design as show does.
func printDesign(cmd *cobra.Command, reg string, d reportdesign.Design, from, why string) error {
	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(map[string]any{
			"registry":    reg,
			"from":        from,
			"why":         why,
			"sha256":      d.SHA256(),
			"logo":        d.Logo != (reportdesign.Logo{}),
			"coverImage":  d.CoverImage != (reportdesign.Image{}),
			"customCover": !d.CustomCover.IsZero(),
			"design":      json.RawMessage(d.Canonical()),
		})
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "design:    %s (based on %s, version %d)\n", d.Name, d.BasedOn, d.Version)
	_, _ = fmt.Fprintf(out, "chosen:    %s\n", why)
	_, _ = fmt.Fprintf(out, "sha256:    %s\n", d.SHA256())
	_, _ = fmt.Fprintf(out, "paper:     %s, %s margins\n", paperLabel(d.Paper), d.Margins)
	_, _ = fmt.Fprintf(out, "fonts:     %s (headings), %s (body), %s (code)\n", d.Fonts.Headings, d.Fonts.Body, d.Fonts.Code)
	colours := d.Accent + " (accent)"
	if d.Secondary != "" {
		colours += ", " + d.Secondary + " (secondary)"
	}
	_, _ = fmt.Fprintf(out, "colours:   %s\n", colours)
	_, _ = fmt.Fprintf(out, "brand:     %s\n", brandLabel(d.Brand))
	_, _ = fmt.Fprintf(out, "title:     %s\n", d.TitleBlock)
	_, _ = fmt.Fprintf(out, "headings:  %s, %s scale\n", d.Headings, d.HeadingScale)
	_, _ = fmt.Fprintf(out, "tables:    %s\n", tablesLabel(d.Tables))
	_, _ = fmt.Fprintf(out, "quotes:    %s\n", d.Quotes)
	_, _ = fmt.Fprintf(out, "header:    %s\n", slotsLabel(d.Header.Style, d.Header.Slots))
	_, _ = fmt.Fprintf(out, "footer:    %s\n", slotsLabel("page numbers "+d.Footer.PageNumbers, d.Footer.Slots))
	_, _ = fmt.Fprintf(out, "cover:     %s\n", coverLabel(d.Cover))
	_, _ = fmt.Fprintf(out, "logo:      %s\n", logoLabel(d.Logo))
	_, _ = fmt.Fprintf(out, "images:    %s\n", assetsLabel(d))
	return nil
}

// brandLabel says what the brand block holds, in the words show prints.
func brandLabel(b reportdesign.Brand) string {
	var s []string
	if b.Name != "" {
		s = append(s, b.Name)
	}
	if b.Strapline != "" {
		s = append(s, b.Strapline)
	}
	s = append(s, b.Contact...)
	if b.Website != "" {
		s = append(s, b.Website)
	}
	if b.Confidentiality != "" {
		s = append(s, fmt.Sprintf("confidentiality %q", b.Confidentiality))
	}
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, " · ")
}

// slotsLabel says a header's or a footer's style and its slots.
func slotsLabel(style string, slots [reportdesign.SlotCount]string) string {
	label := style
	var filled []string
	for _, s := range slots {
		if s != "" {
			filled = append(filled, fmt.Sprintf("%q", s))
		}
	}
	if len(filled) > 0 {
		label += ", slots " + strings.Join(filled, " ")
	}
	return label
}

// coverLabel says the cover's style, when it is drawn and what it shows.
func coverLabel(c reportdesign.Cover) string {
	when := map[string]string{"long": "for a long report", "always": "for every titled report", "never": "never"}[c.When]
	return fmt.Sprintf("%s, %s, showing %s", c.Style, when, strings.Join(c.Shows, ", "))
}

// assetsLabel says which images a design carries besides the logo.
func assetsLabel(d reportdesign.Design) string {
	var s []string
	if d.CoverImage != (reportdesign.Image{}) {
		if data, format, err := d.CoverImage.Decode(); err == nil {
			s = append(s, fmt.Sprintf("a cover image (%s, %d KB)", strings.ToUpper(format), (len(data)+1023)/1024))
		} else {
			s = append(s, "a cover image (unreadable)")
		}
	}
	if !d.CustomCover.IsZero() {
		s = append(s, fmt.Sprintf("a custom cover (%d bytes of Typst, %d images)", len(d.CustomCover.Source), len(d.CustomCover.Images)))
	}
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// logoLabel says what a design's logo is, in the words show prints.
func logoLabel(l reportdesign.Logo) string {
	if l == (reportdesign.Logo{}) {
		return "none"
	}
	b, err := l.Decode()
	if err != nil {
		return "unreadable"
	}
	cfg, err := pngConfig(b)
	if err != nil {
		return "unreadable"
	}
	return fmt.Sprintf("a PNG of %d×%d pixels, %d KB, drawn in the title block, on a cover that shows it and in a logoLeft header", cfg.Width, cfg.Height, (len(b)+1023)/1024)
}

// designOfChoice is the design a choice names: its document, or the
// built-in theme it names.
func designOfChoice(choice engine.ReportTheme) (reportdesign.Design, error) {
	if choice.Design != nil {
		return reportdesign.Parse(choice.Design)
	}
	d, ok := reportdesign.Builtin(choice.Name)
	if !ok {
		return reportdesign.Design{}, fmt.Errorf("%q is not a report theme", choice.Name)
	}
	return d, nil
}

// accountState is what show learned of the account's default design.
type accountState int

const (
	accountNotAsked  accountState = iota // not signed in at the registry
	accountDriver                        // a driver's run, which asks no account
	accountNone                          // signed in, no default
	accountUnread                        // signed in, the read failed
	accountHasDesign                     // signed in, a default (taken, or refused by this CLI)
)

// whyChosen says what chose a design, in the words show prints.
func whyChosen(choice engine.ReportTheme, reg string, acct accountState) string {
	if choice.From == engine.ThemeFromAccount {
		return "your account's default report design at " + reg
	}
	var why string
	switch choice.From {
	case engine.ThemeFromConfig, engine.ThemeFromDesignConfig:
		why = string(choice.From)
		if path, err := configPath(); err == nil {
			why += " (" + path + ")"
		}
	case engine.ThemeFromDefault:
		why = "the default, as the config sets none"
	default:
		return string(choice.From)
	}
	switch acct {
	case accountNotAsked:
		why += "; you are not signed in at " + reg
	case accountDriver:
		why += "; a driver's run asks no account"
	case accountNone:
		why += "; your account at " + reg + " has no default report design"
	case accountUnread:
		why += "; your account's report design at " + reg + " could not be read"
	case accountHasDesign:
		why += "; your account's default report design at " + reg + " is not one this CLI can draw"
	}
	return why
}

func paperLabel(p string) string {
	if p == "letter" {
		return "US Letter"
	}
	return strings.ToUpper(p)
}

func tablesLabel(t reportdesign.Tables) string {
	var s []string
	if t.Tinted {
		s = append(s, "tinted header")
	}
	if t.Banded {
		s = append(s, "banded rows")
	}
	if len(s) == 0 {
		return "plain"
	}
	return strings.Join(s, ", ")
}

func newReportDesignUseCmd() *cobra.Command {
	var none bool
	cmd := &cobra.Command{
		Use:   "use NAME|ID",
		Short: "Make one of your library's report designs the default your runs take",
		Long: "Makes the design a name or an id names the default: the design your runs'\n" +
			"PDFs take when you pick none, on every computer you are signed in on.\n" +
			"--none makes no design the default, so your runs take report.design or\n" +
			"report.theme in the config, or Plain.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case none && len(args) == 1:
				return &exitError{code: 2, err: errors.New("use takes a name or --none, not both")}
			case !none && len(args) == 0:
				return &exitError{code: 2, err: errors.New("use takes the name or the id of a design of your library, or --none")}
			}
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runReportDesignUse(cmd, ref)
		},
	}
	cmd.Flags().BoolVar(&none, "none", false, "make no design the default")
	return cmd
}

func runReportDesignUse(cmd *cobra.Command, ref string) error {
	lib, reg, err := signedInLibrary(cmd, "")
	if err != nil {
		return err
	}
	if ref == "" {
		if _, err := lib.setDefault(cmd.Context(), ""); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No design of your account at %s is the default now. Your runs' PDFs take %s.\n", reg, configuredDesign(cmd))
		return nil
	}
	sum, err := libraryNamed(cmd.Context(), lib, ref)
	if err != nil {
		return err
	}
	if _, err := lib.setDefault(cmd.Context(), sum.ID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s (%s) is your account's default report design at %s. Your runs' PDFs take it from the next run.\n", sum.Name, sum.ID, reg)
	return nil
}

// configuredDesign names what runs take when the account has no default,
// in the words use and delete print.
func configuredDesign(cmd *cobra.Command) string {
	now := "the configured design or Plain"
	if cfg, err := loadCLIConfig(); err == nil {
		if choice, err := chooseReportTheme(cmd.Context(), reportFlags{}, cfg, nil, io.Discard); err == nil {
			if d, err := designOfChoice(choice); err == nil {
				now = d.Name + " (" + string(choice.From) + ")"
			}
		}
	}
	return now
}

func newReportDesignDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME|ID",
		Short: "Remove a report design, its images included, from your library",
		Long: "Removes the design a name or an id names from your account's library at the\n" +
			"registry, with its logo, cover image and custom cover. Removing the\n" +
			"default leaves your account with none.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReportDesignDelete(cmd, args[0])
		},
	}
}

func runReportDesignDelete(cmd *cobra.Command, ref string) error {
	lib, reg, err := signedInLibrary(cmd, "")
	if err != nil {
		return err
	}
	sum, err := libraryNamed(cmd.Context(), lib, ref)
	if err != nil {
		return err
	}
	if err := lib.remove(cmd.Context(), sum.ID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s (%s) from your account's library at %s.\n", sum.Name, sum.ID, reg)
	if sum.Default {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "It was the default: your runs' PDFs now take %s. Pick another with `agentry report-design use NAME`.\n", configuredDesign(cmd))
	}
	return nil
}

func newReportDesignExportCmd() *cobra.Command {
	var theme string
	var force bool
	cmd := &cobra.Command{
		Use:   "export [NAME|ID] FILE",
		Short: "Write a report design of your library, or a built-in theme, to a file",
		Long: "Writes your account's default report design to FILE as its canonical\n" +
			"document, images included; with a name or an id before FILE, that design of\n" +
			"your library; with --theme NAME, a built-in theme's. FILE - writes to\n" +
			"stdout. An existing file is kept unless --force. import reads the file\n" +
			"back unchanged.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, file := "", args[0]
			if len(args) == 2 {
				ref, file = args[0], args[1]
			}
			return runReportDesignExport(cmd, ref, file, theme, force)
		},
	}
	cmd.Flags().StringVar(&theme, "theme", "", "write this built-in theme instead of a design of your library ("+strings.Join(toolsrv.BuiltinThemes(), ", ")+")")
	cmd.Flags().BoolVar(&force, "force", false, "replace FILE when it exists")
	return cmd
}

func runReportDesignExport(cmd *cobra.Command, ref, file, theme string, force bool) error {
	var d reportdesign.Design
	var from string
	switch {
	case theme != "" && ref != "":
		return &exitError{code: 2, err: errors.New("export takes a design's name or --theme, not both")}
	case theme != "":
		b, ok := reportdesign.Builtin(theme)
		if !ok {
			return &exitError{code: 2, err: fmt.Errorf("--theme %q is not a report theme (they are: %s)", theme, strings.Join(toolsrv.BuiltinThemes(), ", "))}
		}
		d, from = b, "the built-in theme "+theme
	default:
		lib, reg, err := signedInLibrary(cmd, "export a built-in theme with --theme NAME")
		if err != nil {
			return err
		}
		var rec libraryRecord
		if ref == "" {
			list, err := lib.list(cmd.Context())
			if err != nil {
				return err
			}
			if list.Default == "" {
				return &exitError{code: 2, err: fmt.Errorf("your account at %s has no default report design; name one of your library (`agentry report-design list`), or export a built-in theme with --theme NAME", reg)}
			}
			if rec, err = lib.get(cmd.Context(), list.Default, true); err != nil {
				return err
			}
			from = "your account's default report design at " + reg
		} else {
			sum, err := libraryNamed(cmd.Context(), lib, ref)
			if err != nil {
				return err
			}
			if rec, err = lib.get(cmd.Context(), sum.ID, true); err != nil {
				return err
			}
			from = fmt.Sprintf("the design %q of your account's library at %s", sum.Name, reg)
		}
		doc, err := designOfRecord(rec)
		if err != nil {
			return err
		}
		if d, err = reportdesign.Parse(doc); err != nil {
			return fmt.Errorf("your account's report design is not one this CLI can read (%w); update agentry", err)
		}
	}
	doc := d.Canonical()
	if file == "-" {
		_, err := cmd.OutOrStdout().Write(doc)
		return err
	}
	mode := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		mode = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(file, mode, 0o644) //nolint:gosec // a file the person named
	if errors.Is(err, fs.ErrExist) {
		return &exitError{code: 2, err: fmt.Errorf("%s exists; --force replaces it", file)}
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(doc); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	what := d.Name
	if images := imagesLabel(d.Logo != (reportdesign.Logo{}), d.CoverImage != (reportdesign.Image{}), !d.CustomCover.IsZero()); images != "-" {
		what += " with its " + images
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s, %s (sha256 %s), to %s.\n", what, from, d.SHA256(), file)
	return nil
}

func newReportDesignImportCmd() *cobra.Command {
	var name string
	var dflt, replace bool
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Check a report design file and add it to your account's library",
		Long: "Reads a report design document from FILE (- reads stdin), holds it to every\n" +
			"rule of the design (a refusal names the field), and adds it to your\n" +
			"account's library at the registry under the name the document gives\n" +
			"itself, or --name. A design of that name already in the library is kept\n" +
			"unless --replace, which puts the file in its place and keeps its id.\n" +
			"--default makes the design the one your runs take.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReportDesignImport(cmd, args[0], name, dflt, replace)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "save the design under this name instead of the document's")
	cmd.Flags().BoolVar(&dflt, "default", false, "make it your account's default")
	cmd.Flags().BoolVar(&replace, "replace", false, "put the file in the place of the design of the same name")
	return cmd
}

func runReportDesignImport(cmd *cobra.Command, file, name string, dflt, replace bool) error {
	in := cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file) //nolint:gosec // a file the person named
		if err != nil {
			return &exitError{code: 2, err: err}
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	d, _, err := readDesign(in)
	if err == nil && name != "" {
		d.Name = name
		err = d.Validate()
	}
	if err == nil {
		// The core stores every image as its own re-encoding, so the
		// design is sent that way and its hash is the one saved.
		d, err = normalizeDesignImages(d)
	}
	if err != nil {
		// Refused here, before any sign-in is needed; the error names the
		// field (pkg/reportdesign's FieldError).
		return &exitError{code: 2, err: fmt.Errorf("%s: %w", file, err)}
	}
	lib, reg, err := signedInLibrary(cmd, "")
	if err != nil {
		return err
	}
	list, err := lib.list(cmd.Context())
	if err != nil {
		return err
	}
	existing, found := findDesign(list, d.Name)
	if found == nil && !strings.EqualFold(existing.ID, d.Name) && !replace {
		return &exitError{code: 2, err: fmt.Errorf("your account's library already has a report design named %q (%s); --replace puts the file in its place, --name NAME saves it under another name", d.Name, existing.ID)}
	}
	var rec libraryRecord
	var verb string
	switch {
	case found == nil && replace:
		if rec, err = lib.update(cmd.Context(), existing.ID, d.Canonical()); err != nil {
			return err
		}
		verb = "Replaced"
		if dflt && !rec.Default {
			if _, err := lib.setDefault(cmd.Context(), rec.ID); err != nil {
				return err
			}
			rec.Default = true
		}
	default:
		if len(list.Designs) >= reportdesign.MaxLibrary {
			return &exitError{code: 2, err: fmt.Errorf("your account's library at %s holds %d report designs, the most it keeps; delete one, or --replace one of the same name", reg, len(list.Designs))}
		}
		if rec, err = lib.create(cmd.Context(), d.Canonical(), dflt); err != nil {
			return err
		}
		verb = "Added"
	}
	doc, err := designOfRecord(rec)
	if err != nil {
		return err
	}
	sd, err := reportdesign.Parse(doc)
	if err != nil {
		return fmt.Errorf("the registry saved a report design this CLI cannot read (%w); update agentry", err)
	}
	// A replaced design sent without images keeps the ones the library
	// holds, so the file is compared with the saved design less those, and
	// the person is told they stayed.
	kept := keptImages(d, sd)
	compare := sd
	if d.Logo == (reportdesign.Logo{}) {
		compare.Logo = reportdesign.Logo{}
	}
	if d.CoverImage == (reportdesign.Image{}) {
		compare.CoverImage = reportdesign.Image{}
	}
	if d.CustomCover.IsZero() {
		compare.CustomCover = reportdesign.CustomCover{}
	}
	if compare.SHA256() != d.SHA256() {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: the registry saved the design as sha256 %s, not the file's %s\n", compare.SHA256(), d.SHA256())
	}
	what := "in"
	if rec.Default {
		what = "as the default of"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s, sha256 %s) %s your account's library at %s.", verb, sd.Name, rec.ID, sd.SHA256(), what, reg)
	if rec.Default {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), " Your runs' PDFs take it from the next run.")
	} else {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), " A run takes it with --report-design "+quoteIfSpaced(sd.Name)+", or make it the default with `agentry report-design use`.")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	if kept != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The file has no %s, so the design keeps the one it has; remove it on the Report design page of your settings.\n", kept)
	}
	return nil
}

// keptImages names the images a saved design kept that the file did not
// carry.
func keptImages(file, saved reportdesign.Design) string {
	var s []string
	if file.Logo == (reportdesign.Logo{}) && saved.Logo != (reportdesign.Logo{}) {
		s = append(s, "logo")
	}
	if file.CoverImage == (reportdesign.Image{}) && saved.CoverImage != (reportdesign.Image{}) {
		s = append(s, "cover image")
	}
	if file.CustomCover.IsZero() && !saved.CustomCover.IsZero() {
		s = append(s, "custom cover")
	}
	return strings.Join(s, ", ")
}

// normalizeDesignImages is a design with each image as the core would
// store it: decoded and encoded again by pkg/reportdesign's own
// normalizers.
func normalizeDesignImages(d reportdesign.Design) (reportdesign.Design, error) {
	var err error
	if d.Logo != (reportdesign.Logo{}) {
		raw, derr := d.Logo.Decode()
		if derr != nil {
			return d, &reportdesign.FieldError{Field: "logo.png", Message: "must be a PNG in standard base64"}
		}
		if d.Logo, err = reportdesign.NormalizeLogo(raw); err != nil {
			return d, err
		}
	}
	if d.CoverImage != (reportdesign.Image{}) {
		raw, _, derr := d.CoverImage.Decode()
		if derr != nil {
			return d, &reportdesign.FieldError{Field: "coverImage", Message: "must be a PNG or a JPEG in standard base64"}
		}
		if d.CoverImage, err = reportdesign.NormalizeCoverImage(raw); err != nil {
			return d, err
		}
	}
	if !d.CustomCover.IsZero() {
		for i, img := range d.CustomCover.Images {
			raw, _, derr := img.Decode()
			if derr != nil {
				return d, &reportdesign.FieldError{Field: fmt.Sprintf("customCover.images.%d", i), Message: "must be a PNG or a JPEG in standard base64"}
			}
			if d.CustomCover.Images[i], err = reportdesign.NormalizeCustomCoverImage(raw); err != nil {
				return d, err
			}
		}
		if d.CustomCover.Thumbnail != (reportdesign.Image{}) {
			raw, _, derr := d.CustomCover.Thumbnail.Decode()
			if derr != nil {
				return d, &reportdesign.FieldError{Field: "customCover.thumbnail", Message: "must be a PNG in standard base64"}
			}
			if d.CustomCover.Thumbnail, err = reportdesign.NormalizeThumbnail(raw); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}

// pngConfig reads a PNG's size.
func pngConfig(b []byte) (image.Config, error) {
	return png.DecodeConfig(bytes.NewReader(b))
}
