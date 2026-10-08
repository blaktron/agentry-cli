package toolsrv

// export_pdf's tests (plan scratch-and-pdf-reports.md §3 invariants 3-6, 8,
// 9; agentry-cli#730). Most run the tool against a fake renderer, a shell
// script in Typst's place started the way Typst is (through this test
// binary as the confine trampoline, TestMain in confinement_test.go), so
// they hold the server's half on any Linux machine. The ones named for
// what Typst draws run the real renderer from the pinned image when
// AGENTRY_TYPST_DIR names its files (typst, fonts/, packages/), as the
// typst-image CI job does; AGENTRY_TYPST_REQUIRED=1 makes their absence a
// failure there.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// A test run as root, as CI's jobs are, starts the renderer as typstID,
// which must reach the trampoline and the renderer as it reaches the runner
// image's (/usr/local/bin, /opt/agentry/typst): the go-build and t.TempDir
// directories are root's alone. So, as root, both are staged in a directory
// anyone may enter, made once a test binary and removed by TestMain.
var openDir struct {
	sync.Mutex
	path   string
	copies map[string]string
}

// openTempDir is a new directory anyone may enter: under the shared one as
// root, a t.TempDir otherwise.
func openTempDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		return t.TempDir()
	}
	openDir.Lock()
	defer openDir.Unlock()
	if openDir.path == "" {
		d, err := os.MkdirTemp("", "agentry-toolsrv-test-")
		if err != nil {
			t.Fatal(err)
		}
		openDir.path, openDir.copies = d, map[string]string{}
	}
	d, err := os.MkdirTemp(openDir.path, "t-")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{openDir.path, d} {
		if err := os.Chmod(p, 0o755); err != nil { //nolint:gosec // entered by the renderer's own user
			t.Fatal(err)
		}
	}
	return d
}

// removeOpenDir removes the shared directory, when one was made.
func removeOpenDir() {
	if openDir.path != "" {
		_ = os.RemoveAll(openDir.path)
	}
}

// openCopy is src (a file or a tree) where the renderer's user can reach
// it: src itself unless the test runs as root, else a copy made once.
func openCopy(t *testing.T, key, src string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		return src
	}
	openDir.Lock()
	if c, ok := openDir.copies[key]; ok {
		openDir.Unlock()
		return c
	}
	openDir.Unlock()
	dst := filepath.Join(openTempDir(t), filepath.Base(src))
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p) //nolint:gosec // the test's own fixture
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o755) //nolint:gosec // run or read by the renderer's own user
	})
	if err != nil {
		t.Fatal(err)
	}
	openDir.Lock()
	openDir.copies[key] = dst
	openDir.Unlock()
	return dst
}

// openTrampoline is this test binary, the confine trampoline, where the
// renderer's user can start it.
func openTrampoline(t *testing.T) string {
	t.Helper()
	return openCopy(t, "trampoline", os.Args[0])
}

// fakeTypst is a renderer that runs body under sh with ROOT set to the
// render root (Typst's --root; the environment is empty), the bundled font
// and package folders empty. What it prints is the PDF.
func fakeTypst(t *testing.T, body string) typstFiles {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake renderer is a shell script")
	}
	dir := openTempDir(t)
	bin := filepath.Join(dir, "typst")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --root ] && ROOT=$2; shift; done\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test script
		t.Fatal(err)
	}
	for _, d := range []string{"fonts", "packages"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return typstFiles{bin: bin, fonts: filepath.Join(dir, "fonts"), packages: filepath.Join(dir, "packages")}
}

// fakePDF is a fake renderer's PDF: a header and a page tree, followed by
// whatever else the script prints.
const fakePDF = `printf '%%PDF-1.7\n1 0 obj <</Type/Pages/Count 3/Kids[]>> endobj\n'`

// pdfSpec is a release's run spec with the scratch folder and export_pdf.
func pdfSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.ScratchDir = t.TempDir()
	spec.InputsDir = t.TempDir()
	spec.PDF = PDF{Enabled: true, Ledger: filepath.Join(spec.OutDir, "receipts", "pdfs.ndjson")}
	return spec
}

// pdfServer is a server for spec rendering with typst, held as this
// machine holds a render.
func pdfServer(t *testing.T, spec *Spec, typst typstFiles) *server {
	t.Helper()
	s := newTestServer(t, spec)
	s.typst = typst
	abi, abiErr := confine.ABI()
	s.render = chooseRenderConfinement(runtime.GOOS, abi, abiErr, os.Geteuid(), openTrampoline(t))
	if s.render.refused != "" {
		t.Skipf("this machine cannot hold a render: %s", s.render.refused)
	}
	return s
}

func exportPDF(t *testing.T, s *server, args map[string]any) (string, bool) {
	t.Helper()
	return s.toolExportPDF(rawArgs(t, args))
}

func readReport(t *testing.T, spec *Spec, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(spec.OutDir, "reports", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExportPDFIsGrantedWithItsFlagAlone(t *testing.T) {
	spec := baseSpec(t)
	if slices.Contains(spec.GrantedTools(), ToolExportPDF) {
		t.Fatal("export_pdf granted without its flag")
	}
	spec.PDF.Enabled = true
	if got := spec.GrantedTools(); !slices.Contains(got, ToolExportPDF) {
		t.Fatalf("export_pdf not granted with its flag: %v", got)
	}
	s := newTestServer(t, spec)
	if _, ok := s.tools[ToolExportPDF]; !ok {
		t.Fatal("export_pdf not registered")
	}
	bad := baseSpec(t)
	bad.PDF = PDF{Ledger: filepath.Join(bad.OutDir, "receipts", "pdfs.ndjson")}
	if err := validateSpec(bad); err == nil {
		t.Error("a PDF ledger without the grant was accepted")
	}
	bad.PDF = PDF{Enabled: true, Ledger: "/elsewhere/pdfs.ndjson"}
	if err := validateSpec(bad); err == nil {
		t.Error("a PDF ledger outside the output folder was accepted")
	}
}

// TestExportPDFWritesAndRecords: the PDF lands in reports/ beside its
// Markdown (or at the top of reports/ for a scratch source), its answer
// names its pages, and the ledger records its path, SHA-256 and pages.
func TestExportPDFWritesAndRecords(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, fakePDF))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "sub", "summary.md"), []byte("# Summary\n\nText.\n"))
	mustWrite(t, filepath.Join(spec.ScratchDir, "notes", "draft.md"), []byte("# Draft\n"))

	got, isErr := exportPDF(t, s, map[string]any{"source": "reports/sub/summary.md"})
	if isErr || !strings.HasPrefix(got, "report written: reports/sub/summary.pdf (3 pages, 1 KiB; plain theme)") {
		t.Fatalf("export = (%q, %v)", got, isErr)
	}
	got, isErr = exportPDF(t, s, map[string]any{"source": "/scratch-is-not-this/x.md"})
	if !isErr {
		t.Errorf("a source outside reports/ and the scratch folder = (%q, %v)", got, isErr)
	}
	got, isErr = exportPDF(t, s, map[string]any{"source": spec.ScratchDir + "/notes/draft.md"})
	if isErr || !strings.HasPrefix(got, "report written: reports/draft.pdf ") {
		t.Fatalf("export from scratch = (%q, %v)", got, isErr)
	}
	got, isErr = exportPDF(t, s, map[string]any{"source": "scratch/notes/draft.md", "path": "reports/out/final.PDF"})
	if isErr || !strings.HasPrefix(got, "report written: reports/out/final.PDF ") {
		t.Fatalf("export to a path = (%q, %v)", got, isErr)
	}
	for _, bad := range []string{"final.md", "../final.pdf", "/etc/x.pdf/.."} {
		if got, isErr := exportPDF(t, s, map[string]any{"source": "sub/summary.md", "path": bad}); !isErr {
			t.Errorf("path %q = (%q, %v), want refused", bad, got, isErr)
		}
	}
	pdf := readReport(t, spec, "sub/summary.pdf")
	b, err := os.ReadFile(spec.PDF.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("ledger has %d lines, want 3:\n%s", len(lines), b)
	}
	var e PDFEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pdf)
	if e.Path != "reports/sub/summary.pdf" || e.SHA256 != hex.EncodeToString(sum[:]) || e.Pages != 3 || e.Bytes != int64(len(pdf)) {
		t.Errorf("ledger entry = %+v", e)
	}
	if _, err := os.Stat(filepath.Join(spec.OutDir, "reports", "sub", "summary.md")); err != nil {
		t.Errorf("the Markdown is not kept beside the PDF: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "agentry-pdf-*")); len(left) > 0 {
		for _, l := range left {
			if fi, err := os.Stat(l); err == nil && time.Since(fi.ModTime()) < time.Minute {
				t.Errorf("a render root was left behind: %s", l)
			}
		}
	}
}

// TestWriteReportRefusesAPDF (invariant 3): write_report writes no file
// named .pdf, and no content that would open as a PDF, appended or not;
// a report that mentions a PDF header past the first KiB is still text.
func TestWriteReportRefusesAPDF(t *testing.T) {
	spec := baseSpec(t)
	s := newTestServer(t, spec)
	for name, args := range map[string]map[string]any{
		"a .pdf name":        {"path": "x.pdf", "content": "text"},
		"a .PDF name":        {"path": "sub/x.PDF", "content": "text"},
		"a header":           {"path": "x.md", "content": "%PDF-1.7\n%âãÏÓ\n"},
		"a header after 900": {"path": "x.md", "content": strings.Repeat("a", 900) + "%PDF-1.4"},
	} {
		if got, isErr := s.toolWriteReport(rawArgs(t, args)); !isErr || !strings.Contains(got, "export_pdf") {
			t.Errorf("%s = (%q, %v), want refused, pointing at export_pdf", name, got, isErr)
		}
	}
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "short.md", "content": "short"})); isErr {
		t.Fatalf("a short report = %q", got)
	}
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "short.md", "content": "%PDF-1.7", "append": true})); !isErr {
		t.Errorf("appending a header into the first KiB = (%q, %v), want refused", got, isErr)
	}
	long := strings.Repeat("a", 1100) + "\nThe file begins `%PDF-1.7`.\n"
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "about-pdfs.md", "content": long})); isErr {
		t.Errorf("a report quoting a header past the first KiB = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports"))
	for _, e := range entries {
		if strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			t.Errorf("write_report left %s", e.Name())
		}
	}
	if b := readReport(t, spec, "short.md"); string(b) != "short" {
		t.Errorf("the refused append changed the report: %q", b)
	}
}

// TestTypstCannotWriteOutsideItsRenderRoot (invariant 3): the renderer
// writes nothing at all, its render root included (the PDF comes back on
// its stdout), so neither the reports folder nor the ledger.
func TestTypstCannotWriteOutsideItsRenderRoot(t *testing.T) {
	if _, err := confine.ABI(); err != nil {
		t.Skipf("no Landlock here (%v): the render runs as its own user, which TestChooseRenderConfinement covers", err)
	}
	spec := pdfSpec(t)
	outside := t.TempDir()
	reports := filepath.Join(spec.OutDir, "reports")
	s := pdfServer(t, spec, fakeTypst(t, fmt.Sprintf(`for d in %q %q %q "$ROOT"; do (echo x > "$d/planted") 2>/dev/null && echo "wrote $d" >&2; done
(echo x >> %q) 2>/dev/null && echo "wrote the ledger" >&2
(echo x >> "$ROOT/report.md") 2>/dev/null && echo "wrote the render root" >&2
[ -e "$ROOT/planted" ] && echo "PLANTED-IN-ROOT"
`, outside, reports, spec.ScratchDir, spec.PDF.Ledger)+"\n"+fakePDF))
	if err := os.MkdirAll(filepath.Dir(spec.PDF.Ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.PDF.Ledger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(reports, "r.md"), []byte("# R\n"))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"}); isErr {
		t.Fatalf("export = %q", got)
	}
	for _, d := range []string{outside, reports, spec.ScratchDir} {
		if _, err := os.Stat(filepath.Join(d, "planted")); err == nil {
			t.Errorf("the renderer wrote %s", d)
		}
	}
	if pdf := readReport(t, spec, "r.pdf"); bytes.Contains(pdf, []byte("PLANTED-IN-ROOT")) {
		t.Error("the renderer wrote its render root")
	}
	if b, _ := os.ReadFile(spec.PDF.Ledger); bytes.Contains(b, []byte("x\n")) && !bytes.HasPrefix(b, []byte("{")) {
		t.Errorf("the renderer wrote the ledger: %q", b)
	}
}

// TestTypstGetsNoEnvironment (invariant 4): the renderer starts with an
// empty environment, so none of the server's secrets.
func TestTypstGetsNoEnvironment(t *testing.T) {
	t.Setenv("AGENTRY_T730_SECRET", "the-value")
	spec := pdfSpec(t)
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.invalid.example"}, SecretEnv: []string{"AGENTRY_T730_SECRET"}}
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; env | grep -v '^PWD=' | grep -v '^SHLVL=' | grep -v '^_=' | grep -v '^OLDPWD=' | sed 's/^/ENV:/'`))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n"))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"}); isErr {
		t.Fatalf("export = %q", got)
	}
	if b := readReport(t, spec, "r.pdf"); bytes.Contains(b, []byte("ENV:")) {
		t.Errorf("the renderer had an environment:\n%s", b)
	}
}

// TestASymlinkNeverReachesTheRenderRoot (invariant 5): a source reached
// through a link is refused, and an image reached through one, at the end
// or in a folder on its way, is drawn as its alt text and never copied.
func TestASymlinkNeverReachesTheRenderRoot(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; (cd "$ROOT" && find . -type f | sort | sed 's/^/FILE:/'; cat meta.json)`))
	secretDir := t.TempDir()
	mustWrite(t, filepath.Join(secretDir, "secret.md"), []byte("# Secret\n"))
	mustWrite(t, filepath.Join(secretDir, "secret.png"), append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...))
	mustWrite(t, filepath.Join(spec.ScratchDir, "real.png"), append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...))
	for link, target := range map[string]string{
		filepath.Join(spec.ScratchDir, "linked.md"):  filepath.Join(secretDir, "secret.md"),
		filepath.Join(spec.ScratchDir, "linked.png"): filepath.Join(secretDir, "secret.png"),
		filepath.Join(spec.ScratchDir, "dir"):        secretDir,
		filepath.Join(spec.ScratchDir, "inner.png"):  filepath.Join(spec.ScratchDir, "real.png"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if got, isErr := exportPDF(t, s, map[string]any{"source": "scratch/linked.md"}); !isErr || !strings.Contains(got, "link") {
		t.Errorf("a linked source = (%q, %v), want refused", got, isErr)
	}
	if got, isErr := exportPDF(t, s, map[string]any{"source": "scratch/dir/secret.md"}); !isErr {
		t.Errorf("a source through a linked folder = (%q, %v), want refused", got, isErr)
	}
	mustWrite(t, filepath.Join(spec.ScratchDir, "r.md"), []byte("# R\n\n![one](linked.png) ![two](dir/secret.png) ![three](inner.png) ![four](real.png)\n"))
	got, isErr := exportPDF(t, s, map[string]any{"source": "scratch/r.md"})
	if isErr {
		t.Fatalf("export = %q", got)
	}
	for _, src := range []string{"linked.png", "dir/secret.png", "inner.png"} {
		if !strings.Contains(got, src+" (") {
			t.Errorf("the answer does not say %s was drawn as its alt text: %q", src, got)
		}
	}
	pdf := string(readReport(t, spec, "r.pdf"))
	files := regexp.MustCompile(`FILE:\./(\S+)`).FindAllStringSubmatch(pdf, -1)
	var names []string
	for _, f := range files {
		names = append(names, f[1])
	}
	if want := []string{"img/1.png", "main.typ", "meta.json", "report.md", "theme.json"}; !slices.Equal(names, want) {
		t.Errorf("the render root held %v, want %v", names, want)
	}
	if !strings.Contains(pdf, `"real.png":{"path":"img/1.png","name":"real.png"}`) || strings.Contains(pdf, `"linked.png"`) {
		t.Errorf("meta.json maps the wrong images:\n%s", pdf)
	}
}

// TestARenderRootRaceNeverWritesOutside (invariants 3 and 10): a declared
// command may write /tmp and may leave a process running, so it can plant
// a link in a render root while an export is under way. The server makes
// every name there new through an os.Root, so the export fails and
// nothing outside the root is written; the PDF itself comes from Typst's
// stdout, so there is no file to swap.
func TestARenderRootRaceNeverWritesOutside(t *testing.T) {
	for _, planted := range []string{"report.md", "main.typ", "meta.json", "img"} {
		t.Run(planted, func(t *testing.T) {
			spec := pdfSpec(t)
			s := pdfServer(t, spec, fakeTypst(t, fakePDF))
			outside := t.TempDir()
			victim := filepath.Join(outside, "calls.ndjson")
			mustWrite(t, victim, []byte("kept\n"))
			s.renderRootMade = func(dir string) {
				target := victim
				if planted == "img" {
					target = outside
				}
				if err := os.Symlink(target, filepath.Join(dir, planted)); err != nil {
					t.Fatal(err)
				}
			}
			png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
			mustWrite(t, filepath.Join(spec.OutDir, "reports", "i.png"), png)
			mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n\n![i](i.png)\n"))
			if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"}); !isErr {
				t.Errorf("an export whose render root was tampered with = %q, want refused", got)
			}
			if b, _ := os.ReadFile(victim); string(b) != "kept\n" {
				t.Errorf("the planted link was followed: %q", b)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 1 {
				t.Errorf("the folder outside holds %d entries, want the victim alone", len(entries))
			}
			if _, err := os.Stat(filepath.Join(spec.OutDir, "reports", "r.pdf")); err == nil {
				t.Error("a PDF was written from a tampered render root")
			}
		})
	}
}

// TestARemoteImageIsItsAltText (invariant 5, the server's half): an image
// with a scheme, or outside the run's folders, is never fetched or copied,
// and the answer says it was drawn as its alt text. TestRealRenderDrawsOnly
// LocalImages holds the template's half.
func TestARemoteImageIsItsAltText(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; cat "$ROOT/meta.json"`))
	mustWrite(t, filepath.Join(spec.InputsDir, "photo.jpg"), append([]byte("\xff\xd8\xff\xe0"), make([]byte, 32)...))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte(strings.Join([]string{
		"# R",
		"![remote](https://example.com/x.png) ![data](data:image/png;base64,AAAA) ![abs](/etc/hostname)",
		"![up](../../etc/passwd) ![input](" + spec.InputsDir + "/photo.jpg)",
		`<img src="http://example.com/y.png" alt="html">`,
		"[ref]: https://example.com/z.png",
	}, "\n")))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if isErr {
		t.Fatalf("export = %q", got)
	}
	for _, src := range []string{"https://example.com/x.png (not a local file)", "data:image/png;base64,AAAA (not a local file)", "/etc/hostname (outside", "../../etc/passwd (outside", "http://example.com/y.png (not a local file)"} {
		if !strings.Contains(got, src) {
			t.Errorf("the answer does not name %q: %q", src, got)
		}
	}
	pdf := string(readReport(t, spec, "r.pdf"))
	if !strings.Contains(pdf, `"images":{"`+spec.InputsDir+`/photo.jpg":{"path":"img/1.jpg","name":"photo.jpg"}}`) {
		t.Errorf("meta.json does not map the input image alone:\n%s", pdf)
	}
}

// TestExportPDFTimesOut (invariant 6): a render past its time is stopped,
// and the error names the limit.
func TestExportPDFTimesOut(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, `sleep 30`))
	s.pdfTimeout = 500 * time.Millisecond
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n"))
	start := time.Now()
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if !isErr || !strings.Contains(got, "longer than its 500ms limit") {
		t.Errorf("a slow render = (%q, %v)", got, isErr)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the slow render took %s to stop", took)
	}
	if s.callFault {
		t.Error("a render past its time is the agent's report, not the run's fault")
	}
}

// TestExportPDFMemoryCap (invariant 6): the renderer starts in a 1 GiB
// address space, and a render that runs out says so, naming the limit.
func TestExportPDFMemoryCap(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; echo "LIMIT:$(ulimit -v)"`))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n"))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"}); isErr {
		t.Fatalf("export = %q", got)
	}
	if pdf := readReport(t, spec, "r.pdf"); !bytes.Contains(pdf, []byte("LIMIT:1048576\n")) {
		t.Errorf("the renderer's address space was not 1 GiB:\n%s", pdf)
	}
	s.typst = fakeTypst(t, `echo "memory allocation of 2147483648 bytes failed" >&2; kill -ABRT $$`)
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if !isErr || !strings.Contains(got, "1024 MiB memory limit") {
		t.Errorf("a render out of memory = (%q, %v)", got, isErr)
	}
}

// TestExportPDFInputCaps (invariant 6): a source over 1 MiB, more than 20
// images and an image over 10 MiB are each refused, naming the limit.
func TestExportPDFInputCaps(t *testing.T) {
	spec := pdfSpec(t)
	s := pdfServer(t, spec, fakeTypst(t, fakePDF))
	reports := filepath.Join(spec.OutDir, "reports")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

	mustWrite(t, filepath.Join(reports, "big.md"), bytes.Repeat([]byte("a"), MaxPDFSourceBytes+1))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "big.md"}); !isErr || !strings.Contains(got, "1 MiB") {
		t.Errorf("a source over 1 MiB = (%q, %v)", got, isErr)
	}
	var md strings.Builder
	md.WriteString("# Many\n")
	for i := range MaxPDFImages + 1 {
		name := fmt.Sprintf("i%02d.png", i)
		mustWrite(t, filepath.Join(reports, name), png)
		fmt.Fprintf(&md, "![%d](%s)\n", i, name)
	}
	mustWrite(t, filepath.Join(reports, "many.md"), []byte(md.String()))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "many.md"}); !isErr || !strings.Contains(got, "more than 20 images") {
		t.Errorf("21 images = (%q, %v)", got, isErr)
	}
	mustWrite(t, filepath.Join(reports, "huge.png"), append(png, make([]byte, MaxPDFImageBytes)...))
	mustWrite(t, filepath.Join(reports, "huge.md"), []byte("# Huge\n\n![huge](huge.png)\n"))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "huge.md"}); !isErr || !strings.Contains(got, "10 MiB") {
		t.Errorf("an image over 10 MiB = (%q, %v)", got, isErr)
	}
	mustWrite(t, filepath.Join(reports, "bin.md"), []byte{0xff, 0xfe, 0x00})
	if got, isErr := exportPDF(t, s, map[string]any{"source": "bin.md"}); !isErr || !strings.Contains(got, "not UTF-8 text") {
		t.Errorf("a binary source = (%q, %v)", got, isErr)
	}
	if got, isErr := exportPDF(t, s, map[string]any{"source": "absent.md"}); !isErr || !strings.Contains(got, "write_report") {
		t.Errorf("a missing source = (%q, %v)", got, isErr)
	}
	if s.callFault {
		t.Error("an export over a limit is the agent's doing, not the run's fault")
	}
}

// TestExportPDFUsesTheRunTheme (invariant 9): the render gets the theme the
// engine stamped, byte for byte, and the model has no theme argument; a
// stamp whose document is not the one hashed never serves.
func TestExportPDFUsesTheRunTheme(t *testing.T) {
	plain, err := BuiltinTheme(DefaultTheme)
	if err != nil {
		t.Fatal(err)
	}
	custom := bytes.Replace(bytes.Replace(plain, []byte(`"name": "plain"`), []byte(`"name": "house"`), 1), []byte(`"marginMm": 20`), []byte(`"marginMm": 31`), 1)
	stamp, err := StampTheme(custom)
	if err != nil {
		t.Fatal(err)
	}
	spec := pdfSpec(t)
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; cat "$ROOT/theme.json"`))
	if props := exportPDFSchema()["properties"].(map[string]any); len(props) != 2 || props["theme"] != nil {
		t.Errorf("export_pdf takes %v, want source and path alone", props)
	}
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n"))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md", "theme": "formal"})
	if isErr || !strings.Contains(got, "house theme") {
		t.Fatalf("export = (%q, %v)", got, isErr)
	}
	if pdf := readReport(t, spec, "r.pdf"); !bytes.HasSuffix(pdf, custom) {
		t.Errorf("the render did not get the stamped theme:\n%s", pdf)
	}
	forged := *stamp
	forged.Document = strings.Replace(forged.Document, `"marginMm": 31`, `"marginMm": 32`, 1)
	spec.PDF.Theme = &forged
	if err := validateSpec(spec); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("a stamp whose document changed = %v", err)
	}
}

func TestParseTheme(t *testing.T) {
	plain, err := BuiltinTheme("plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTheme(plain); err != nil {
		t.Fatalf("plain: %v", err)
	}
	if _, err := BuiltinTheme("../plain"); err == nil {
		t.Error("a theme name that is a path was looked up")
	}
	for name, change := range map[string][2]string{
		"unknown field": {`"agentryTheme": 1,`, `"agentryTheme": 1, "script": "#read(\"/etc/passwd\")",`},
		"version 2":     {`"agentryTheme": 1`, `"agentryTheme": 2`},
		"system font":   {`"body": "Noto Sans"`, `"body": "DejaVu Sans"`},
		"colour":        {`"text": "#1a1a1a"`, `"text": "red"`},
		"paper":         {`"paper": "a4"`, `"paper": "a0"`},
		"margin":        {`"marginMm": 20`, `"marginMm": 2`},
		"page numbers":  {`"pageNumbers": "n"`, `"pageNumbers": "#counter(page)"`},
		"scale":         {`[1.7, 1.35, 1.15, 1.0, 1.0, 1.0]`, `[1.7]`},
	} {
		doc := bytes.Replace(plain, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(doc, plain) {
			t.Fatalf("%s: the change did not apply", name)
		}
		if _, err := ParseTheme(doc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestChooseRenderConfinement(t *testing.T) {
	noLandlock := errors.New("landlock: function not implemented")
	c := chooseRenderConfinement("linux", 4, nil, 0, "/usr/local/bin/agentry-runner")
	if name, argv := c.argv("/opt/agentry/typst/typst", []string{"compile"}); name != "/usr/local/bin/agentry-runner" ||
		strings.Join(argv, " ") != "confine --no-network --max-memory 1073741824 -- /opt/agentry/typst/typst compile" || !c.asTypst {
		t.Errorf("Landlock ABI 4 as root = %+v: %s %q", c, name, argv)
	}
	c = chooseRenderConfinement("linux", 3, nil, 1000, "/x/agentry")
	if _, argv := c.argv("/t", nil); strings.Contains(strings.Join(argv, " "), "--no-network") || c.asTypst {
		t.Errorf("Landlock ABI 3, not root = %+v %q: TCP cannot be refused, nor the user changed", c, argv)
	}
	if !strings.Contains(c.describe(), "TCP is not refused") {
		t.Errorf("describe = %q", c.describe())
	}
	c = chooseRenderConfinement("linux", 0, noLandlock, 0, "/usr/local/bin/agentry-runner")
	if _, argv := c.argv("/t", nil); strings.Join(argv, " ") != "confine --limits-only --max-memory 1073741824 -- /t" || !c.asTypst {
		t.Errorf("no Landlock as root = %+v %q, want its own user and the limits", c, argv)
	}
	if c := chooseRenderConfinement("linux", 0, noLandlock, 1000, "/x"); c.refused == "" {
		t.Errorf("no Landlock, not root = %+v, want refused", c)
	}
	if c := chooseRenderConfinement("darwin", 0, noLandlock, 501, "/x"); !c.none {
		t.Errorf("off Linux = %+v", c)
	}
}

func TestScanMarkdown(t *testing.T) {
	src := strings.Join([]string{
		"Intro",
		"",
		"### Starts *deep* ###",
		"```md",
		"# not a heading",
		"![nope](in-fence.png)",
		"```",
		"##### Five",
		"> #### Quoted",
		"Setext two",
		"----------",
		"- ###### In a list",
		"#hashtag is text",
		"    # indented code",
		"",
		"---",
		"![a](one.png) ![b](<two words.png>) ![c](three.png \"title\") ![a](one.png)",
		`<img alt="x" src='four.png'>`,
		"![shortcut][Five  X] [a link][notes]",
		"[ref]: five%20x.png",
		"[five x]: six.png",
		"[notes]: ./notes.md",
	}, "\n")
	doc := scanMarkdown(src)
	want := strings.Join([]string{
		"Intro",
		"",
		"# Starts *deep* ###",
		"```md",
		"# not a heading",
		"![nope](in-fence.png)",
		"```",
		"## Five",
		"> ### Quoted",
		"Setext two",
		"----------",
		"- ### In a list",
		"#hashtag is text",
		"    # indented code",
		"",
		"---",
		"![a](one.png) ![b](<two words.png>) ![c](three.png \"title\") ![a](one.png)",
		`<img alt="x" src='four.png'>`,
		"![shortcut][Five  X] [a link][notes]",
		"[ref]: five%20x.png",
		"[five x]: six.png",
		"[notes]: ./notes.md",
	}, "\n")
	if doc.text != want {
		t.Errorf("renumbered:\n%s\nwant:\n%s", doc.text, want)
	}
	if doc.title != "Starts deep" {
		t.Errorf("title = %q", doc.title)
	}
	// In the scanner's order: inline images, reference definitions, raw
	// HTML; the template looks each up by name, so the order is no matter.
	// A reference definition no image names (ref, notes) is a link's.
	if want := []string{"in-fence.png", "one.png", "two words.png", "three.png", "six.png", "four.png"}; !slices.Equal(doc.images, want) {
		t.Errorf("images = %q, want %q", doc.images, want)
	}
	if got := scanMarkdown("Title\n-----\n\n### Next\n").text; got != "Title\n=====\n\n## Next\n" {
		t.Errorf("a first setext heading of level 2 = %q", got)
	}
	if got := imageCandidates("five%20x.png"); !slices.Equal(got, []string{"five%20x.png", "five x.png"}) {
		t.Errorf("candidates = %q", got)
	}
	for _, remote := range []string{"https://x/y.png", "data:image/png;base64,AA", "C:x"} {
		if got := imageCandidates(remote); got != nil {
			t.Errorf("candidates of %q = %q, want none", remote, got)
		}
	}
}

// ---- the real renderer ----

// realTypst is the pinned image's renderer, or a skip (a failure where
// AGENTRY_TYPST_REQUIRED=1).
func realTypst(t *testing.T) typstFiles {
	t.Helper()
	dir := os.Getenv("AGENTRY_TYPST_DIR")
	if dir == "" {
		if os.Getenv("AGENTRY_TYPST_REQUIRED") == "1" {
			t.Fatal("AGENTRY_TYPST_DIR is not set, and this job must render with the real Typst")
		}
		t.Skip("AGENTRY_TYPST_DIR is not set: the real renderer is the typst-image CI job's")
	}
	dir = openCopy(t, "typst", dir)
	return typstFiles{bin: filepath.Join(dir, "typst"), fonts: filepath.Join(dir, "fonts"), packages: filepath.Join(dir, "packages")}
}

func realExport(t *testing.T, md string, files map[string][]byte) (*Spec, []byte) {
	t.Helper()
	return realExportIn(t, DefaultTheme, md, files)
}

// realExportIn is realExport under a built-in theme.
func realExportIn(t *testing.T, theme, md string, files map[string][]byte) (*Spec, []byte) {
	t.Helper()
	doc, err := BuiltinTheme(theme)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := StampTheme(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec := pdfSpec(t)
	spec.PDF.Author = "acme/demo@1.2.3"
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, realTypst(t))
	for name, b := range files {
		mustWrite(t, filepath.Join(spec.OutDir, "reports", name), b)
	}
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte(md))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if isErr {
		t.Fatalf("export = %q", got)
	}
	return spec, readReport(t, spec, "r.pdf")
}

var pdfURI = regexp.MustCompile(`/URI\s*\(([^)]*)\)`)

func uris(pdf []byte) []string {
	var out []string
	for _, m := range pdfURI.FindAllSubmatch(pdf, -1) {
		out = append(out, string(m[1]))
	}
	slices.Sort(out)
	return out
}

// TestRawTypstIsText (invariant 5): Typst markup in the Markdown, in its
// text or in a raw-typst comment, an @preview import included, is never
// run. Each line would stop the render if it ran (#panic, a package the
// renderer cannot download, a file outside the render root), so the render
// succeeding is the proof; and none of the links it would make is there.
// Under every theme (agentry-cli#731): the first heading, markup included,
// is Formal's cover title and Letter's letterhead, and Letter's kept lines
// put a backslash after short ones.
func TestRawTypstIsText(t *testing.T) {
	for _, theme := range BuiltinThemes() {
		t.Run(theme, func(t *testing.T) { rawTypstIsText(t, theme) })
	}
}

func rawTypstIsText(t *testing.T, theme string) {
	_, pdf := realExportIn(t, theme, strings.Join([]string{
		`# Injection #panic("heading ran") #link("https://heading.example")[h]`,
		"",
		`#panic("short line ran")`,
		`#read("/etc/passwd")`,
		`#link("https://text-markup.example")[x] #import "@preview/whatever:0.1.0": * #read("/etc/passwd") #panic("text ran")`,
		"",
		`<!--raw-typst #link("https://raw-typst.example")[y] -->`,
		"",
		`<!--raw-typst #panic("raw typst ran") -->`,
		"",
		`<!--raw-typst #import "@preview/whatever:0.1.0": * -->`,
		"",
		"$ #link(\"https://math.example\")[z] #panic(\"math ran\") $",
	}, "\n"), nil)
	if got := uris(pdf); len(got) != 0 {
		t.Errorf("Typst code in the Markdown ran: links %q", got)
	}
}

// TestLinksAreHttpOrMail (invariant 5): http, https and mailto stay links;
// every other scheme, an anchor and a link in raw HTML are text, under
// every theme (agentry-cli#731).
func TestLinksAreHttpOrMail(t *testing.T) {
	for _, theme := range BuiltinThemes() {
		t.Run(theme, func(t *testing.T) { linksAreHttpOrMail(t, theme) })
	}
}

func linksAreHttpOrMail(t *testing.T, theme string) {
	_, pdf := realExportIn(t, theme, strings.Join([]string{
		"# Links",
		"[a](https://a.example) [b](http://b.example) [c](mailto:c@example.com) <https://d.example>",
		"[e](javascript:alert(1)) [f](file:///etc/passwd) [g](#links) [h](ftp://h.example) [i](data:text/html,x)",
		`<a href="javascript:x">j</a> <a href="https://k.example">k</a>`,
	}, "\n"), nil)
	want := []string{"http://b.example", "https://a.example", "https://d.example", "https://k.example", "mailto:c@example.com"}
	if got := uris(pdf); !slices.Equal(got, want) {
		t.Errorf("links = %q, want %q", got, want)
	}
}

// TestRealRenderDrawsOnlyLocalImages (invariant 5, the template's half): a
// remote image is its alt text in the PDF, and a local one is drawn.
func TestRealRenderDrawsOnlyLocalImages(t *testing.T) {
	png, err := os.ReadFile(filepath.Join("testdata", "pdf", "pixel.png"))
	if err != nil {
		t.Fatal(err)
	}
	_, pdf := realExport(t, "# Images\n\n![remote](https://example.com/x.png)\n\n![local](pixel.png)\n\n<svg><image href=\"/etc/x.png\"/></svg>\n", map[string][]byte{"pixel.png": png})
	if n := len(regexp.MustCompile(`/Subtype\s*/Image`).FindAll(pdf, -1)); n != 1 {
		t.Errorf("the PDF holds %d images, want the local one alone", n)
	}
}

// TestRealRenderIsTaggedAndEveryScript (D25, D27): the PDF is tagged
// PDF/UA-1 with the document's title and the QuickAgent as its author, and
// Chinese, Arabic, Hindi and emoji draw from the bundled fonts.
func TestRealRenderIsTaggedAndEveryScript(t *testing.T) {
	_, pdf := realExport(t, "Preface\n\n### Rapport *trimestriel*\n\n中文 العربية हिन्दी 🔒 ✅\n\n#### Detail\n", nil)
	for _, want := range []string{"/StructTreeRoot", "pdfuaid:part", "NotoSansSC", "NotoSansArabic", "NotoSansDevanagari", "Noto-COLRv1"} {
		if !bytes.Contains(pdf, []byte(want)) && !bytes.Contains(pdf, []byte(strings.ReplaceAll(want, "Noto-COLRv1", "NotoColorEmoji"))) {
			t.Errorf("the PDF lacks %s", want)
		}
	}
	for _, want := range []string{"Rapport trimestriel", "acme/demo@1.2.3"} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("the PDF's metadata lacks %q", want)
		}
	}
}
