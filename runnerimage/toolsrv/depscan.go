package toolsrv

// dependency_scan: the Coding agent's scan of a project's dependencies for
// known vulnerabilities, with OSV-Scanner from its toolchain image, offline,
// against OSV's databases that the runner keeps on the person's computer
// and mounts here read-only. The scan never asks OSV's service: what it
// knows is what the databases hold. A database it cannot read, or one it
// does not have for an ecosystem the project uses, is an error, never "no
// known vulnerabilities": OSV-Scanner prints an empty result beside such an
// error, so the error lines and its exit code are what this reads first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ToolDependencyScan is the tool's name.
const ToolDependencyScan = "dependency_scan"

// DependencyScan configures dependency_scan (under Exec.Any).
type DependencyScan struct {
	// DBDir is where OSV's databases are mounted: osv-scalibr/<ecosystem>/
	// all.zip beneath it.
	DBDir string `json:"dbDir"`
	// Ecosystems are the ecosystems the runner keeps a database of.
	Ecosystems []string `json:"ecosystems,omitempty"`
}

// depScanTimeout bounds one scan.
const depScanTimeout = 5 * time.Minute

func (s *server) dependencyScanDescription() string {
	eco := "the ecosystems it keeps"
	if e := s.spec.Exec.DependencyScan.Ecosystems; len(e) > 0 {
		eco = strings.Join(e, ", ")
	}
	return "Scan a project's dependencies for known vulnerabilities with OSV-Scanner, offline, against OSV's databases kept on the person's computer (" + eco +
		"): pass dir, a folder beneath /agent; it reads the lockfiles and manifests beneath it (go.mod, package-lock.json, requirements.txt, Cargo.lock, pom.xml and others). " +
		"Returns each vulnerable package with its version, the vulnerabilities' ids and the version that fixes each, or that none is known. " +
		"A database it does not have, or cannot read, is an error that names it: it is never reported as no vulnerabilities. The databases are refreshed once a day while the agent runs."
}

func dependencyScanSchema() map[string]any {
	return objSchema(map[string]any{
		"dir": strProp("A folder beneath /agent to scan, such as data/myproject"),
	}, "dir")
}

// depFinding is one vulnerable package.
type depFinding struct {
	source, ecosystem, name, version string
	ids                              []string
	fixed                            []string
}

func (s *server) toolDependencyScan(raw json.RawMessage) (string, bool) {
	var a struct {
		Dir string `json:"dir"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "dependency_scan: " + err.Error(), true
	}
	if s.confine.refused != "" {
		return "dependency_scan: " + s.confine.refused, true
	}
	dir := s.spec.WorkspaceDir
	if d := strings.TrimSpace(a.Dir); d != "" && d != "." {
		abs, err := resolveUnder(s.spec.WorkspaceDir, path.Clean(strings.TrimPrefix(d, s.spec.WorkspaceDir+"/")))
		if err != nil {
			return "dependency_scan: dir: " + err.Error(), true
		}
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			return fmt.Sprintf("dependency_scan: dir %q is not a folder beneath %s", a.Dir, s.spec.WorkspaceDir), true
		}
		dir = abs
	}
	db := s.spec.Exec.DependencyScan.DBDir
	if entries, err := os.ReadDir(filepath.Join(db, "osv-scalibr")); err != nil || len(entries) == 0 {
		return "dependency_scan: no OSV database is on this computer yet (it is downloaded when the agent starts, once a day): nothing was scanned", true
	}
	prog, err := exec.LookPath("osv-scanner")
	if err != nil {
		return "dependency_scan: osv-scanner is not in the toolchain image", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), depScanTimeout)
	defer cancel()
	name, argv := s.confine.argv(prog, []string{"scan", "source", "--offline", "--recursive", "--format", "json", dir})
	cmd := exec.CommandContext(ctx, name, argv...)
	s.holdAny(cmd, dir, "")
	cmd.Env = append(cmd.Env, "OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY="+db)
	stdout, stderr := &capWriter{max: 32 << 20}, &capWriter{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	failed, runErr := s.confine.run(cmd)
	_ = killGroup(cmd)
	if failed != "" {
		return "dependency_scan: " + failed, true
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("dependency_scan: the scan took longer than %s", depScanTimeout), true
	}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	default:
		return "dependency_scan: " + runErr.Error(), true
	}
	text, isErr := depScanAnswer(code, stdout.buf.Bytes(), stderr.buf.String(), dir, s.spec.WorkspaceDir, dbAgeOf(db))
	return s.scrubSecrets(text), isErr
}

// depScanAnswer reads OSV-Scanner's exit code, its JSON and its error lines
// into the tool's answer. The error lines come first: a missing or broken
// database prints an empty result beside them.
func depScanAnswer(code int, out []byte, errText, dir, root, dbAge string) (string, bool) {
	var problems []string
	for _, line := range strings.Split(errText, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "could not load db") || strings.HasPrefix(l, "Error during extraction") {
			problems = append(problems, l)
		}
	}
	if len(problems) > 0 || code == 127 {
		msg := "dependency_scan: the scan could not read a database it needs, so its result is not complete and is not given"
		if len(problems) > 0 {
			msg += ": " + strings.Join(problems, "; ")
		} else if detail := firstLines(errText, 5); detail != "" {
			msg += ": " + detail
		}
		return msg, true
	}
	if code == 128 {
		return "dependency_scan: no lockfile or manifest OSV-Scanner reads was found beneath " + rel(root, dir), true
	}
	if code != 0 && code != 1 {
		return fmt.Sprintf("dependency_scan: osv-scanner exited %d: %s", code, firstLines(errText, 5)), true
	}
	var report struct {
		Results []struct {
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
			Packages []struct {
				Package struct {
					Name      string `json:"name"`
					Version   string `json:"version"`
					Ecosystem string `json:"ecosystem"`
				} `json:"package"`
				Vulnerabilities []struct {
					ID       string `json:"id"`
					Affected []struct {
						Ranges []struct {
							Events []struct {
								Fixed string `json:"fixed"`
							} `json:"events"`
						} `json:"ranges"`
					} `json:"affected"`
				} `json:"vulnerabilities"`
				Groups []struct {
					IDs []string `json:"ids"`
				} `json:"groups"`
			} `json:"packages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		return "dependency_scan: osv-scanner's answer could not be read: " + err.Error(), true
	}
	var found []depFinding
	for _, r := range report.Results {
		for _, p := range r.Packages {
			if len(p.Vulnerabilities) == 0 {
				continue
			}
			f := depFinding{source: rel(root, r.Source.Path), ecosystem: p.Package.Ecosystem, name: p.Package.Name, version: p.Package.Version}
			fixed := map[string]bool{}
			for _, g := range p.Groups {
				if len(g.IDs) > 0 {
					f.ids = append(f.ids, g.IDs[0])
				}
			}
			if len(f.ids) == 0 {
				for _, v := range p.Vulnerabilities {
					f.ids = append(f.ids, v.ID)
				}
			}
			for _, v := range p.Vulnerabilities {
				for _, af := range v.Affected {
					for _, rg := range af.Ranges {
						for _, ev := range rg.Events {
							if ev.Fixed != "" {
								fixed[ev.Fixed] = true
							}
						}
					}
				}
			}
			for v := range fixed {
				f.fixed = append(f.fixed, v)
			}
			sort.Strings(f.fixed)
			found = append(found, f)
		}
	}
	if len(found) == 0 {
		if code == 1 {
			return "dependency_scan: osv-scanner reported vulnerabilities its answer did not list: " + firstLines(errText, 5), true
		}
		return "No known vulnerabilities in the dependencies beneath " + rel(root, dir) + ", against OSV's databases on this computer" + dbAge + ".", false
	}
	var b strings.Builder
	n := 0
	for _, f := range found {
		n += len(f.ids)
	}
	fmt.Fprintf(&b, "%d known vulnerabilit%s in %d package%s beneath %s, against OSV's databases on this computer%s:\n", n, plural(n, "y", "ies"), len(found), plural(len(found), "", "s"), rel(root, dir), dbAge)
	for _, f := range found {
		fixed := "no fixed version is known"
		if len(f.fixed) > 0 {
			fixed = "fixed in " + strings.Join(f.fixed, ", ")
		}
		fmt.Fprintf(&b, "- %s %s (%s, in %s): %s; %s\n", f.name, f.version, f.ecosystem, f.source, strings.Join(f.ids, ", "), fixed)
	}
	return b.String(), false
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}

// dbAgeOf says how old the databases at db are, from the record the runner
// keeps beside them (current.json: when each was downloaded), as a clause
// of the answer: the oldest's date, and stale past a week. Empty when the
// record cannot be read.
func dbAgeOf(db string) string {
	b, err := os.ReadFile(filepath.Join(db, "current.json")) //nolint:gosec // the runner's own record, mounted read-only
	if err != nil {
		return ""
	}
	var st struct {
		Fetched map[string]time.Time `json:"fetched"`
	}
	if json.Unmarshal(b, &st) != nil || len(st.Fetched) == 0 {
		return ""
	}
	var oldest time.Time
	for _, t := range st.Fetched {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	age := " (downloaded " + oldest.UTC().Format("2006-01-02") + " or later"
	if time.Since(oldest) > 7*24*time.Hour {
		age += "; stale: they have not been refreshed for over a week, so newer vulnerabilities are missing"
	}
	return age + ")"
}
