package toolsrv

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A declared command may write the workspace, the scratch folder, /tmp and
// the folders a PDF is drawn from, and may leave a process running, so a
// folder a tool has just checked can become a link out of its root before
// the tool uses the name. These tests swap a folder for such a link in
// exactly that gap (server.afterCheck), once per root and per tool that
// opens by root, and hold that the tool refuses and that nothing outside is
// read or written. They run on whatever container runtime the test binary
// runs under; the runner image's roots hold on all of them.

const outsideSecret = "OUTSIDE-SECRET"

// swapper makes dir a link to outside the first time the gap of root is
// reached for the nth time (counting from one).
type swapper struct {
	root, dir, outside string
	nth, seen          int
	swapped            bool
}

func (w *swapper) hook(t *testing.T) func(root string) {
	return func(root string) {
		if root != w.root || w.swapped {
			return
		}
		if w.seen++; w.seen < w.nth {
			return
		}
		w.swapped = true
		if err := os.RemoveAll(w.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(w.outside, w.dir); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
}

// outsideFolder is a folder none of the roots holds, with a file a tool
// that followed the link would read.
func outsideFolder(t *testing.T) string {
	t.Helper()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "today.md"), []byte(outsideSecret+"\n"))
	mustWrite(t, filepath.Join(outside, "r.md"), []byte("# "+outsideSecret+"\n"))
	return outside
}

func outsideHolds(t *testing.T, outside string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the folder outside holds %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "today.md")); string(b) != outsideSecret+"\n" {
		t.Fatalf("a file outside was changed: %q", b)
	}
}

func TestTheWorkspaceToolsRefuseAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	setup := func(t *testing.T) (*server, *Spec, string) {
		spec := baseSpec(t)
		spec.FSRead = Access{Enabled: true}
		spec.FSWrite = Access{Enabled: true}
		mustWrite(t, filepath.Join(spec.WorkspaceDir, "notes", "today.md"), []byte("inside\n"))
		s := newTestServer(t, spec)
		outside := outsideFolder(t)
		sw := &swapper{root: "workspace", dir: filepath.Join(spec.WorkspaceDir, "notes"), outside: outside, nth: 1}
		s.afterCheck = sw.hook(t)
		return s, spec, outside
	}
	t.Run("fs_read", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "notes/today.md"}))
		if !isErr || strings.Contains(got, outsideSecret) {
			t.Fatalf("fs_read through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("fs_list", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "notes"}))
		if !isErr || strings.Contains(got, "today.md") {
			t.Fatalf("fs_list through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("fs_write", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "notes/new.md", "content": "x"}))
		if !isErr {
			t.Fatalf("fs_write through a swapped link = %q", got)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("fs_write making a folder", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "notes/deeper/new.md", "content": "x"}))
		if !isErr {
			t.Fatalf("fs_write through a swapped link = %q", got)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
}

func TestTheScratchToolsRefuseAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	setup := func(t *testing.T) (*server, *Spec, string) {
		spec := scratchSpec(t)
		spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.example.com"}}
		mustWrite(t, filepath.Join(spec.ScratchDir, "notes", "today.md"), []byte("inside\n"))
		s := newTestServer(t, spec)
		outside := outsideFolder(t)
		sw := &swapper{root: "scratch", dir: filepath.Join(spec.ScratchDir, "notes"), outside: outside, nth: 1}
		s.afterCheck = sw.hook(t)
		return s, spec, outside
	}
	t.Run("scratch_read", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolScratchRead(rawArgs(t, map[string]any{"path": "notes/today.md"}))
		if !isErr || strings.Contains(got, outsideSecret) {
			t.Fatalf("scratch_read through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("scratch_list", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolScratchList(rawArgs(t, map[string]any{"path": "notes"}))
		if !isErr || strings.Contains(got, "today.md") {
			t.Fatalf("scratch_list through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("scratch_search", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolScratchSearch(rawArgs(t, map[string]any{"query": "secret", "path": "notes"}))
		if strings.Contains(got, outsideSecret) {
			t.Fatalf("scratch_search read through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("scratch_write", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "notes/new.md", "content": "x"}))
		if !isErr {
			t.Fatalf("scratch_write through a swapped link = %q", got)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("scratch_write appending", func(t *testing.T) {
		s, _, outside := setup(t)
		got, isErr := s.toolScratchWrite(rawArgs(t, map[string]any{"path": "notes/today.md", "content": "x", "append": true}))
		if !isErr {
			t.Fatalf("scratch_write through a swapped link = %q", got)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("a body fetch saves", func(t *testing.T) {
		s, _, outside := setup(t)
		_, _, err := s.saveStream("notes/body.bin", strings.NewReader("a body"))
		if err == nil {
			t.Fatal("a body was saved through a swapped link")
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("a body fetch saves unasked", func(t *testing.T) {
		spec := scratchSpec(t)
		s := newTestServer(t, spec)
		outside := outsideFolder(t)
		// The save makes its folder, so a link is planted where it is to be.
		planted := false
		s.afterCheck = func(root string) {
			if root == "scratch" && !planted {
				planted = true
				if err := os.Symlink(outside, filepath.Join(spec.ScratchDir, fetchDir)); err != nil {
					t.Skipf("no symlinks here: %v", err)
				}
			}
		}
		u, err := url.Parse("https://api.example.com/body.bin")
		if err != nil {
			t.Fatal(err)
		}
		if rel, err := s.saveFetched(u, []byte("a body")); err == nil {
			t.Fatalf("a body was saved through a swapped link to %s", rel)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
}

// The folders export_pdf reads and writes: the reports, the scratch folder
// and the inputs, and the render root it draws in under /tmp.
func TestExportPDFRefusesAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	typst := func(t *testing.T) typstFiles {
		return fakeTypst(t, fakePDF+`; (cd "$ROOT" && find . -type f | sort | sed 's/^/FILE:/')`)
	}
	setup := func(t *testing.T) (*server, *Spec, string) {
		spec := pdfSpec(t)
		s := pdfServer(t, spec, typst(t))
		return s, spec, outsideFolder(t)
	}
	filesOf := func(pdf string) []string {
		var names []string
		for _, m := range regexp.MustCompile(`FILE:\./(\S+)`).FindAllStringSubmatch(pdf, -1) {
			names = append(names, m[1])
		}
		return names
	}
	t.Run("a source in the reports folder", func(t *testing.T) {
		s, spec, outside := setup(t)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "notes", "r.md"), []byte("# Inside\n"))
		s.afterCheck = (&swapper{root: "reports", dir: filepath.Join(spec.OutDir, "reports", "notes"), outside: outside, nth: 1}).hook(t)
		got, isErr := exportPDF(t, s, map[string]any{"source": "notes/r.md"})
		if !isErr || strings.Contains(got, outsideSecret) {
			t.Fatalf("export through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("a source in the scratch folder", func(t *testing.T) {
		s, spec, outside := setup(t)
		mustWrite(t, filepath.Join(spec.ScratchDir, "notes", "r.md"), []byte("# Inside\n"))
		s.afterCheck = (&swapper{root: "scratch", dir: filepath.Join(spec.ScratchDir, "notes"), outside: outside, nth: 1}).hook(t)
		got, isErr := exportPDF(t, s, map[string]any{"source": "scratch/notes/r.md"})
		if !isErr || strings.Contains(got, outsideSecret) {
			t.Fatalf("export through a swapped link = (%q, %v)", got, isErr)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	t.Run("the PDF written to the reports folder", func(t *testing.T) {
		s, spec, outside := setup(t)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# Inside\n"))
		if err := os.MkdirAll(filepath.Join(spec.OutDir, "reports", "out"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The gap is reached once to read the source, then to write the PDF.
		s.afterCheck = (&swapper{root: "reports", dir: filepath.Join(spec.OutDir, "reports", "out"), outside: outside, nth: 2}).hook(t)
		got, isErr := exportPDF(t, s, map[string]any{"source": "r.md", "path": "out/r.pdf"})
		if !isErr {
			t.Fatalf("a PDF was written through a swapped link: %q", got)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
	for _, c := range []struct{ name, root string }{
		{"an image in the reports folder", "reports"},
		{"an image in the scratch folder", "scratch"},
		{"an image in the inputs folder", "inputs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, spec, outside := setup(t)
			mustWrite(t, filepath.Join(outside, "a.png"), png)
			dirs := map[string]string{
				"reports": filepath.Join(spec.OutDir, "reports"),
				"scratch": spec.ScratchDir,
				"inputs":  spec.InputsDir,
			}
			pics := filepath.Join(dirs[c.root], "pics")
			mustWrite(t, filepath.Join(pics, "a.png"), png)
			mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n\n!["+c.root+"]("+pics+"/a.png)\n"))
			// The gap for the image's folder comes after the source's, in the
			// reports folder; the others are reached only by the image.
			nth := 1
			if c.root == "reports" {
				nth = 2
			}
			s.afterCheck = (&swapper{root: c.root, dir: pics, outside: outside, nth: nth}).hook(t)
			got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
			if isErr {
				t.Fatalf("export = %q", got)
			}
			if !strings.Contains(got, "a.png (") {
				t.Errorf("the answer does not say the image was drawn as its alt text: %q", got)
			}
			if names := filesOf(string(readReport(t, spec, "r.pdf"))); slices.Contains(names, "img/1.png") {
				t.Errorf("the image was copied through a swapped link: %v", names)
			}
			outsideHolds(t, outside, "a.png", "today.md", "r.md")
		})
	}
	t.Run("the render root", func(t *testing.T) {
		s, spec, outside := setup(t)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "i.png"), png)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n\n![i](i.png)\n"))
		var made string
		s.renderRootMade = func(dir string) { made = dir }
		swapped := false
		s.afterCheck = func(root string) {
			if root != "render" || swapped {
				return
			}
			swapped = true
			// img was just made; a command swaps it for a link out of the render root.
			if err := os.Remove(filepath.Join(made, "img")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(made, "img")); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		}
		// An image that cannot be copied is drawn as its alt text; the PDF
		// is Typst's stdout, so there is no file to swap. What matters is
		// that nothing reached the folder the link leads to.
		got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
		if isErr {
			t.Fatalf("export = %q", got)
		}
		if !strings.Contains(got, "i.png (") {
			t.Errorf("the answer does not say the image was drawn as its alt text: %q", got)
		}
		if names := filesOf(string(readReport(t, spec, "r.pdf"))); slices.Contains(names, "img/1.png") {
			t.Errorf("an image was copied through a swapped link: %v", names)
		}
		outsideHolds(t, outside, "today.md", "r.md")
	})
}
