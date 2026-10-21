package engine

// A persistent agent's share outbox: the folder an MCP server that sees the
// share saves into, mounted writable at spec.AgentRoot/spec.AgentShareOutDir
// in each such server and in no other container. The server cannot write
// the share, so after each turn the runner copies what it saved into the
// share itself: through an os.Root at each end, regular files at the
// outbox's top level only, each under a name the share does not hold yet,
// never over a file, within the spec.ShareOut* caps; then it empties the
// outbox. A copied file is a new file of the share, so the next turn's
// share check reads it as it reads any file the person adds, and the agent's
// own fs_write stays the only way to change a file already there.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// shareOutName is the share outbox's folder in a run directory: beside its
// cfg, not in it, so that it is a bind root of its own that the docker
// filter holds to containers with no network.
const shareOutName = "share-out"

// shareOutCaps are the spec.ShareOut* caps the copy holds to, a variable so
// a test can hold it to smaller ones.
var shareOutCaps = struct {
	files       int
	file, total int64
}{spec.ShareOutMaxFiles, spec.ShareOutFileMax, spec.ShareOutTotalMax}

// shareOutNameTries bounds the names tried for one file: <name>, then
// "<name> (2)" up to "<name> (shareOutNameTries)".
const shareOutNameTries = 100

// shareOutNameMax bounds a saved file's name, in bytes, leaving room for the
// " (n)" a clash adds within the 255 bytes most file systems allow.
const shareOutNameMax = 200

// prepareShareOut gives a segment whose servers see the share its outbox: an
// empty folder of the run's own, labelled for the servers' containers on an
// SELinux host.
func prepareShareOut(p Params, rd *runDirs) error {
	if !spec.SharesAny(p.Plan.MCPServers) {
		return nil
	}
	if rd.share == "" {
		return Classify(ExitCompile, errors.New("the segment's servers are given a share outbox without a share folder"))
	}
	dir := filepath.Join(rd.root, shareOutName)
	if err := os.RemoveAll(dir); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the share outbox: %w", err))
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the share outbox: %w", err))
	}
	if err := gatewayCanMount(dir, gatewayOS); err != nil {
		return Classify(ExitCLI, fmt.Errorf("the share outbox %s: %w", dir, err))
	}
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	relabelShared(context.Background(), stderr, dir)
	rd.shareOut = dir
	return nil
}

// shareOutRoot is the share outbox in the gateway's spelling, as a root:
// none when no server of the run sees the share.
func (rd *runDirs) shareOutRoot() []string {
	if rd.shareOut == "" {
		return nil
	}
	return []string{rd.gatewayPath(rd.shareOut)}
}

// outboxCopy is what became of one entry of the share outbox: copied into
// the share as Path, or Refused with why. It is a line of the segment's
// receipts/share-checks.ndjson.
type outboxCopy struct {
	Time time.Time `json:"time"`
	// Outbox is the entry's name in the outbox.
	Outbox string `json:"outbox"`
	// Path is the copy's path beneath the share; empty for a refused entry.
	Path    string `json:"path,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Refused string `json:"refused,omitempty"`
	// Left is why the entry could not be removed from the outbox.
	Left string `json:"left,omitempty"`
}

// errNotOutboxFile refuses an entry that is not a plain file of the outbox:
// a link, a folder, a device, or a file with another link to it.
type errNotOutboxFile struct{ why string }

func (e errNotOutboxFile) Error() string { return e.why }

// copyShareOut copies the regular files at the top level of outbox into
// share and empties outbox, answering what became of each entry, by name.
// Each copy is written under a name share does not hold, created
// exclusively, so nothing there is ever replaced; past the caps a file is
// refused. Every entry is removed from the outbox, copied or not.
func copyShareOut(outbox, share string) ([]outboxCopy, error) {
	src, err := os.OpenRoot(outbox)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenRoot(share)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dst.Close() }()
	entries, err := fs.ReadDir(src.FS(), ".")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out []outboxCopy
	var total int64
	files := 0
	for _, de := range entries {
		name := de.Name()
		c := outboxCopy{Time: now, Outbox: name}
		switch {
		case de.Type()&fs.ModeSymlink != 0:
			c.Refused = "a link: only regular files are copied"
		case de.IsDir():
			c.Refused = "a folder: only files at the top of " + spec.AgentRoot + "/" + spec.AgentShareOutDir + " are copied"
		case !de.Type().IsRegular():
			c.Refused = "not a regular file"
		case outboxNameError(name) != nil:
			c.Refused = outboxNameError(name).Error()
		case files >= shareOutCaps.files:
			c.Refused = fmt.Sprintf("past the %d files copied a turn", shareOutCaps.files)
		default:
			c.Path, c.SHA256, c.Size, err = copyOutboxFile(src, dst, name, shareOutCaps.total-total)
			if err != nil {
				c.Path, c.SHA256, c.Size, c.Refused = "", "", 0, err.Error()
			} else {
				files++
				total += c.Size
			}
		}
		if err := src.RemoveAll(name); err != nil {
			c.Left = err.Error()
		}
		out = append(out, c)
	}
	return out, nil
}

// copyOutboxFile copies name, a file of the outbox, into the share under the
// first name the share does not hold, answering that name, the copy's
// SHA-256 and its size. The file must be a regular file with no other link
// to it, the one the outbox listed, and at most the file cap and room bytes,
// held to that as it is read.
func copyOutboxFile(src, dst *os.Root, name string, room int64) (string, string, int64, error) {
	limit := min(shareOutCaps.file, room)
	fi, err := src.Lstat(name)
	if err != nil {
		return "", "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", "", 0, errNotOutboxFile{"not a regular file"}
	}
	if outboxLinks(fi) > 1 {
		return "", "", 0, errNotOutboxFile{"a file with another link to it: only a file of its own is copied"}
	}
	if fi.Size() > limit {
		return "", "", 0, outboxTooLarge(limit)
	}
	in, err := src.OpenFile(name, outboxOpenFlags, 0)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = in.Close() }()
	// The file opened is the one listed: a name swapped for a link or
	// another file since is refused.
	if ofi, err := in.Stat(); err != nil || !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) || outboxLinks(ofi) > 1 {
		return "", "", 0, errNotOutboxFile{"it changed while it was being copied"}
	}
	as, w, err := createNewShareFile(dst, name)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(w, h), io.LimitReader(in, limit+1))
	if err := w.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil && n > limit {
		cerr = outboxTooLarge(limit)
	}
	if cerr != nil {
		_ = dst.Remove(as)
		return "", "", 0, cerr
	}
	return as, hex.EncodeToString(h.Sum(nil)), n, nil
}

// outboxTooLarge refuses a file past the bytes left to copy.
func outboxTooLarge(limit int64) error {
	if limit < shareOutCaps.file {
		return fmt.Errorf("past the %s copied a turn", mib(shareOutCaps.total))
	}
	return fmt.Errorf("larger than %s", mib(shareOutCaps.file))
}

// mib spells a cap in MiB, or in bytes below one.
func mib(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

// createNewShareFile creates, exclusively, the first of name, "name (2)" …
// that the share does not hold, so a file already there, or one made there
// at the same moment, is never written over.
func createNewShareFile(dst *os.Root, name string) (string, *os.File, error) {
	for n := 1; n <= shareOutNameTries; n++ {
		as := numberedName(name, n)
		w, err := dst.OpenFile(as, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return as, w, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("the share holds %s and %d more files of that name", name, shareOutNameTries-1)
}

// numberedName is name for n = 1, and name with " (n)" before its extension
// after that: "report.docx", "report (2).docx".
func numberedName(name string, n int) string {
	if n <= 1 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
}

// windowsDeviceNames are names Windows takes for a device, whatever the
// extension.
var windowsDeviceNames = []string{"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

// outboxNameError refuses a name the share is not given: one that is
// hidden, that reads as something else (a control or formatting character,
// which can turn "fdp.exe" around), that some system cannot hold, or that is
// too long to number.
func outboxNameError(name string) error {
	switch {
	case !utf8.ValidString(name):
		return errors.New("its name is not UTF-8")
	case len(name) > shareOutNameMax:
		return fmt.Errorf("its name is longer than %d bytes", shareOutNameMax)
	case strings.HasPrefix(name, "."):
		return errors.New("a hidden file: its name starts with a dot")
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return errors.New("its name ends with a dot or a space")
	case strings.ContainsAny(name, `<>:"/\|?*`):
		return errors.New(`its name holds one of < > : " / \ | ? *`)
	case strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0:
		return errors.New("its name holds a control or formatting character")
	}
	base, _, _ := strings.Cut(name, ".")
	if slices.ContainsFunc(windowsDeviceNames, func(d string) bool { return strings.EqualFold(strings.TrimRight(base, " "), d) }) {
		return errors.New("its name is one Windows keeps for a device")
	}
	return nil
}

// shareOutHandOver gives what a container saved in the outbox to the person
// before the runner reads it: on an engine whose containers' root is not
// the person (rootful Docker), a file saved there as root with a mode that
// shuts the person out, or a folder they cannot empty, would stay unread
// and unremoved. It runs only when such an entry is there, as a one-shot of
// the runner image with CHOWN, DAC_READ_SEARCH and FOWNER alone, on no
// network, given the outbox alone. A variable so a test can stand in.
var shareOutHandOver = func(ctx context.Context, d dockerx.Client, p Params, rd *runDirs) error {
	user := rd.personUser()
	if user == "" || !outboxNeedsHandOver(rd.shareOut) {
		return nil
	}
	const target = "/share-out"
	name := containerName(rd.runID, "share-out")
	s := readOnlyRoot(hardened(dockerx.RunSpec{
		Name:       name,
		Image:      p.Plan.Proxy.Image,
		Entrypoint: "sh",
		// Neither walk follows a link: chmod -R passes over the links it
		// meets, and chown -R -h changes a link itself.
		Argv:       []string{"-c", `chmod -R u+rwX -- "$1" && chown -R -h -- "$2" "$1"`, "sh", target, user},
		User:       "0:0",
		Network:    "none",
		Mounts:     []dockerx.Mount{{Source: rd.shareOut, Target: target, Relabel: true}},
		Labels:     runLabels(rd.runID),
		AutoRemove: true,
		Runtime:    p.Plan.Runtime(),
	}, p.limits()))
	s.CapAdd = []string{"CHOWN", "DAC_READ_SEARCH", "FOWNER"}
	code, err := dockerx.RunForegroundUnder(ctx, d, s, "share outbox's one-shot", nil, io.Discard, io.Discard)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		_ = d.RemoveForce(context.WithoutCancel(ctx), name)
		return fmt.Errorf("give the share outbox's files to you: %w", err)
	}
	return nil
}

// copyShareOut copies the share outbox into the share, once a turn is over
// and at the segment's end: what is copied and what is refused is a line
// each in receipts/share-checks.ndjson, a notice for the person, and a note
// carried into the agent's next turn with the names the files took.
func (s *liveSession) copyShareOut(ctx context.Context) {
	rd := s.rd
	if rd == nil || rd.shareOut == "" {
		return
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if err := shareOutHandOver(ctx, s.d, s.p, rd); err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "the share outbox: " + s.redact.String(err.Error())})
	}
	copies, err := copyShareOut(rd.shareOut, rd.share)
	if err != nil {
		s.emit(session.Event{Kind: session.KindNotice, Text: "what the MCP servers saved could not be copied into the share: " + s.redact.String(err.Error())})
		return
	}
	if len(copies) == 0 {
		return
	}
	s.logShareOut(copies)
	var copied, refused []string
	out := spec.AgentRoot + "/" + spec.AgentShareOutDir + "/"
	for _, c := range copies {
		if c.Refused != "" {
			refused = append(refused, fmt.Sprintf("%s%s (%s)", out, c.Outbox, c.Refused))
			s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%s%s, saved by an MCP server, was not copied into the share: %s", out, c.Outbox, c.Refused)})
			continue
		}
		copied = append(copied, fmt.Sprintf("%s%s as %s/%s", out, c.Outbox, spec.AgentShareDir, c.Path))
		s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("an MCP server saved %s/%s", spec.AgentShareDir, c.Path)})
	}
	for _, c := range copies {
		if c.Left != "" {
			s.emit(session.Event{Kind: session.KindNotice, Text: fmt.Sprintf("%s%s could not be removed from the share outbox: %s", out, c.Outbox, s.redact.String(c.Left))})
		}
	}
	var note []string
	if len(copied) > 0 {
		note = append(note, "Copied into the share after the last turn: "+strings.Join(copied, "; ")+".")
	}
	if len(refused) > 0 {
		note = append(note, "Not copied into the share, and removed: "+strings.Join(refused, "; ")+".")
	}
	s.mu.Lock()
	if !s.ending {
		s.pendingNotices = append(s.pendingNotices, strings.Join(note, " "))
	}
	s.mu.Unlock()
}

// logShareOut appends what became of the outbox's entries to the segment's
// receipt of the share.
func (s *liveSession) logShareOut(copies []outboxCopy) {
	f, err := os.OpenFile(filepath.Join(s.rd.receiptsDir, ShareChecksLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt the run owns
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	for _, c := range copies {
		if line, err := json.Marshal(c); err == nil {
			_, _ = f.Write(append(line, '\n'))
		}
	}
}
