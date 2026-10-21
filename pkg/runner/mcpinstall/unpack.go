package mcpinstall

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The builder installs a server inside the container's own filesystem, at the
// server's prefix, and ends by writing that tree to its stdout as one tar
// archive. Many small writes then stay inside the engine, which matters where
// the engine runs in a VM and a folder of this machine reaches it through a
// file-sharing mount (Docker Desktop and Rancher Desktop on Windows, where it
// is 9p): an install of thousands of files through such a mount took minutes,
// and the same install inside the VM takes seconds. unpackStore writes the
// archive into the server's folder here, and the store's digest is taken over
// what it wrote, as before.

// Bounds on one server's archive, far above any real install, so a builder
// that writes without end fills neither the disk nor the CLI's time.
const (
	maxStoreEntries = 1_000_000
	maxStoreBytes   = 32 << 30
)

// unpackStore writes the tar archive read from r into dir, the server's folder
// on this machine, which must exist and be empty. Every name is taken relative
// to dir and every write goes through an os.Root on it, so nothing lands
// outside it. It accepts folders and regular files and refuses any other
// entry: a link of either kind (the builders leave none, since Windows lets
// only a privileged process make one), a device, a FIFO, a name that leaves
// the folder, and a second entry for a name already written.
func unpackStore(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	var entries int
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the builder's archive: %w", err)
		}
		if entries++; entries > maxStoreEntries {
			return fmt.Errorf("the builder's archive holds more than %d entries", maxStoreEntries)
		}
		name, err := archiveName(h.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("unpack %s: %w", h.Name, err)
			}
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > maxStoreBytes {
				return fmt.Errorf("the builder's archive holds more than %d bytes", int64(maxStoreBytes))
			}
			if err := unpackFile(root, name, h, tr); err != nil {
				return fmt.Errorf("unpack %s: %w", h.Name, err)
			}
		default:
			return fmt.Errorf("unpack %s: an entry of type %q is not a file or a folder, and a server's folder holds nothing else", h.Name, string(h.Typeflag))
		}
	}
}

// unpackFile writes one regular file, new, with the archive's executable
// bits: the store's digest records them, and a launcher needs them. Its
// modification time is now, not the archive's: setting a time is a second
// open of each file, which on Windows waits on the virus scanner's check of
// the file just written (19 ms a file, measured), and nothing in a store
// reads it (the PyPI builder compiles Python's modules to be loaded without
// a check against their source's time).
func unpackFile(root *os.Root, name string, h *tar.Header, body io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if h.Mode&0o111 != 0 {
		mode = 0o755
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(body, h.Size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != h.Size {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// archiveName is an entry's name as a path inside the server's folder, in
// slash form, or an error for one that is absolute or climbs out of it.
func archiveName(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." {
		return ".", nil
	}
	if strings.Contains(name, `\`) || !filepath.IsLocal(filepath.FromSlash(clean)) || !fs.ValidPath(clean) {
		return "", fmt.Errorf("unpack %s: the name is not a path inside the server's folder", name)
	}
	return clean, nil
}

// archiveSink is the builder's stdout: what is written to it is unpacked into
// a folder as it arrives (unpackStore). Close says whether it was.
type archiveSink struct {
	pw   *io.PipeWriter
	done chan error
}

func newArchiveSink(dir string) *archiveSink {
	pr, pw := io.Pipe()
	s := &archiveSink{pw: pw, done: make(chan error, 1)}
	go func() {
		err := unpackStore(pr, dir)
		// Whatever follows the archive's end (a tar writer pads its last
		// record), or the rest of a refused one, is read and dropped, so
		// the builder is never left blocked on a full pipe.
		_, _ = io.Copy(io.Discard, pr)
		s.done <- err
	}()
	return s
}

func (s *archiveSink) Write(p []byte) (int, error) { return s.pw.Write(p) }

// Close ends the stream and waits for the unpack: its error, or nil.
func (s *archiveSink) Close() error {
	_ = s.pw.Close()
	return <-s.done
}

// stagingPrefix names the folders an install of id unpacks into, beside its
// own and hidden, like its lock.
func stagingPrefix(id string) string { return "." + id + ".install-" }

// removeStaging removes the staging folders of id that an install killed
// before it finished left in the store.
func removeStaging(storeDir, id string) {
	left, _ := filepath.Glob(filepath.Join(storeDir, stagingPrefix(id)+"*"))
	for _, dir := range left {
		_ = os.RemoveAll(dir)
	}
}

// replaceDir puts the unpacked staging folder in the server's place,
// removing what an earlier, unrecorded install left there.
func replaceDir(staging, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the earlier install of the server: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return fmt.Errorf("put the installed server in place: %w", err)
	}
	return nil
}
