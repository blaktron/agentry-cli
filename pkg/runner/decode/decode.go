// Package decode is the runner's decode step (plan knowledge.md §4.2, D14,
// D21; agentry-cli#318, and agentry-cli#321 for a hosted run's inputs): the
// documents a run is given are turned into plain text before any agent
// starts, by the runner image's own `decode` role in an image that holds
// the three decoders and nothing else, in a container with no network and
// no capabilities, the documents mounted read-only. No model runs a
// decoder, and nothing a document contains reaches a decoder's argv but its
// own path. The catalogue of what provides each decoder is named once, here,
// for every caller.
package decode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/plainpath"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// Tool is one external decoder: the command the image keeps and the Alpine
// packages that provide it. tesseract needs its language data beside it:
// the package alone ships none.
type Tool struct {
	Command  string
	Packages []string
}

// Catalogue is every external decoder, named once for every caller. It is
// the application's, not the operator's exec catalogue: a decode step is
// the runner's own, like its proxy, and runs no command a QuickAgent
// declared. The policy's apkMirror still applies to the image build.
var Catalogue = []Tool{
	{Command: "pandoc", Packages: []string{"pandoc-cli"}},
	{Command: "pdftotext", Packages: []string{"poppler-utils"}},
	{Command: "tesseract", Packages: []string{"tesseract-ocr", "tesseract-ocr-data-eng"}},
}

// ImageSpec is the decode image: the runner image with the catalogue's
// packages, minimized to the three decoders (and the runner binary, whose
// decode role drives them).
func ImageSpec(runnerImage, apkMirror string) spec.ExecImageSpec {
	var commands, packages []string
	for _, t := range Catalogue {
		commands = append(commands, t.Command)
		packages = append(packages, t.Packages...)
	}
	sort.Strings(commands)
	sort.Strings(packages)
	return spec.ExecImageSpec{
		Tag:       spec.ExecImageTag(runnerImage, commands, packages, apkMirror),
		Commands:  commands,
		Packages:  packages,
		APKMirror: apkMirror,
	}
}

// Image is a built decode image and the package versions its build
// resolved.
type Image struct {
	Tag      string
	Packages []string
}

// Ensure builds the decode image when it is missing, the way an exec image
// is built, and reads back the versions it resolved.
func Ensure(ctx context.Context, d dockerx.Client, runnerImage string, pol *policy.Policy, ctxDir string, logw io.Writer) (Image, error) {
	img := ImageSpec(runnerImage, pol.Exec.APKMirror)
	pkgs, err := gateway.EnsureExecImage(ctx, d, runnerImage, img, ctxDir, logw)
	if err != nil {
		return Image{}, fmt.Errorf("the decode image: %w", err)
	}
	return Image{Tag: img.Tag, Packages: pkgs}, nil
}

// Result is one decode: the report the container wrote, checked, and the
// image that wrote it. Its files are read only through its own methods,
// which hold every read to the two directories.
type Result struct {
	Report runnerdecode.Report
	Image  Image
	// In holds the documents as given, each at its File.Path; Out the
	// texts, each at its File.Output.
	In, Out string
}

// The bounds on what the host reads back from the decode container.
const (
	// MaxTextBytes is the most of one decoded text the host reads: the
	// decoder's own cut, a whole knowledge pack.
	MaxTextBytes = 10 << 20
	// maxReportBytes bounds decode.json.
	maxReportBytes = 4 << 20
	// Timeout ends a whole decode.
	Timeout = 30 * time.Minute
)

// Run decodes every file under in into out. Both must be directories the
// caller made for this decode — they are relabelled for the container on a
// daemon with SELinux, which is a write to the host's labels and so never
// done to a directory a person owns — and in should hold copies of the
// documents, not the originals.
func Run(ctx context.Context, d dockerx.Client, img Image, in, out string, lim policy.Limits, stderr io.Writer) (*Result, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	name := "agentry-decode-" + hex.EncodeToString(suffix)
	r := dockerx.RunSpec{
		Name:    name,
		Image:   img.Tag,
		Argv:    []string{"decode", "--in", "/in", "--out", "/out"},
		Network: "none",
		Mounts: []dockerx.Mount{
			{Source: in, Target: "/in", ReadOnly: true, Relabel: true},
			{Source: out, Target: "/out", Relabel: true},
		},
		// The texts belong to the person who ran the command, and a
		// decoder runs with no more than they have.
		User:       invokingUser(),
		AutoRemove: true,
		Labels:     map[string]string{"agentry.decode": "1"},
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	// The decoders' scratch is a bounded tmpfs, so a document that expands
	// without end fills that, not the host (runnerimage/decode writes only
	// a capped copy to /out).
	r.ReadOnly, r.Tmpfs = true, []string{"/tmp:size=512m,mode=1777"}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var errOut bytes.Buffer
	code, err := d.RunForeground(ctx, r, nil, io.Discard, io.MultiWriter(stderr, &errOut))
	if err != nil {
		return nil, fmt.Errorf("the decode container: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("the decode container failed (exit %d): %s", code, strings.TrimSpace(tail(errOut.String(), 400)))
	}
	rep, err := checkReport(in, out)
	if err != nil {
		return nil, err
	}
	return &Result{Report: rep, Image: img, In: in, Out: out}, nil
}

// checkReport reads the report the container wrote and holds it to what
// was staged. Everything under out was written by a process that read a
// hostile document, so it is read through an os.Root, regular files only,
// under a cap; and every entry must name a document that was staged, with
// its text, when there is one, at exactly <path>.txt.
func checkReport(in, out string) (runnerdecode.Report, error) {
	var rep runnerdecode.Report
	inRoot, err := os.OpenRoot(in)
	if err != nil {
		return rep, err
	}
	defer func() { _ = inRoot.Close() }()
	outRoot, err := os.OpenRoot(out)
	if err != nil {
		return rep, err
	}
	defer func() { _ = outRoot.Close() }()
	raw, err := readRegular(outRoot, runnerdecode.ReportFile, maxReportBytes)
	if err != nil {
		return rep, fmt.Errorf("the decode wrote no readable %s: %w", runnerdecode.ReportFile, err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("%s: %w", runnerdecode.ReportFile, err)
	}
	if rep.Schema != runnerdecode.Schema {
		return rep, fmt.Errorf("%s: schema %q is not %q", runnerdecode.ReportFile, rep.Schema, runnerdecode.Schema)
	}
	seen := map[string]bool{}
	for _, f := range rep.Files {
		if !relPath(f.Path) || seen[f.Path] {
			return rep, fmt.Errorf("%s names %q, which is not a document once", runnerdecode.ReportFile, f.Path)
		}
		seen[f.Path] = true
		fi, err := inRoot.Lstat(filepath.FromSlash(f.Path))
		if err != nil {
			return rep, fmt.Errorf("%s names %q, which was not staged: %w", runnerdecode.ReportFile, f.Path, err)
		}
		switch f.Outcome {
		case runnerdecode.OutcomeRefused:
		case runnerdecode.OutcomeText, runnerdecode.OutcomeUndecodable, runnerdecode.OutcomeDecoded:
			if !fi.Mode().IsRegular() {
				return rep, fmt.Errorf("%s: %q is not a regular file", runnerdecode.ReportFile, f.Path)
			}
		default:
			return rep, fmt.Errorf("%s: %q has the unknown outcome %q", runnerdecode.ReportFile, f.Path, f.Outcome)
		}
		if f.Outcome != runnerdecode.OutcomeDecoded {
			if f.Output != "" {
				return rep, fmt.Errorf("%s: %q was not decoded but names an output", runnerdecode.ReportFile, f.Path)
			}
			continue
		}
		if f.Output != f.Path+".txt" {
			return rep, fmt.Errorf("%s names an output outside the output root: %q is not %q", runnerdecode.ReportFile, f.Output, f.Path+".txt")
		}
		ti, err := plainpath.File(outRoot, f.Output)
		if err != nil || ti.Size() > MaxTextBytes {
			return rep, fmt.Errorf("%s: the text of %q is not a regular file within %d bytes", runnerdecode.ReportFile, f.Path, MaxTextBytes)
		}
	}
	// A staged document the report leaves out is not decoded, and says so,
	// rather than vanishing from what a run is told it was given.
	err = fs.WalkDir(inRoot.FS(), ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || seen[p] {
			return err
		}
		rep.Files = append(rep.Files, runnerdecode.File{Path: p, Family: runnerdecode.FamilyUnknown, Outcome: runnerdecode.OutcomeUndecodable, Reason: "the decode's report does not name it"})
		return nil
	})
	if err != nil {
		return rep, err
	}
	sort.Slice(rep.Files, func(i, j int) bool { return rep.Files[i].Path < rep.Files[j].Path })
	return rep, nil
}

func relPath(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, "\\")
}

// readRegular reads a regular file under root, refusing a link, a device
// or more than limit bytes.
func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	if _, err := plainpath.File(root, filepath.ToSlash(name)); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, nil
}

// Decoded counts the files that were decoded into text.
func (r *Result) Decoded() int { return r.count(runnerdecode.OutcomeDecoded) }

// Text counts the files read as they are.
func (r *Result) Text() int { return r.count(runnerdecode.OutcomeText) }

// Undecodable names the files neither decoded nor read as text: those no
// decoder reads, those a decoder failed on, and those refused.
func (r *Result) Undecodable() []string {
	var out []string
	for _, f := range r.Report.Files {
		if f.Outcome == runnerdecode.OutcomeUndecodable || f.Outcome == runnerdecode.OutcomeRefused {
			out = append(out, f.Path)
		}
	}
	return out
}

func (r *Result) count(outcome runnerdecode.Outcome) int {
	n := 0
	for _, f := range r.Report.Files {
		if f.Outcome == outcome {
			n++
		}
	}
	return n
}

// Line is the receipt's decode line: the mechanism, the versions, and what
// happened to the files.
func (r *Result) Line() string {
	pkgs := append([]string(nil), r.Image.Packages...)
	sort.Strings(pkgs)
	commands := make([]string, 0, len(Catalogue))
	for _, t := range Catalogue {
		commands = append(commands, t.Command)
	}
	sort.Strings(commands)
	line := "the inputs were decoded before the run, in " + r.Image.Tag +
		" with no network, every capability dropped and the inputs read-only," +
		" by " + andList(commands) + " (" + strings.Join(pkgs, ", ") + ")" +
		" and the runner's own PPTX and XLSX reader: " +
		fmt.Sprintf("%d decoded, %d read as text", r.Decoded(), r.Text())
	if bad := r.Undecodable(); len(bad) > 0 {
		shown := bad
		if len(shown) > 5 {
			shown = shown[:5]
		}
		line += fmt.Sprintf(", %d not decoded (%s", len(bad), strings.Join(shown, ", "))
		if len(bad) > len(shown) {
			line += fmt.Sprintf(" and %d more", len(bad)-len(shown))
		}
		line += ")"
	}
	return line
}

// andList is "a", "a and b", "a, b and c".
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func invokingUser() string {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
