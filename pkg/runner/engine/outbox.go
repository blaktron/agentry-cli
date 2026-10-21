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
// the share as Path, Refused with why and removed, or Kept for the next
// turn when the copy failed for a reason of the machine's (a full disk, a
// file it could not open). It is a line of the segment's
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
	Kept    string `json:"kept,omitempty"`
	// Left is why the entry could not be removed from the outbox.
	Left string `json:"left,omitempty"`
}

// outboxRefusal refuses an entry by the outbox's rules: not a plain file of
// its own, a name the share is not given, or past a cap. A refused entry is
// removed; any other failure keeps the entry for the next turn.
type outboxRefusal struct{ why string }

func (e outboxRefusal) Error() string { return e.why }

// shareOutMaxEntries bounds the entries of the outbox one copy looks at and
// reports one by one; the rest are removed unread and counted.
const shareOutMaxEntries = 1000

// copyOutboxInto copies the regular files at the top level of outbox into
// share and empties outbox, answering what became of each entry it looked
// at, by name, and how many more it removed unread. Each copy is written
// under a name share does not hold, created exclusively, so nothing there
// is ever replaced; past the caps a file is refused.
func copyOutboxInto(outbox, share string) ([]outboxCopy, int, error) {
	src, err := os.OpenRoot(outbox)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenRoot(share)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = dst.Close() }()
	entries, err := readOutbox(src, shareOutMaxEntries)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	var out []outboxCopy
	var total int64
	files := 0
	for _, de := range entries {
		name := de.Name()
		c := outboxCopy{Time: now, Outbox: name}
		var nameErr error
		if de.Type().IsRegular() {
			nameErr = outboxNameError(name)
		}
		switch {
		case de.Type()&fs.ModeSymlink != 0:
			c.Refused = "a link: only regular files are copied"
		case de.IsDir():
			c.Refused = "a folder: only files at the top of " + spec.AgentShareOutPath + " are copied"
		case !de.Type().IsRegular():
			c.Refused = "not a regular file"
		case nameErr != nil:
			c.Refused = nameErr.Error()
		case files >= shareOutCaps.files:
			c.Refused = fmt.Sprintf("past the %d files copied a turn", shareOutCaps.files)
		default:
			c.Path, c.SHA256, c.Size, err = copyOutboxFile(src, dst, name, shareOutCaps.total-total)
			var refusal outboxRefusal
			switch {
			case errors.As(err, &refusal):
				c.Refused = refusal.why
			case err != nil:
				c.Kept = err.Error()
			default:
				files++
				total += c.Size
			}
		}
		if c.Kept == "" {
			if err := src.RemoveAll(name); err != nil {
				c.Left = err.Error()
			}
		}
		out = append(out, c)
	}
	return out, removeOutboxRest(src, len(entries)), nil
}

// readOutbox lists at most n of the outbox's entries, by name.
func readOutbox(src *os.Root, n int) ([]fs.DirEntry, error) {
	d, err := src.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

// removeOutboxRest removes, unread, what the outbox holds past the entries
// a copy looked at, when it looked at a full batch, and answers how many it
// removed. It stops at a batch it cannot remove any of, so an entry that
// will not go cannot hold it.
func removeOutboxRest(src *os.Root, looked int) int {
	if looked < shareOutMaxEntries {
		return 0
	}
	removed := 0
	for range 100 {
		entries, err := readOutbox(src, shareOutMaxEntries)
		if err != nil || len(entries) == 0 {
			break
		}
		gone := 0
		for _, de := range entries {
			if src.RemoveAll(de.Name()) == nil {
				gone++
			}
		}
		removed += gone
		if gone == 0 {
			break
		}
	}
	return removed
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
		return "", "", 0, outboxRefusal{"not a regular file"}
	}
	if outboxLinks(fi) > 1 {
		return "", "", 0, outboxRefusal{"a file with another link to it: only a file of its own is copied"}
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
		return "", "", 0, outboxRefusal{"it changed while it was being copied"}
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
		return outboxRefusal{fmt.Sprintf("past the %s copied a turn", mib(shareOutCaps.total))}
	}
	return outboxRefusal{fmt.Sprintf("larger than %s", mib(shareOutCaps.file))}
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
	return "", nil, outboxRefusal{fmt.Sprintf("the share holds %s and %d more files of that name", name, shareOutNameTries-1)}
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

// shareOutListed bounds the refused entries a copy names to the person and
// to the agent one by one; the receipt has every line.
const shareOutListed = 20

// shareOutHandOverTimeout bounds the hand-over's one-shot.
const shareOutHandOverTimeout = 2 * time.Minute

// copyShareOut copies the share outbox into the share, once a turn is over
// and at the segment's end: what is copied and what is refused is a line
// each in receipts/share-checks.ndjson, a notice for the person, and a note
// carried into the agent's next turn with the names the files took. A name
// that was refused is quoted wherever it is shown, so its characters are
// never taken for the terminal's or the conversation's own.
func (s *liveSession) copyShareOut(ctx context.Context) {
	rd := s.rd
	if rd == nil || rd.shareOut == "" {
		return
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, shareOutHandOverTimeout)
	err := shareOutHandOver(hctx, s.d, s.p, rd)
	cancel()
	if err != nil {
		s.sayShareOut("the share outbox: " + err.Error())
	}
	copies, more, err := copyOutboxInto(rd.shareOut, rd.share)
	if err != nil {
		s.sayShareOut("what the MCP servers saved could not be copied into the share: " + err.Error())
		return
	}
	if more > 0 {
		copies = append(copies, outboxCopy{Time: time.Now().UTC(), Refused: fmt.Sprintf("%d more entries past the %d a turn looks at, removed unread", more, shareOutMaxEntries)})
	}
	if len(copies) == 0 {
		return
	}
	s.logShareOut(copies)
	var copied, refused []string
	unlisted := 0
	for _, c := range copies {
		switch {
		case c.Path != "":
			copied = append(copied, fmt.Sprintf("%s/%s as %s/%s", spec.AgentShareOutPath, c.Outbox, spec.AgentShareDir, c.Path))
			s.sayShareOut(fmt.Sprintf("an MCP server saved %s/%s", spec.AgentShareDir, c.Path))
		case len(refused) >= shareOutListed:
			unlisted++
		case c.Outbox == "":
			refused = append(refused, c.Refused)
			s.sayShareOut("the share outbox: " + c.Refused)
		case c.Kept != "":
			refused = append(refused, fmt.Sprintf("%q (kept for the next turn: %s)", c.Outbox, c.Kept))
			s.sayShareOut(fmt.Sprintf("%q, saved by an MCP server in %s, could not be copied into the share and is kept for the next turn: %s", c.Outbox, spec.AgentShareOutPath, c.Kept))
		default:
			refused = append(refused, fmt.Sprintf("%q (%s)", c.Outbox, c.Refused))
			s.sayShareOut(fmt.Sprintf("%q, saved by an MCP server in %s, was not copied into the share: %s", c.Outbox, spec.AgentShareOutPath, c.Refused))
		}
		if c.Left != "" && len(refused) < shareOutListed {
			s.sayShareOut(fmt.Sprintf("%q could not be removed from the share outbox: %s", c.Outbox, c.Left))
		}
	}
	if unlisted > 0 {
		refused = append(refused, fmt.Sprintf("%d more, named in receipts/%s", unlisted, ShareChecksLog))
		s.sayShareOut(fmt.Sprintf("%d more entries of the share outbox were not copied; receipts/%s names each", unlisted, ShareChecksLog))
	}
	var note []string
	if len(copied) > 0 {
		note = append(note, "Copied into the share after the last turn: "+strings.Join(copied, "; ")+".")
	}
	if len(refused) > 0 {
		note = append(note, "Not copied into the share from "+spec.AgentShareOutPath+": "+strings.Join(refused, "; ")+".")
	}
	s.mu.Lock()
	if !s.ending {
		s.pendingNotices = append(s.pendingNotices, s.redact.String(strings.Join(note, " ")))
	}
	s.mu.Unlock()
}

// sayShareOut tells the person what the outbox's copy did: in the session,
// or, once it has ended (the copy at its end), on the segment's stderr.
func (s *liveSession) sayShareOut(text string) {
	if ended, _ := s.Ended(); ended {
		if s.p.Stderr != nil {
			_, _ = fmt.Fprintln(s.p.Stderr, "note: "+s.redact.String(text))
		}
		return
	}
	s.emit(session.Event{Kind: session.KindNotice, Text: text})
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
