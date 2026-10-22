package persistent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestTheAssistantsSegmentHasItsMemory: the Assistant's segment is given
// its memory volume, its seed (the built-in's SCHEMA.md, index.md and log.md,
// the log dated with the memory's making, and pages/) and the notes folder
// it carries in, with the lines the log and the index take when it does;
// the Coding agent keeps no memory and is given none.
func TestTheAssistantsSegmentHasItsMemory(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{}
	sv := startAssistant(t, home, st)
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	seg := st.segments[0].Persistent.Memory
	if seg == nil || seg.Volume != MemoryVolumeIn(home, "assistant") || seg.Carry != "notes" || seg.CarryTo != "pages/notes" || seg.Seeded {
		t.Fatalf("the segment's memory: %+v", seg)
	}
	for _, f := range []string{"SCHEMA.md", "index.md", "log.md", "pages/"} {
		if _, ok := seg.Seed[f]; !ok {
			t.Errorf("the seed has no %s", f)
		}
	}
	day := time.Now().Format(time.DateOnly)
	if log := string(seg.Seed["log.md"]); !strings.HasPrefix(log, "# Memory log") || !strings.HasSuffix(log, "## ["+day+"] seed | the memory was made\n") {
		t.Errorf("the seed's log: %q", log)
	}
	carried := engine.MemoryCarriedDir + "/"
	if !strings.Contains(string(seg.Seed[carried+"log.md"]), "## ["+day+"] ingest | data/notes/") ||
		!strings.Contains(string(seg.Seed[carried+"index.md"]), "(pages/notes/)") {
		t.Errorf("the carried lines: %q, %q", seg.Seed[carried+"log.md"], seg.Seed[carried+"index.md"])
	}
	// The start recorded the seed, so the next one does not seed again.
	if next, err := memorySegment(home, assistantAgent(t), time.Now()); err != nil || !next.Seeded {
		t.Errorf("the next start's memory: %+v %v", next, err)
	}
	if m, err := memorySegment(home, coderAgent(t), time.Now()); err != nil || m != nil {
		t.Errorf("the Coding agent's memory: %+v %v", m, err)
	}
	if _, ok := BuildRoster(coderAgent(t), Config{}, nil).Entry(EntryMemory); ok {
		t.Error("the Coding agent's roster has a memory entry")
	}
}

// memoryArchive is a tar archive of the entries, as the export's one-shot
// writes it.
func memoryArchive(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = "page " + h.Name + "\n"
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// exportFake is an engine holding the Assistant's memory volume, whose
// export one-shot writes archive.
func exportFake(home string, archive []byte) *dockerx.Fake {
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"-volume-export": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
		_, err := stdout.Write(archive)
		return 0, err
	}}}}
	f.Volumes = []dockerx.Labeled{{Name: MemoryVolumeIn(home, "assistant"), Labels: engine.PersistentVolumeLabels("assistant", HomeID(home))}}
	return f
}

// TestMemoryExportCopiesIntoANewFolder: the export writes the memory's files
// and folders into the new folder it makes, from a one-shot that mounts the
// memory volume read-only on no network; a link is left out and counted, an
// existing folder is refused, and an agent with no memory volume yet is told
// so.
func TestMemoryExportCopiesIntoANewFolder(t *testing.T) {
	home := shortHome(t)
	a := assistantAgent(t)
	archive := memoryArchive(t,
		tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755},
		tar.Header{Typeflag: tar.TypeReg, Name: "./index.md", Mode: 0o644},
		tar.Header{Typeflag: tar.TypeDir, Name: "./pages/", Mode: 0o755},
		tar.Header{Typeflag: tar.TypeReg, Name: "./pages/sam.md", Mode: 0o644},
		tar.Header{Typeflag: tar.TypeSymlink, Name: "./pages/passwd", Linkname: "/etc/passwd"},
	)
	f := exportFake(home, archive)
	dir := filepath.Join(t.TempDir(), "memory")
	out, err := ExportMemory(context.Background(), f, home, a, "agentry-runner:test", "", policy.Limits{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Files != 2 || out.Skipped != 1 {
		t.Errorf("exported %+v", out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pages", "sam.md")); err != nil || string(b) != "page ./pages/sam.md\n" {
		t.Errorf("pages/sam.md: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "pages", "passwd")); err == nil {
		t.Error("the link was written")
	}
	for p, want := range map[string]os.FileMode{dir: 0o700 | os.ModeDir, filepath.Join(dir, "pages"): 0o700 | os.ModeDir, filepath.Join(dir, "index.md"): 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode() != want {
			t.Errorf("%s: mode %v, want %v (%v)", p, fi.Mode(), want, err)
		}
	}
	run, _ := f.FindRun("-volume-export")
	if run.Network != "none" || len(run.Mounts) != 1 || run.Mounts[0].Source != MemoryVolumeIn(home, "assistant") || !run.Mounts[0].ReadOnly {
		t.Errorf("the export's one-shot: network %q mounts %+v", run.Network, run.Mounts)
	}
	if _, err := ExportMemory(context.Background(), f, home, a, "agentry-runner:test", "", policy.Limits{}, dir); err == nil || !strings.Contains(err.Error(), "new folder") {
		t.Errorf("an export into an existing folder: %v", err)
	}
	empty := &dockerx.Fake{}
	if _, err := ExportMemory(context.Background(), empty, home, a, "agentry-runner:test", "", policy.Limits{}, filepath.Join(t.TempDir(), "m")); err == nil || !strings.Contains(err.Error(), "no memory yet") {
		t.Errorf("an export with no memory volume: %v", err)
	}
	if _, err := ExportMemory(context.Background(), f, home, coderAgent(t), "agentry-runner:test", "", policy.Limits{}, filepath.Join(t.TempDir(), "m")); err == nil {
		t.Error("the Coding agent's memory was exported")
	}
}

// TestMemoryExportStaysInItsFolder: an entry that climbs out of the folder
// refuses the export, nothing is written beside it, and the folder it made
// is removed.
func TestMemoryExportStaysInItsFolder(t *testing.T) {
	home := shortHome(t)
	parent := t.TempDir()
	f := exportFake(home, memoryArchive(t,
		tar.Header{Typeflag: tar.TypeReg, Name: "./index.md", Mode: 0o644},
		tar.Header{Typeflag: tar.TypeReg, Name: "../escaped.md", Mode: 0o644},
	))
	dir := filepath.Join(parent, "memory")
	if _, err := ExportMemory(context.Background(), f, home, assistantAgent(t), "agentry-runner:test", "", policy.Limits{}, dir); err == nil || !strings.Contains(err.Error(), "outside it") {
		t.Fatalf("an entry outside the folder: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "escaped.md")); err == nil {
		t.Error("the entry was written outside the folder")
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("the failed export left its folder")
	}
}
