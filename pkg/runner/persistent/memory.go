package persistent

// A persistent agent's memory: a wiki it writes on a volume of its own,
// mounted read-write at /agent/memory in its tools server alone, with the
// person's share as its sources. The volume is made at the agent's first
// start that has it, seeded from its built-in's memory/ folder with the
// date, and given a copy of the notes the agent kept in its data volume
// before, which are left where they were. The export copies it out into a
// new folder; Reset removes it.

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// EntryMemory is the memory's roster entry: fixed on, for an agent that
// keeps one.
const EntryMemory = "memory"

// KindMemory is the memory's roster entry kind.
const KindMemory EntryKind = "memory"

// memoryLine is the memory entry's sentence.
const memoryLine = "Keeps a memory of what you tell it and what you share with it, as pages it writes on a volume of its own, kept until you reset it; you can export it to a folder"

// MemoryVolumeIn is the agent's memory volume in the runner home home.
func MemoryVolumeIn(home, name string) string {
	return "agentry-persistent-" + name + "-" + HomeID(home) + "-memory"
}

// The notes an agent kept in its data volume before it had a memory, and
// where a new memory keeps its copy of them.
const (
	notesFolder   = "notes"
	notesInMemory = "pages/notes"
)

// The seed's files the supervisor dates: the log, its first entry the
// memory's making; and the lines appended to the log and the index when the
// notes are carried in.
const (
	memoryLog   = "log.md"
	memoryIndex = "index.md"
	memoryPages = "pages/"
)

// memorySegment is the memory's part of a segment: its volume, its seed
// dated on, and the notes it carries in. An agent that keeps no memory has
// none.
func memorySegment(home string, a Agent, on time.Time) (volume string, seed map[string][]byte, carry, carryTo string, err error) {
	if !a.Builtin.KeepsMemory() {
		return "", nil, "", "", nil
	}
	tree, err := a.Builtin.MemorySeed()
	if err != nil {
		return "", nil, "", "", err
	}
	day := on.Format(time.DateOnly)
	seed = maps.Clone(tree)
	seed[memoryPages] = nil
	seed[memoryLog] = append(seed[memoryLog], []byte("\n## ["+day+"] seed | the memory was made\n")...)
	carried := engine.MemoryCarriedDir + "/"
	seed[carried+memoryLog] = []byte("\n## [" + day + "] ingest | data/" + notesFolder + "/: the notes kept before this memory, copied as they were into " + notesInMemory + "/\n")
	seed[carried+memoryIndex] = []byte("\n## Notes kept before this memory\n\n- [" + notesInMemory + "/](" + notesInMemory + "/): the notes from data/" + notesFolder + "/, copied in on " + day + "; fold each into a page when it comes up, citing it\n")
	return MemoryVolumeIn(home, a.Name), seed, notesFolder, notesInMemory, nil
}

// The bounds of an export: the files it writes, and their bytes in all.
const (
	maxExportFiles = 100_000
	maxExportBytes = 4 << 30
)

// Exported is what an export wrote: the files, their bytes, and the entries
// of the volume it left out because they were neither a file nor a folder.
type Exported struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped,omitempty"`
}

// ExportMemory copies the agent's memory into dir, a new folder it makes
// beneath a folder that exists: its files and folders as the volume holds
// them at that moment, read by a one-shot of image (the runner image)
// under runtime and lim. A link or any other entry is left out, and
// nothing is written outside dir. A failed export removes dir.
func ExportMemory(ctx context.Context, d dockerx.Client, home string, a Agent, image, runtime string, lim policy.Limits, dir string) (Exported, error) {
	if !a.Builtin.KeepsMemory() {
		return Exported{}, fmt.Errorf("the %s agent keeps no memory", a.Name)
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Exported{}, fmt.Errorf("the folder %q is not a clean absolute path", dir)
	}
	if _, err := os.Lstat(dir); err == nil {
		return Exported{}, fmt.Errorf("%s exists: the memory is exported into a new folder", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Exported{}, err
	}
	volume := MemoryVolumeIn(home, a.Name)
	have, err := d.VolumesByLabel(ctx, engine.PersistentHomeLabel+"="+HomeID(home))
	if err != nil {
		return Exported{}, fmt.Errorf("list the volumes of the %s agent: %w", a.Name, err)
	}
	if !slices.Contains(have, volume) {
		return Exported{}, fmt.Errorf("the %s agent has no memory yet: it is made at the agent's first start", a.Name)
	}
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the person's own folder, theirs to read
		return Exported{}, err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := engine.ExportVolume(ctx, d, image, volume, runtime, lim, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	out, err := extractMemory(pr, dir)
	_ = pr.CloseWithError(err)
	if runErr := <-done; err == nil {
		err = runErr
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return Exported{}, err
	}
	return out, nil
}

// extractMemory writes the files and folders of the tar archive r into dir,
// through a root at dir so no entry, and no link already in it, reaches
// outside; each file is new. Any other entry is counted and left out.
func extractMemory(r io.Reader, dir string) (Exported, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Exported{}, err
	}
	defer func() { _ = root.Close() }()
	var out Exported
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("read the memory's export: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return out, fmt.Errorf("the memory's export has an entry %q outside it", h.Name)
		}
		rel := filepath.FromSlash(name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, 0o755); err != nil {
				return out, err
			}
		case tar.TypeReg:
			if out.Files++; out.Files > maxExportFiles {
				return out, fmt.Errorf("the memory holds more than %d files", maxExportFiles)
			}
			if out.Bytes += h.Size; out.Bytes > maxExportBytes {
				return out, fmt.Errorf("the memory holds more than %d GiB", maxExportBytes>>30)
			}
			if parent := filepath.Dir(rel); parent != "." {
				if err := root.MkdirAll(parent, 0o755); err != nil {
					return out, err
				}
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return out, err
			}
			_, err = io.CopyN(f, tr, h.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return out, fmt.Errorf("write %s: %w", name, err)
			}
		default:
			out.Skipped++
		}
	}
}
