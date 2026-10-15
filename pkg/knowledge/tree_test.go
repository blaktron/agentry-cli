package knowledge

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteTreeIsTheCanonicalTar(t *testing.T) {
	root := t.TempDir()
	write(t, root, digestFixture)
	var buf bytes.Buffer
	d, err := WriteTree(root, &buf)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digestFixtureSHA {
		t.Fatalf("the written tar hashes to %s, want the pinned %s", got, digestFixtureSHA)
	}
	if d.SHA256 != digestFixtureSHA || d.Bytes != 54 || d.Files != 4 {
		t.Fatalf("digest %+v", d)
	}
}

func TestExtractTreeRoundTrip(t *testing.T) {
	root := t.TempDir()
	write(t, root, digestFixture)
	var buf bytes.Buffer
	if _, err := WriteTree(root, &buf); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	d, err := ExtractTree(&buf, dst, digestFixtureSHA)
	if err != nil {
		t.Fatal(err)
	}
	if d.SHA256 != digestFixtureSHA || d.Files != 4 {
		t.Fatalf("digest %+v", d)
	}
	for rel, body := range digestFixture {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || string(got) != body {
			t.Fatalf("%s = %q, %v; want %q", rel, got, err, body)
		}
	}
	// An empty directory that already exists is a fine destination.
	empty := t.TempDir()
	buf.Reset()
	if _, err := WriteTree(root, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTree(&buf, empty, digestFixtureSHA); err != nil {
		t.Fatalf("an empty existing destination: %v", err)
	}
}

// tarEntry is one header of a hand-made tar.
type tarEntry struct {
	name  string
	typ   byte
	body  string
	mode  int64
	link  string
	mtime time.Time
}

func handTar(t *testing.T, entries []tarEntry, trailer string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		mt := e.mtime
		if mt.IsZero() {
			mt = time.Unix(0, 0)
		}
		hdr := &tar.Header{Typeflag: typ, Name: e.name, Mode: mode, ModTime: mt, Linkname: e.link, Format: tar.FormatPAX}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(trailer)
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:])
}

func TestExtractTreeRefuses(t *testing.T) {
	ok := []tarEntry{{name: "sources/a.txt", body: "a"}, {name: "wiki/index.md", body: "i"}}
	many := make([]tarEntry, 0, MaxFiles+1)
	for i := 0; i <= MaxFiles; i++ {
		many = append(many, tarEntry{name: fmt.Sprintf("sources/f%05d.txt", i), body: "x"})
	}
	cases := []struct {
		name    string
		entries []tarEntry
		trailer string
		want    string // "" = the tar's own digest
		errHas  string
	}{
		{name: "a symlink", entries: []tarEntry{{name: "wiki/index.md", typ: tar.TypeSymlink, link: "/etc/passwd"}}, errHas: "regular file"},
		{name: "a directory entry", entries: []tarEntry{{name: "wiki/", typ: tar.TypeDir}}, errHas: "regular file"},
		{name: "a path out of the pack", entries: []tarEntry{{name: "wiki/../../escape.txt", body: "x"}}, errHas: "path"},
		{name: "an absolute path", entries: []tarEntry{{name: "/wiki/index.md", body: "x"}}, errHas: "path"},
		{name: "a dot segment", entries: []tarEntry{{name: "wiki/./index.md", body: "x"}}, errHas: "path"},
		{name: "an empty segment", entries: []tarEntry{{name: "wiki//index.md", body: "x"}}, errHas: "path"},
		{name: "a backslash", entries: []tarEntry{{name: `wiki\index.md`, body: "x"}}, errHas: "path"},
		{name: "outside wiki and sources", entries: []tarEntry{{name: "manifest.json", body: "{}"}}, errHas: "wiki/ or sources/"},
		{name: "a name twice", entries: []tarEntry{{name: "sources/a.txt", body: "a"}, {name: "sources/a.txt", body: "b"}}, errHas: "order"},
		{name: "out of byte order", entries: []tarEntry{{name: "wiki/index.md", body: "i"}, {name: "sources/a.txt", body: "a"}}, errHas: "order"},
		{name: "too many files", entries: many, errHas: "files"},
		{name: "too large", entries: []tarEntry{{name: "sources/big.bin", body: strings.Repeat("x", MaxBytes+1)}}, errHas: "size cap"},
		{name: "not the digest named", entries: ok, want: "sha256:" + strings.Repeat("0", 64), errHas: "digest"},
		{name: "bytes after the end", entries: ok, trailer: "garbage", want: "", errHas: "canonical"},
		{name: "not canonical (a mode)", entries: []tarEntry{{name: "sources/a.txt", body: "a", mode: 0o755}}, errHas: "canonical"},
		{name: "not canonical (a time)", entries: []tarEntry{{name: "sources/a.txt", body: "a", mtime: time.Unix(1_700_000_000, 0)}}, errHas: "canonical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lowerLocalCap(t)
			raw, sum := handTar(t, c.entries, c.trailer)
			want := c.want
			if want == "" {
				want = sum
			}
			dst := filepath.Join(t.TempDir(), "out")
			_, err := ExtractTree(bytes.NewReader(raw), dst, want)
			if err == nil || !strings.Contains(err.Error(), c.errHas) {
				t.Fatalf("err = %v, want one naming %q", err, c.errHas)
			}
			if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
				t.Fatalf("a refused tree left %s behind (%v)", dst, statErr)
			}
		})
	}
}

func TestExtractTreeIntoANonEmptyDirectoryIsRefused(t *testing.T) {
	root := t.TempDir()
	write(t, root, digestFixture)
	var buf bytes.Buffer
	if _, err := WriteTree(root, &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTree(&buf, dst, digestFixtureSHA); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "keep.txt")); err != nil || string(got) != "mine" {
		t.Fatalf("the person's file was touched: %q %v", got, err)
	}
}

// TestUnpackMakesAPackFromATree: a registry's tree tar becomes a pack
// directory the Creator and the runner load, its manifest's counts and
// digest the tree's own whatever the caller said; a tree that is not its
// digest leaves nothing.
func TestUnpackMakesAPackFromATree(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"wiki/index.md":    "# Index\n",
		"wiki/log.md":      "# Log\n",
		"wiki/topics/a.md": "---\ntitle: A\n---\nA [src: doc:a.md]\n",
		"sources/a.md":     "# A\n",
	})
	var buf bytes.Buffer
	d, err := WriteTree(root, &buf)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "notes")
	p, err := Unpack(bytes.NewReader(buf.Bytes()), d.SHA256, dst, Manifest{Name: "notes", ID: "kp_0123", Pages: 99, Bytes: 1, SHA256: "sha256:lie"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	if m.Schema != Schema || m.Name != "notes" || m.ID != "kp_0123" || m.Pages != 1 || m.Bytes != d.Bytes || m.SHA256 != d.SHA256 {
		t.Fatalf("manifest = %+v, want the tree's counts and digest", m)
	}
	if _, err := Load(dst); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), "sha256:"+strings.Repeat("0", 64), bad, Manifest{Name: "notes"}); err == nil {
		t.Fatal("a tree that is not its digest was unpacked")
	}
	if entries, _ := os.ReadDir(bad); len(entries) != 0 {
		t.Fatalf("a refused tree left %d entries", len(entries))
	}
	named := filepath.Join(t.TempDir(), "named")
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), d.SHA256, named, Manifest{Name: "Bad Name"}); err == nil {
		t.Fatal("a pack with a name that is not a pack's was unpacked")
	}
	if entries, _ := os.ReadDir(named); len(entries) != 0 {
		t.Fatalf("a refused pack left %d entries", len(entries))
	}
}
