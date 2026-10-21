package toolsrv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shareSpec is a persistent agent's folders, data/ and share/, with the
// share checked against a verdicts file in its own folder.
func shareSpec(t *testing.T) (*Spec, string) {
	t.Helper()
	spec := baseSpec(t)
	spec.WorkspaceName = "your folders"
	spec.FSRead = Access{Enabled: true, Paths: []string{"data", "share"}}
	spec.FSWrite = Access{Enabled: true, Paths: []string{"data", "share"}}
	verdicts := filepath.Join(t.TempDir(), "verdicts.json")
	spec.ShareChecks = &ShareChecks{Folder: "share", File: verdicts}
	return spec, verdicts
}

func writeVerdicts(t *testing.T, file string, files map[string]ShareVerdict) {
	t.Helper()
	b, err := json.Marshal(ShareVerdicts{Version: ShareVerdictsVersion, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func verdictFor(t *testing.T, file, verdict, signature string) ShareVerdict {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return ShareVerdict{SHA256: hex.EncodeToString(sum[:]), Size: fi.Size(), MTimeNano: fi.ModTime().UnixNano(), Verdict: verdict, Signature: signature}
}

// TestShareFileIsReadOnlyOnceChecked: a file in a checked share is refused
// until a verdict for its bytes is written, refused naming what ClamAV found,
// read when clean, and refused again once its bytes change; the agent's own
// folder is never held to a check.
func TestShareFileIsReadOnlyOnceChecked(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := newTestServer(t, spec)
	ws := spec.WorkspaceDir
	notes := filepath.Join(ws, "share", "notes.md")
	mustWrite(t, notes, []byte("the person's notes\n"))
	mustWrite(t, filepath.Join(ws, "data", "mine.md"), []byte("the agent's own\n"))
	read := func(p string) (string, bool) { return s.toolFSRead(rawArgs(t, map[string]any{"path": p})) }

	if got, isErr := read("share/notes.md"); !isErr || !strings.Contains(got, "not been checked yet") {
		t.Fatalf("an unchecked file: (%q, %v)", got, isErr)
	}
	if got, isErr := read("data/mine.md"); isErr || !strings.Contains(got, "the agent's own") {
		t.Fatalf("the agent's own folder: (%q, %v)", got, isErr)
	}
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"notes.md": verdictFor(t, notes, ShareFound, "Eicar-Signature")})
	if got, isErr := read("share/notes.md"); !isErr || !strings.Contains(got, "ClamAV found Eicar-Signature") {
		t.Fatalf("a file ClamAV found: (%q, %v)", got, isErr)
	}
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"notes.md": verdictFor(t, notes, ShareClean, "")})
	if got, isErr := read("share/notes.md"); isErr || !strings.Contains(got, "the person's notes") {
		t.Fatalf("a clean file: (%q, %v)", got, isErr)
	}
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"notes.md": verdictFor(t, notes, ShareUnchecked, "")})
	if got, isErr := read("share/notes.md"); isErr || !strings.Contains(got, "the person's notes") {
		t.Fatalf("a file ClamAV could not check is read, as a run's inputs are: (%q, %v)", got, isErr)
	}
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"notes.md": verdictFor(t, notes, "maybe", "")})
	if _, isErr := read("share/notes.md"); !isErr {
		t.Fatal("a verdict the server does not know was read as clean")
	}
	mustWrite(t, notes, []byte("changed since the check\n"))
	if got, isErr := read("share/notes.md"); !isErr || !strings.Contains(got, "not been checked") {
		t.Fatalf("a file changed since its check: (%q, %v)", got, isErr)
	}
}

// TestShareFileTheAgentWroteIsRead: what fs_write wrote in the share is
// read back before the runner's next check, and the same bytes under a
// checked name elsewhere in the share pass by their digest.
func TestShareFileTheAgentWroteIsRead(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := newTestServer(t, spec)
	if got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "share/out/summary.md", "content": "a summary\n"})); isErr {
		t.Fatalf("fs_write: %s", got)
	}
	if got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "share/out/summary.md"})); isErr || !strings.Contains(got, "a summary") {
		t.Fatalf("reading back its own write: (%q, %v)", got, isErr)
	}
	moved := filepath.Join(spec.WorkspaceDir, "share", "a.txt")
	mustWrite(t, moved, []byte("checked bytes\n"))
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"old/name.txt": verdictFor(t, moved, ShareClean, "")})
	if got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "share/a.txt"})); isErr {
		t.Fatalf("checked bytes under another name: %s", got)
	}
}

// TestShareChecksMustNameAGrantedFolder: the spec is refused when the
// checked folder is not one of the read grant's own.
func TestShareChecksMustNameAGrantedFolder(t *testing.T) {
	spec, _ := shareSpec(t)
	spec.ShareChecks.Folder = "elsewhere"
	if err := validateSpec(spec); err == nil {
		t.Fatal("a checked folder outside the read grant was accepted")
	}
	spec.ShareChecks.Folder = "share/.."
	if err := validateSpec(spec); err == nil {
		t.Fatal("a checked folder that is not a plain name was accepted")
	}
}

// TestGrepLeavesOutUncheckedShareFiles: a grep of the agent's folders shows
// a checked file's matches, and leaves out an unchecked one's, saying so.
func TestGrepLeavesOutUncheckedShareFiles(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := searchServer(t, spec, realRipgrep(t))
	ws := spec.WorkspaceDir
	checked := filepath.Join(ws, "share", "checked.md")
	mustWrite(t, checked, []byte("invoice 1\n"))
	mustWrite(t, filepath.Join(ws, "share", "new.md"), []byte("invoice 2\n"))
	mustWrite(t, filepath.Join(ws, "data", "notes.md"), []byte("invoice 3\n"))
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"checked.md": verdictFor(t, checked, ShareClean, "")})
	got, isErr := grepCall(t, s, map[string]any{"pattern": "invoice", "path": ws})
	if isErr || !strings.Contains(got, ws+"/share/checked.md") || !strings.Contains(got, ws+"/data/notes.md") {
		t.Fatalf("checked files missing: (%q, %v)", got, isErr)
	}
	if strings.Contains(got, "new.md") || !strings.Contains(got, "1 file(s) of the share were left out") {
		t.Fatalf("an unchecked file was shown, or not counted: %q", got)
	}
}

// keepText writes the text the runner kept for the bytes of file, beside
// the verdicts.
func keepText(t *testing.T, verdicts, file, text string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	mustWrite(t, filepath.Join(filepath.Dir(verdicts), ShareTextDir, ShareTextName(hex.EncodeToString(sum[:]))), []byte(text))
}

// TestDecodedTextBoundToTheCheckedDigest: fs_read serves a document in the
// share by the text kept for the digest of the bytes it read, headed with
// what read it; once the file's bytes change it serves nothing until they
// are checked, even though a text is still kept for the old bytes, and text
// kept under another digest is never served for them.
func TestDecodedTextBoundToTheCheckedDigest(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := newTestServer(t, spec)
	doc := filepath.Join(spec.WorkspaceDir, "share", "cv.docx")
	mustWrite(t, doc, []byte("PK\x03\x04\x00\x00 a word file"))
	keepText(t, verdicts, doc, "Jane Doe\nEngineer\n")
	v := verdictFor(t, doc, ShareClean, "")
	v.Text, v.Tool = true, "pandoc"
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"cv.docx": v})
	read := func(args map[string]any) (string, bool) { return s.toolFSRead(rawArgs(t, args)) }

	got, isErr := read(map[string]any{"path": "share/cv.docx"})
	if isErr || !strings.HasPrefix(got, "[share/cv.docx: the text pandoc read out of this file") || !strings.Contains(got, "Jane Doe\nEngineer") {
		t.Fatalf("the document's text: (%q, %v)", got, isErr)
	}
	if got, isErr := read(map[string]any{"path": "share/cv.docx", "offset": 9}); isErr || !strings.Contains(got, "Engineer") || strings.Contains(got, "Jane") {
		t.Fatalf("a later page of the text: (%q, %v)", got, isErr)
	}
	if got, isErr := read(map[string]any{"path": "share/cv.docx", "view": "original"}); isErr || !strings.Contains(got, "base64") || strings.Contains(got, "Jane") {
		t.Fatalf("the original: (%q, %v)", got, isErr)
	}

	// The bytes change after the check: the old digest's text stays kept,
	// and is not served for the new bytes.
	mustWrite(t, doc, []byte("PK\x03\x04\x00\x00 another word file"))
	if got, isErr := read(map[string]any{"path": "share/cv.docx"}); !isErr || strings.Contains(got, "Jane") || !strings.Contains(got, "not been checked") {
		t.Fatalf("changed bytes: (%q, %v)", got, isErr)
	}
	// A verdict for the new bytes whose text was never kept: nothing is
	// served in its place.
	v = verdictFor(t, doc, ShareClean, "")
	v.Text, v.Tool = true, "pandoc"
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"cv.docx": v})
	if got, isErr := read(map[string]any{"path": "share/cv.docx"}); !isErr || strings.Contains(got, "Jane") {
		t.Fatalf("a verdict with no text kept for its digest: (%q, %v)", got, isErr)
	}
}

// TestADocumentClamAVFoundIsRefusedWithItsText: a finding is refused, its
// kept text and its original alike.
func TestADocumentClamAVFoundIsRefusedWithItsText(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := newTestServer(t, spec)
	doc := filepath.Join(spec.WorkspaceDir, "share", "bad.pdf")
	mustWrite(t, doc, []byte("%PDF-1.7\x00\xff"))
	keepText(t, verdicts, doc, "the text\n")
	v := verdictFor(t, doc, ShareFound, "Eicar-Signature")
	v.Text, v.Tool = true, "pdftotext"
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"bad.pdf": v})
	for _, view := range []string{"", "text", "original"} {
		args := map[string]any{"path": "share/bad.pdf"}
		if view != "" {
			args["view"] = view
		}
		if got, isErr := s.toolFSRead(rawArgs(t, args)); !isErr || !strings.Contains(got, "ClamAV found Eicar-Signature") || strings.Contains(got, "the text") {
			t.Fatalf("view %q of a finding: (%q, %v)", view, got, isErr)
		}
	}
}

// TestAnUnreadableDocumentSaysWhy: a binary file no reader could read says
// why, in the decode step's words, and its original is still there; a
// text file is read as it is.
func TestAnUnreadableDocumentSaysWhy(t *testing.T) {
	spec, verdicts := shareSpec(t)
	s := newTestServer(t, spec)
	blob := filepath.Join(spec.WorkspaceDir, "share", "blob.bin")
	mustWrite(t, blob, []byte{0, 1, 2, 0xff})
	notes := filepath.Join(spec.WorkspaceDir, "share", "notes.md")
	mustWrite(t, notes, []byte("plain notes\n"))
	v := verdictFor(t, blob, ShareUnchecked, "")
	v.Reason, v.Unread = "the malware check is off on this computer", "not text, and no decoder reads this kind of file"
	writeVerdicts(t, verdicts, map[string]ShareVerdict{"blob.bin": v, "notes.md": verdictFor(t, notes, ShareUnchecked, "")})
	if got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "share/blob.bin"})); !isErr || !strings.Contains(got, "no decoder reads this kind of file") {
		t.Fatalf("an unreadable file: (%q, %v)", got, isErr)
	}
	if got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "share/blob.bin", "view": "original"})); isErr || !strings.Contains(got, "4 bytes") {
		t.Fatalf("an unreadable file's original: (%q, %v)", got, isErr)
	}
	if got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "share/notes.md"})); isErr || got != "plain notes\n" {
		t.Fatalf("a text file: (%q, %v)", got, isErr)
	}
}

// TestFSReadNamesTheFormatsWhereTheShareIsChecked: the description and the
// view are there for a checked share alone.
func TestFSReadNamesTheFormatsWhereTheShareIsChecked(t *testing.T) {
	spec, _ := shareSpec(t)
	s := newTestServer(t, spec)
	d := s.tools[ToolFSRead]
	props, _ := d.schema["properties"].(map[string]any)
	if !strings.Contains(d.description, "Word (DOCX)") || !strings.Contains(d.description, "by OCR") || props["view"] == nil {
		t.Fatalf("a checked share's fs_read: %q, %v", d.description, props)
	}
	plain := baseSpec(t)
	plain.FSRead = Access{Enabled: true}
	p := newTestServer(t, plain)
	d = p.tools[ToolFSRead]
	props, _ = d.schema["properties"].(map[string]any)
	if strings.Contains(d.description, "DOCX") || props["view"] != nil {
		t.Fatalf("a workspace's fs_read: %q, %v", d.description, props)
	}
	if got, isErr := p.toolFSRead(rawArgs(t, map[string]any{"path": "x", "view": "text"})); !isErr || !strings.Contains(got, "view") {
		t.Fatalf("a view outside a checked share: (%q, %v)", got, isErr)
	}
}
