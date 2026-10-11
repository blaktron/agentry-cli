package source

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git's checksum
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexPackNamesEveryObjectAsGitDoes packs a repository with git's own
// pack-objects, deltas and all, and holds every name this reader computes
// to git's.
func TestIndexPackNamesEveryObjectAsGitDoes(t *testing.T) {
	g := gitBin(t)
	bare := newFixture(t, true).bare
	list := exec.Command(g, "rev-list", "--objects", "--all")
	list.Dir = bare
	objs, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	var names bytes.Buffer
	want := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(objs)), "\n") {
		oid, _, _ := strings.Cut(line, " ")
		want[oid] = true
		names.WriteString(oid + "\n")
	}
	pk := exec.Command(g, "pack-objects", "--stdout", "--delta-base-offset", "--depth=50", "--window=50")
	pk.Dir = bare
	pk.Stdin = &names
	raw, err := pk.Output()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pack")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pf.Close() }()
	p, err := indexPack(pf, 100000)
	if err != nil {
		t.Fatalf("indexPack: %v", err)
	}
	deltas := 0
	for _, e := range p.entries {
		if e.typ == objOfsDelta || e.typ == objRefDelta {
			deltas++
		}
		if !want[e.oid] {
			t.Errorf("the reader named an object %s git does not have (type %d)", e.oid, e.ftyp)
		}
	}
	if deltas == 0 {
		t.Error("the pack carried no delta, so the delta path went untested")
	}
	if len(p.byOID) != len(want) {
		t.Errorf("named %d objects, git lists %d", len(p.byOID), len(want))
	}
	// A flipped byte anywhere fails the checksum.
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	bad, _ := os.Open(path)
	defer func() { _ = bad.Close() }()
	if _, err := indexPack(bad, 100000); err == nil {
		t.Error("a corrupted pack was read")
	}
}

func TestApplyDeltaHoldsEveryBound(t *testing.T) {
	base := []byte("hello, world")
	// src 12, dst 5, copy offset 7 size 5.
	ok := []byte{12, 5, 0x80 | 0x01 | 0x10, 7, 5}
	out, err := applyDelta(base, ok)
	if err != nil || string(out) != "world" {
		t.Fatalf("applyDelta = %q, %v", out, err)
	}
	for name, d := range map[string][]byte{
		"wrong source size": {11, 5, 0x80 | 0x01 | 0x10, 7, 5},
		"copy past base":    {12, 5, 0x80 | 0x01 | 0x10, 10, 5},
		"past declared":     {12, 2, 0x80 | 0x01 | 0x10, 7, 5},
		"short of declared": {12, 9, 0x80 | 0x01 | 0x10, 7, 5},
		"reserved op":       {12, 1, 0},
		"insert past end":   {12, 3, 5, 'a'},
		"truncated copy":    {12, 5, 0x80 | 0x01},
	} {
		if _, err := applyDelta(base, d); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
}

func FuzzApplyDelta(f *testing.F) {
	f.Add([]byte("hello, world"), []byte{12, 5, 0x91, 7, 5})
	f.Fuzz(func(t *testing.T, base, delta []byte) {
		out, err := applyDelta(base, delta)
		if err == nil && int64(len(out)) > maxDeltaObject {
			t.Fatal("a delta rebuilt past the bound")
		}
	})
}

func FuzzIndexPack(f *testing.F) {
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// The trailer is made right, so the fuzzer reaches the parser
		// rather than stopping at the checksum.
		sum := sha1.Sum(data) //nolint:gosec // git's checksum
		data = append(data, sum[:]...)
		path := filepath.Join(t.TempDir(), "pack")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		pf, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pf.Close() }()
		_, _ = indexPack(pf, 1000)
	})
}

func FuzzPktLines(f *testing.F) {
	f.Add([]byte("001e# service=git-upload-pack\n0000000eversion 2\n0000"))
	f.Fuzz(func(t *testing.T, data []byte) {
		pr := &pktReader{r: bytes.NewReader(data)}
		for i := 0; i < 1000; i++ {
			if _, _, err := pr.line(); err != nil {
				return
			}
		}
	})
}
