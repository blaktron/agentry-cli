package payload

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A payload packs files and folders under their members' names and lays
// them down again, digest checked, with nothing outside the folder.
func TestAPayloadRoundTrips(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "a")
	write(t, filepath.Join(src, "pack", "pages", "p.md"), "page")
	if err := os.Symlink("/etc", filepath.Join(src, "pack", "outside")); err != nil {
		t.Fatal(err)
	}
	p, err := Pack([]Entry{{Name: "inputs/a.txt", Src: filepath.Join(src, "a.txt")}, {Name: "knowledge/pack", Src: filepath.Join(src, "pack")}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Remove()
	f, err := p.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dst := filepath.Join(t.TempDir(), "files")
	if err := Unpack(f, p.Size, p.Digest, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"inputs/a.txt": "a", "knowledge/pack/pages/p.md": "page"} {
		if got, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "knowledge", "pack", "outside")); err == nil {
		t.Error("a link to a folder outside travelled")
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o700 {
		t.Errorf("the files folder is %v", fi.Mode().Perm())
	}
	p.Remove()
	if _, err := os.Stat(p.Path); err == nil {
		t.Error("Remove left the packed tar")
	}
}

func tarOf(t *testing.T, hs ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	_ = tw.Close()
	return b.Bytes()
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// What a device could send that is not a run's files is refused: a climb, a
// member of no kind, a link, and a stream that is not the one the job names.
func TestAPayloadRefusesWhatIsNotARunsFiles(t *testing.T) {
	for name, h := range map[string]*tar.Header{
		"a climb":     {Typeflag: tar.TypeReg, Name: "inputs/../../x", Size: 1},
		"no kind":     {Typeflag: tar.TypeReg, Name: "etc/passwd", Size: 1},
		"absolute":    {Typeflag: tar.TypeReg, Name: "/inputs/x", Size: 1},
		"a symlink":   {Typeflag: tar.TypeSymlink, Name: "inputs/x", Linkname: "/etc/passwd"},
		"a hard link": {Typeflag: tar.TypeLink, Name: "inputs/x", Linkname: "inputs/y"},
		"a bare kind": {Typeflag: tar.TypeReg, Name: "inputs", Size: 1},
		"a backslash": {Typeflag: tar.TypeReg, Name: `inputs/a\b`, Size: 1},
	} {
		raw := tarOf(t, h)
		if err := Unpack(bytes.NewReader(raw), int64(len(raw)), digestOf(raw), filepath.Join(t.TempDir(), "f")); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	raw := tarOf(t, &tar.Header{Typeflag: tar.TypeReg, Name: "inputs/a", Size: 3})
	if err := Unpack(bytes.NewReader(raw), int64(len(raw)), digestOf([]byte("other")), filepath.Join(t.TempDir(), "f")); err == nil || !strings.Contains(err.Error(), "not the") {
		t.Errorf("a digest that does not match: %v", err)
	}
	if err := Unpack(bytes.NewReader(raw[:len(raw)-600]), int64(len(raw)), digestOf(raw), filepath.Join(t.TempDir(), "f")); err == nil {
		t.Error("a stream cut short unpacked")
	}
	if _, err := Pack([]Entry{{Name: "inputs/a", Src: "/nonexistent"}, {Name: "inputs/a", Src: "/nonexistent"}}); err == nil {
		t.Error("two entries of one name packed")
	}
	if _, err := Pack([]Entry{{Name: "../a", Src: "/etc/hosts"}}); err == nil {
		t.Error("an entry outside the kinds packed")
	}
}
