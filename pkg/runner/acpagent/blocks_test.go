package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
}

func TestAChangedDeliverableIsWithheld(t *testing.T) {
	dir := t.TempDir()
	f := put(t, dir, "reports/a.md", "before")
	put(t, dir, "reports/a.md", "after!")
	blocks, notes := deliverableBlocks(dir, &receipt.Deliverables{Files: []receipt.Deliverable{f}})
	if len(blocks) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "changed") {
		t.Fatalf("blocks=%v notes=%q", kinds(blocks), notes)
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
}

func TestAFileURIKeepsTheWholePathInOnePiece(t *testing.T) {
	if got := fileURI("/tmp/a b/c#d%e.md"); got != "file:///tmp/a%20b/c%23d%25e.md" {
		t.Fatal(got)
	}
}
