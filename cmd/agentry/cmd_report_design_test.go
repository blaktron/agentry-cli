package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// storedDesign is one design of the fake library: its document without
// its images, and its images as the core keeps them.
type storedDesign struct {
	id          string
	doc         reportdesign.Design
	isDefault   bool
	created     time.Time
	updated     time.Time
	logo        reportdesign.Logo
	coverImage  reportdesign.Image
	customCover reportdesign.CustomCover
}

// designRegistry is a fake of the core's /user/report-designs: the list,
// create, read, replace, delete and default routes, each holding a design
// to pkg/reportdesign, refusing it 422 on design.<field>, storing its
// canonical document and its images as the core re-encodes them, and
// answering the images only with include=assets. Like the core's writeJSON,
// the answer embeds the design compacted, not as it was stored.
type designRegistry struct {
	*httptest.Server
	mu      sync.Mutex
	designs []*storedDesign
	next    int
	// writes counts the create, replace, delete and default calls.
	writes int
	// lastInclude and lastAuth are the include parameter and the
	// Authorization header of the last request.
	lastInclude, lastAuth string
	// refuse makes every create and replace answer this field's 422.
	refuse string
	// broken makes every call answer 500.
	broken bool
}

func newDesignRegistry(t *testing.T) *designRegistry {
	t.Helper()
	dr := &designRegistry{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user/report-designs", dr.collection(t))
	mux.HandleFunc("/api/v1/user/report-designs/", dr.item(t))
	dr.Server = httptest.NewServer(mux)
	t.Cleanup(dr.Close)
	// The fake is the self-hosted registry an AGENTRY_TOKEN a test sets
	// authenticates to.
	t.Setenv(envRegistry, dr.URL)
	return dr
}

func (dr *designRegistry) guard(w http.ResponseWriter, r *http.Request) bool {
	dr.lastAuth = r.Header.Get("Authorization")
	dr.lastInclude = r.URL.Query().Get("include")
	if dr.lastAuth != "Bearer agy_test" {
		writeProblem(w, http.StatusUnauthorized, "Sign in required", "This request carried no valid session or token.")
		return false
	}
	if dr.broken {
		writeProblem(w, http.StatusInternalServerError, "Internal error", "")
		return false
	}
	return true
}

func (dr *designRegistry) summary(d *storedDesign) map[string]any {
	return map[string]any{"id": d.id, "name": d.doc.Name, "basedOn": d.doc.BasedOn, "version": d.doc.Version, "default": d.isDefault,
		"hasLogo": d.logo != (reportdesign.Logo{}), "hasCoverImage": d.coverImage != (reportdesign.Image{}), "hasCustomCover": !d.customCover.IsZero(),
		"createdAt": d.created, "updatedAt": d.updated}
}

func (dr *designRegistry) listDoc() map[string]any {
	designs := []map[string]any{}
	doc := map[string]any{}
	for _, d := range dr.designs {
		designs = append(designs, dr.summary(d))
		if d.isDefault {
			doc["default"] = d.id
		}
	}
	doc["designs"] = designs
	return doc
}

func (dr *designRegistry) record(d *storedDesign, assets bool) map[string]any {
	doc := d.doc
	if assets {
		doc.Logo, doc.CoverImage, doc.CustomCover = d.logo, d.coverImage, d.customCover
	}
	rec := dr.summary(d)
	rec["design"] = json.RawMessage(doc.Canonical())
	delete(rec, "name")
	delete(rec, "basedOn")
	delete(rec, "version")
	return rec
}

// take reads a design from a body, refusing it as the core would, and
// stores its images as the core re-encodes them.
func (dr *designRegistry) take(t *testing.T, w http.ResponseWriter, body []byte, into *storedDesign) bool {
	t.Helper()
	var wrapped struct {
		Design  json.RawMessage `json:"design"`
		Default bool            `json:"default"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil || len(wrapped.Design) == 0 {
		writeProblem(w, http.StatusBadRequest, "Bad request", "a design is required")
		return false
	}
	d, err := reportdesign.Parse(wrapped.Design)
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
		return false
	}
	if d.Logo != (reportdesign.Logo{}) {
		raw, _ := d.Logo.Decode()
		if into.logo, err = reportdesign.NormalizeLogo(raw); err != nil {
			t.Fatalf("NormalizeLogo of a parsed logo: %v", err)
		}
	}
	if d.CoverImage != (reportdesign.Image{}) {
		raw, _, _ := d.CoverImage.Decode()
		if into.coverImage, err = reportdesign.NormalizeCoverImage(raw); err != nil {
			t.Fatalf("NormalizeCoverImage of a parsed image: %v", err)
		}
	}
	if !d.CustomCover.IsZero() {
		into.customCover = d.CustomCover
	}
	into.doc = d.WithoutAssets()
	into.updated = time.Now().UTC()
	if wrapped.Default {
		for _, other := range dr.designs {
			other.isDefault = false
		}
		into.isDefault = true
	}
	return true
}

func (dr *designRegistry) collection(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dr.mu.Lock()
		defer dr.mu.Unlock()
		if !dr.guard(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, dr.listDoc())
		case http.MethodPost:
			dr.writes++
			if len(dr.designs) >= reportdesign.MaxLibrary {
				writeProblem(w, http.StatusConflict, "The library is full", "Your library holds ten designs.")
				return
			}
			body, _ := io.ReadAll(r.Body)
			d := &storedDesign{created: time.Now().UTC()}
			if !dr.take(t, w, body, d) {
				return
			}
			dr.next++
			d.id = fmt.Sprintf("00000000-0000-4000-8000-%012d", dr.next)
			dr.designs = append(dr.designs, d)
			writeJSON(w, http.StatusCreated, dr.record(d, dr.lastInclude == "assets"))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}
}

func (dr *designRegistry) item(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dr.mu.Lock()
		defer dr.mu.Unlock()
		if !dr.guard(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/user/report-designs/")
		if id == "default" {
			if r.Method != http.MethodPut {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			dr.writes++
			var body struct {
				ID *string `json:"id"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			var chosen *storedDesign
			if body.ID != nil {
				for _, d := range dr.designs {
					if d.id == *body.ID {
						chosen = d
					}
				}
				if chosen == nil {
					writeProblem(w, http.StatusNotFound, "No such design", "")
					return
				}
			}
			for _, d := range dr.designs {
				d.isDefault = d == chosen
			}
			writeJSON(w, http.StatusOK, dr.listDoc())
			return
		}
		var found *storedDesign
		for _, d := range dr.designs {
			if d.id == id {
				found = d
			}
		}
		if found == nil {
			writeProblem(w, http.StatusNotFound, "No such design", "")
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, dr.record(found, dr.lastInclude == "assets"))
		case http.MethodPut:
			dr.writes++
			body, _ := io.ReadAll(r.Body)
			replaced := *found
			if !dr.take(t, w, body, &replaced) {
				return
			}
			*found = replaced
			writeJSON(w, http.StatusOK, dr.record(found, dr.lastInclude == "assets"))
		case http.MethodDelete:
			dr.writes++
			dr.designs = slicesWithout(dr.designs, found)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}
}

func slicesWithout(all []*storedDesign, gone *storedDesign) []*storedDesign {
	var out []*storedDesign
	for _, d := range all {
		if d != gone {
			out = append(out, d)
		}
	}
	return out
}

// add puts a design in the library as a site would, and answers its id.
func (dr *designRegistry) add(t *testing.T, doc []byte, dflt bool) string {
	t.Helper()
	dr.mu.Lock()
	defer dr.mu.Unlock()
	d := &storedDesign{created: time.Now().UTC()}
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"design": json.RawMessage(doc), "default": dflt})
	if !dr.take(t, rec, body, d) {
		t.Fatalf("add: %s", rec.Body.String())
	}
	dr.next++
	d.id = fmt.Sprintf("00000000-0000-4000-8000-%012d", dr.next)
	dr.designs = append(dr.designs, d)
	return d.id
}

func (dr *designRegistry) setDefault(id string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	for _, d := range dr.designs {
		d.isDefault = d.id == id
	}
}

// stored is the library's designs by name, each its canonical document
// without images, and the default's name.
func (dr *designRegistry) stored() (map[string][]byte, string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	out := map[string][]byte{}
	dflt := ""
	for _, d := range dr.designs {
		out[d.doc.Name] = d.doc.Canonical()
		if d.isDefault {
			dflt = d.doc.Name
		}
	}
	return out, dflt
}

func (dr *designRegistry) byName(name string) *storedDesign {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	for _, d := range dr.designs {
		if d.doc.Name == name {
			return d
		}
	}
	return nil
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

// Export then import round-trips byte for byte: a built-in exported is its
// own document; a design imported is stored as the file holds it, and
// exported again by name is the same file. A valid document in another
// layout is stored and exported in the canonical one. A second import of
// the same name is refused unless --replace, which keeps the id.
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
	out, _, err := reportDesign(t, reg, "import", house)
	if err != nil || !strings.Contains(out, "Added House & Co") || !strings.Contains(out, `--report-design "House & Co"`) {
		t.Fatalf("import: %q %v", out, err)
	}
	if stored, dflt := reg.stored(); !bytes.Equal(stored["House & Co"], readFile(t, house)) || dflt != "" {
		t.Errorf("stored %s (default %q), want the file byte for byte and no default", stored["House & Co"], dflt)
	}
	again := filepath.Join(dir, "again.json")
	if _, _, err := reportDesign(t, reg, "export", "House & Co", again); err != nil {
		t.Fatalf("export by name: %v", err)
	}
	if !bytes.Equal(readFile(t, again), readFile(t, house)) {
		t.Errorf("export after import:\n%s\nwant\n%s", readFile(t, again), readFile(t, house))
	}
	if _, _, err := reportDesign(t, reg, "import", again); exitOf(err) != 2 || !strings.Contains(err.Error(), "already has a report design named") {
		t.Errorf("a second import of the name = %v", err)
	}
	id := reg.byName("House & Co").id
	if out, _, err := reportDesign(t, reg, "import", "--replace", again); err != nil || !strings.Contains(out, "Replaced House & Co ("+id) {
		t.Fatalf("import --replace: %q %v", out, err)
	}
	if stored, _ := reg.stored(); !bytes.Equal(stored["House & Co"], readFile(t, house)) || len(stored) != 1 {
		t.Errorf("a replace stored %v", stored)
	}

	// The same design compacted with its keys in another order, under a
	// name of its own.
	var m map[string]any
	if err := json.Unmarshal(readFile(t, house), &m); err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(m)
	loose := filepath.Join(dir, "loose.json")
	mustWriteFile(t, loose, compact)
	if _, _, err := reportDesign(t, reg, "import", "--name", "Loose", loose); err != nil {
		t.Fatalf("import of a compact document: %v", err)
	}
	out, _, err = reportDesign(t, reg, "export", "Loose", "-")
	want := bytes.Replace(readFile(t, house), []byte(`"name": "House & Co"`), []byte(`"name": "Loose"`), 1)
	if err != nil || out != string(want) {
		t.Errorf("export - after a compact import = %q %v, want the canonical document", out, err)
	}
	if _, _, err := runCLI(t, bytes.NewReader(compact), "report-design", "import", "-", "--name", "Stdin", "--registry", reg.URL); err != nil {
		t.Errorf("import - from stdin: %v", err)
	}
}

// import refuses a file that is not a design, naming the field, before it
// asks the registry anything; a refusal from the registry is shown with
// its field too; a full library refuses an eleventh.
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
		{"an unknown key", []byte(`{"agentryTheme": 3, "logoUrl": "https://example.com/x.png"}`), `unknown key "logoUrl"`},
		{"a version-2 theme", bytes.Replace(designOf(t, "plain", "Old", "#2b2f36"), []byte(`"agentryTheme": 3`), []byte(`"agentryTheme": 2`), 1), "a theme document version 2; this CLI draws version 3"},
		{"not JSON", []byte("accent: blue\n"), "not a JSON object"},
	} {
		f := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+".json")
		mustWriteFile(t, f, c.doc)
		_, _, err := reportDesign(t, reg, "import", f)
		if exitOf(err) != 2 || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), f) {
			t.Errorf("%s: %v (exit %d), want exit 2 naming %q and the file", c.name, err, exitOf(err), c.says)
		}
	}
	if reg.writes != 0 {
		t.Errorf("an invalid file reached the registry %d times", reg.writes)
	}

	ok := filepath.Join(dir, "ok.json")
	mustWriteFile(t, ok, designOf(t, "classic", "Fine", "#1f3a5f"))
	reg.refuse = "fonts.headings"
	if _, _, err := reportDesign(t, reg, "import", ok); err == nil || !strings.Contains(err.Error(), "design.fonts.headings") {
		t.Errorf("a registry's refusal = %v", err)
	}
	reg.refuse = ""
	for i := range reportdesign.MaxLibrary {
		reg.add(t, designOf(t, "plain", fmt.Sprintf("Design %d", i), "#2b2f36"), false)
	}
	if _, _, err := reportDesign(t, reg, "import", ok); exitOf(err) != 2 || !strings.Contains(err.Error(), "the most it keeps") {
		t.Errorf("an eleventh = %v", err)
	}

	t.Setenv(envToken, "")
	if _, _, err := reportDesign(t, reg, "import", ok); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
	// The file is held to the rules before a sign-in is looked for.
	pale := filepath.Join(dir, "an-accent-that-cannot-carry-its-text.json")
	if _, _, err := reportDesign(t, reg, "import", pale); exitOf(err) != 2 || !strings.Contains(err.Error(), "accent") {
		t.Errorf("an invalid file signed out = %v, want its field", err)
	}
	if _, _, err := reportDesign(t, reg, "import", filepath.Join(dir, "none.json")); exitOf(err) != 2 || !strings.Contains(err.Error(), "none.json") {
		t.Errorf("a missing file = %v", err)
	}
}

// show prints the design a run would take now and what chose it: the
// account's default, then the config's, then Plain, as chooseReportTheme
// decides; with a name, that design of the library.
func TestReportDesignShow(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)

	out, _, err := reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:    Plain (based on plain") || !strings.Contains(out, "you are not signed in at "+reg.URL) {
		t.Fatalf("signed out, nothing set: %q %v", out, err)
	}
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "classic"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envToken, "agy_test")
	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:    Classic") || !strings.Contains(out, "report.theme in the CLI's config") || !strings.Contains(out, "your account at "+reg.URL+" has no default report design") {
		t.Fatalf("the config's theme: %q %v", out, err)
	}

	house := designOf(t, "modern", "House", "#0b5394")
	d, _ := reportdesign.Parse(house)
	f := filepath.Join(t.TempDir(), "house.json")
	mustWriteFile(t, f, house)
	if _, _, err := reportDesign(t, reg, "import", f); err != nil {
		t.Fatal(err)
	}
	// In the library, not the default: the config's theme still runs.
	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:    Classic") {
		t.Fatalf("a library design that is not the default: %q %v", out, err)
	}
	out, _, err = reportDesign(t, reg, "show", "House")
	for _, want := range []string{"design:    House (based on modern, version 1)", `the design "House" (00000000-0000-4000-8000-000000000001) of your account's library`, "--report-design House", "sha256:    " + d.SHA256(), "colours:   #0b5394 (accent)", "header:    titleAndPage, slots \"{title}\" \"{page} of {pages}\"", "cover:     band, for a long report, showing title, subtitle, author, date"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("show House: %q %v, want %q", out, err, want)
		}
	}
	if _, _, err := reportDesign(t, reg, "use", "House"); err != nil {
		t.Fatal(err)
	}
	out, _, err = reportDesign(t, reg, "show")
	for _, want := range []string{"design:    House (based on modern, version 1)", "your account's default report design at " + reg.URL, "sha256:    " + d.SHA256(), "tables:    tinted header, banded rows"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("the account's default: %q %v, want %q", out, err, want)
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
	if got, err := reportdesign.Parse(doc.Design); err != nil || got.SHA256() != d.SHA256() || doc.SHA256 != d.SHA256() || doc.From != string(engine.ThemeFromAccount) {
		t.Errorf("show --json = %+v (%v)", doc, err)
	}
	if _, _, err := reportDesign(t, reg, "show", "Nope"); exitOf(err) != 2 || !strings.Contains(err.Error(), `no report design named "Nope"`) {
		t.Errorf("show Nope = %v", err)
	}

	// An account that cannot be read, or whose default this CLI refuses,
	// is said to be so, never "has none".
	reg.broken = true
	out, errOut, err := reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:    Classic") || !strings.Contains(out, "could not be read") || !strings.Contains(errOut, "warning") {
		t.Errorf("an account that cannot be read: %q %q %v", out, errOut, err)
	}
	reg.broken = false
	reg.mu.Lock()
	reg.designs[0].doc.Fonts.Body = "DejaVu Sans"
	reg.mu.Unlock()
	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:    Classic") || !strings.Contains(out, "is not one this CLI can draw") {
		t.Errorf("an account design this CLI refuses: %q %v", out, err)
	}

	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "fancy"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reportDesign(t, reg, "show"); exitOf(err) != engine.ExitUsage || !strings.Contains(err.Error(), "fancy") {
		t.Errorf("a config theme that is not one = %v, want the run's usage error", err)
	}
}

// list prints the library, oldest first, with the default marked, and says
// what runs take when nothing is the default; --json prints it whole.
func TestReportDesignList(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	if _, _, err := reportDesign(t, reg, "list"); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
	t.Setenv(envToken, "agy_test")
	out, _, err := reportDesign(t, reg, "list")
	if err != nil || !strings.Contains(out, "has no report design") {
		t.Fatalf("an empty library: %q %v", out, err)
	}
	reg.add(t, designOf(t, "modern", "House", "#0b5394"), false)
	reg.add(t, withLogo(t, designOf(t, "classic", "Formal", "#1f3a5f"), testLogo(t, 100, 40, "m")), true)
	out, _, err = reportDesign(t, reg, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[1], "House") || !strings.HasPrefix(lines[2], "Formal") ||
		!strings.Contains(lines[2], "default") || !strings.Contains(lines[2], "logo") || strings.Contains(lines[1], "default") || strings.Contains(out, "None is the default") {
		t.Errorf("list:\n%s", out)
	}
	reg.setDefault("")
	if out, _, err := reportDesign(t, reg, "list"); err != nil || !strings.Contains(out, "None is the default") {
		t.Errorf("list with no default: %q %v", out, err)
	}
	out, _, err = reportDesign(t, reg, "list", "--json")
	var doc struct {
		Default string           `json:"default"`
		Designs []librarySummary `json:"designs"`
	}
	if err != nil || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Designs) != 2 || doc.Designs[1].Name != "Formal" || !doc.Designs[1].HasLogo || doc.Default != "" {
		t.Errorf("list --json = %q (%v)", out, err)
	}
}

// use makes a design the default, by name or id, and --none clears it;
// delete removes one and says when the default went with it.
func TestReportDesignUseAndDelete(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	for _, args := range [][]string{{"use", "House"}, {"use", "--none"}, {"delete", "House"}} {
		if _, _, err := reportDesign(t, reg, args...); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
			t.Errorf("%v signed out = %v", args, err)
		}
	}
	t.Setenv(envToken, "agy_test")
	if _, _, err := reportDesign(t, reg, "use"); exitOf(err) != 2 {
		t.Errorf("use with nothing = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "use", "House", "--none"); exitOf(err) != 2 {
		t.Errorf("use with a name and --none = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "use", "House"); exitOf(err) != 2 || !strings.Contains(err.Error(), "has no report design") {
		t.Errorf("use in an empty library = %v", err)
	}
	houseID := reg.add(t, designOf(t, "modern", "House", "#0b5394"), false)
	formalID := reg.add(t, designOf(t, "classic", "Formal", "#1f3a5f"), true)
	out, _, err := reportDesign(t, reg, "use", "House")
	if err != nil || !strings.Contains(out, "House ("+houseID+") is your account's default") {
		t.Fatalf("use House: %q %v", out, err)
	}
	if _, dflt := reg.stored(); dflt != "House" {
		t.Errorf("the default is %q", dflt)
	}
	if out, _, err := reportDesign(t, reg, "use", formalID); err != nil || !strings.Contains(out, "Formal ("+formalID+")") {
		t.Errorf("use by id: %q %v", out, err)
	}
	if _, _, err := reportDesign(t, reg, "use", "Nope"); exitOf(err) != 2 || !strings.Contains(err.Error(), `no report design named "Nope"`) {
		t.Errorf("use Nope = %v", err)
	}
	if err := saveCLIConfig(cliConfig{Report: reportConfig{Theme: "technical"}}); err != nil {
		t.Fatal(err)
	}
	out, _, err = reportDesign(t, reg, "use", "--none")
	if err != nil || !strings.Contains(out, "No design of your account") || !strings.Contains(out, "Technical (report.theme in the CLI's config)") {
		t.Errorf("use --none: %q %v", out, err)
	}
	if _, dflt := reg.stored(); dflt != "" {
		t.Errorf("the default is %q after --none", dflt)
	}
	reg.setDefault(formalID)
	out, _, err = reportDesign(t, reg, "delete", "Formal")
	if err != nil || !strings.Contains(out, "Removed Formal ("+formalID+")") || !strings.Contains(out, "It was the default") {
		t.Errorf("delete the default: %q %v", out, err)
	}
	if stored, dflt := reg.stored(); len(stored) != 1 || dflt != "" {
		t.Errorf("after the delete: %v, default %q", stored, dflt)
	}
	if out, _, err := reportDesign(t, reg, "delete", houseID); err != nil || !strings.Contains(out, "Removed House") || strings.Contains(out, "It was the default") {
		t.Errorf("delete by id: %q %v", out, err)
	}
	if _, _, err := reportDesign(t, reg, "delete", "House"); exitOf(err) != 2 {
		t.Errorf("delete in an empty library = %v", err)
	}
}

// export writes the account's default or a named design, or a built-in,
// never over a file without --force, and says so when there is nothing to
// write.
func TestReportDesignExport(t *testing.T) {
	isolateEnv(t)
	reg := newDesignRegistry(t)
	f := filepath.Join(t.TempDir(), "design.json")

	if _, _, err := reportDesign(t, reg, "export", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("signed out = %v", err)
	}
	t.Setenv(envToken, "agy_test")
	if _, _, err := reportDesign(t, reg, "export", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "has no default report design") {
		t.Errorf("no default = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "export", "--theme", "formal", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "plain, classic, modern, editorial, technical, boardroom, studio, field, civic, lumen") {
		t.Errorf("an unknown theme = %v", err)
	}
	if _, _, err := reportDesign(t, reg, "export", "--theme", "plain", "House", f); exitOf(err) != 2 || !strings.Contains(err.Error(), "not both") {
		t.Errorf("a name and --theme = %v", err)
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
	if _, _, err := reportDesign(t, reg, "export", "--theme", "lumen", "--force", f); err != nil {
		t.Fatal(err)
	}
	if want, _ := reportdesign.BuiltinDocument("lumen"); !bytes.Equal(readFile(t, f), want) {
		t.Errorf("--force wrote %s", readFile(t, f))
	}
	house := designOf(t, "modern", "House", "#0b5394")
	reg.add(t, house, true)
	out, _, err := reportDesign(t, reg, "export", "--force", f)
	if err != nil || !strings.Contains(out, "your account's default report design") || !bytes.Equal(readFile(t, f), house) {
		t.Errorf("export the default: %q %v", out, err)
	}
}

// testLogo is a w×h PNG with a text chunk after its header and bytes after
// its end, both holding marker: a logo within the bounds that is not
// NormalizeLogo's encoding.
func testLogo(t *testing.T, w, h int, marker string) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	data := []byte("Comment\x00" + marker)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data))) //nolint:gosec // a short test chunk
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("tEXt"), data...)))
	return append(append(append(append([]byte{}, b[:33]...), chunk...), b[33:]...), marker...)
}

// withLogo is a design document with logo as its logo.
func withLogo(t *testing.T, doc, logo []byte) []byte {
	t.Helper()
	d, err := reportdesign.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	d.Logo = reportdesign.Logo{PNG: base64.StdEncoding.EncodeToString(logo)}
	return d.Canonical()
}

// The images travel in the design: import sends a file's logo and cover
// image as the core stores them (its own encodings, so the hash the CLI
// prints is the one saved), export asks for them and writes them, a round
// trip is byte for byte, show says there are some, and a replace from a
// file without them keeps the design's and says so. A run signed in takes
// the default with its images.
func TestReportDesignImages(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_test")
	reg := newDesignRegistry(t)
	dir := t.TempDir()

	crafted := testLogo(t, 300, 100, "agentry-816-marker")
	d, err := reportdesign.Parse(withLogo(t, designOf(t, "field", "House", "#2f5d2a"), crafted))
	if err != nil {
		t.Fatal(err)
	}
	d.CoverImage = reportdesign.ImageOf(testLogo(t, 600, 400, "cover-marker"), "png")
	house := filepath.Join(dir, "house.json")
	mustWriteFile(t, house, d.Canonical())
	out, errOut, err := reportDesign(t, reg, "import", "--default", house)
	if err != nil || errOut != "" || !strings.Contains(out, "Added House") || !strings.Contains(out, "as the default") || strings.Contains(out, "keeps the one") {
		t.Fatalf("import with images: %q %q %v", out, errOut, err)
	}
	stored := reg.byName("House")
	logo, _ := stored.logo.Decode()
	cover, _, _ := stored.coverImage.Decode()
	if len(logo) == 0 || bytes.Contains(logo, []byte("agentry-816-marker")) || len(cover) == 0 || bytes.Contains(cover, []byte("cover-marker")) {
		t.Fatalf("the registry stored the logo as %d bytes and the cover image as %d bytes, markers kept: %v %v", len(logo), len(cover), bytes.Contains(logo, []byte("agentry-816-marker")), bytes.Contains(cover, []byte("cover-marker")))
	}

	exported := filepath.Join(dir, "exported.json")
	if out, _, err := reportDesign(t, reg, "export", exported); err != nil || !strings.Contains(out, "House with its logo, cover image") {
		t.Fatalf("export: %q %v", out, err)
	}
	if reg.lastInclude != "assets" {
		t.Errorf("export asked include=%q, want assets", reg.lastInclude)
	}
	ed, err := reportdesign.Parse(readFile(t, exported))
	if err != nil || ed.Logo != stored.logo || ed.CoverImage != stored.coverImage {
		t.Fatalf("the exported design's images are not the stored ones (%v)", err)
	}
	if out, errOut, err := reportDesign(t, reg, "import", "--replace", exported); err != nil || errOut != "" {
		t.Fatalf("import of the export: %q %q %v", out, errOut, err)
	}
	again := filepath.Join(dir, "again.json")
	if _, _, err := reportDesign(t, reg, "export", "House", again); err != nil || !bytes.Equal(readFile(t, again), readFile(t, exported)) {
		t.Errorf("a round trip with images is not byte for byte (%v)", err)
	}

	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "logo:      a PNG of 300×100 pixels") || !strings.Contains(out, "images:    a cover image (PNG") || !strings.Contains(out, "sha256:    "+ed.SHA256()) {
		t.Errorf("show with images: %q %v", out, err)
	}
	out, _, err = reportDesign(t, reg, "show", "--json")
	var shown struct {
		Logo       bool `json:"logo"`
		CoverImage bool `json:"coverImage"`
	}
	if err != nil || json.Unmarshal([]byte(out), &shown) != nil || !shown.Logo || !shown.CoverImage {
		t.Errorf("show --json with images: %q %v", out, err)
	}
	choice, err := chooseReportTheme(t.Context(), reportFlags{}, cliConfig{}, readAccountTheme(reg.URL), io.Discard)
	got, perr := reportdesign.Parse(choice.Design)
	if err != nil || perr != nil || choice.From != engine.ThemeFromAccount || got.SHA256() != ed.SHA256() {
		t.Errorf("a signed-in run's design = %+v (%v), want the default with its images", choice.From, err)
	}
	named, err := chooseReportTheme(t.Context(), reportFlags{design: "House"}, cliConfig{}, readAccountTheme(reg.URL), io.Discard)
	if err != nil || named.From != engine.ThemeFromLibraryFlag || !bytes.Equal(named.Design, choice.Design) {
		t.Errorf("--report-design House = %+v (%v)", named.From, err)
	}

	plain := filepath.Join(dir, "plain.json")
	mustWriteFile(t, plain, designOf(t, "field", "House", "#2f5d2a"))
	out, errOut, err = reportDesign(t, reg, "import", "--replace", plain)
	if err != nil || errOut != "" || !strings.Contains(out, "The file has no logo, cover image, so the design keeps the one it has") {
		t.Errorf("replace without images: %q %q %v", out, errOut, err)
	}
	if out, _, err := reportDesign(t, reg, "show"); err != nil || !strings.Contains(out, "logo:      a PNG of 300×100") {
		t.Errorf("show after an image-less replace: %q %v", out, err)
	}

	// A logo that is not a PNG is refused before the registry is asked.
	writes := reg.writes
	bad := filepath.Join(dir, "bad.json")
	mustWriteFile(t, bad, bytes.Replace(withLogo(t, designOf(t, "modern", "Bad", "#0b5394"), crafted), []byte(`"png": "iVBOR`), []byte(`"png": "/9j/4`), 1))
	if _, _, err := reportDesign(t, reg, "import", bad); exitOf(err) != 2 || !strings.Contains(err.Error(), "logo") {
		t.Errorf("a logo that is not a PNG = %v", err)
	}
	if reg.writes != writes {
		t.Error("a refused logo reached the registry")
	}
}
