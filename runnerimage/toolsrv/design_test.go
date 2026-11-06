package toolsrv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// designSpec is a spec with the Report Designer's grant, its ledger and
// previews folder, and an inputs folder holding Boardroom as the design to
// start from.
func designSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.ScratchDir = t.TempDir()
	spec.InputsDir = t.TempDir()
	spec.Design = Design{Enabled: true, Ledger: filepath.Join(spec.OutDir, "receipts", "previews.ndjson"), Previews: filepath.Join(spec.OutDir, "receipts", "previews")}
	doc, _ := reportdesign.BuiltinDocument("boardroom")
	mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(DesignInputFile)), doc)
	return spec
}

// callForItems makes one tools/call through the protocol and answers the
// result's content items and whether it is an error.
func callForItems(t *testing.T, s *server, name string, args map[string]any) ([]map[string]any, bool) {
	t.Helper()
	var out bytes.Buffer
	s.out = &out
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	s.handleToolsCall(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(1)), Method: "tools/call", Params: params})
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("response %q: %v", out.String(), err)
	}
	r := resultOf(t, m)
	var items []map[string]any
	for _, c := range r["content"].([]any) {
		items = append(items, c.(map[string]any))
	}
	isErr, _ := r["isError"].(bool)
	return items, isErr
}

func TestTheDesignToolsAreGrantedWithTheirFlagAlone(t *testing.T) {
	spec := pdfSpec(t)
	designTools := []string{ToolRenderPreview, ToolCheckDesign, ToolViewReference, ToolDocumentPages, ToolCropLogo}
	for _, tool := range designTools {
		if slices.Contains(spec.GrantedTools(), tool) {
			t.Errorf("%s granted with export_pdf's grant", tool)
		}
	}
	spec = designSpec(t)
	granted := spec.GrantedTools()
	for _, tool := range designTools {
		if !slices.Contains(granted, tool) {
			t.Errorf("%s not granted with the Designer's grant", tool)
		}
	}
	if slices.Contains(granted, ToolExportPDF) {
		t.Error("the Designer's grant brings export_pdf")
	}
	// A ledger without the grant, under reports/ or outside the output
	// folder, and the grant without inputs, are refused.
	bad := []func(*Spec){
		func(s *Spec) { s.Design.Enabled = false },
		func(s *Spec) { s.Design.Ledger = filepath.Join(s.OutDir, "reports", "previews.ndjson") },
		func(s *Spec) { s.Design.Previews = filepath.Join(s.OutDir, "reports") },
		func(s *Spec) { s.Design.Previews = "/tmp/previews" },
		func(s *Spec) { s.Design.DocumentLedger = filepath.Join(s.OutDir, "reports", "document.ndjson") },
		func(s *Spec) {
			s.Design.Enabled, s.Design.Ledger, s.Design.Previews, s.Design.DocumentLedger = false, "", "", filepath.Join(s.OutDir, "receipts", "d.ndjson")
		},
		func(s *Spec) { s.InputsDir = "" },
	}
	for i, f := range bad {
		s := designSpec(t)
		f(s)
		if err := validateSpec(s); err == nil {
			t.Errorf("case %d: validateSpec accepted %+v", i, s.Design)
		}
	}
}

// TestRenderPreviewAnswersAnImage: the preview is a PNG image item beside
// the text, kept under the receipts folder and recorded, with nothing
// written under reports/.
func TestRenderPreviewAnswersAnImage(t *testing.T) {
	typst := realTypst(t)
	spec := designSpec(t)
	mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(DesignImagesDir), "1.png"), coverPNG(t))
	s := pdfServer(t, spec, typst)
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "cover.typ"), []byte(goodCover))
	items, isErr := callForItems(t, s, ToolRenderPreview, map[string]any{"cover": "reports/cover.typ", "title": "Northwind annual report"})
	if isErr || len(items) != 2 {
		t.Fatalf("render_preview = %v (error %v), want text and an image", items, isErr)
	}
	text, img := items[0], items[1]
	if text["type"] != "text" || !strings.Contains(text["text"].(string), "an export would draw it") || !strings.Contains(text["text"].(string), "11 left") {
		t.Errorf("text item %v", text)
	}
	if img["type"] != "image" || img["mimeType"] != "image/png" {
		t.Fatalf("image item %v", img["type"])
	}
	b, err := base64.StdEncoding.DecodeString(img["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	page, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if w := page.Bounds().Dx(); w < 850 || w > 960 {
		t.Errorf("the preview is %d pixels wide, want an A4 page at %d dpi", w, PreviewPPI)
	}
	d, _ := reportdesign.Builtin("boardroom")
	textOn(t, "preview", page, d.Accent)
	kept, err := os.ReadFile(filepath.Join(spec.Design.Previews, "preview-1.png"))
	if err != nil || !bytes.Equal(kept, b) {
		t.Errorf("the preview was not kept as sent (%v)", err)
	}
	ledger, err := os.ReadFile(spec.Design.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var e PreviewEntry
	if err := json.Unmarshal(bytes.TrimSpace(ledger), &e); err != nil {
		t.Fatal(err)
	}
	if e.N != 1 || e.Cover != "custom" || e.CoverSHA256 == "" || e.PNG != "preview-1.png" || e.CoverFailure != "" {
		t.Errorf("ledger entry %+v", e)
	}
	entries, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports"))
	if len(entries) != 1 {
		t.Errorf("reports/ holds %d files, want the cover alone", len(entries))
	}
}

// TestRenderPreviewNamesAFailingCover: a cover an export would pass over
// is named with its kind, Typst's diagnostics go to the agent, whose code
// it is, and the page shows the built-in cover the export would draw.
func TestRenderPreviewNamesAFailingCover(t *testing.T) {
	typst := realTypst(t)
	spec := designSpec(t)
	s := pdfServer(t, spec, typst)
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "cover.typ"), []byte("#import \"@preview/cetz:0.4.2\": canvas\n"+goodCover))
	items, isErr := callForItems(t, s, ToolRenderPreview, map[string]any{"cover": "reports/cover.typ"})
	if isErr {
		t.Fatalf("render_preview = %v", items)
	}
	got := items[0]["text"].(string)
	if !strings.Contains(got, "would pass this custom cover over") || !strings.Contains(got, "failed to download package") {
		t.Errorf("answer %q", got)
	}
	if len(items) != 2 {
		t.Errorf("the built-in cover the export would draw was not shown")
	}
}

// TestRenderPreviewStopsAtItsCap: the thirteenth render is refused.
func TestRenderPreviewStopsAtItsCap(t *testing.T) {
	spec := designSpec(t)
	s := newTestServer(t, spec)
	s.previews = MaxPreviews
	got, isErr := s.toolRenderPreview(rawArgs(t, map[string]any{}))
	if !isErr || !strings.Contains(got, "12 renders") {
		t.Errorf("render_preview past the cap = %q, %v", got, isErr)
	}
}

// TestRenderPreviewTakesItsArgumentsAlone: an argument it does not know,
// and a fact that is Markdown, are refused.
func TestRenderPreviewTakesItsArgumentsAlone(t *testing.T) {
	s := newTestServer(t, designSpec(t))
	for _, args := range []map[string]any{{"accent": "#000000"}, {"title": "# Two titles"}, {"page": "back"}} {
		if got, isErr := s.toolRenderPreview(rawArgs(t, args)); !isErr {
			t.Errorf("render_preview(%v) = %q, want a refusal", args, got)
		}
	}
}

func TestCheckDesign(t *testing.T) {
	spec := designSpec(t)
	s := newTestServer(t, spec)
	d, _ := reportdesign.Builtin("modern")
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "design.json"), d.Canonical())
	got, isErr := s.toolCheckDesign(rawArgs(t, map[string]any{}))
	if isErr || !strings.Contains(got, "passes: a valid design") || !strings.Contains(got, "accent "+d.Accent) {
		t.Errorf("check_design of a built-in = %q, %v", got, isErr)
	}
	// A pale accent under a band title block carries white text, so it
	// fails, and the answer offers a shade that passes.
	var m map[string]any
	_ = json.Unmarshal(d.Canonical(), &m)
	m["accent"], m["titleBlock"] = "#9ec5ff", "band"
	pale, _ := json.Marshal(m)
	mustWrite(t, filepath.Join(spec.ScratchDir, "pale.json"), pale)
	got, isErr = s.toolCheckDesign(rawArgs(t, map[string]any{"path": "scratch/pale.json"}))
	if isErr || !strings.Contains(got, "does not pass") || !strings.Contains(got, "the nearest shade that passes is #") {
		t.Errorf("check_design of a pale accent = %q, %v", got, isErr)
	}
	// The design it was given is checkable too, and nothing outside the
	// three folders is.
	if got, isErr := s.toolCheckDesign(rawArgs(t, map[string]any{"path": "inputs/" + DesignInputFile})); isErr || !strings.Contains(got, "passes") {
		t.Errorf("check_design of the input = %q", got)
	}
	if got, isErr := s.toolCheckDesign(rawArgs(t, map[string]any{"path": "reports/../../etc/passwd"})); !isErr {
		t.Errorf("check_design outside its folders = %q", got)
	}
}

func TestViewReference(t *testing.T) {
	spec := designSpec(t)
	s := newTestServer(t, spec)
	if got, isErr := s.toolViewReference(rawArgs(t, map[string]any{"name": "references/1.png"})); !isErr || !strings.Contains(got, "no reference images") {
		t.Errorf("no references: %q", got)
	}
	ref := coverPNG(t)
	mustWrite(t, filepath.Join(spec.InputsDir, DesignReferencesDir, "1.png"), ref)
	items, isErr := callForItems(t, s, ToolViewReference, map[string]any{"name": "references/1.png"})
	if isErr || len(items) != 2 || items[1]["type"] != "image" {
		t.Fatalf("view_reference = %v, %v", items, isErr)
	}
	if b, _ := base64.StdEncoding.DecodeString(items[1]["data"].(string)); !bytes.Equal(b, ref) {
		t.Error("the image is not the reference")
	}
	for _, name := range []string{"design/design.json", "references/2.png", "../references/1.png"} {
		if got, isErr := s.toolViewReference(rawArgs(t, map[string]any{"name": name})); !isErr {
			t.Errorf("view_reference(%s) = %q", name, got)
		}
	}
	s.referenceViews = MaxReferenceViews
	if _, isErr := s.toolViewReference(rawArgs(t, map[string]any{"name": "references/1.png"})); !isErr {
		t.Error("a view past the cap was shown")
	}
	// A call that answers no image carries a text item alone.
	items, _ = callForItems(t, s, ToolCheckDesign, map[string]any{"path": "inputs/" + DesignInputFile})
	if len(items) != 1 {
		t.Errorf("check_design answered %d items", len(items))
	}
}
