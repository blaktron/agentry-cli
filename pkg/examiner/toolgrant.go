package examiner

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// ScannerUngrantedTool is the registry's scripts-stage finding for runner
// tools the instruction calls for that the manifest does not grant.
const ScannerUngrantedTool = "S6/ungranted-tool"

// ungrantedToolFindings reads the staged instruction file against the
// capabilities the manifest declares, through the core's own table
// (pkg/toolgrant), and gives the registry's finding for each manifest line a
// run would need: high, high confidence, at the instruction's first call for
// it, in the order the instruction first calls for each. An instruction file
// that is not staged or not text gives none, as the registry's inventory
// reports those.
func ungrantedToolFindings(m *manifest.Manifest, pkgDir string) []Finding {
	path, text := stagedInstruction(m, pkgDir)
	if path == "" {
		return nil
	}
	var order []string
	byPath := map[string][]toolgrant.Missing{}
	for _, x := range toolgrant.Check(m.Capabilities, text) {
		if _, ok := byPath[x.Path]; !ok {
			order = append(order, x.Path)
		}
		byPath[x.Path] = append(byPath[x.Path], x)
	}
	out := make([]Finding, 0, len(order))
	for _, p := range order {
		xs := byPath[p]
		tools := make([]string, len(xs))
		for i, x := range xs {
			tools[i] = x.Tool
		}
		out = append(out, Finding{
			Kind: KindRegistryRules, Scanner: ScannerUngrantedTool, Severity: "high", Confidence: "high",
			Title: "The instruction calls for runner tools a run of this release does not have (" + p + "): " + cut(strings.Join(tools, ", "), 160),
			File:  path, Line: xs[0].Line,
			Detail: "Declare " + p + " in agent.yaml, or take the calls out of the instruction.",
		})
	}
	return out
}

// stagedInstruction is the instruction file the manifest names, as staged
// for the run, read as the registry's stages read a text file: its first
// MiB, UTF-8 only. "" when it is not staged or not text.
func stagedInstruction(m *manifest.Manifest, pkgDir string) (path, text string) {
	for _, c := range m.InstructionCandidates() {
		// The manifest's path is the author's: only a regular file beneath
		// the staged copy is read, never a path that climbs out of it or a
		// link (the manifest rules refuse such a path at Submit).
		rel := filepath.FromSlash(c)
		if !filepath.IsLocal(rel) {
			continue
		}
		full := filepath.Join(pkgDir, rel)
		if fi, err := os.Lstat(full); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(full) //nolint:gosec // a regular file beneath the staged copy
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, textScanCap))
		_ = f.Close()
		if err != nil || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return "", ""
		}
		return c, string(b)
	}
	return "", ""
}

// cut keeps a title's list to n bytes, as the registry's finding does.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
