package source

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// The checkout: the commit's tree walked into plain files. Only regular
// files are written, each 0644 whatever mode git gave it; a link, a
// submodule and anything inside a .git folder are left out and said so; a
// name that cannot be a plain file name on every system the runner serves
// is left out too, so a source stages the same on Linux, macOS and
// Windows. The walk is planned whole before a byte is written, so a tree
// over a cap is refused with its count and never staged in part.

// The reasons an entry is left out of the checkout.
const (
	SkipLink      = "link"
	SkipSubmodule = "submodule"
	SkipGit       = "git"
	SkipName      = "name"
	SkipCase      = "case"
	SkipDeep      = "deep"
)

// maxDepth bounds how deep a tree's folders nest.
const maxDepth = 64

// planned is one file to write.
type planned struct {
	path string
	oid  string
	size int64
}

// plan is what the checkout will write and what it left out.
type plan struct {
	files   []planned
	dirs    []string
	bytes   int64
	skipped map[string][]string // reason → paths
}

// treeOf reads a commit and answers its tree's name.
func treeOf(p *pack, commit string) (string, error) {
	typ, data, err := p.object(commit)
	if err != nil {
		return "", err
	}
	if typ != objCommit {
		return "", fmt.Errorf("%s is not a commit", commit)
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	oid, ok := cutPrefix(string(line), "tree ")
	if !ok || !validOID(oid) {
		return "", errors.New("the commit names no tree")
	}
	return oid, nil
}

// planTree walks a tree into a plan.
func planTree(p *pack, tree string) (*plan, error) {
	pl := &plan{skipped: map[string][]string{}}
	return pl, pl.walk(p, tree, "", 0)
}

func (pl *plan) walk(p *pack, tree, dir string, depth int) error {
	typ, data, err := p.object(tree)
	if err != nil {
		return err
	}
	if typ != objTree {
		return fmt.Errorf("%s is not a tree", tree)
	}
	seen := map[string]bool{}
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp <= 0 || nul < sp || len(data) < nul+21 {
			return errors.New("the host sent a malformed tree")
		}
		mode, name := string(data[:sp]), string(data[sp+1:nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		data = data[nul+21:]
		rel := name
		if dir != "" {
			rel = dir + "/" + name
		}
		if strings.ContainsAny(name, "/\x00") || name == "" || name == "." || name == ".." {
			return errors.New("the host sent a tree entry with an unsafe name")
		}
		switch {
		case isGitName(name):
			pl.skip(SkipGit, rel)
			continue
		case mode == "120000":
			pl.skip(SkipLink, rel)
			continue
		case mode == "160000":
			pl.skip(SkipSubmodule, rel)
			continue
		case !portableName(name):
			pl.skip(SkipName, rel)
			continue
		}
		fold := strings.ToLower(name)
		if seen[fold] {
			pl.skip(SkipCase, rel)
			continue
		}
		seen[fold] = true
		switch mode {
		case "40000", "040000":
			if depth+1 >= maxDepth {
				pl.skip(SkipDeep, rel)
				continue
			}
			pl.dirs = append(pl.dirs, rel)
			if err := pl.walk(p, oid, rel, depth+1); err != nil {
				return err
			}
		case "100644", "100755", "100664":
			i, ok := p.byOID[oid]
			if !ok || p.entries[i].ftyp != objBlob {
				return fmt.Errorf("the pack does not hold the content of %s", rel)
			}
			size := p.entries[i].fsize
			pl.files = append(pl.files, planned{path: rel, oid: oid, size: size})
			pl.bytes += size
		default:
			return errors.New("the host sent a tree entry of an unknown mode")
		}
	}
	return nil
}

func (pl *plan) skip(why, rel string) { pl.skipped[why] = append(pl.skipped[why], rel) }

// isGitName says whether a name is git's own folder under any spelling a
// file system folds it to: .git with any case, trailing dots or spaces
// (Windows drops them), or its 8.3 short name.
func isGitName(name string) bool {
	n := strings.ToLower(strings.TrimRight(name, ". "))
	return n == ".git" || n == "git~1"
}

// portableName says whether a name is a plain file name on Linux, macOS and
// Windows alike: no control characters, none of Windows' reserved
// characters, no trailing dot or space, and not a device name.
func portableName(name string) bool {
	if len(name) > 255 {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`\:*?"<>|`, r) {
			return false
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, path.Ext(name)))
	stem, _, _ = strings.Cut(stem, ".")
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		return false
	}
	return true
}

// write writes the plan under root, which must be empty. Every write goes
// through os.Root, so nothing lands outside it whatever a name says.
func (pl *plan) write(p *pack, root *os.Root) error {
	for _, d := range pl.dirs {
		if err := root.Mkdir(d, 0o755); err != nil {
			return fmt.Errorf("make %s: %w", d, err)
		}
	}
	for _, f := range pl.files {
		out, err := root.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		if err := p.writeBlob(f.oid, out); err != nil {
			_ = out.Close()
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}
	return nil
}

// lfsPointers counts the planned files that are Git LFS pointer files,
// which stay as they are.
func (pl *plan) lfsPointers(p *pack) int {
	const sig = "version https://git-lfs.github.com/spec/v1"
	n := 0
	for _, f := range pl.files {
		if f.size > 1024 || f.size < int64(len(sig)) {
			continue
		}
		if _, data, err := p.object(f.oid); err == nil && bytes.HasPrefix(data, []byte(sig)) {
			n++
		}
	}
	return n
}
