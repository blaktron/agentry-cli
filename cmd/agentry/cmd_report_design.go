package main

// agentry report-design (plan report-designs.md D19, agentry-cli#785): the
// person's report design from the command line, so a script or a person
// without the site or the desktop can see which design their PDFs take,
// keep one as a file, and put one on their account. show asks exactly what a
// run asks (chooseReportTheme), so what it prints is what the next run
// draws. Every document this command writes or sends is the design's
// canonical form (pkg/reportdesign), so export then import is byte for byte.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

func newReportDesignCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report-design",
		Short: "Show, export, import or reset the report design your PDFs take",
		Long: "A report design is how a run's PDFs look: the paper, three faces, one\n" +
			"accent, the title block, the headings, the tables, the quotes and the\n" +
			"footer. It is yours, never the agent's. A run takes the first that is set:\n" +
			"--report-design FILE or --report-theme NAME, your account's design when you\n" +
			"are signed in, report.design or report.theme in the config, then Plain.\n" +
			"The built-in themes are " + strings.Join(toolsrv.BuiltinThemes(), ", ") + ".",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newReportDesignShowCmd(), newReportDesignExportCmd(), newReportDesignImportCmd(), newReportDesignResetCmd())
	return cmd
}

func newReportDesignShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the report design a run would take now, and what chose it",
		Long: "Prints the design the next run's PDFs would take, chosen as a run chooses\n" +
			"it without flags: your account's design at the registry when you are\n" +
			"signed in, then report.design or report.theme in the config, then Plain.\n" +
			"--json prints the design's document beside what chose it.",
		Args: cobra.NoArgs,
		RunE: runReportDesignShow,
	}
}

func runReportDesignShow(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	cfg, err := loadCLIConfig()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v; its report settings are not read\n", err)
	}
	account := readAccountTheme(reg)
	choice, err := chooseReportTheme(cmd.Context(), reportFlags{}, cfg, account, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	d, err := designOfChoice(choice)
	if err != nil {
		return err
	}
	why := whyChosen(choice, reg, account != nil)
	if flags.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(map[string]any{
			"registry": reg,
			"from":     string(choice.From),
			"why":      why,
			"sha256":   d.SHA256(),
			"design":   json.RawMessage(d.Canonical()),
		})
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "design:   %s (based on %s, version %d)\n", d.Name, d.BasedOn, d.Version)
	_, _ = fmt.Fprintf(out, "chosen:   %s\n", why)
	_, _ = fmt.Fprintf(out, "sha256:   %s\n", d.SHA256())
	_, _ = fmt.Fprintf(out, "paper:    %s\n", paperLabel(d.Paper))
	_, _ = fmt.Fprintf(out, "fonts:    %s (headings), %s (body), %s (code)\n", d.Fonts.Headings, d.Fonts.Body, d.Fonts.Code)
	_, _ = fmt.Fprintf(out, "accent:   %s\n", d.Accent)
	_, _ = fmt.Fprintf(out, "title:    %s\n", d.TitleBlock)
	_, _ = fmt.Fprintf(out, "headings: %s\n", d.Headings)
	_, _ = fmt.Fprintf(out, "tables:   %s\n", tablesLabel(d.Tables))
	_, _ = fmt.Fprintf(out, "quotes:   %s\n", d.Quotes)
	footer := "page numbers " + d.Footer.PageNumbers
	if d.Footer.Text != "" {
		footer += fmt.Sprintf(", text %q", d.Footer.Text)
	}
	_, _ = fmt.Fprintf(out, "footer:   %s\n", footer)
	return nil
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

// whyChosen says what chose a design, in the words show prints.
func whyChosen(choice engine.ReportTheme, reg string, signedIn bool) string {
	switch choice.From {
	case engine.ThemeFromAccount:
		return "your account's report design at " + reg
	case engine.ThemeFromConfig, engine.ThemeFromDesignConfig:
		why := string(choice.From)
		if path, err := configPath(); err == nil {
			why += " (" + path + ")"
		}
		if signedIn {
			why += "; your account at " + reg + " has none"
		}
		return why
	case engine.ThemeFromDefault:
		if signedIn {
			return "the default: your account at " + reg + " has no report design and the config sets none"
		}
		return "the default: you are not signed in at " + reg + " and the config sets none"
	}
	return string(choice.From)
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

func newReportDesignExportCmd() *cobra.Command {
	var theme string
	var force bool
	cmd := &cobra.Command{
		Use:   "export FILE",
		Short: "Write your account's report design, or a built-in theme, to a file",
		Long: "Writes your account's report design to FILE as its canonical document, or,\n" +
			"with --theme NAME, a built-in theme's. FILE - writes to stdout. An existing\n" +
			"file is kept unless --force. import reads the file back unchanged.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReportDesignExport(cmd, args[0], theme, force)
		},
	}
	cmd.Flags().StringVar(&theme, "theme", "", "write this built-in theme instead of your account's design ("+strings.Join(toolsrv.BuiltinThemes(), ", ")+")")
	cmd.Flags().BoolVar(&force, "force", false, "replace FILE when it exists")
	return cmd
}

func runReportDesignExport(cmd *cobra.Command, file, theme string, force bool) error {
	var d reportdesign.Design
	var from string
	if theme != "" {
		b, ok := reportdesign.Builtin(theme)
		if !ok {
			return &exitError{code: 2, err: fmt.Errorf("--theme %q is not a report theme (they are: %s)", theme, strings.Join(toolsrv.BuiltinThemes(), ", "))}
		}
		d, from = b, "the built-in theme "+theme
	} else {
		reg := registryURL()
		token := credentialFor(reg)
		if token == "" {
			return &exitError{code: 2, err: fmt.Errorf("not signed in at %s: run `agentry login`, or export a built-in theme with --theme NAME", reg)}
		}
		if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), reg, token); err != nil {
			return err
		}
		doc, err := getReportDesign(cmd.Context(), reg, token)
		if err != nil {
			return err
		}
		if doc == nil {
			return &exitError{code: 2, err: fmt.Errorf("your account at %s has no report design; export a built-in theme with --theme NAME", reg)}
		}
		if d, err = reportdesign.Parse(doc); err != nil {
			return fmt.Errorf("your account's report design is not one this CLI can read (%w); update agentry", err)
		}
		from = "your account's report design at " + reg
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
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s, %s (sha256 %s), to %s.\n", d.Name, from, d.SHA256(), file)
	return nil
}

func newReportDesignImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import FILE",
		Short: "Check a report design file and save it as your account's design",
		Long: "Reads a report design document from FILE (- reads stdin), holds it to every\n" +
			"rule of the design (a refusal names the field), and saves it to your\n" +
			"account at the registry, where the site, the desktop and your runs read it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReportDesignImport(cmd, args[0])
		},
	}
}

func runReportDesignImport(cmd *cobra.Command, file string) error {
	var doc []byte
	var err error
	if file == "-" {
		doc, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), reportdesign.MaxDocumentBytes+1))
	} else {
		doc, err = readDesignFile(file)
	}
	var d reportdesign.Design
	if err == nil {
		d, err = reportdesign.Parse(doc)
	}
	if err != nil {
		// Refused here, before any sign-in is needed; the error names the
		// field (pkg/reportdesign's FieldError).
		return &exitError{code: 2, err: fmt.Errorf("%s: %w", file, err)}
	}
	reg := registryURL()
	saved, err := putReportSettings(cmd, reg, map[string]any{"design": json.RawMessage(d.Canonical())})
	if err != nil {
		return err
	}
	sd, err := reportdesign.Parse(saved)
	if err != nil {
		return fmt.Errorf("the registry saved a report design this CLI cannot read (%w); update agentry", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Saved %s (sha256 %s) as your account's report design at %s. Your runs' PDFs take it from the next run.\n", sd.Name, sd.SHA256(), reg)
	return nil
}

func newReportDesignResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Clear your account's report design",
		Long: "Clears your account's report design at the registry, back to never chosen:\n" +
			"your runs then take report.design or report.theme in the config, or Plain.",
		Args: cobra.NoArgs,
		RunE: runReportDesignReset,
	}
}

func runReportDesignReset(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	if _, err := putReportSettings(cmd, reg, map[string]any{}); err != nil {
		return err
	}
	now := "the configured design or Plain"
	if cfg, err := loadCLIConfig(); err == nil {
		if choice, err := chooseReportTheme(cmd.Context(), reportFlags{}, cfg, nil, io.Discard); err == nil {
			if d, err := designOfChoice(choice); err == nil {
				now = d.Name + " (" + string(choice.From) + ")"
			}
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Cleared your account's report design at %s. Your runs' PDFs now take %s.\n", reg, now)
	return nil
}

// putReportSettings writes the account's report settings and answers the
// design the registry saved, nil when it holds none.
func putReportSettings(cmd *cobra.Command, reg string, body map[string]any) ([]byte, error) {
	token := credentialFor(reg)
	if token == "" {
		return nil, &exitError{code: 2, err: fmt.Errorf("not signed in at %s: run `agentry login`", reg)}
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := client.UpdateUserReportSettingsWithBodyWithResponse(cmd.Context(), "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("save your report design at %s: %w", reg, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, apiProblem("save your report design", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	return designOfSettings(resp.Body)
}
