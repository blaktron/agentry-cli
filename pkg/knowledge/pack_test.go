package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write lays files out under root, creating directories.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// digestFixture is the tree whose digest is pinned below. The literal was
// computed independently of archive/tar, by a POSIX ustar encoder written
// by hand from the field table TreeDigest documents (Python):
// each header's fields set one by one, the checksum summed, the data
// padded to 512, two zero blocks at the end. Python's own tarfile differs
// in one field — it leaves devmajor/devminor NUL for a regular file — which
// is why the form is spelled out field by field rather than by library.
var digestFixture = map[string]string{
	"wiki/index.md":         "# Index\n",
	"wiki/log.md":           "# Log\n",
	"wiki/topics/a-page.md": "---\ntitle: A page\n---\nbody\n",
	"sources/doc.txt":       "the document\n",
}

const digestFixtureSHA = "sha256:88a677666a5288a1222fc455a730b580215be6b4f213d7cd0def6468a24ef951"

func TestTreeDigestIsTheCanonicalTar(t *testing.T) {
	root := t.TempDir()
	write(t, root, digestFixture)
	got, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != digestFixtureSHA {
		t.Fatalf("digest = %s, want %s", got.SHA256, digestFixtureSHA)
	}
	if got.Bytes != 54 || got.Files != 4 {
		t.Fatalf("bytes %d files %d", got.Bytes, got.Files)
	}
}

func TestTreeDigestProperties(t *testing.T) {
	base := func(t *testing.T) string {
		root := t.TempDir()
		write(t, root, digestFixture)
		return root
	}
	digest := func(t *testing.T, root string) string {
		t.Helper()
		d, err := TreeDigest(root)
		if err != nil {
			t.Fatal(err)
		}
		return d.SHA256
	}
	t.Run("times, modes and the manifest do not move it", func(t *testing.T) {
		root := base(t)
		p := filepath.Join(root, "wiki", "index.md")
		if err := os.Chtimes(p, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		write(t, root, map[string]string{"manifest.json": `{"anything":1}`})
		if got := digest(t, root); got != digestFixtureSHA {
			t.Fatalf("digest moved to %s", got)
		}
	})
	t.Run("content moves it", func(t *testing.T) {
		root := base(t)
		write(t, root, map[string]string{"sources/doc.txt": "the document!\n"})
		if digest(t, root) == digestFixtureSHA {
			t.Fatal("changed content kept the digest")
		}
	})
	t.Run("a rename moves it", func(t *testing.T) {
		root := base(t)
		if err := os.Rename(filepath.Join(root, "sources", "doc.txt"), filepath.Join(root, "sources", "doc2.txt")); err != nil {
			t.Fatal(err)
		}
		if digest(t, root) == digestFixtureSHA {
			t.Fatal("a rename kept the digest")
		}
	})
	t.Run("a link is refused, not followed", func(t *testing.T) {
		root := base(t)
		if err := os.Symlink("/etc/hostname", filepath.Join(root, "sources", "link.txt")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := TreeDigest(root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNameRule(t *testing.T) {
	for _, ok := range []string{"aws-docs", "a", "my_notes.2", strings.Repeat("a", 64)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "AWS", "-lead", ".", "..", "a/b", "a b", strings.Repeat("a", 65), "é"} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
}

// validPack writes a pack a person could have made by hand, with its
// manifest's digest filled in from the tree.
func validPack(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	write(t, root, digestFixture)
	d, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Schema: Schema, Name: name, ID: "kp_test", Pages: 1, Bytes: d.Bytes, SHA256: d.SHA256}
	b, _ := json.Marshal(m)
	write(t, root, map[string]string{"manifest.json": string(b)})
	return root
}

func TestLoadVerifiesAPack(t *testing.T) {
	t.Run("a valid pack", func(t *testing.T) {
		p, err := Load(validPack(t, "aws-docs"))
		if err != nil {
			t.Fatal(err)
		}
		if p.Manifest.Name != "aws-docs" || p.Manifest.SHA256 != digestFixtureSHA || p.Manifest.Scan.Scanned {
			t.Fatalf("manifest %+v", p.Manifest)
		}
	})

	tests := []struct {
		name    string
		break_  func(t *testing.T, root string)
		wantErr string
		is      error
	}{
		{name: "no manifest", break_: func(t *testing.T, root string) { _ = os.Remove(filepath.Join(root, "manifest.json")) },
			wantErr: "manifest.json"},
		{name: "another schema", break_: func(t *testing.T, root string) {
			rewriteManifest(t, root, func(m *Manifest) { m.Schema = "knowledge/v2" })
		}, wantErr: `schema "knowledge/v2"`},
		{name: "a bad name", break_: func(t *testing.T, root string) {
			rewriteManifest(t, root, func(m *Manifest) { m.Name = "Bad Name" })
		}, wantErr: "name"},
		{name: "a tree changed after its digest", break_: func(t *testing.T, root string) {
			write(t, root, map[string]string{"sources/doc.txt": "tampered\n"})
		}, is: ErrDigestMismatch},
		{name: "no index", break_: func(t *testing.T, root string) {
			_ = os.Remove(filepath.Join(root, "wiki", "index.md"))
			reseal(t, root)
		}, wantErr: "wiki/index.md"},
		{name: "a stray top-level file", break_: func(t *testing.T, root string) {
			write(t, root, map[string]string{"notes.txt": "x"})
		}, wantErr: "notes.txt"},
		{name: "a page count the tree does not have", break_: func(t *testing.T, root string) {
			rewriteManifest(t, root, func(m *Manifest) { m.Pages = 900 })
		}, wantErr: "says 900 pages"},
		{name: "a byte count the tree does not have", break_: func(t *testing.T, root string) {
			rewriteManifest(t, root, func(m *Manifest) { m.Bytes = 7 })
		}, wantErr: "7 bytes"},
		{name: "past the cap", break_: func(t *testing.T, root string) {
			lowerLocalCap(t)
			write(t, root, map[string]string{"sources/big.bin": strings.Repeat("x", MaxBytes)})
			reseal(t, root)
		}, is: ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := validPack(t, "aws-docs")
			tc.break_(t, root)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func rewriteManifest(t *testing.T, root string, f func(*Manifest)) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	f(&m)
	b, _ = json.Marshal(m)
	write(t, root, map[string]string{"manifest.json": string(b)})
}

// reseal recomputes the manifest's digest, so a test reaches the check
// after the digest one.
func reseal(t *testing.T, root string) {
	t.Helper()
	d, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, root, func(m *Manifest) { m.SHA256, m.Bytes = d.SHA256, d.Bytes })
}

func TestScanRecord(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(`{"schema":"knowledge/v1","name":"a","scan":"not scanned"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Scan.Scanned || m.Scan.Describe() != "not scanned" {
		t.Fatalf("scan %+v", m.Scan)
	}
	if err := json.Unmarshal([]byte(`{"schema":"knowledge/v1","name":"a","scan":{"state":"ready","findings":2}}`), &m); err != nil {
		t.Fatal(err)
	}
	// A record in a file is a claim: unconfirmed, it never reads as a scan.
	if !m.Scan.Scanned || m.Scan.Describe() != "a scan record this client has not confirmed with the registry" {
		t.Fatalf("scan %+v reads %q", m.Scan, m.Scan.Describe())
	}
	if confirmed := (Scan{Scanned: true, Verified: true}); confirmed.Describe() != "scanned" {
		t.Fatalf("a confirmed record reads %q", confirmed.Describe())
	}
	if b, err := json.Marshal(m.Scan); err != nil || string(b) != `{"state":"ready","findings":2}` {
		t.Fatalf("a registry's record does not round-trip as written: %s, %v", b, err)
	}
	out, _ := json.Marshal(Manifest{Schema: Schema, Name: "a"})
	if !strings.Contains(string(out), `"scan":"not scanned"`) {
		t.Fatalf("an unscanned pack marshals as %s", out)
	}
	if err := json.Unmarshal([]byte(`{"schema":"knowledge/v1","name":"a","scan":"clean"}`), &m); err == nil {
		t.Fatal("an unknown scan word was accepted")
	}
}

func TestCopyPack(t *testing.T) {
	t.Run("a pack copies whole and loads", func(t *testing.T) {
		src := validPack(t, "aws-docs")
		dst := filepath.Join(t.TempDir(), "copy")
		if err := CopyPack(src, dst); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dst); err != nil {
			t.Fatalf("the copy does not load: %v", err)
		}
	})
	t.Run("a link is refused, not followed", func(t *testing.T) {
		src := validPack(t, "aws-docs")
		if err := os.Symlink("/etc/hostname", filepath.Join(src, "sources", "host.txt")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "copy")
		if err := CopyPack(src, dst); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dst, "sources", "host.txt")); err == nil {
			t.Fatal("the link's target was copied")
		}
	})
	t.Run("more than the cap is refused", func(t *testing.T) {
		lowerLocalCap(t)
		src := validPack(t, "aws-docs")
		write(t, src, map[string]string{"sources/big.bin": strings.Repeat("x", MaxBytes)})
		if err := CopyPack(src, filepath.Join(t.TempDir(), "copy")); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
}

// lowerLocalCap holds this computer's cap to Agentry's for one test, so a
// test past it writes ten megabytes rather than a gigabyte.
func lowerLocalCap(t *testing.T) {
	t.Helper()
	prev := localCap
	localCap = MaxBytes
	t.Cleanup(func() { localCap = prev })
}

// A pack past Agentry's 10 MB is a pack on this computer: it loads,
// copies, and goes through the canonical tree and back.
func TestAPackPastAgentrysCapIsALocalPack(t *testing.T) {
	root := validPack(t, "aws-docs")
	write(t, root, map[string]string{"sources/big.bin": strings.Repeat("x", MaxBytes+1<<20)})
	reseal(t, root)
	p, err := Load(root)
	if err != nil {
		t.Fatalf("Load refused a pack past 10 MB: %v", err)
	}
	if p.Manifest.Bytes <= MaxBytes {
		t.Fatalf("the pack is %d bytes", p.Manifest.Bytes)
	}
	if err := CopyPack(root, filepath.Join(t.TempDir(), "copy")); err != nil {
		t.Fatalf("CopyPack: %v", err)
	}
	var tree bytes.Buffer
	d, err := WriteTree(root, &tree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTree(&tree, filepath.Join(t.TempDir(), "x"), d.SHA256); err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}
	if FormatCap(MaxBytes) != "10 MB" || FormatCap(MaxLocalBytes) != "1 GB" {
		t.Fatalf("FormatCap: %s, %s", FormatCap(MaxBytes), FormatCap(MaxLocalBytes))
	}
}
