package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func put(t *testing.T, dir, rel, content string) receipt.Deliverable {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	typ := strings.TrimPrefix(strings.ToLower(filepath.Ext(rel)), ".")
	return receipt.Deliverable{Path: rel, Title: "t " + filepath.Base(rel), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Type: typ, Marked: true}
}

func kinds(blocks []map[string]any) []string {
	var k []string
	for _, b := range blocks {
		k = append(k, b["type"].(string))
	}
	return k
}

func TestADeliverableThatLeavesTheRunFolderIsNamedAndNeverHandedBack(t *testing.T) {
	dir := t.TempDir()
	good := put(t, dir, "reports/ok.md", "fine")
	outside := filepath.Join(filepath.Dir(dir), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("secret"))
	digest := hex.EncodeToString(sum[:])
	hostile := []receipt.Deliverable{good}
	for _, p := range []string{"../secret.md", "reports/../../secret.md", outside, "/etc/passwd", "result.json", "reports/../receipts/events.ndjson", `reports\..\secret.md`} {
		hostile = append(hostile, receipt.Deliverable{Path: p, Title: "x", SHA256: digest, Bytes: 6, Type: "md"})
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "reports", "link.md")); err != nil {
			t.Fatal(err)
		}
		hostile = append(hostile, receipt.Deliverable{Path: "reports/link.md", Title: "link", SHA256: digest, Bytes: 6, Type: "md"})
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "reports", "up")); err != nil {
			t.Fatal(err)
		}
		hostile = append(hostile, receipt.Deliverable{Path: "reports/up/secret.md", Title: "up", SHA256: digest, Bytes: 6, Type: "md"})
	}
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: hostile})
	if len(blocks) != 1 || kinds(blocks)[0] != "resource" {
		t.Fatalf("blocks %v", kinds(blocks))
	}
	for _, b := range blocks {
		if strings.Contains(strings.ToLower(toJSON(b)), "secret") {
			t.Fatalf("a file outside the run folder was handed back: %s", toJSON(b))
		}
	}
	if len(notes) != len(hostile)-1 {
		t.Fatalf("%d of %d refused files were named: %q", len(notes), len(hostile)-1, notes)
	}
	// Each one is named with its title and the reason it was refused.
	var want []string
	for i := 0; i < 7; i++ {
		want = append(want, `"x" was not handed back: it is not under the run's reports folder.`)
	}
	if runtime.GOOS != "windows" {
		want = append(want, `"link" was not handed back: it is, or sits behind, a link.`, `"up" was not handed back: it is, or sits behind, a link.`)
	}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(notes, "\n"), strings.Join(want, "\n"))
	}
}

func TestAChangedDeliverableIsWithheld(t *testing.T) {
	dir := t.TempDir()
	f := put(t, dir, "reports/a.md", "before")
	put(t, dir, "reports/a.md", "after!")
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{f}})
	if len(blocks) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "changed") {
		t.Fatalf("blocks=%v notes=%q", kinds(blocks), notes)
	}
	if want := `"t a.md" was not handed back: it is not the file the run listed (its bytes changed).`; notes[0] != want {
		t.Fatalf("note %q, want %q", notes[0], want)
	}

	// Every reason a file is refused has its own words; anything else could
	// not be checked.
	for reason, why := range map[string]string{
		receipt.DeliveryErrChanged:    "it is not the file the run listed (its bytes changed)",
		receipt.DeliveryErrMissing:    "it is not in the run's folder",
		receipt.DeliveryErrOutside:    "it is not under the run's reports folder",
		receipt.DeliveryErrLink:       "it is, or sits behind, a link",
		receipt.DeliveryErrUnreadable: "it could not be read",
		receipt.DeliveryErrType:       "its type is one a file manager acts on when it is merely viewed",
		receipt.DeliveryErrTooLarge:   "it is too large to hand back in a message",
		"a reason with no words":      "it could not be checked",
	} {
		if got, want := withheld("t", &deliver.ReadError{Reason: reason}), fmt.Sprintf("%q was not handed back: %s.", "t", why); got != want {
			t.Errorf("%s: %q, want %q", reason, got, want)
		}
	}
	if got, want := withheld("t", errors.New("boom")), `"t" was not handed back: it could not be checked.`; got != want {
		t.Errorf("an error that is not a refusal: %q, want %q", got, want)
	}
}

func TestTextIsEmbeddedWithinTheBoundsAndTheRestIsLinked(t *testing.T) {
	dir := t.TempDir()
	var files []receipt.Deliverable
	files = append(files, put(t, dir, "reports/small.md", "# small"))
	files = append(files, put(t, dir, "reports/big.md", strings.Repeat("a", InlineEach+1)))
	files = append(files, put(t, dir, "reports/bin.json", "\xff\xfe not utf-8"))
	files = append(files, put(t, dir, "reports/deck.pptx", "PK"))
	files = append(files, put(t, dir, "reports/odd.xyz", "z"))
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: files})
	if got := strings.Join(kinds(blocks), ","); got != "resource,resource_link,resource_link,resource_link,resource_link" || len(notes) != 0 {
		t.Fatalf("%s %q", got, notes)
	}
	if blocks[3]["mimeType"] != "application/vnd.openxmlformats-officedocument.presentationml.presentation" || blocks[4]["mimeType"] != "application/octet-stream" {
		t.Fatalf("mime types: %v %v", blocks[3]["mimeType"], blocks[4]["mimeType"])
	}
	// Many mid-size texts: the total is bounded.
	dir2 := t.TempDir()
	var many []receipt.Deliverable
	for i := 0; i < 8; i++ {
		many = append(many, put(t, dir2, "reports/m"+string(rune('a'+i))+".txt", strings.Repeat("b", 200<<10)))
	}
	blocks, _ = deliverableBlocks(dir2, &receipt.Deliverables{Files: many})
	inline := 0
	for _, b := range blocks {
		if b["type"] == "resource" {
			inline += len(b["resource"].(map[string]any)["text"].(string))
		}
	}
	if inline > InlineTotal || inline == 0 {
		t.Fatalf("%d bytes embedded, bound %d", inline, InlineTotal)
	}

	// A run that listed no deliverables hands back nothing.
	if b, n := deliverableBlocks(dir2, nil); len(b) != 0 || len(n) != 0 {
		t.Fatalf("no deliverables gave %v %q", kinds(b), n)
	}

	// Each file type has its own type: text is embedded under its own, the
	// rest linked under theirs, and a name's case does not matter.
	office := "application/vnd.openxmlformats-officedocument."
	for _, tc := range []struct {
		ext, mime string
		inline    bool
	}{
		{"md", "text/markdown", true}, {"txt", "text/plain", true}, {"csv", "text/csv", true}, {"json", "application/json", true},
		{"MD", "text/markdown", true},
		{"pdf", "application/pdf", false}, {"png", "image/png", false}, {"jpg", "image/jpeg", false}, {"jpeg", "image/jpeg", false},
		{"docx", office + "wordprocessingml.document", false}, {"xlsx", office + "spreadsheetml.sheet", false},
		{"pptx", office + "presentationml.presentation", false},
		{"html", "application/octet-stream", false}, {"xyz", "application/octet-stream", false},
	} {
		dir3 := t.TempDir()
		f := put(t, dir3, "reports/f."+tc.ext, "x")
		bs, ns := deliverableBlocks(dir3, &receipt.Deliverables{Files: []receipt.Deliverable{f}})
		if len(bs) != 1 || len(ns) != 0 {
			t.Fatalf("%s: %v %q", tc.ext, kinds(bs), ns)
		}
		meta := toJSON(map[string]any{"agentry": map[string]any{"sha256": f.SHA256, "path": "reports/f." + tc.ext, "marked": true}})
		uri := fileURI(filepath.Join(dir3, "reports", "f."+tc.ext))
		if tc.inline {
			r, _ := bs[0]["resource"].(map[string]any)
			if bs[0]["type"] != "resource" || r["mimeType"] != tc.mime || r["text"] != "x" || r["uri"] != uri || toJSON(r["_meta"]) != meta {
				t.Errorf("%s: %v", tc.ext, toJSON(bs[0]))
			}
			continue
		}
		if bs[0]["type"] != "resource_link" || bs[0]["mimeType"] != tc.mime || bs[0]["uri"] != uri || bs[0]["name"] != "f."+tc.ext ||
			bs[0]["title"] != "t f."+tc.ext || bs[0]["size"] != int64(1) || toJSON(bs[0]["_meta"]) != meta {
			t.Errorf("%s: %v", tc.ext, toJSON(bs[0]))
		}
	}

	// A file with no title of its own is shown under its file name, and so is
	// a refusal of it.
	dir4 := t.TempDir()
	plain := put(t, dir4, "reports/untitled.pdf", "x")
	plain.Title = "   "
	gone := receipt.Deliverable{Path: "reports/gone.pdf", SHA256: plain.SHA256, Bytes: 1, Type: "pdf"}
	bs, ns := deliverableBlocks(dir4, &receipt.Deliverables{Files: []receipt.Deliverable{plain, gone}})
	if len(bs) != 1 || bs[0]["title"] != "untitled.pdf" || len(ns) != 1 || ns[0] != `"gone.pdf" was not handed back: it is not in the run's folder.` {
		t.Fatalf("untitled files: %v %q", bs, ns)
	}

	// A list the run cut short says so, once.
	bs, ns = deliverableBlocks(dir4, &receipt.Deliverables{Files: []receipt.Deliverable{plain}, Truncated: true})
	if len(bs) != 1 || len(ns) != 1 || ns[0] != fmt.Sprintf("The run had more than %d deliverables; the rest are not listed.", receipt.MaxDeliverables) {
		t.Fatalf("a truncated list: %v %q", bs, ns)
	}
}

func TestAFileURIKeepsTheWholePathInOnePiece(t *testing.T) {
	if got := fileURI("/tmp/a b/c#d%e.md"); got != "file:///tmp/a%20b/c%23d%25e.md" {
		t.Fatal(got)
	}
	if got := fileURI("rel/a b.md"); got != "file:///rel/a%20b.md" {
		t.Fatal(got)
	}
}
