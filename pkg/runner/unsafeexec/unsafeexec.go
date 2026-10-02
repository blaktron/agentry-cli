// Package unsafeexec is the data side of Unsafe Mode (agentry-notes
// plans/unsafe-mode.md §4.3; agentry-cli#490): the unsafe base image's
// command index, extras list and ambiguous list, read out of the pulled
// image and held to a grammar, and the placement of a manifest's declared
// commands through them.
//
// Unsafe Mode lifts one check: this machine's exec catalogue
// (policy exec.packages) is not consulted (D1). A client turns it on per
// compile by passing a *Data (compile.Inputs.Unsafe); no policy key and no
// `agentry run` flag does (D5), so it never reaches a hosted run (D7).
//
// The data files are read from an image, so they are data, not code: every
// command name, package name, pip requirement and path is held to a grammar
// here, and one that fails is never placed (invariant 3). The exec image
// build (pkg/runner/gateway) then hands the placed values to its install
// script in files, never in a shell line.
package unsafeexec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// DataDir is where the unsafe base carries its data files
// (agentry-dockerimages build/agentry-unsafe-kali).
const DataDir = "/etc/agentry/unsafe"

// The files under DataDir.
const (
	CommandsFile  = DataDir + "/commands.tsv"
	ExtrasFile    = DataDir + "/extras.tsv"
	AmbiguousFile = DataDir + "/ambiguous.tsv"
)

// The grammars, the same the image's make-index.py applies: a plain
// command name, a Debian package name (never ending in "-", which apt
// reads as "remove"), an exact pip requirement, a relative path inside a
// package; an image reference pinned by digest is policy.PinnedByDigest's,
// anchored at both ends so no line can ride before the digest into a FROM.
var (
	commandRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*[a-z0-9+]$`)
	pipSpecRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.+!_-]*$`)
	pathRE    = regexp.MustCompile(`^(?:[A-Za-z0-9._+-]+/)*[A-Za-z0-9._+-]+$`)
)

// RunnerCommand is the runner binary's name in the exec image, its
// entrypoint: no declared command may be placed onto it.
const RunnerCommand = "agentry-runner"

// The grammars, for a caller that holds a plan's values to them again
// (pkg/runner/gateway, before a build).
func ValidCommand(c string) bool { return commandRE.MatchString(c) && c != RunnerCommand }
func ValidPackage(p string) bool { return packageRE.MatchString(p) }
func ValidPipSpec(s string) bool { return pipSpecRE.MatchString(s) }
func ValidLinkPath(p string) bool {
	return pathRE.MatchString(p) && !strings.Contains("/"+p+"/", "/../")
}

// ValidBase says whether an image reference is pinned by digest.
func ValidBase(ref string) error {
	if !policy.PinnedByDigest(ref) {
		return fmt.Errorf("the Unsafe Mode base %q is not an image reference pinned by digest (policy images.unsafe wants name:tag@sha256:…)", ref)
	}
	return nil
}

// Data is the unsafe base's placement data.
type Data struct {
	// Base is the base image, pinned by digest.
	Base      string
	index     map[string]string
	extras    map[string]spec.UnsafeExtra
	ambiguous map[string][]string
	// Dropped counts the index and ambiguous rows refused by the grammar.
	Dropped int
}

// Parse reads the three data files. An index or ambiguous row that fails
// the grammar is dropped and counted; an extras list that does not hold
// the closed recipe set is refused whole, because it is maintained by
// hand and a bad row is a mistake to fix, not a row to skip.
func Parse(base string, commands, extras, ambiguous []byte) (*Data, error) {
	if err := ValidBase(base); err != nil {
		return nil, err
	}
	d := &Data{Base: base, index: map[string]string{}, extras: map[string]spec.UnsafeExtra{}, ambiguous: map[string][]string{}}
	for _, f := range rows(commands) {
		if len(f) != 2 || !ValidCommand(f[0]) || !ValidPackage(f[1]) {
			d.Dropped++
			continue
		}
		d.index[f[0]] = f[1]
	}
	for _, f := range rows(ambiguous) {
		if len(f) != 2 || !ValidCommand(f[0]) {
			d.Dropped++
			continue
		}
		var pkgs []string
		for _, p := range strings.Split(f[1], ",") {
			if ValidPackage(p) {
				pkgs = append(pkgs, p)
			}
		}
		sort.Strings(pkgs)
		d.ambiguous[f[0]] = pkgs
	}
	for n, f := range rows(extras) {
		e, err := extra(f)
		if err != nil {
			return nil, fmt.Errorf("the Unsafe Mode base's extras list, row %d: %w", n+1, err)
		}
		if _, dup := d.extras[e.Command]; dup {
			return nil, fmt.Errorf("the Unsafe Mode base's extras list names %s twice", e.Command)
		}
		d.extras[e.Command] = e
	}
	return d, nil
}

func extra(f []string) (spec.UnsafeExtra, error) {
	switch {
	case len(f) == 3 && f[0] == string(spec.ExtraPip):
		e := spec.UnsafeExtra{Command: f[1], Kind: spec.ExtraPip, Spec: f[2]}
		if err := CheckExtra(e); err != nil {
			return spec.UnsafeExtra{}, err
		}
		return e, nil
	case len(f) == 4 && f[0] == string(spec.ExtraLink):
		e := spec.UnsafeExtra{Command: f[1], Kind: spec.ExtraLink, Package: f[2], Path: f[3]}
		if err := CheckExtra(e); err != nil {
			return spec.UnsafeExtra{}, err
		}
		return e, nil
	}
	return spec.UnsafeExtra{}, fmt.Errorf("want pip<TAB>command<TAB>name==version or link<TAB>command<TAB>package<TAB>path, got %q", strings.Join(f, "\t"))
}

// CheckExtra holds one recipe to its kind's grammar.
func CheckExtra(e spec.UnsafeExtra) error {
	if !ValidCommand(e.Command) {
		return fmt.Errorf("an extra's command %q is not a command name, or is the runner's own", e.Command)
	}
	switch e.Kind {
	case spec.ExtraPip:
		if !ValidPipSpec(e.Spec) {
			return fmt.Errorf("pip %s wants name==version, got %q", e.Command, e.Spec)
		}
	case spec.ExtraLink:
		if !ValidPackage(e.Package) || !ValidLinkPath(e.Path) {
			return fmt.Errorf("link %s wants a package name and a relative path inside it, got %q %q", e.Command, e.Package, e.Path)
		}
	default:
		return fmt.Errorf("an extra's kind %q is neither pip nor link", e.Kind)
	}
	return nil
}

// rows splits a tab-separated file into its non-comment rows.
func rows(b []byte) [][]string {
	var out [][]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, strings.Split(line, "\t"))
	}
	return out
}

// Place resolves the declared commands the way the plan says (§4.3): the
// extras list first, then the index. It never refuses: a command neither
// places, or one that is not a plain command name, is unplaced (D8), with
// the packages that ship it when the index left it ambiguous. It returns
// the Kali packages to install (each placed command's, and each link
// extra's), sorted and deduplicated, and the Unsafe Mode part of the exec
// image spec.
func (d *Data) Place(commands []string) (packages []string, u spec.UnsafeExecSpec) {
	u.Base = d.Base
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			packages = append(packages, p)
		}
	}
	sorted := append([]string(nil), commands...)
	sort.Strings(sorted)
	for _, c := range sorted {
		if !ValidCommand(c) {
			u.Unplaced = append(u.Unplaced, spec.UnplacedCommand{Command: c})
			continue
		}
		if e, ok := d.extras[c]; ok {
			u.Extras = append(u.Extras, e)
			if e.Kind == spec.ExtraLink {
				add(e.Package)
			}
			continue
		}
		if p, ok := d.index[c]; ok {
			add(p)
			u.Apt = append(u.Apt, spec.AptPlacement{Command: c, Package: p})
			continue
		}
		u.Unplaced = append(u.Unplaced, spec.UnplacedCommand{Command: c, Candidates: d.ambiguous[c]})
	}
	sort.Strings(packages)
	return packages, u
}

// Load reads the base's data from the image, pulling it first when this
// machine does not have it (turning Unsafe Mode on pulls it, D2; a run
// finds it here).
func Load(ctx context.Context, docker dockerx.Client, base string, logw io.Writer) (*Data, error) {
	if err := ValidBase(base); err != nil {
		return nil, err
	}
	ok, err := docker.ImageExists(ctx, base)
	if err != nil {
		return nil, err
	}
	if !ok {
		_, _ = fmt.Fprintf(logw, "pulling the Unsafe Mode base %s\n", base)
		if err := docker.Pull(ctx, base); err != nil {
			return nil, fmt.Errorf("pull the Unsafe Mode base: %w", err)
		}
	}
	var files [3][]byte
	for i, path := range []string{CommandsFile, ExtrasFile, AmbiguousFile} {
		if files[i], err = docker.ImageFile(ctx, base, path); err != nil {
			return nil, fmt.Errorf("read the Unsafe Mode base's %s: %w", path, err)
		}
	}
	return Parse(base, files[0], files[1], files[2])
}
