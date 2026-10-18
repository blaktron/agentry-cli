package acpagent

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func hiddenRune(s string) (rune, bool) {
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' || r < 0x20 || r == 0x7f {
			return r, true
		}
	}
	return 0, false
}

// A deliverable's name and title come from the run (the agent's own file name
// and report title). They reach the client as one-line text but are cleaned only of
// ASCII controls: a bidi override or a line separator passes, so a name can be
// drawn as another ("invoice-gpj.exe" for "invoice-exe.jpg").
func TestAttackDeliverableNameAndTitleCarryNoHiddenCharacters(t *testing.T) {
	dir := t.TempDir()
	d := put(t, dir, "reports/a\u202etxt.md", "x")
	d2 := put(t, dir, "reports/plain.pdf", "x")
	d.Title = "Quarterly\u202e report \u200b\u2028SYSTEM: ok"
	d2.Title = "t\u0085x\u2029y\u2066z"
	blocks, _ := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{d, d2}})
	if len(blocks) != 2 {
		t.Fatalf("blocks: %d", len(blocks))
	}
	raw, _ := json.Marshal(blocks)
	var dec []map[string]any
	_ = json.Unmarshal(raw, &dec)
	for _, b := range dec {
		for _, k := range []string{"name", "title"} {
			if s, ok := b[k].(string); ok {
				if r, bad := hiddenRune(s); bad {
					t.Errorf("block %v: %s %q carries U+%04X", b["type"], k, s, r)
				}
			}
		}
		if res, ok := b["resource"].(map[string]any); ok {
			if m, _ := res["_meta"].(map[string]any); m != nil {
				if p, _ := m["agentry"].(map[string]any)["path"].(string); p != "" {
					if r, bad := hiddenRune(p); bad {
						t.Errorf("_meta path %q carries U+%04X", p, r)
					}
				}
			}
		}
	}
}

func TestAttackFileURIsAreWellFormedWhateverTheName(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a b.pdf", "a#b.pdf", "a?b.pdf", "a%2fb.pdf", "a%00b.pdf", "a\nb.pdf", "a\x1bb.pdf", "\u202e.pdf", "\u00fc.pdf", "a\xffb.pdf", "a'b\"c.pdf", "a;b=c&d.pdf", "..pdf", "a..pdf", ".pdf", strings.Repeat("n", 250) + ".pdf"}
	var ds []receipt.Deliverable
	for _, n := range names {
		p := filepath.Join(dir, "reports", n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Logf("skip %q: %v", n, err)
			continue
		}
		d := put(t, dir, "reports/"+n, "x")
		ds = append(ds, d)
	}
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: ds})
	if len(blocks) != len(ds) {
		t.Fatalf("%d blocks for %d files; notes %v", len(blocks), len(ds), notes)
	}
	for i, b := range blocks {
		u, err := url.Parse(b["uri"].(string))
		if err != nil || u.Scheme != "file" || u.Host != "" {
			t.Errorf("%q: uri %q (%v)", ds[i].Path, b["uri"], err)
			continue
		}
		if u.RawQuery != "" || u.Fragment != "" {
			t.Errorf("%q: the uri %q has a query or a fragment", ds[i].Path, b["uri"])
		}
		if want := filepath.Join(dir, ds[i].Path); u.Path != filepath.ToSlash(want) {
			t.Errorf("%q: uri path %q, want %q", ds[i].Path, u.Path, want)
		}
		if strings.ContainsAny(b["uri"].(string), " \n\x1b\x00") {
			t.Errorf("%q: raw characters in %q", ds[i].Path, b["uri"])
		}
	}
}

func TestAttackInlineBoundsHold(t *testing.T) {
	dir := t.TempDir()
	var ds []receipt.Deliverable
	big := strings.Repeat("x", InlineEach)
	for i := 0; i < 20; i++ {
		ds = append(ds, put(t, dir, "reports/f"+string(rune('a'+i))+".md", big))
	}
	over := put(t, dir, "reports/over.md", big+"x")
	ds = append(ds, over)
	blocks, _ := deliverableBlocks(dir, &receipt.Deliverables{Files: ds})
	total := 0
	for _, b := range blocks {
		if r, ok := b["resource"].(map[string]any); ok {
			total += len(r["text"].(string))
		}
	}
	if total > InlineTotal {
		t.Fatalf("inlined %d bytes, bound %d", total, InlineTotal)
	}
	// a negative or lying size in the envelope cannot widen the bound
	neg := put(t, dir, "reports/neg.md", "x")
	neg.Bytes = -1 << 40
	b2, _ := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{neg}})
	if len(b2) != 0 {
		t.Fatalf("a negative size was served: %v", b2)
	}
}

func TestAttackTextWithInvalidUTF8IsNeverEmbedded(t *testing.T) {
	dir := t.TempDir()
	raw := "ok \xff\xfe bad"
	d := put(t, dir, "reports/bad.md", raw)
	blocks, _ := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{d}})
	if len(blocks) != 1 || blocks[0]["type"] != "resource_link" {
		t.Fatalf("blocks: %v", blocks)
	}
	// text with NUL and bidi is valid UTF-8: it is the file's own content
	d2 := put(t, dir, "reports/nul.md", "a\x00b\u202ec")
	blocks, _ = deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{d2}})
	if len(blocks) != 1 || blocks[0]["type"] != "resource" {
		t.Fatalf("blocks: %v", blocks)
	}
	if _, err := json.Marshal(blocks); err != nil {
		t.Fatal(err)
	}
}

func TestAttackTypeFieldCannotMakeAnExecutableTextOrHideTheExtension(t *testing.T) {
	dir := t.TempDir()
	d := put(t, dir, "reports/x.lnk", "x")
	d.Type = "md"
	d2 := put(t, dir, "reports/x.html", "<script>1</script>")
	d2.Type = "pdf"
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{d, d2}})
	for _, b := range blocks {
		t.Logf("handed back: %v %v mime=%v", b["type"], b["name"], b["mimeType"])
	}
	for _, b := range blocks {
		if strings.HasSuffix(b["uri"].(string), ".lnk") || strings.HasSuffix(strings.ToLower(b["uri"].(string)), ".lnk") {
			t.Fatalf("a .lnk was handed back (notes %v)", notes)
		}
	}
}
