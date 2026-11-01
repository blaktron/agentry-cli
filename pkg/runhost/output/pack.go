package output

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// MaxOutput bounds the plaintext of one run's output: what Pack packs and
// Unpack takes.
const MaxOutput = 4 << 30

// Members are what a device takes back of a run's output folder: the
// result, the reports, the receipts and the outputs. Nothing else of the
// folder leaves the server.
var memberDirs = []string{"reports", "receipts", "outputs"}

const resultFile = "result.json"

// member says whether name, slash-separated and relative to the output
// folder, is one a device takes back.
func member(name string) bool {
	if name == resultFile {
		return true
	}
	for _, d := range memberDirs {
		if strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// cleanName is name if it is a clean relative path inside the folder, else
// "".
func cleanName(name string) string {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return ""
	}
	c := path.Clean(name)
	if c != name || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// Pack writes a tar of dir's members to w: regular files only, so a link
// the run left never carries anything from outside the folder.
func Pack(w io.Writer, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var names []string
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && member(p) {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	tw := tar.NewWriter(w)
	var total int64
	for _, n := range names {
		if err := packOne(tw, root, n, &total); err != nil {
			return err
		}
	}
	return tw.Close()
}

func packOne(tw *tar.Writer, root *os.Root, name string, total *int64) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	if *total += st.Size(); *total > MaxOutput {
		return fmt.Errorf("the run's output is over %d bytes", int64(MaxOutput))
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, st.Size())
	return err
}

// Unpack writes the tar r into dir, which it creates: regular files only,
// each a member under a clean relative name, so no entry lands outside the
// folder or outside the members a run's output may hold.
func Unpack(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := cleanName(h.Name)
		if h.Typeflag != tar.TypeReg || name == "" || !member(name) {
			return fmt.Errorf("the output holds %q, which is not a file a run's output carries", h.Name)
		}
		if total += h.Size; h.Size < 0 || total > MaxOutput {
			return fmt.Errorf("the output is over %d bytes", int64(MaxOutput))
		}
		if d := path.Dir(name); d != "." {
			if err := root.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
				return err
			}
		}
		f, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, cerr := io.CopyN(f, tr, h.Size)
		if err := f.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
	}
}
