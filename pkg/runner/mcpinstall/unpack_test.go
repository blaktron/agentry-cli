package mcpinstall

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// archiveFile is one regular file of a test archive.
type archiveFile struct {
	name, body string
	mode       int64
}

// storeArchive is a builder's stdout holding files, as a fake builder prints
// it.
func storeArchive(t *testing.T, files ...archiveFile) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// rawArchive is an archive of arbitrary headers, each with its body.
func rawArchive(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg && h.Size == 0 {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The archive's folders and files land inside the server's folder, with
// their executable bits.
func TestUnpackStoreWritesFoldersAndFiles(t *testing.T) {
	dir := t.TempDir()
	when := time.Unix(1700000000, 0)
	archive := rawArchive(t,
		&tar.Header{Name: "venv/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "venv/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3, ModTime: when},
		&tar.Header{Name: "./venv/lib/mod.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2, ModTime: when},
		&tar.Header{Name: "empty/", Typeflag: tar.TypeDir, Mode: 0o755},
	)
	if err := unpackStore(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	tool, err := os.Stat(filepath.Join(dir, "venv", "bin", "tool"))
	if err != nil || tool.Size() != 3 || tool.Mode().Perm()&0o111 == 0 {
		t.Fatalf("venv/bin/tool = %v (%v), want 3 bytes, executable", tool, err)
	}
	mod, err := os.Stat(filepath.Join(dir, "venv", "lib", "mod.py"))
	if err != nil || mod.Mode().Perm()&0o111 != 0 {
		t.Fatalf("venv/lib/mod.py = %v (%v), want not executable", mod, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty folder = %v (%v)", fi, err)
	}
}

// Nothing in the archive reaches outside the server's folder, and nothing
// but folders and regular files is written: every link is refused, whether
// it points in or out, as are devices, FIFOs and a second entry for a name.
func TestUnpackStoreRefusesWhatIsNotAFileOrAFolderInside(t *testing.T) {
	cases := map[string][]*tar.Header{
		"an absolute name":      {{Name: "/etc/evil", Typeflag: tar.TypeReg}},
		"a name that climbs":    {{Name: "../evil", Typeflag: tar.TypeReg}},
		"a climb in the middle": {{Name: "venv/../../evil", Typeflag: tar.TypeReg}},
		"a backslash":           {{Name: `venv\..\..\evil`, Typeflag: tar.TypeReg}},
		"a link out":            {{Name: "venv/bin/python", Typeflag: tar.TypeSymlink, Linkname: "/usr/local/bin/python3"}},
		"a link inside":         {{Name: "venv/lib64", Typeflag: tar.TypeSymlink, Linkname: "lib"}},
		"a hard link":           {{Name: "a", Typeflag: tar.TypeReg}, {Name: "b", Typeflag: tar.TypeLink, Linkname: "a"}},
		"a device":              {{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		"a FIFO":                {{Name: "fifo", Typeflag: tar.TypeFifo}},
		"a name twice":          {{Name: "a", Typeflag: tar.TypeReg}, {Name: "a", Typeflag: tar.TypeReg}},
		"a file over a folder":  {{Name: "a/", Typeflag: tar.TypeDir}, {Name: "a", Typeflag: tar.TypeReg}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "server")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := unpackStore(bytes.NewReader(rawArchive(t, entries...)), dir); err == nil {
				t.Fatal("unpacked")
			}
			if _, err := os.Lstat(filepath.Join(parent, "evil")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an entry landed beside the server's folder: %v", err)
			}
		})
	}
}

// An archive that is not one is refused, as is a file cut short, a stream
// cut between two entries (before the archive's end-of-archive marker) and
// an empty stream, which is a builder that wrote no tree.
func TestUnpackStoreRefusesABrokenArchive(t *testing.T) {
	two := rawArchive(t, &tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 512}, &tar.Header{Name: "b", Typeflag: tar.TypeReg, Size: 512})
	if err := unpackStore(bytes.NewReader(two), t.TempDir()); err != nil {
		t.Fatalf("a whole archive refused: %v", err)
	}
	// Each entry is a 512-byte header and a 512-byte body; the marker
	// is the last 1024 bytes.
	if err := unpackStore(bytes.NewReader(two[:1024]), t.TempDir()); err == nil || !strings.Contains(err.Error(), "end-of-archive") {
		t.Fatalf("a stream cut after its first entry = %v, want refused", err)
	}
	if err := unpackStore(bytes.NewReader(two[:2048]), t.TempDir()); err == nil {
		t.Fatal("a stream cut before its end-of-archive marker was unpacked")
	}
	if err := unpackStore(bytes.NewReader(nil), t.TempDir()); err == nil {
		t.Fatal("an empty stream was unpacked")
	}
	if err := unpackStore(strings.NewReader("not a tar archive at all, but long enough to be read as a header block"+strings.Repeat(" ", 512)), t.TempDir()); err == nil {
		t.Fatal("a stream that is no archive was unpacked")
	}
	whole := rawArchive(t, &tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: 4096})
	if err := unpackStore(bytes.NewReader(whole[:1024]), t.TempDir()); err == nil {
		t.Fatal("a file cut short was unpacked")
	}
}

// The sink reads the whole stream whatever the unpack decided, so a builder
// writing more after a refused entry, or padding after the archive's end, is
// never left blocked.
func TestArchiveSinkDrainsWhatFollows(t *testing.T) {
	dir := t.TempDir()
	sink := newArchiveSink(dir)
	refused := rawArchive(t, &tar.Header{Name: "/abs", Typeflag: tar.TypeReg})
	if _, err := sink.Write(refused); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("a write after the refusal blocked or failed: %v", err)
	}
	if err := sink.Close(); err == nil {
		t.Fatal("the refusal was not reported")
	}
	ok := newArchiveSink(t.TempDir())
	if _, err := io.Copy(ok, bytes.NewReader(append(rawArchive(t, &tar.Header{Name: "f", Typeflag: tar.TypeReg}), make([]byte, 10240)...))); err != nil {
		t.Fatal(err)
	}
	if err := ok.Close(); err != nil {
		t.Fatalf("padding after the archive's end refused it: %v", err)
	}
}

// EnsureInstalled installs from the builder's archive: the server's folder is
// the archive's tree, its digest is taken over what was unpacked, and an
// archive the unpack refuses fails the install and leaves neither a folder
// nor a record nor the staging folder behind.
func TestEnsureInstalledUnpacksTheBuildersArchive(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "https://npm.invalid", PyPI: "https://unused.invalid"}
	server := func() []Resolved {
		return []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-A"}}
	}
	t.Run("the folder is the archive's tree", func(t *testing.T) {
		store := t.TempDir()
		// A folder an earlier install left with no record is replaced, and
		// a killed install's staging folder is removed.
		if err := os.MkdirAll(filepath.Join(store, "npm-server-x", "stale"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store, stagingPrefix("npm-server-x")+"123", "partial"), 0o700); err != nil {
			t.Fatal(err)
		}
		fake := withRunner(&dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-npm-server-x": {Print: func(dockerx.RunSpec) string {
				return storeArchive(t, archiveFile{"node_modules/.bin/server-x", "#!/bin/sh\n", 0o755}, archiveFile{"package.json", "{}", 0o644})
			}},
		}})
		res := server()
		if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
			t.Fatal(err)
		}
		want, err := storeDigest(filepath.Join(store, "npm-server-x"))
		if err != nil || res[0].StoreDigest != want {
			t.Fatalf("recorded digest %q, the folder's %q (%v)", res[0].StoreDigest, want, err)
		}
		if _, err := os.Stat(filepath.Join(store, "npm-server-x", "stale")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the earlier install's leftover is still there: %v", err)
		}
		if got := storeEntries(t, store); got != "npm-server-x|npm-server-x.install-proxy.log|npm-server-x.json" {
			t.Fatalf("store holds %s, want the folder, the proxy's log and the record", got)
		}
	})
	t.Run("a refused archive installs nothing", func(t *testing.T) {
		store := t.TempDir()
		fake := withRunner(&dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-npm-server-x": {Print: func(dockerx.RunSpec) string {
				return string(rawArchive(t,
					&tar.Header{Name: "node_modules/.bin/server-x", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}))
			}},
		}})
		err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, policy.DockerPolicy{}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "archive was refused") {
			t.Fatalf("install = %v, want the archive refused", err)
		}
		if got := storeEntries(t, store); got != "npm-server-x.install-proxy.log" {
			t.Fatalf("a refused install left %s, want the proxy's log alone", got)
		}
	})
	t.Run("a failed build installs nothing", func(t *testing.T) {
		store := t.TempDir()
		fake := withRunner(&dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-npm-server-x": {ExitCode: 1, Print: func(dockerx.RunSpec) string {
				return storeArchive(t, archiveFile{"partial", "x", 0o644})
			}},
		}})
		if err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, policy.DockerPolicy{}, io.Discard); err == nil {
			t.Fatal("a failed build installed")
		}
		if got := storeEntries(t, store); got != "npm-server-x.install-proxy.log" {
			t.Fatalf("a failed build left %s, want the proxy's log alone", got)
		}
	})
}

// storeEntries lists a store folder's names, "|"-separated, sorted.
func storeEntries(t *testing.T, store string) string {
	t.Helper()
	des, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return strings.Join(names, "|")
}
