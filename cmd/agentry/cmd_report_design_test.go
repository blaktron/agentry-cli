package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// designRegistry is a fake of the core's /user/report-settings: GET answers
// the stored design or {}; PUT holds a design to pkg/reportdesign, refusing
// it 422 on design.<field>, stores its canonical document, and a body
// without one clears it. Like the core's writeJSON, the answer embeds the
// design compacted, not as it was stored. The logo is stored beside the
// design as NormalizeLogo writes it: a design PUT with one replaces it, one
// without keeps it, and an answer carries it only with include=logo.
type designRegistry struct {
	*httptest.Server
	mu     sync.Mutex
	stored []byte
	logo   reportdesign.Logo
	puts   int
	// asked is the include parameter of the last request.
	asked string
	// refuse makes every PUT answer this field's 422.
	refuse string
	// broken makes every GET answer 500.
	broken bool
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
		dr.asked = r.URL.Query().Get("include")
		switch r.Method {
		case http.MethodGet:
			if dr.broken {
				writeProblem(w, http.StatusInternalServerError, "Internal error", "")
				return
			}
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
				dr.stored, dr.logo = nil, reportdesign.Logo{}
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
			if d.Logo != (reportdesign.Logo{}) {
				raw, _ := d.Logo.Decode()
				if dr.logo, err = reportdesign.NormalizeLogo(raw); err != nil {
					t.Fatalf("NormalizeLogo of a parsed logo: %v", err)
				}
				d.Logo = reportdesign.Logo{}
			}
			dr.stored = d.Canonical()
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		answer := map[string]any{}
		if dr.stored != nil {
			answer["design"] = json.RawMessage(dr.stored)
			if dr.asked == "logo" && dr.logo != (reportdesign.Logo{}) {
				d, err := reportdesign.Parse(dr.stored)
				if err != nil {
					t.Fatal(err)
				}
				d.Logo = dr.logo
				answer["design"] = json.RawMessage(d.Canonical())
			}
		}
		writeJSON(w, http.StatusOK, answer)
	})
	dr.Server = httptest.NewServer(mux)
	t.Cleanup(dr.Close)
	// The fake is the self-hosted registry an AGENTRY_TOKEN a test sets
	// authenticates to.
	t.Setenv(envRegistry, dr.URL)
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

// Export then import round-trips byte for byte: a built-in exported is its
// own document; a design imported is stored as the file holds it, and
// exported again is the same file. A valid document in another layout is
// stored and exported in the canonical one.
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
	if err != nil || !strings.Contains(out, "design:   Classic") || !strings.Contains(out, "report.theme in the CLI's config") || !strings.Contains(out, "your account at "+reg.URL+" has no report design") {
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

	// An account that cannot be read, or holds a design this CLI refuses,
	// is said to be so, never "has none".
	reg.broken = true
	out, errOut, err := reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:   Classic") || !strings.Contains(out, "could not be read") || !strings.Contains(errOut, "warning") {
		t.Errorf("an account that cannot be read: %q %q %v", out, errOut, err)
	}
	reg.broken = false
	reg.mu.Lock()
	reg.stored = bytes.Replace(house, []byte(`"agentryTheme": 2`), []byte(`"agentryTheme": 3`), 1)
	reg.mu.Unlock()
	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "design:   Classic") || !strings.Contains(out, "is not one this CLI can draw") {
		t.Errorf("an account design this CLI refuses: %q %v", out, err)
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

// The logo travels in the design: import sends a file's logo as the core
// stores it (NormalizeLogo's encoding, so the hash the CLI prints is the
// one saved), export asks for it and writes it, a round trip is byte for
// byte, show says there is one, and a file without a logo keeps the
// account's and says so. A run signed in takes the design with its logo.
func TestReportDesignLogo(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_test")
	reg := newDesignRegistry(t)
	dir := t.TempDir()

	crafted := testLogo(t, 300, 100, "agentry-816-marker")
	house := filepath.Join(dir, "house.json")
	mustWriteFile(t, house, withLogo(t, designOf(t, "modern", "House", "#0b5394"), crafted))
	out, errOut, err := reportDesign(t, reg, "import", house)
	if err != nil || errOut != "" || !strings.Contains(out, "Saved House") || strings.Contains(out, "keeps the logo") {
		t.Fatalf("import with a logo: %q %q %v", out, errOut, err)
	}
	reg.mu.Lock()
	logo := reg.logo
	reg.mu.Unlock()
	stored, _ := logo.Decode()
	if len(stored) == 0 || bytes.Contains(stored, []byte("agentry-816-marker")) {
		t.Fatalf("the registry stored the logo as %d bytes, marker kept: %v", len(stored), bytes.Contains(stored, []byte("agentry-816-marker")))
	}

	exported := filepath.Join(dir, "exported.json")
	if out, _, err := reportDesign(t, reg, "export", exported); err != nil || !strings.Contains(out, "House with its logo") {
		t.Fatalf("export: %q %v", out, err)
	}
	if reg.asked != "logo" {
		t.Errorf("export asked include=%q, want logo", reg.asked)
	}
	d, err := reportdesign.Parse(readFile(t, exported))
	if err != nil || d.Logo != logo {
		t.Fatalf("the exported design's logo is not the stored one (%v)", err)
	}
	if out, errOut, err := reportDesign(t, reg, "import", exported); err != nil || errOut != "" {
		t.Fatalf("import of the export: %q %q %v", out, errOut, err)
	}
	again := filepath.Join(dir, "again.json")
	if _, _, err := reportDesign(t, reg, "export", again); err != nil || !bytes.Equal(readFile(t, again), readFile(t, exported)) {
		t.Errorf("a round trip with a logo is not byte for byte (%v)", err)
	}

	out, _, err = reportDesign(t, reg, "show")
	if err != nil || !strings.Contains(out, "logo:     a PNG of 300×100 pixels") || !strings.Contains(out, "sha256:   "+d.SHA256()) {
		t.Errorf("show with a logo: %q %v", out, err)
	}
	out, _, err = reportDesign(t, reg, "show", "--json")
	var shown struct {
		Logo bool `json:"logo"`
	}
	if err != nil || json.Unmarshal([]byte(out), &shown) != nil || !shown.Logo {
		t.Errorf("show --json with a logo: %q %v", out, err)
	}
	choice, err := chooseReportTheme(t.Context(), reportFlags{}, cliConfig{}, readAccountTheme(reg.URL), io.Discard)
	got, perr := reportdesign.Parse(choice.Design)
	if err != nil || perr != nil || choice.From != engine.ThemeFromAccount || got != d {
		t.Errorf("a signed-in run's design = %+v (%v), want the account's with its logo", choice.From, err)
	}

	plain := filepath.Join(dir, "plain.json")
	mustWriteFile(t, plain, designOf(t, "classic", "No logo", "#1f3a5f"))
	out, errOut, err = reportDesign(t, reg, "import", plain)
	if err != nil || errOut != "" || !strings.Contains(out, "The file has no logo, so your account keeps the logo it has") {
		t.Errorf("import without a logo: %q %q %v", out, errOut, err)
	}
	if out, _, err := reportDesign(t, reg, "show"); err != nil || !strings.Contains(out, "design:   No logo") || !strings.Contains(out, "logo:     a PNG of 300×100") {
		t.Errorf("show after a logo-less import: %q %v", out, err)
	}

	if _, _, err := reportDesign(t, reg, "reset"); err != nil {
		t.Fatal(err)
	}
	if out, _, err := reportDesign(t, reg, "show"); err != nil || !strings.Contains(out, "logo:     none") {
		t.Errorf("show after reset: %q %v", out, err)
	}

	// A logo that is not a PNG is refused before the registry is asked.
	_, puts := reg.state()
	bad := filepath.Join(dir, "bad.json")
	mustWriteFile(t, bad, bytes.Replace(withLogo(t, designOf(t, "modern", "Bad", "#0b5394"), crafted), []byte(`"png": "iVBOR`), []byte(`"png": "/9j/4`), 1))
	if _, _, err := reportDesign(t, reg, "import", bad); exitOf(err) != 2 || !strings.Contains(err.Error(), "logo") {
		t.Errorf("a logo that is not a PNG = %v", err)
	}
	if _, after := reg.state(); after != puts {
		t.Error("a refused logo reached the registry")
	}
}
