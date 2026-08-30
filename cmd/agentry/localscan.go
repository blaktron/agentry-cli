// The S1 half of local/remote validation parity (D-12).
//
// `agentry validate` used to run the manifest V-rules plus the S3/S4 content
// scan, and stop. That left a real gap: a committed .dylib, a vendored
// tarball, or a Latin-1 README passes locally and is rejected by the server's
// S1 inventory at submit — the exact "works on my machine, fails at publish"
// round-trip D-12 exists to prevent.
//
// This file closes that gap by running the same three S1 checks the pipeline
// runs, over the resolved include set, with the same severities:
//
//	S1/binary-executable  critical  content-sniffed ELF/Mach-O/PE/class
//	S1/nested-archive     critical  content-sniffed zip/gzip/zstd/bzip2/xz/tar
//	S1/non-utf8           low       text file that is not valid UTF-8
//
// Detection is by MAGIC BYTES, never by extension: renaming evil.so to
// notes.md is the first thing anyone tries, and the server is not fooled by
// it either.
//
// The fourth S1-adjacent check §19 calls for — oversized files against the
// archive quotas — is deliberately NOT repeated here: manifest rule V8
// already enforces MaxFiles/MaxFileBytes/MaxImageFileBytes/MaxArchiveBytes
// over the same resolved include set, in the same command, and reporting one
// oversized file twice would make the output worse rather than safer.
//
// Parity is asserted rather than asserted-to: TestLocalS1MatchesPipeline runs
// the real internal/scan orchestrator over the same fixtures and requires the
// two to agree, so a change to the server's magic tables breaks this build.
package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/archive"
)

// localFinding is one S1-subset result, shaped for the same printer the S3/S4
// findings use.
type localFinding struct {
	// Scanner is the pipeline scanner id, e.g. "S1/binary-executable".
	Scanner string
	// Severity matches the server's vocabulary ("critical", "low").
	Severity string
	// Title is the one-line description the server would print.
	Title string
	// FilePath is the include-set-relative path.
	FilePath string
	// Detail explains why the file was flagged.
	Detail string
}

// Detection (magic bytes, text heuristic, image extensions) is pkg/archive —
// the same predicates the server's S1 stage classifies with, which is what
// makes this subset parity by construction rather than by mirroring.

// scanS1Local runs the S1 subset over already-read file contents. It takes
// the same []rules.File-shaped pairs validate already assembled, so no file
// is read twice.
func scanS1Local(files []localFile) []localFinding {
	var out []localFinding
	for _, f := range files {
		switch {
		case archive.IsExecutable(f.Content):
			out = append(out, localFinding{
				Scanner: "S1/binary-executable", Severity: "critical",
				Title:    "Executable binary in package",
				FilePath: f.Path,
				Detail:   "executables and shared libraries are blocked for all kinds in v1",
			})
		case archive.IsArchiveData(f.Content):
			out = append(out, localFinding{
				Scanner: "S1/nested-archive", Severity: "critical",
				Title:    "Nested archive in package",
				FilePath: f.Path,
				Detail:   "nested archives are not allowed (unpack depth > 1)",
			})
		case archive.IsImagePath(f.Path):
			// An image: not text, not a finding.
		case archive.LooksText(archive.Head(f.Content)):
			if !utf8.Valid(f.Content) {
				out = append(out, localFinding{
					Scanner: "S1/non-utf8", Severity: "low",
					Title:    "Non-UTF-8 text file",
					FilePath: f.Path,
					Detail:   "the registry inventories package text as UTF-8",
				})
			}
		default:
			out = append(out, localFinding{
				Scanner: "S1/opaque-binary", Severity: "info",
				Title:    "Opaque binary file",
				FilePath: f.Path,
				Detail:   "not text, not an image — a reviewer will be asked to look",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Scanner < out[j].Scanner
	})
	return out
}

// localFile is one file of the resolved include set.
type localFile struct {
	Path    string
	Content []byte
}

// localSeverityRank orders S1 findings for display, most severe first.
var localSeverityRank = map[string]int{
	"critical": 0, "high": 1, "medium": 2, "low": 3, "info": 4,
}

// printLocalFindings renders the S1 subset in the same shape as the S3/S4
// block, so one output format covers the whole scan.
func printLocalFindings(w io.Writer, findings []localFinding) {
	ordered := append([]localFinding(nil), findings...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if localSeverityRank[ordered[i].Severity] != localSeverityRank[ordered[j].Severity] {
			return localSeverityRank[ordered[i].Severity] < localSeverityRank[ordered[j].Severity]
		}
		return ordered[i].FilePath < ordered[j].FilePath
	})
	for _, f := range ordered {
		_, _ = fmt.Fprintf(w, "  %-8s %s %s — %s\n",
			strings.ToUpper(f.Severity), f.Scanner, f.FilePath, f.Title)
		if f.Detail != "" {
			_, _ = fmt.Fprintf(w, "           %s\n", f.Detail)
		}
	}
}

// localFailingFindings counts the S1 findings that would fail the command.
//
// "info" does not fail: the pipeline routes an opaque binary to a REVIEWER,
// it does not reject the submission, and a local check that is stricter than
// the server is a different parity bug from the one this file fixes.
func localFailingFindings(findings []localFinding) int {
	n := 0
	for _, f := range findings {
		if f.Severity != "info" {
			n++
		}
	}
	return n
}
