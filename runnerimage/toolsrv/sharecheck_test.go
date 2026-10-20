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

	if got, isErr := read("share/notes.md"); !isErr || !strings.Contains(got, "not been checked for malware yet") {
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
