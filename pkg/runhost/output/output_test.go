package output

import (
	"archive/tar"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sealed(t *testing.T, plain []byte, to []To, job string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Seal(&buf, bytes.NewReader(plain), to, job); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every paired device opens the output; nothing else does.
func TestAnOutputOpensForEachPairedDeviceAndNoOther(t *testing.T) {
	a, b, other := newKey(t), newKey(t), newKey(t)
	plain := make([]byte, 3*ChunkSize+17)
	_, _ = rand.Read(plain)
	box := sealed(t, plain, []To{{"aaaa", a.PublicKey()}, {"bbbb", b.PublicKey()}}, "job1")
	for _, d := range []struct {
		id  string
		key *ecdh.PrivateKey
	}{{"aaaa", a}, {"bbbb", b}} {
		var got bytes.Buffer
		if err := Open(&got, bytes.NewReader(box), d.id, d.key, "job1"); err != nil || !bytes.Equal(got.Bytes(), plain) {
			t.Fatalf("device %s: %v (%d bytes)", d.id, err, got.Len())
		}
	}
	for name, try := range map[string]func() error{
		"another device's key":   func() error { return Open(&bytes.Buffer{}, bytes.NewReader(box), "aaaa", other, "job1") },
		"a device it is not for": func() error { return Open(&bytes.Buffer{}, bytes.NewReader(box), "cccc", other, "job1") },
		"another job":            func() error { return Open(&bytes.Buffer{}, bytes.NewReader(box), "aaaa", a, "job2") },
		"one device's key as the other": func() error {
			return Open(&bytes.Buffer{}, bytes.NewReader(box), "bbbb", a, "job1")
		},
	} {
		if err := try(); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A changed byte anywhere, a cut stream, a frame dropped and bytes after the
// end are each refused.
func TestATamperedOutputIsRefused(t *testing.T) {
	k := newKey(t)
	plain := bytes.Repeat([]byte("x"), 2*ChunkSize+5)
	box := sealed(t, plain, []To{{"aaaa", k.PublicKey()}}, "job1")
	_, hb, err := ReadHeader(bytes.NewReader(box))
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := len(Magic) + 4 + len(hb)
	frame := 4 + ChunkSize + 16
	cases := map[string][]byte{
		"a body byte changed":  flip(box, headerEnd+100),
		"the last byte":        flip(box, len(box)-1),
		"cut at a frame":       box[:headerEnd+frame],
		"the first frame gone": append(append([]byte{}, box[:headerEnd]...), box[headerEnd+frame:]...),
		"bytes after the end":  append(append([]byte{}, box...), 0),
		"a recipient renamed":  []byte(strings.Replace(string(box), `"device":"aaaa"`, `"device":"aaab"`, 1)),
	}
	for name, b := range cases {
		var got bytes.Buffer
		if err := Open(&got, bytes.NewReader(b), "aaaa", k, "job1"); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	// A header re-addressed to another device's key with the same name opens
	// no frame: the frames are bound to the header's hash.
	other := newKey(t)
	re := sealed(t, plain, []To{{"aaaa", other.PublicKey()}}, "job1")
	_, rhb, _ := ReadHeader(bytes.NewReader(re))
	mixed := append(append([]byte{}, re[:len(Magic)+4+len(rhb)]...), box[headerEnd:]...)
	if err := Open(&bytes.Buffer{}, bytes.NewReader(mixed), "aaaa", other, "job1"); !errors.Is(err, ErrOpen) {
		t.Errorf("a re-addressed output: %v", err)
	}
}

func flip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 1
	return c
}

func TestAnEmptyOutputSealsAndOpens(t *testing.T) {
	k := newKey(t)
	box := sealed(t, nil, []To{{"aaaa", k.PublicKey()}}, "job1")
	var got bytes.Buffer
	if err := Open(&got, bytes.NewReader(box), "aaaa", k, "job1"); err != nil || got.Len() != 0 {
		t.Fatalf("%v, %d", err, got.Len())
	}
}

// Pack takes the result, reports, receipts and outputs, as regular files
// only; Unpack gives them back.
func TestPackTakesOnlyTheMembers(t *testing.T) {
	src := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("result.json", "{}")
	write("reports/a.md", "report")
	write("receipts/events.ndjson", "e")
	write("outputs/x/y.txt", "y")
	write("scratch/secret", "no")
	write("policy.yaml", "no")
	secret := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "reports", "link.md")); err != nil {
		t.Fatal(err)
	}
	var tarball bytes.Buffer
	if err := Pack(&tarball, src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := Unpack(bytes.NewReader(tarball.Bytes()), dst); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, _ error) error {
		if fi.Mode().IsRegular() {
			r, _ := filepath.Rel(dst, p)
			got = append(got, filepath.ToSlash(r))
		}
		return nil
	})
	want := "outputs/x/y.txt receipts/events.ndjson reports/a.md result.json"
	if strings.Join(got, " ") != want {
		t.Errorf("unpacked %v, want %s", got, want)
	}
}

// Unpack refuses an entry outside the folder, outside the members, or not a
// regular file.
func TestUnpackRefusesWhatAnOutputNeverHolds(t *testing.T) {
	for name, h := range map[string]tar.Header{
		"a parent path":        {Typeflag: tar.TypeReg, Name: "reports/../../x"},
		"an absolute path":     {Typeflag: tar.TypeReg, Name: "/reports/x"},
		"not a member":         {Typeflag: tar.TypeReg, Name: "policy.yaml"},
		"a link":               {Typeflag: tar.TypeSymlink, Name: "reports/x", Linkname: "/etc/passwd"},
		"a directory entry":    {Typeflag: tar.TypeDir, Name: "reports/"},
		"an unclean name":      {Typeflag: tar.TypeReg, Name: "reports//x"},
		"a backslashed name":   {Typeflag: tar.TypeReg, Name: `reports\..\x`},
		"the folder itself":    {Typeflag: tar.TypeReg, Name: "."},
		"a member's lookalike": {Typeflag: tar.TypeReg, Name: "reportsx/y"},
	} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		h.Mode = 0o644
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		if err := Unpack(&b, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
}
