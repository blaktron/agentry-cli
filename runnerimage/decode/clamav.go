package decode

// The malware check.
// Every regular file a decode takes, as staged or unpacked from an archive,
// archives included, is checked by ClamAV's clamscan before any decoder
// reads it, in one clamscan run for the whole decode: loading the
// signatures costs about 18 seconds and a gigabyte, so they are loaded once.
// The argv is fixed; the files to check are named in a list the runner
// writes into its own scratch, so nothing a document contains reaches an
// argv, not even a path. clamscan also opens each archive itself, so what it
// can reach inside one is checked twice: once in the archive, once as the
// member the runner unpacked. A file clamscan finds is never decoded; the
// host refuses the run. The signatures are the bundle the host mounts
// read-only; with none, every file is unchecked, and says so.

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Check is what the malware check made of one file.
type Check string

// The checks.
const (
	// CheckClean: clamscan read it and found nothing.
	CheckClean Check = "clean"
	// CheckFound: clamscan found a signature in it (Signature names it).
	CheckFound Check = "found"
	// CheckUnchecked: it was not checked, for the reason Unchecked gives.
	CheckUnchecked Check = "unchecked"
)

// Unchecked is the closed vocabulary of why a file was not checked.
type Unchecked string

// The reasons.
const (
	// UncheckedNoSignatures: the host mounted no signatures, so clamscan
	// did not run.
	UncheckedNoSignatures Unchecked = "no-signatures"
	// UncheckedTooLarge: the file, or what clamscan unpacked from it, is
	// past clamscan's limits (Heuristics.Limits.Exceeded).
	UncheckedTooLarge Unchecked = "too-large"
	// UncheckedFailed: clamscan could not read it, failed or was stopped
	// before it answered for it.
	UncheckedFailed Unchecked = "failed"
	// UncheckedNotRisky: a file in an input source that is not an
	// executable or binary risky type, which only those are checked in a
	// source (SourceRisky).
	UncheckedNotRisky Unchecked = "not-risky"
	// UncheckedNotExecutable: a file that is not an executable, in a decode
	// that checks only executables (Limits.ExecutablesOnly).
	UncheckedNotExecutable Unchecked = "not-executable"
)

// WhyFound: a file clamscan found a signature in is never decoded.
const WhyFound Why = "found"

// clamTimeout bounds the one clamscan run: the signatures load in under a
// minute, and the files are a decode's (2,000 at most, 512 MiB unpacked).
var clamTimeout = 15 * time.Minute

// The bounds clamscan reads within. It inflates what it extracts from a
// file (a zip's member, a document's parts) into its temporary directory,
// the decoders' 512 MiB tmpfs, up to the file bound before that bound
// stops it, so both bounds are half the tmpfs: at 512M a zip bomb's member
// filled it, and clamscan could only say it failed to write. The scan bound
// is never above the file bound, or a member past the file bound is skipped
// in silence and the archive answered OK. Past either bound a file is
// reported past the limit (--alert-exceeds-max), and so unchecked, never
// passed as clean unread.
const (
	clamMaxFile = "256M"
	clamMaxScan = "256M"
)

// signatureRE is the grammar a signature's name is held to before it is
// reported: ClamAV's names (Win.Test.EICAR_HDB-1, Eicar-Test-Signature,
// Doc.Dropper.Agent-123.UNOFFICIAL). Anything else is reported unnamed.
var signatureRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ValidSignature says whether a signature name is within that grammar; the
// host holds a report to it.
func ValidSignature(s string) bool { return signatureRE.MatchString(s) }

// clamscanArgv is clamscan's argv after the command: the signature
// directory, scratch, the file list, nothing else variable. Potentially
// unwanted applications stay off; scripts/check.sh in
// agentry-dockerimages runs the same argv.
func clamscanArgv(db, scratch, list string) []string {
	return []string{
		"--database=" + db, "--tempdir=" + scratch,
		"--no-summary", "--stdout", "--detect-pua=no", "--alert-exceeds-max=yes",
		"--max-filesize=" + clamMaxFile, "--max-scansize=" + clamMaxScan,
		"--file-list=" + list,
	}
}

// scanTarget is one file the check reads: its report entry and where it is.
type scanTarget struct {
	idx int
	src string
}

// scan checks every target with one clamscan run and records the outcome
// on each one's report entry.
func (d *decoder) scan(ctx context.Context) {
	if len(d.targets) == 0 {
		return
	}
	set := func(t scanTarget, c Check, sig string, why Unchecked) {
		f := &d.files[t.idx]
		f.Check, f.Signature, f.Unchecked = c, sig, why
	}
	if d.lim.ClamDB == "" {
		for _, t := range d.targets {
			set(t, CheckUnchecked, "", UncheckedNoSignatures)
		}
		return
	}
	work, err := os.MkdirTemp(d.lim.ScratchDir, "clamscan-")
	if err != nil {
		for _, t := range d.targets {
			set(t, CheckUnchecked, "", UncheckedFailed)
		}
		return
	}
	defer func() { _ = os.RemoveAll(work) }()
	// listed maps each path the list names to its target. A path with a
	// line break cannot be a line of the list or of clamscan's answer, so
	// such a file is checked as a copy under a name of the runner's own.
	listed := map[string]scanTarget{}
	var list bytes.Buffer
	for i, t := range d.targets {
		p := t.src
		if strings.ContainsAny(p, "\n\r") {
			p = filepath.Join(work, "copy-"+strconv.Itoa(i))
			if err := copyFile(t.src, p); err != nil {
				set(t, CheckUnchecked, "", UncheckedFailed)
				continue
			}
		}
		listed[p] = t
		list.WriteString(p + "\n")
	}
	listFile := filepath.Join(work, "list")
	if err := os.WriteFile(listFile, list.Bytes(), 0o600); err != nil {
		for _, t := range d.targets {
			set(t, CheckUnchecked, "", UncheckedFailed)
		}
		return
	}
	sctx, cancel := context.WithTimeout(ctx, clamTimeout)
	defer cancel()
	cmd := exec.CommandContext(sctx, d.tools.Clamscan, clamscanArgv(d.lim.ClamDB, work, listFile)...) //nolint:gosec // fixed argv: no document-derived value
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "LC_ALL=C.UTF-8"}
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 16 << 20}
	cmd.Stderr = io.Discard
	// Exit 1 is "found"; 2 an error for some file; a signal (the memory
	// limit) leaves files unanswered. Whatever the exit, the answer is per
	// file: a file without a line was not checked.
	_ = cmd.Run()
	answered := map[int]bool{}
	unattributed := false
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		t, rest, ok := splitClamLine(sc.Text(), listed)
		if !ok {
			unattributed = unattributed || strings.HasSuffix(sc.Text(), " FOUND")
			continue
		}
		switch {
		case rest == "OK" || rest == "Empty file":
			set(t, CheckClean, "", "")
		case strings.HasSuffix(rest, " FOUND"):
			sig := strings.TrimSuffix(rest, " FOUND")
			if strings.HasPrefix(sig, "Heuristics.Limits.Exceeded") {
				set(t, CheckUnchecked, "", UncheckedTooLarge)
				break
			}
			if !ValidSignature(sig) {
				sig = ""
			}
			set(t, CheckFound, sig, "")
		default:
			set(t, CheckUnchecked, "", UncheckedFailed)
		}
		answered[t.idx] = true
	}
	for _, t := range d.targets {
		if answered[t.idx] || d.files[t.idx].Check != "" {
			continue
		}
		// A finding no listed path claims can only be one of the files
		// without an answer, so each of those is taken as found: the run
		// is refused rather than one passed unchecked.
		if unattributed {
			set(t, CheckFound, "", "")
		} else {
			set(t, CheckUnchecked, "", UncheckedFailed)
		}
	}
}

// splitClamLine splits one line of clamscan's answer, "<path>: <result>",
// at the longest listed path it starts with: a path may itself hold ": ".
func splitClamLine(line string, listed map[string]scanTarget) (scanTarget, string, bool) {
	for i := len(line) - 2; i > 0; i-- {
		if line[i] != ':' || line[i+1] != ' ' {
			continue
		}
		if t, ok := listed[line[:i]]; ok {
			return t, line[i+2:], true
		}
	}
	return scanTarget{}, "", false
}

// copyFile copies a file the decode reads into its scratch.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a file under the decode's own roots
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the decode's scratch
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
