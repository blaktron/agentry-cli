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
// command name, a Debian package name, an exact pip requirement, and a
// relative path inside a package.
var (
	commandRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]+$`)
	pipSpecRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9][A-Za-z0-9.+!_-]*$`)
	pathRE    = regexp.MustCompile(`^(?:[A-Za-z0-9._+-]+/)*[A-Za-z0-9._+-]+$`)
	digestRE  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// ValidCommand says whether a declared command is a name the index could
// hold. A declared command that is not is never placed.
func ValidCommand(c string) bool { return commandRE.MatchString(c) }

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
	if !digestRE.MatchString(base) {
		return nil, fmt.Errorf("the Unsafe Mode base %q is not pinned by digest (policy images.unsafe wants name:tag@sha256:…)", base)
	}
	d := &Data{Base: base, index: map[string]string{}, extras: map[string]spec.UnsafeExtra{}, ambiguous: map[string][]string{}}
	for _, f := range rows(commands) {
		if len(f) != 2 || !commandRE.MatchString(f[0]) || !packageRE.MatchString(f[1]) {
			d.Dropped++
			continue
		}
		d.index[f[0]] = f[1]
	}
	for _, f := range rows(ambiguous) {
		if len(f) != 2 || !commandRE.MatchString(f[0]) {
			d.Dropped++
			continue
		}
		var pkgs []string
		for _, p := range strings.Split(f[1], ",") {
			if packageRE.MatchString(p) {
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
	case len(f) == 3 && f[0] == "pip":
		if !commandRE.MatchString(f[1]) || !pipSpecRE.MatchString(f[2]) {
			return spec.UnsafeExtra{}, fmt.Errorf("pip wants a command name and name==version, got %q %q", f[1], f[2])
		}
		return spec.UnsafeExtra{Command: f[1], Kind: "pip", Spec: f[2]}, nil
	case len(f) == 4 && f[0] == "link":
		if !commandRE.MatchString(f[1]) || !packageRE.MatchString(f[2]) || !pathRE.MatchString(f[3]) || strings.Contains("/"+f[3]+"/", "/../") {
			return spec.UnsafeExtra{}, fmt.Errorf("link wants a command name, a package name and a relative path, got %q %q %q", f[1], f[2], f[3])
		}
		return spec.UnsafeExtra{Command: f[1], Kind: "link", Package: f[2], Path: f[3]}, nil
	}
	return spec.UnsafeExtra{}, fmt.Errorf("want pip<TAB>command<TAB>name==version or link<TAB>command<TAB>package<TAB>path, got %q", strings.Join(f, "\t"))
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
		if !commandRE.MatchString(c) {
			u.Unplaced = append(u.Unplaced, spec.UnplacedCommand{Command: c})
			continue
		}
		if e, ok := d.extras[c]; ok {
			u.Extras = append(u.Extras, e)
			if e.Kind == "link" {
				add(e.Package)
			}
			continue
		}
		if p, ok := d.index[c]; ok {
			add(p)
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
	if !digestRE.MatchString(base) {
		return nil, fmt.Errorf("the Unsafe Mode base %q is not pinned by digest (policy images.unsafe wants name:tag@sha256:…)", base)
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
