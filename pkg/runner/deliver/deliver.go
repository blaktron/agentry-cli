// Package deliver copies a run's deliverables out of its output folder to a
// folder the person chose. The output folder is the run's proof and a
// delivery only reads it: a file is copied, never moved, rewritten or
// deleted, and only after its bytes are shown to be the ones the envelope's
// deliverables block names by SHA-256. What the agent wrote stays hostile:
// the source is reached through an os.Root with no link anywhere in its path,
// the destination's names are made safe (SafeName), a copy is made under a
// hidden name and takes its own only once its digest is right, never
// replacing a file that is already there, a type the system acts on when a
// folder is merely viewed is not copied, and on Windows and macOS a copy
// carries the system's flag for a download.
package deliver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// LedgerFile is the delivery record under the run's receipts, one JSON line
// (receipt.Delivery) per file per destination.
const LedgerFile = "deliveries.ndjson"

// maxEnvelopeBytes bounds the result.json a delivery reads back.
const maxEnvelopeBytes = 16 << 20

// maxCollisions is how many " (n)" names a file tries before it gives up.
const maxCollisions = 1000

// FolderOptions says where a run's deliverables go and what the folder is
// called: <Root>/<Agent>/<When as YYYY-MM-DD HHmm> <Label>/.
type FolderOptions struct {
	// Root is the folder the person chose, an absolute path. It is created
	// when it does not exist.
	Root string
	// Agent is the QuickAgent's name (AgentName), Label the title of the one
	// deliverable or else the run id (Label).
	Agent string
	Label string
	// When is the time in the folder's name; zero is now.
	When time.Time
}

// AgentName is the QuickAgent's name in a release ref (namespace/name@version).
func AgentName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return ref
}

// Label is what names a run's folder: the title of its one deliverable, or
// the run id when it has several (or none a title names).
func Label(files []receipt.Deliverable, runID string) string {
	if len(files) == 1 && strings.TrimSpace(files[0].Title) != "" {
		return files[0].Title
	}
	return runID
}

// ToFolder copies each file of a run's deliverables from runDir to the folder
// o names, and returns one record per file. It never fails the caller: a file
// that was refused or could not be written is a record with a status and a
// closed reason, and the others are still copied.
func ToFolder(runDir string, files []receipt.Deliverable, o FolderOptions) []receipt.Delivery {
	if len(files) == 0 {
		return nil
	}
	out := make([]receipt.Delivery, 0, len(files))
	all := func(status, reason string) []receipt.Delivery {
		for _, f := range files {
			out = append(out, receipt.Delivery{Destination: o.Root, Kind: receipt.DeliveryKindFolder, SHA256: f.SHA256, Status: status, Error: reason})
		}
		return out
	}
	if insideRunFolder(o.Root, runDir) {
		return all(receipt.DeliveryFailed, receipt.DeliveryErrDestination)
	}
	dest, destDir, err := openDestination(o)
	if err != nil {
		return all(receipt.DeliveryFailed, receipt.DeliveryErrDestination)
	}
	defer func() { _ = dest.Close() }()
	src, err := os.OpenRoot(runDir)
	if err != nil {
		return all(receipt.DeliveryRefused, receipt.DeliveryErrUnreadable)
	}
	defer func() { _ = src.Close() }()
	for _, f := range files {
		out = append(out, copyOne(src, dest, destDir, o.Root, f))
	}
	return out
}

// openDestination makes the run's folder under the root and opens it. The
// root is followed as given, being the person's own choice; everything below
// it is reached through an os.Root, which refuses a link that leaves it.
func openDestination(o FolderOptions) (*os.Root, string, error) {
	if !filepath.IsAbs(o.Root) {
		return nil, "", errors.New("the folder is not an absolute path")
	}
	if err := os.MkdirAll(o.Root, 0o755); err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(o.Root)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()
	when := o.When
	if when.IsZero() {
		when = time.Now()
	}
	rel := filepath.Join(SafeName(o.Agent), when.Format("2006-01-02 1504")+" "+SafeName(o.Label))
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return nil, "", err
	}
	dest, err := root.OpenRoot(rel)
	if err != nil {
		return nil, "", err
	}
	return dest, filepath.Join(o.Root, rel), nil
}

// insideRunFolder says whether dir (which need not exist yet) is the run
// folder or beneath it, links resolved: a delivery that wrote into the proof
// would add to it, and a later one would list what it added. A path that
// cannot be resolved is taken as inside.
func insideRunFolder(dir, runDir string) bool {
	d, err1 := resolveAhead(dir)
	r, err2 := filepath.EvalSymlinks(runDir)
	if err1 != nil || err2 != nil {
		return true
	}
	rel, err := filepath.Rel(r, d)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveAhead is p with its links resolved as far as p exists, and the rest
// of its names, which a delivery would create, as given.
func resolveAhead(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return "", err
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}

// dangerousTypes are the types a file manager acts on when a folder is merely
// viewed or a link is followed, with no one opening the file: Windows shell
// links and shortcuts (which can name a network path to fetch an icon from),
// search connectors and library files, Linux launchers and macOS location
// files. They are listed in a run's envelope and never copied.
var dangerousTypes = map[string]bool{
	"lnk": true, "scf": true, "url": true, "library-ms": true, "searchconnector-ms": true,
	"desktop": true, "webloc": true, "fileloc": true, "appref-ms": true, "glk": true,
}

// extType is a name's extension in lower case, without its dot.
func extType(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

func copyOne(src, dest *os.Root, destDir, destination string, f receipt.Deliverable) receipt.Delivery {
	rec := receipt.Delivery{Destination: destination, Kind: receipt.DeliveryKindFolder, SHA256: f.SHA256}
	refuse := func(reason string) receipt.Delivery {
		rec.Status, rec.Error = receipt.DeliveryRefused, reason
		return rec
	}
	rel := path.Clean(f.Path)
	if rel == "reports" || !strings.HasPrefix(rel, "reports/") {
		return refuse(receipt.DeliveryErrOutside)
	}
	if dangerousTypes[extType(rel)] {
		return refuse(receipt.DeliveryErrType)
	}
	in, reason := openNoLinks(src, rel)
	if reason != "" {
		return refuse(reason)
	}
	defer func() { _ = in.Close() }()

	// The copy is made under a hidden name of its own and takes its name only
	// once its bytes are shown to be the ones the envelope names, so no
	// unverified file is ever seen, or left, under a deliverable's name.
	partial, out, err := createPartial(dest)
	if err != nil {
		rec.Status, rec.Error = receipt.DeliveryFailed, receipt.DeliveryErrWrite
		return rec
	}
	discard := func() {
		_ = out.Close()
		_ = dest.Remove(partial)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, f.Bytes+1))
	if err != nil {
		discard()
		rec.Status, rec.Error = receipt.DeliveryFailed, receipt.DeliveryErrWrite
		return rec
	}
	if n != f.Bytes || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		// The file is not the one the envelope names: the copy made for
		// it goes, and the run folder is as it was.
		discard()
		return refuse(receipt.DeliveryErrChanged)
	}
	notMarked := markOpen(out) != nil
	if err := out.Close(); err != nil {
		_ = dest.Remove(partial)
		rec.Status, rec.Error = receipt.DeliveryFailed, receipt.DeliveryErrWrite
		return rec
	}
	name, err := publish(dest, partial, SafeName(path.Base(rel)))
	if err != nil {
		_ = dest.Remove(partial)
		rec.Status, rec.Error = receipt.DeliveryFailed, receipt.DeliveryErrWrite
		return rec
	}
	rec.Object = filepath.Join(destDir, name)
	rec.Status = receipt.DeliveryDelivered
	if notMarked || markPublished(destDir, name) != nil {
		rec.Error = receipt.DeliveryErrNotMarked
	}
	return rec
}

// openNoLinks opens the regular file at rel beneath root, reached through no
// link: every component is looked at before it is opened, and a link in any
// of them refuses the file. The reason is one of receipt's closed words.
func openNoLinks(root *os.Root, rel string) (*os.File, string) {
	parts := strings.Split(rel, "/")
	for i := range parts {
		fi, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, receipt.DeliveryErrMissing
		}
		if err != nil {
			return nil, receipt.DeliveryErrUnreadable
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, receipt.DeliveryErrLink
		}
		if i == len(parts)-1 && !fi.Mode().IsRegular() {
			return nil, receipt.DeliveryErrLink
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, receipt.DeliveryErrUnreadable
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, receipt.DeliveryErrUnreadable
	}
	return f, ""
}

// createPartial creates a new file under a hidden name in dest.
func createPartial(dest *os.Root) (string, *os.File, error) {
	for range maxCollisions {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		name := ".agentry-partial-" + hex.EncodeToString(b[:])
		f, err := dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("no free name")
}

// publish gives the verified file partial its name in dest, new: a file that
// is already there is never replaced, and the name takes " (2)", " (3)" …
// before its extension until it is free. The name is linked first, which
// fails when the name is taken; where the file system has no links (FAT,
// exFAT), the free name is checked and the file renamed to it. It returns the
// name made, and the partial name is removed.
func publish(dest *os.Root, partial, name string) (string, error) {
	ext := path.Ext(name)
	if ext == name {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	try := name
	for i := 2; i <= maxCollisions+1; i++ {
		err := dest.Link(partial, try)
		switch {
		case err == nil:
			return try, dest.Remove(partial)
		case errors.Is(err, fs.ErrExist):
		default:
			if _, serr := dest.Lstat(try); serr == nil {
				break
			}
			if rerr := dest.Rename(partial, try); rerr != nil {
				return "", rerr
			}
			return try, nil
		}
		try = stem + " (" + strconv.Itoa(i) + ")" + ext
	}
	return "", errors.New("no free name")
}

// WriteLedger records deliveries in the run's receipts/deliveries.ndjson, one
// JSON line each. With appendTo false the file is made new, as a run does for
// its own; with it true the lines are added, as a later delivery of a run
// folder does, which leaves every other file of the folder, result.json
// included, as it was.
func WriteLedger(runDir string, ds []receipt.Delivery, appendTo bool) error {
	if len(ds) == 0 && appendTo {
		return nil
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("receipts", 0o755); err != nil {
		return err
	}
	if fi, err := root.Lstat("receipts/" + LedgerFile); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("receipts/%s is not a regular file", LedgerFile)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if appendTo {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := root.OpenFile("receipts/"+LedgerFile, flags, 0o644)
	if err != nil {
		return err
	}
	var werr error
	for _, d := range ds {
		b, err := json.Marshal(d)
		if err != nil {
			werr = errors.Join(werr, err)
			continue
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			werr = errors.Join(werr, err)
		}
	}
	return errors.Join(werr, f.Close())
}

// ReadEnvelope reads the run/v1 envelope of a run folder, its result.json.
func ReadEnvelope(runDir string) (*receipt.Envelope, error) {
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, fmt.Errorf("%s is not a run folder: %w", runDir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open("result.json")
	if err != nil {
		return nil, fmt.Errorf("%s holds no result.json: it is not a run's output folder", runDir)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxEnvelopeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s/result.json: %w", runDir, err)
	}
	if len(b) > maxEnvelopeBytes {
		return nil, fmt.Errorf("%s/result.json is over %d MiB", runDir, maxEnvelopeBytes>>20)
	}
	var env receipt.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("%s/result.json is not a result envelope: %w", runDir, err)
	}
	if env.Agentry != receipt.EnvelopeVersion {
		return nil, fmt.Errorf("%s/result.json is not a %s envelope", runDir, receipt.EnvelopeVersion)
	}
	return &env, nil
}

// Redeliver copies the deliverables an existing run folder's envelope lists
// to the folder in o (Agent, Label and When default from the envelope and the
// clock) and adds the records to its receipts/deliveries.ndjson. The folder is
// otherwise untouched. An error is returned only when there is nothing to
// deliver: a folder that is not a run's, a run that did not succeed, one that
// lists no deliverable.
func Redeliver(runDir string, o FolderOptions) ([]receipt.Delivery, error) {
	env, err := ReadEnvelope(runDir)
	if err != nil {
		return nil, err
	}
	if env.Status != "ok" {
		return nil, fmt.Errorf("the run in %s did not succeed (%s), so it lists no deliverables", runDir, env.ExitClass)
	}
	if env.Deliverables == nil || len(env.Deliverables.Files) == 0 {
		return nil, fmt.Errorf("the run in %s lists no deliverables", runDir)
	}
	if o.Agent == "" {
		o.Agent = AgentName(env.Ref)
	}
	if o.Label == "" {
		o.Label = Label(env.Deliverables.Files, env.RunID)
	}
	ds := ToFolder(runDir, env.Deliverables.Files, o)
	if err := WriteLedger(runDir, ds, true); err != nil {
		return ds, fmt.Errorf("the files were copied, but %s could not be recorded in the run's receipts: %w", LedgerFile, err)
	}
	return ds, nil
}
