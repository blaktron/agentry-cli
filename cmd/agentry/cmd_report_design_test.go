package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// designRegistry is a fake of the core's /user/report-settings
// (agentry#1024, handlers_user_report_settings.go): GET answers the stored
// design or {}; PUT holds a design to pkg/reportdesign, refusing it 422 on
// design.<field>, stores its canonical document, and a body without one
// clears it. Like the core's writeJSON, the answer embeds the design
// compacted, not as it was stored.
type designRegistry struct {
	*httptest.Server
	mu     sync.Mutex
	stored []byte
	puts   int
	// refuse makes every PUT answer this field's 422.
	refuse string
}

func newDesignRegistry(t *testing.T) *designRegistry {
	t.Helper()
	dr := &designRegistry{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/report-settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agy_test" {
			writeProblem(w, http.StatusUnauthorized, "Sign in required", "This request carried no valid session or token.")
			return
		}
		dr.mu.Lock()
		defer dr.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			dr.puts++
			var body struct {
				Design json.RawMessage `json:"design"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				writeProblem(w, http.StatusBadRequest, "Bad request", err.Error())
				return
			}
			if len(body.Design) == 0 {
				dr.stored = nil
				break
			}
			d, err := reportdesign.Parse(body.Design)
			var fe *reportdesign.FieldError
			switch {
			case dr.refuse != "":
				fe = &reportdesign.FieldError{Field: dr.refuse, Message: "refused by the registry"}
			case errors.As(err, &fe):
			case err != nil:
				t.Fatalf("Parse answered %v, not a FieldError", err)
			}
			if fe != nil {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": "The report design is not valid.", "status": 422,
					"errors": []map[string]string{{"field": "design." + fe.Field, "message": fe.Message}}})
				return
			}
			dr.stored = d.Canonical()
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		answer := map[string]any{}
		if dr.stored != nil {
			answer["design"] = json.RawMessage(dr.stored)
		}
		writeJSON(w, http.StatusOK, answer)
	})
	dr.Server = httptest.NewServer(mux)
	t.Cleanup(dr.Close)
	return dr
}

func (dr *designRegistry) state() ([]byte, int) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	return dr.stored, dr.puts
}

// reportDesign runs `agentry report-design …` against reg.
func reportDesign(t *testing.T, reg *designRegistry, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, strings.NewReader(""), append(append([]string{"report-design"}, args...), "--registry", reg.URL)...)
}

// exitOf is the exit code main would give err.
func exitOf(err error) int {
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	if code, ok := exitCodeOf(err); ok {
		return code
	}
	if err != nil {
		return 1
	}
	return 0
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Export then import round-trips byte for byte (agentry-cli#785): a
// built-in exported is its own document; a design imported is stored as
// the file holds it, and exported again is the same file. A valid document
// in another layout is stored and exported in the canonical one.
func TestReportDesignExportImportRoundTrip(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_test")
	reg := newDesignRegistry(t)
	dir := t.TempDir()

	modern := filepath.Join(dir, "modern.json")
	if out, _, err := reportDesign(t, reg, "export", "--theme", "modern", modern); err != nil || !strings.Contains(out, "the built-in theme modern") {
		t.Fatalf("export --theme: %q %v", out, err)
	}
	if want, _ := reportdesign.BuiltinDocument("modern"); !bytes.Equal(readFile(t, modern), want) {
		t.Errorf("the exported built-in is not its own document:\n%s", readFile(t, modern))
	}

	house := filepath.Join(dir, "house.json")
	mustWriteFile(t, house, designOf(t, "editorial", "House & Co", "#7a1f1f"))
	if out, _, err := reportDesign(t, reg, "import", house); err != nil || !strings.Contains(out, "Saved House & Co") {
		t.Fatalf("import: %q %v", out, err)
	}
	if stored, _ := reg.state(); !bytes.Equal(stored, readFile(t, house)) {
		t.Errorf("stored %s, want the file byte for byte", stored)
	}
	again := filepath.Join(dir, "again.json")
	if _, _, err := reportDesign(t, reg, "export", again); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !bytes.Equal(readFile(t, again), readFile(t, house)) {
		t.Errorf("export after import:\n%s\nwant\n%s", readFile(t, again), readFile(t, house))
	}
	if _, _, err := reportDesign(t, reg, "import", again); err != nil {
		t.Fatalf("import of the export: %v", err)
	}
	if stored, _ := reg.state(); !bytes.Equal(stored, readFile(t, house)) {
		t.Errorf("a second round trip stored %s", stored)
	}

	// The same design compacted with its keys in another order.
	var m map[string]any
	if err := json.Unmarshal(readFile(t, house), &m); err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(m)
	loose := filepath.Join(dir, "loose.json")
	mustWriteFile(t, loose, compact)
	if _, _, err := reportDesign(t, reg, "import", loose); err != nil {
		t.Fatalf("import of a compact document: %v", err)
	}
	out, _, err := reportDesign(t, reg, "export", "-")
	if err != nil || out != string(readFile(t, house)) {
		t.Errorf("export - after a compact import = %q %v, want the canonical document", out, err)
	}
	if _, _, err := runCLI(t, bytes.NewReader(compact), "report-design", "import", "-", "--registry", reg.URL); err != nil {
		t.Errorf("import - from stdin: %v", err)
	}
}

// import refuses a file that is not a design, naming the field, before it
// asks the registry anything; a refusal from the registry is shown with
// its field too.
func TestReportDesignImportRefusesAnInvalidFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_test")
	reg := newDesignRegistry(t)
	dir := t.TempDir()
	bodyMono := bytes.Replace(designOf(t, "plain", "Mono body", "#2b2f36"), []byte(`"body": "Inter"`), []byte(`"body": "IBM Plex Mono"`), 1)
	for _, c := range []struct {
		name string
		doc  []byte
		says string
	}{
		{"an accent that cannot carry its text", designOf(t, "modern", "Pale", "#7fb3d5"), "accent"},
		{"a mono body", bodyMono, "fonts.body"},
		{"an unknown key", []byte(`{"agentryTheme": 2, "logoUrl": "https://example.com/x.png"}`), `unknown key "logoUrl"`},
		{"a version-1 theme", bytes.Replace(designOf(t, "plain", "Old", "#2b2f36"), []byte(`"agentryTheme": 2`), []byte(`"agentryTheme": 1`), 1), "agentryTheme"},
		{"not JSON", []byte("accent: blue\n"), "not a JSON object"},
	} {
		f := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+".json")
		mustWriteFile(t, f, c.doc)
		_, _, err := reportDesign(t, reg, "import", f)
		if exitOf(err) != 2 || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), f) {
			t.Errorf("%s: %v (exit %d), want exit 2 naming %q and the file", c.name, err, exitOf(err), c.says)
		}
	}
	if _, puts := reg.state(); puts != 0 {
		t.Errorf("an invalid file reached the registry %d times", puts)
	}

	ok := filepath.Join(dir, "ok.json")
	mustWriteFile(t, ok, designOf(t, "classic", "Fine", "#1f3a5f"))
	reg.refuse = "fonts.headings"
	if _, _, err := reportDesign(t, reg, "import", ok); err == nil || !strings.Contains(err.Error(), "design.fonts.headings") {
		t.Errorf("a registry's refusal = %v, want the field it named", err)
	}

	t.Setenv(envToken, "")
	if _, _, err := reportDesign(t, reg, "import", ok); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
}

// show prints the design a run would take now and what chose it: the
// account's, then the config's, then Plain, as chooseReportTheme decides.
func TestReportDesignShow(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)

	out, _, err := reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:   Plain (based on plain") || !strings.Contains(out, "you are not signed in at "+reg.URL) {
		t.Fatalf("signed out, nothing set: %q %v", out, err)
	}
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "classic"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envToken, "agy_test")
	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:   Classic") || !strings.Contains(out, "report.theme in the CLI's config") || !strings.Contains(out, "has none") {
		t.Fatalf("the config's theme: %q %v", out, err)
	}

	house := designOf(t, "modern", "House", "#0b5394")
	d, _ := reportdesign.Parse(house)
	f := filepath.Join(t.TempDir(), "house.json")
	mustWriteFile(t, f, house)
	if _, _, err := reportDesign(t, reg, "import", f); err != nil {
		t.Fatal(err)
	}
	out, _, err = reportDesign(t, reg, "show")
	for _, want := range []string{"design:   House (based on modern, version 1)", "your account's report design at " + reg.URL, "sha256:   " + d.SHA256(), "accent:   #0b5394", "tables:   tinted header, banded rows"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("the account's design: %q %v, want %q", out, err, want)
		}
	}
	out, _, err = reportDesign(t, reg, "show", "--json")
	var doc struct {
		From   string          `json:"from"`
		SHA256 string          `json:"sha256"`
		Design json.RawMessage `json:"design"`
	}
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil {
		t.Fatalf("show --json: %q %v", out, err)
	}
	if got, err := reportdesign.Parse(doc.Design); err != nil || got != d || doc.SHA256 != d.SHA256() || doc.From != string(engine.ThemeFromAccount) {
		t.Errorf("show --json = %+v (%v)", doc, err)
	}

	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "fancy"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reportDesign(t, reg, "show"); exitOf(err) != engine.ExitUsage || !strings.Contains(err.Error(), "fancy") {
		t.Errorf("a config theme that is not one = %v, want the run's usage error", err)
	}
}

// export writes the account's design or a built-in, never over a file
// without --force, and says so when there is nothing to write.
func TestReportDesignExport(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	f := filepath.Join(t.TempDir(), "design.json")

	if _, _, err := reportDesign(t, reg, "export", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
	t.Setenv(envToken, "agy_test")
	if _, _, err := reportDesign(t, reg, "export", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "has no report design") {
		t.Errorf("never chosen = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "export", "--theme", "formal", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "plain, classic, modern, editorial, technical") {
		t.Errorf("an unknown theme = %v", err)
	}
	if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused export wrote %s", f)
	}
	mustWriteFile(t, f, []byte("mine\n"))
	if _, _, err := reportDesign(t, reg, "export", "--theme", "plain", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "--force") {
		t.Errorf("over an existing file = %v", err)
	}
	if got := readFile(t, f); string(got) != "mine\n" {
		t.Errorf("the existing file became %q", got)
	}
	if _, _, err := reportDesign(t, reg, "export", "--theme", "technical", "--force", f); err != nil {
		t.Fatal(err)
	}
	if want, _ := reportdesign.BuiltinDocument("technical"); !bytes.Equal(readFile(t, f), want) {
		t.Errorf("--force wrote %s", readFile(t, f))
	}
}

// reset clears the account's design, back to never chosen, and says what
// runs take now.
func TestReportDesignReset(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	if _, _, err := reportDesign(t, reg, "reset"); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
	t.Setenv(envToken, "agy_test")
	f := filepath.Join(t.TempDir(), "house.json")
	mustWriteFile(t, f, designOf(t, "modern", "House", "#0b5394"))
	if _, _, err := reportDesign(t, reg, "import", f); err != nil {
		t.Fatal(err)
	}
	out, _, err := reportDesign(t, reg, "reset")
	if err != nil || !strings.Contains(out, "Cleared") || !strings.Contains(out, "now take Plain (the default)") {
		t.Fatalf("reset: %q %v", out, err)
	}
	if stored, _ := reg.state(); stored != nil {
		t.Errorf("reset left %s", stored)
	}
	if out, _, err := reportDesign(t, reg, "show"); err != nil || !strings.Contains(out, "your account at "+reg.URL+" has no report design") {
		t.Errorf("show after reset: %q %v", out, err)
	}
}
