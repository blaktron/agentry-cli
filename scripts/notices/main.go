// Command notices writes THIRD_PARTY_NOTICES: the licence text of every
// third-party Go module compiled into the agentry binary, for each target
// the release builds (linux, darwin and windows, with cgo off, as `make
// dist` builds them), and the licence of the moby seccomp profile the
// runner image sources carry.
//
// The output is a pure function of the module graph and the module cache:
// modules sorted by path, files sorted by path, no timestamps. Run it from
// the repository root through scripts/notices/generate.sh.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ownModulePrefix is the project's own modules: not third party.
const ownModulePrefix = "github.com/blaktron/"

// mainPackage is the package whose dependencies are the binary's.
const mainPackage = "./cmd/agentry"

// suppliedDir holds licence text for a module whose zip ships none, one
// directory per module path, as <suppliedDir>/<module>/LICENSE. An optional
// SOURCE file beside it is the one-line note printed with the text.
const suppliedDir = "scripts/notices/supplied"

// seccompDir holds the moby seccomp profile and its licence files.
const seccompDir = "runnerimage/sockfilter"

// target is one GOOS/GOARCH the release builds.
type target struct{ goos, goarch string }

var targets = []target{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"},
}

// A file's role in a module's notices.
const (
	kindLicence = iota // classified; decides the module's SPDX expression
	kindNotice         // NOTICE, COPYRIGHT and patent grants, printed as is
)

// fileKinds are the file names read from a package's directory and from
// each directory above it up to the module root. Documentation-only
// licences (LICENSE.docs) and Go files named license.go are deliberately
// absent.
var fileKinds = map[string]int{
	"LICENSE": kindLicence, "LICENSE.txt": kindLicence, "LICENSE.md": kindLicence,
	"LICENCE": kindLicence, "COPYING": kindLicence, "COPYING.txt": kindLicence,
	"NOTICE": kindNotice, "NOTICE.txt": kindNotice,
	"COPYRIGHT": kindNotice, "COPYRIGHT.txt": kindNotice,
	"PATENTS": kindNotice,
}

type noticeFile struct {
	rel  string // path inside the module
	kind int
	spdx string // licence files only
	text string
}

type module struct {
	path, version, dir string
	targets            map[string]bool
	pkgDirs            map[string]bool
	files              []noticeFile
	note               string // set when the text did not come from the module
}

func main() {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(w io.Writer) error {
	mods, err := collect()
	if err != nil {
		return err
	}
	var problems []string
	for _, m := range mods {
		if err := m.load(); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("modules with no usable licence text:\n  %s", strings.Join(problems, "\n  "))
	}
	seccomp, err := loadSeccomp()
	if err != nil {
		return err
	}
	return render(w, mods, seccomp)
}

// collect lists the third-party modules compiled into mainPackage for every
// target, with the targets each is used on and the directories of its
// packages that are compiled.
func collect() ([]*module, error) {
	goBin := os.Getenv("GO")
	if goBin == "" {
		goBin = "go"
	}
	const format = `{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}{{"\t"}}{{.Module.Version}}{{"\t"}}{{.Module.Dir}}{{"\t"}}{{.Dir}}{{"\t"}}{{if .Module.Replace}}replaced{{end}}{{end}}{{end}}`
	byPath := map[string]*module{}
	for _, t := range targets {
		cmd := exec.Command(goBin, "list", "-deps", "-f", format, mainPackage)
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+t.goos, "GOARCH="+t.goarch)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list for %s/%s: %w\n%s", t.goos, t.goarch, err, stderr.String())
		}
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 5 {
				return nil, fmt.Errorf("unexpected go list line %q", line)
			}
			if f[4] != "" {
				return nil, fmt.Errorf("module %s is replaced; its licence text cannot be taken from the module cache", f[0])
			}
			if strings.HasPrefix(f[0], ownModulePrefix) {
				continue
			}
			m := byPath[f[0]]
			if m == nil {
				m = &module{path: f[0], version: f[1], dir: f[2], targets: map[string]bool{}, pkgDirs: map[string]bool{}}
				byPath[f[0]] = m
			}
			if m.version != f[1] {
				return nil, fmt.Errorf("module %s resolves to %s and %s on different targets", f[0], m.version, f[1])
			}
			m.targets[t.goos] = true
			m.pkgDirs[f[3]] = true
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	mods := make([]*module, 0, len(byPath))
	for _, m := range byPath {
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].path < mods[j].path })
	return mods, nil
}

// load reads the module's licence and notice files: those in each compiled
// package's directory and in every directory between it and the module root.
func (m *module) load() error {
	seen := map[string]bool{}
	for pkgDir := range m.pkgDirs {
		for dir := pkgDir; ; dir = filepath.Dir(dir) {
			rel, err := filepath.Rel(m.dir, dir)
			if err != nil || strings.HasPrefix(rel, "..") {
				break
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return fmt.Errorf("%s: %w", m.path, err)
			}
			for _, e := range entries {
				kind, ok := fileKinds[e.Name()]
				if !ok || e.IsDir() {
					continue
				}
				relPath := filepath.ToSlash(filepath.Join(rel, e.Name()))
				if seen[relPath] {
					continue
				}
				seen[relPath] = true
				text, err := readText(filepath.Join(dir, e.Name()))
				if err != nil {
					return fmt.Errorf("%s: %w", m.path, err)
				}
				m.files = append(m.files, noticeFile{rel: relPath, kind: kind, text: text})
			}
			if rel == "." {
				break
			}
		}
	}
	if !hasLicence(m.files) {
		if err := m.supply(); err != nil {
			return err
		}
	}
	sort.Slice(m.files, func(i, j int) bool { return m.files[i].rel < m.files[j].rel })
	for i := range m.files {
		f := &m.files[i]
		if f.kind != kindLicence {
			continue
		}
		f.spdx = classify(f.text)
		if f.spdx == "" {
			return fmt.Errorf("%s %s: %s matches no known licence text", m.path, m.version, f.rel)
		}
	}
	return nil
}

func hasLicence(files []noticeFile) bool {
	for _, f := range files {
		if f.kind == kindLicence {
			return true
		}
	}
	return false
}

// supply fills in a module whose zip has no licence file from suppliedDir.
func (m *module) supply() error {
	dir := filepath.Join(suppliedDir, filepath.FromSlash(m.path))
	text, err := readText(filepath.Join(dir, "LICENSE"))
	if err != nil {
		return fmt.Errorf("%s %s ships no licence file and %s has none", m.path, m.version, dir)
	}
	m.files = append(m.files, noticeFile{rel: "LICENSE", kind: kindLicence, text: text})
	if note, err := readText(filepath.Join(dir, "SOURCE")); err == nil {
		m.note = strings.TrimSpace(note)
	}
	return nil
}

// readText returns a file's text with LF line ends and one final newline.
func readText(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.TrimRight(s, "\n") + "\n", nil
}

// classify names the SPDX licences a licence file holds, sorted and joined
// by AND, or "" when it holds none of the texts this project's dependencies
// are known to use. One file can hold several (a project that bundles
// another's code under that code's licence).
func classify(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	var ids []string
	if (strings.Contains(t, "Apache License") && strings.Contains(t, "Version 2.0, January 2004")) ||
		strings.Contains(t, `Licensed under the Apache License, Version 2.0 (the "License")`) {
		// The second form is the header some projects ship in place of the
		// full text.
		ids = append(ids, "Apache-2.0")
	}
	if strings.Contains(t, "Permission is hereby granted, free of charge, to any person obtaining a copy") {
		ids = append(ids, "MIT")
	}
	if strings.Contains(t, "Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted") {
		ids = append(ids, "ISC")
	}
	if strings.Contains(t, "Redistribution and use in source and binary forms, with or without modification") {
		if strings.Contains(strings.ToLower(t), "endorse") {
			ids = append(ids, "BSD-3-Clause")
		} else {
			ids = append(ids, "BSD-2-Clause")
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, " AND ")
}

// spdx is the module's expression: its licence files' identifiers, sorted,
// joined by AND.
func (m *module) spdx() string {
	set := map[string]bool{}
	for _, f := range m.files {
		if f.kind != kindLicence {
			continue
		}
		for _, id := range strings.Split(f.spdx, " AND ") {
			set[id] = true
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, " AND ")
}

// usedOn is "" when the module is compiled on every target OS, else the
// sorted list of the OSes that compile it.
func (m *module) usedOn() string {
	if len(m.targets) == 3 {
		return ""
	}
	oses := make([]string, 0, len(m.targets))
	for name := range m.targets {
		oses = append(oses, name)
	}
	sort.Strings(oses)
	return strings.Join(oses, ", ")
}

type seccompFiles struct{ licence, notice string }

func loadSeccomp() (seccompFiles, error) {
	var s seccompFiles
	var err error
	if s.licence, err = readText(filepath.Join(seccompDir, "chromium-seccomp.LICENSE")); err != nil {
		return s, err
	}
	if classify(s.licence) != "Apache-2.0" {
		return s, fmt.Errorf("%s/chromium-seccomp.LICENSE is not the Apache-2.0 text", seccompDir)
	}
	s.notice, err = readText(filepath.Join(seccompDir, "chromium-seccomp.NOTICE"))
	return s, err
}

const rule = "================================================================================\n"

func render(w io.Writer, mods []*module, seccomp seccompFiles) error {
	var b strings.Builder
	b.WriteString("Third-party notices for the agentry command-line client\n\n")
	b.WriteString("The client is released under the MIT licence (see LICENSE). It is built from\n")
	b.WriteString("the Go modules listed here, which keep their own licences, and carries the\n")
	b.WriteString("moby seccomp profile described at the end. The list covers every module\n")
	b.WriteString("compiled into the client on linux, darwin and windows (amd64 and arm64, cgo\n")
	b.WriteString("off), so a module used on one system only says which.\n\n")
	b.WriteString("This file is generated by scripts/notices/generate.sh from the module graph\n")
	b.WriteString("and the modules' own licence files; do not edit it by hand.\n\n")
	fmt.Fprintf(&b, "Modules (%d)\n\n", len(mods))
	for _, m := range mods {
		line := fmt.Sprintf("  %s %s  %s", m.path, m.version, m.spdx())
		if on := m.usedOn(); on != "" {
			line += "  (" + on + " only)"
		}
		b.WriteString(line + "\n")
	}
	for _, m := range mods {
		b.WriteString("\n" + rule)
		fmt.Fprintf(&b, "%s %s\n", m.path, m.version)
		fmt.Fprintf(&b, "Licence: %s\n", m.spdx())
		if on := m.usedOn(); on != "" {
			fmt.Fprintf(&b, "Compiled on: %s only\n", on)
		}
		for _, f := range m.files {
			b.WriteString("\n")
			head := "--- " + f.rel
			if m.note != "" && f.kind == kindLicence {
				head += " (" + m.note + ")"
			}
			b.WriteString(head + "\n\n")
			b.WriteString(f.text)
		}
	}
	b.WriteString("\n" + rule)
	b.WriteString("moby default seccomp profile (runnerimage/sockfilter/chromium-seccomp.json)\n")
	b.WriteString("Licence: Apache-2.0\n")
	b.WriteString("\n--- chromium-seccomp.NOTICE\n\n")
	b.WriteString(seccomp.notice)
	b.WriteString("\n--- chromium-seccomp.LICENSE\n\n")
	b.WriteString(seccomp.licence)
	_, err := io.WriteString(w, b.String())
	return err
}
