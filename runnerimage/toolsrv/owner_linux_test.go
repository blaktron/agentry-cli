//go:build linux

package toolsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// write_report and fs_write hand what they create to the spec's Owner
// (agentry-cli#55). Chowning to one's own uid/gid is allowed without
// privileges, so the test proves the path is taken, not the privilege.
func TestWritesAreOwnedBySpecOwner(t *testing.T) {
	out := t.TempDir()
	ws := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	spec := &Spec{SpecVersion: SpecVersion, WorkspaceDir: ws, OutDir: out,
		FSWrite: Access{Enabled: true}, Owner: &Owner{UID: uid, GID: gid}}
	s, err := newServer(spec, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if msg, isErr := s.toolWriteReport(json.RawMessage(`{"path":"nested/deep/hello.md","content":"hi"}`)); isErr {
		t.Fatalf("write_report: %s", msg)
	}
	if msg, isErr := s.toolFSWrite(json.RawMessage(`{"path":"notes/new.txt","content":"x"}`)); isErr {
		t.Fatalf("fs_write: %s", msg)
	}
	for _, p := range []string{
		filepath.Join(out, "reports"), filepath.Join(out, "reports", "nested"), filepath.Join(out, "reports", "nested", "deep"),
		filepath.Join(out, "reports", "nested", "deep", "hello.md"),
		filepath.Join(ws, "notes"), filepath.Join(ws, "notes", "new.txt"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no Stat_t on this platform")
		}
		if int(st.Uid) != uid || int(st.Gid) != gid {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, gid)
		}
	}
}
