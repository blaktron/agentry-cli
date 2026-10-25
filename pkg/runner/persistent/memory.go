package persistent

// A persistent agent's memory: a wiki it writes on a volume of its own,
// mounted read-write at /agent/memory in its tools server alone, with the
// person's share as its sources. The volume is made at a start that finds
// none and seeded once from its built-in's memory/ folder with the date;
// a data volume's notes/ folder is copied into it then and left where it
// is. The export copies the memory out into a new folder; Reset removes
// it.

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

// The data volume's folder of notes a seed copies into the memory, and
// where in the memory it copies it.
const (
	notesFolder   = "notes"
	notesInMemory = "pages/notes"
)

// The seed's log and index, which the supervisor adds dated lines to, and
// its pages folder.
const (
	memoryLog   = "log.md"
	memoryIndex = "index.md"
	memoryPages = "pages/"
)

// memoryLogTime is the form of a log line's date and time, as the memory's
// schema writes it.
const memoryLogTime = "2006-01-02T15:04"

// memorySeededFile, in the agent's state folder, names the memory volume a
// start seeded, so a later start seeds it only if it did not.
const memorySeededFile = "memory-seeded"

// memorySegment is the memory's part of a segment: its volume, its seed
// dated on, the notes folder it copies in, and whether a start has seeded
// it. An agent that keeps no memory has none.
func memorySegment(home string, a Agent, on time.Time) (*engine.PersistentMemory, error) {
	if !a.Builtin.KeepsMemory() {
		return nil, nil
	}
	tree, err := a.Builtin.MemorySeed()
	if err != nil {
		return nil, err
	}
	volume := MemoryVolumeIn(home, a.Name)
	kept, _ := os.ReadFile(filepath.Join(Dir(home, a.Name), memorySeededFile))
	day, at := on.Format(time.DateOnly), on.Format(memoryLogTime)
	seed := maps.Clone(tree)
	seed[memoryPages] = nil
	seed[memoryLog] = append(seed[memoryLog], []byte(at+" seed agentry memory\n")...)
	carried := engine.MemoryCarriedDir + "/"
	seed[carried+memoryLog] = []byte(at + " ingest data/" + notesFolder + "/ " + notesFolder + "/ copied into " + notesInMemory + "/ as sources\n")
	seed[carried+memoryIndex] = []byte(notesFolder + "/|src|the notes from data/" + notesFolder + "/, copied into " + notesInMemory + "/ on " + day + " as sources: fold each into pages\n")
	return &engine.PersistentMemory{Volume: volume, Seed: seed, Carry: notesFolder, CarryTo: notesInMemory,
		Seeded: strings.TrimSpace(string(kept)) == volume}, nil
}

// memoryStarted records that a start seeded the agent's memory, for a
// segment that has one.
func memoryStarted(home, name string, m *engine.PersistentMemory) error {
	if m == nil || m.Seeded {
		return nil
	}
	return os.WriteFile(filepath.Join(Dir(home, name), memorySeededFile), []byte(m.Volume+"\n"), 0o600)
}

// The bounds of an export: the files and folders it writes, and the files'
// bytes in all.
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
	// The memory holds what the agent learned from the person's files, so
	// the copy is theirs alone to read, as the volume is.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Exported{}, err
	}
	// A failed write stops the one-shot rather than leave it writing to a
	// pipe nobody reads.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := engine.ExportVolume(ctx, d, image, volume, runtime, lim, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	out, err := extractMemory(pr, dir)
	if err != nil {
		cancel()
	}
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
// outside; each file is new, and the files and folders are the person's
// alone to read. Any other entry is counted and left out.
func extractMemory(r io.Reader, dir string) (Exported, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Exported{}, err
	}
	defer func() { _ = root.Close() }()
	var out Exported
	entries := 0
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
		if h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeReg {
			if entries++; entries > maxExportFiles {
				return out, fmt.Errorf("the memory holds more than %d files and folders", maxExportFiles)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, 0o700); err != nil {
				return out, err
			}
		case tar.TypeReg:
			out.Files++
			if out.Bytes += h.Size; out.Bytes > maxExportBytes {
				return out, fmt.Errorf("the memory holds more than %d GiB", maxExportBytes>>30)
			}
			if parent := filepath.Dir(rel); parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return out, err
				}
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
