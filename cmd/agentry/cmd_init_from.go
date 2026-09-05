// `agentry init --from` (agentry#71, PLAN §5.6/§11): import wizards that
// seed a BriefAgent workspace from something that already exists — an
// Agent Skills SKILL.md (with its sibling files) or an MCP registry
// server.json — and populate the manifest from what the source states.
// The capability contract is never guessed: what the source cannot state
// (egress, secrets, exec, filesystem) is asked on a terminal, with hints
// found in the source offered as defaults the author confirms, or taken
// from explicit --egress/--secrets/--exec/--fs-* answers; without either,
// the wizard refuses rather than writing a "pure text" contract nobody
// decided on.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// summaryMax is the manifest's V8 summary limit.
const summaryMax = manifest.MaxSummaryChars

// capAnswers is the capability contract as the author decided it.
type capAnswers struct {
	// Egress: nil = none; ["*"] = wildcard; else hosts.
	Egress  []string
	Secrets []string
	Exec    []string
	// FSRead / FSWrite: "none" or "workspace".
	FSRead  string
	FSWrite string
}

// fromFlags carries the --from flags.
type fromFlags struct {
	from    string
	egress  string
	secrets string
	exec    string
	fsRead  string
	fsWrite string
}

// answersFromFlags turns explicit flags into answers; ok is false when the
// two answers that cannot default (egress, secrets) were not given.
func (f fromFlags) answersFromFlags() (capAnswers, bool) {
	if f.egress == "" || f.secrets == "" {
		return capAnswers{}, false
	}
	a := capAnswers{
		Egress:  splitAnswer(f.egress),
		Secrets: splitAnswer(f.secrets),
		Exec:    splitAnswer(f.exec),
		FSRead:  fsAnswer(f.fsRead),
		FSWrite: fsAnswer(f.fsWrite),
	}
	return a, true
}

// splitAnswer parses "a, b" ("none" / "" → nil).
func splitAnswer(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "none") {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fsAnswer(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "workspace") {
		return "workspace"
	}
	return "none"
}

// imported is what a source stated plus what the author answered.
type imported struct {
	Name      string
	Namespace string
	Summary   string
	License   string
	Version   string
	// Files to copy into the workspace: relative path → absolute source.
	Files map[string]string
	// Include is the publish allowlist.
	Include []string
	// Servers are declared MCP servers (server.json import).
	Servers []manifest.MCPServer
	Answers capAnswers
	// Hints found in the source, shown to the author (never applied).
	Hints capHints
}

// capHints are candidates the wizard offers as defaults.
type capHints struct {
	Hosts   []string
	Secrets []string
	Exec    []string
}

// runInitFrom performs the import.
func runInitFrom(cmd *cobra.Command, dir, name, namespace string, ff fromFlags) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(absDir, "agent.yaml")); err == nil {
		return fmt.Errorf("%s already contains an agent.yaml — refusing to overwrite", absDir)
	}
	src, err := filepath.Abs(ff.from)
	if err != nil {
		return fmt.Errorf("resolve --from %s: %w", ff.from, err)
	}

	imp, err := detectAndRead(src)
	if err != nil {
		return err
	}
	if name != "" {
		imp.Name = name
	}
	if !manifest.ValidName(imp.Name) {
		return fmt.Errorf("agent name %q (from the source) is invalid (lowercase [a-z0-9-], 1-64 chars); pass --name", imp.Name)
	}
	if namespace == "" {
		namespace = defaultNamespace()
	}
	if !manifest.ValidNamespace(namespace) {
		return fmt.Errorf("namespace %q is invalid (lowercase [a-z0-9-], 1-38 chars); pass --namespace", namespace)
	}
	imp.Namespace = namespace

	// The contract: answered, never guessed.
	errOut := cmd.ErrOrStderr()
	if a, ok := ff.answersFromFlags(); ok {
		imp.Answers = a
		printHints(errOut, imp.Hints)
	} else if stdinIsTTY(cmd.InOrStdin()) {
		p := &setup.TTYPrompter{In: cmd.InOrStdin(), Err: errOut}
		imp.Answers, err = askCapabilities(p, imp)
		if err != nil {
			return err
		}
	} else {
		printHints(errOut, imp.Hints)
		return errors.New("the capability contract cannot be guessed from the source: answer with " +
			"--egress and --secrets (use \"none\" for none; --exec, --fs-read, --fs-write optional), or run on a terminal for the wizard")
	}

	// Write the workspace.
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", absDir, err)
	}
	var created []string
	for rel, from := range imp.Files {
		to := filepath.Join(absDir, filepath.FromSlash(rel))
		if from == to {
			continue
		}
		if _, err := os.Stat(to); err == nil {
			_, _ = fmt.Fprintf(errOut, "keeping existing %s\n", rel)
			continue
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
		created = append(created, rel)
	}
	generated := []scaffoldFile{{"agent.yaml", importedYAML(imp)}}
	if _, ok := imp.Files["README.md"]; !ok {
		generated = append(generated, scaffoldFile{"README.md", readmeMD(imp.Name, imp.Namespace)})
	}
	if _, ok := imp.Files["LICENSE"]; !ok {
		generated = append(generated, scaffoldFile{"LICENSE", licenseNote})
	}
	if _, ok := imp.Files["SKILL.md"]; !ok {
		generated = append(generated, scaffoldFile{"SKILL.md", skillMD(imp.Name)})
	}
	for _, f := range generated {
		full := filepath.Join(absDir, f.path)
		if _, err := os.Stat(full); err == nil {
			_, _ = fmt.Fprintf(errOut, "keeping existing %s\n", f.path)
			continue
		}
		if err := os.WriteFile(full, []byte(f.content), 0o644); err != nil { //nolint:gosec // workspace content is world-readable by design
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		created = append(created, f.path)
	}
	sort.Strings(created)

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "imported BriefAgent %s/%s (modes: autonomous) from %s into %s\n", imp.Namespace, imp.Name, ff.from, absDir)
	for _, p := range created {
		_, _ = fmt.Fprintf(out, "  created %s\n", p)
	}
	_, _ = fmt.Fprintf(out, "capabilities declared: %s\n", describeAnswers(imp.Answers, imp.Servers))
	_, _ = fmt.Fprintln(out, "next steps:")
	_, _ = fmt.Fprintln(out, "  1. review agent.yaml (summary, capabilities) and the entrypoint content")
	_, _ = fmt.Fprintln(out, "  2. make sure LICENSE holds the full license text")
	_, _ = fmt.Fprintln(out, "  3. agentry validate "+dir)
	return nil
}

// detectAndRead recognizes the source: a SKILL.md (or a directory holding
// one) or a server.json (or a directory holding one).
func detectAndRead(src string) (*imported, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("--from %s: %w", src, err)
	}
	if fi.IsDir() {
		if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err == nil {
			return readSkill(filepath.Join(src, "SKILL.md"))
		}
		if _, err := os.Stat(filepath.Join(src, "server.json")); err == nil {
			return readServerJSON(filepath.Join(src, "server.json"))
		}
		return nil, fmt.Errorf("--from %s: no SKILL.md or server.json in the directory (importable sources: an Agent Skills SKILL.md, an MCP registry server.json)", src)
	}
	switch strings.ToLower(filepath.Base(src)) {
	case "skill.md":
		return readSkill(src)
	case "server.json":
		return readServerJSON(src)
	}
	return nil, fmt.Errorf("--from %s: not an importable source (want SKILL.md or server.json, or a directory holding one)", src)
}

// skillFrontmatter is the agentskills.io frontmatter subset we map.
type skillFrontmatter struct {
	Name         string         `yaml:"name"`
	Description  string         `yaml:"description"`
	License      string         `yaml:"license"`
	AllowedTools yaml.Node      `yaml:"allowed-tools"`
	Metadata     map[string]any `yaml:"metadata"`
}

// readSkill imports an Agent Skills SKILL.md and its sibling files.
func readSkill(path string) (*imported, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // user-named source
	if err != nil {
		return nil, err
	}
	fm, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var front skillFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &front); err != nil {
		return nil, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	srcDir := filepath.Dir(path)
	imp := &imported{
		Name:    front.Name,
		Summary: oneLine(front.Description),
		License: front.License,
		Files:   map[string]string{},
	}
	if imp.Name == "" {
		imp.Name = filepath.Base(srcDir)
	}
	if imp.Summary == "" {
		imp.Summary = "TODO one line (at most 140 chars) saying what this BriefAgent does"
	}
	if v, ok := front.Metadata["version"].(string); ok && v != "" {
		imp.Version = v
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if n == "agent.yaml" || n == ".git" || n == "node_modules" || strings.HasPrefix(n, ".") {
			continue
		}
		if e.IsDir() {
			if err := filepath.WalkDir(filepath.Join(srcDir, n), func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(srcDir, p)
				imp.Files[filepath.ToSlash(rel)] = p
				return nil
			}); err != nil {
				return nil, err
			}
			imp.Include = append(imp.Include, n+"/**")
			continue
		}
		imp.Files[n] = filepath.Join(srcDir, n)
		imp.Include = append(imp.Include, n)
	}
	for _, must := range []string{"agent.yaml", "README.md", "LICENSE", "SKILL.md"} {
		if !contains(imp.Include, must) {
			imp.Include = append(imp.Include, must)
		}
	}
	sort.Strings(imp.Include)
	imp.Hints = hintsFromText(body + "\n" + allowedToolsText(front.AllowedTools))
	return imp, nil
}

// splitFrontmatter separates a leading "---\n…\n---" block from the body.
func splitFrontmatter(s string) (front, body string, err error) {
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", s, errors.New("SKILL.md has no YAML frontmatter (expected a leading --- block with name and description)")
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", errors.New("SKILL.md frontmatter is not closed by ---")
	}
	front = rest[:end]
	body = rest[end+4:]
	return front, body, nil
}

func allowedToolsText(n yaml.Node) string {
	var list []string
	if n.Kind == yaml.SequenceNode {
		_ = n.Decode(&list)
		return strings.Join(list, " ")
	}
	return n.Value
}

// oneLine squashes whitespace and truncates to the manifest's limit.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > summaryMax {
		s = strings.TrimSpace(string(r[:summaryMax-1])) + "…"
	}
	return s
}

var (
	hostRE   = regexp.MustCompile(`https?://([a-z0-9][a-z0-9.-]*\.[a-z]{2,})`)
	secretRE = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*(?:_TOKEN|_KEY|_SECRET|_PAT|_PASSWORD|API_KEY)\b`)
	execRE   = regexp.MustCompile("(?i)\\b(?:bash|shell|run|execute)\\b[^\\n`]*`([a-z][a-z0-9_.-]*)")
)

// hintsFromText finds candidates the author is asked to confirm.
func hintsFromText(text string) capHints {
	var h capHints
	seen := map[string]bool{}
	for _, m := range hostRE.FindAllStringSubmatch(strings.ToLower(text), -1) {
		if !seen["h:"+m[1]] {
			seen["h:"+m[1]] = true
			h.Hosts = append(h.Hosts, m[1])
		}
	}
	for _, m := range secretRE.FindAllString(text, -1) {
		if !seen["s:"+m] {
			seen["s:"+m] = true
			h.Secrets = append(h.Secrets, m)
		}
	}
	for _, m := range execRE.FindAllStringSubmatch(text, -1) {
		if !seen["x:"+m[1]] {
			seen["x:"+m[1]] = true
			h.Exec = append(h.Exec, m[1])
		}
	}
	return h
}

// serverJSON is the MCP registry server.json subset we map (schema
// 2025-12-11: reverse-DNS name, packages[] with registryType/identifier/
// version and secret-flagged environment variables, remotes[]).
type serverJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Packages    []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Version      string `json:"version"`
		EnvVars      []struct {
			Name     string `json:"name"`
			IsSecret bool   `json:"isSecret"`
		} `json:"environmentVariables"`
	} `json:"packages"`
	Remotes []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"remotes"`
}

// readServerJSON imports a server.json as a skill BriefAgent that declares
// the published package under capabilities.mcpServers (DEV-060): the
// registry entry describes an installable server, and a BriefAgent that
// uses one names it there with an exact pin and its secret names.
func readServerJSON(path string) (*imported, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // user-named source
	if err != nil {
		return nil, err
	}
	var sj serverJSON
	if err := json.Unmarshal(raw, &sj); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	imp := &imported{Summary: oneLine(sj.Description), Version: sj.Version,
		Files: map[string]string{}, Include: []string{"agent.yaml", "LICENSE", "README.md", "SKILL.md"}}
	var hints capHints
	for _, p := range sj.Packages {
		var srv manifest.MCPServer
		switch strings.ToLower(p.RegistryType) {
		case "npm":
			srv.NPM = p.Identifier
		case "pypi":
			srv.PyPI = p.Identifier
		default:
			continue
		}
		if p.Version == "" || strings.ContainsAny(p.Version, "^~*<>=x") {
			return nil, fmt.Errorf("%s: package %s has no exact version (%q); the manifest pins servers exactly (V5)", path, p.Identifier, p.Version)
		}
		srv.Version = p.Version
		for _, ev := range p.EnvVars {
			if ev.IsSecret && ev.Name != "" {
				srv.Secrets = append(srv.Secrets, ev.Name)
			}
		}
		imp.Servers = append(imp.Servers, srv)
		if imp.Name == "" {
			imp.Name = lastSegment(p.Identifier)
		}
	}
	if len(imp.Servers) == 0 {
		kinds := []string{}
		for _, p := range sj.Packages {
			kinds = append(kinds, p.RegistryType)
		}
		for _, r := range sj.Remotes {
			kinds = append(kinds, "remote:"+r.Type)
		}
		return nil, fmt.Errorf("%s: no npm or PyPI package to pin (found: %s); the runner installs declared servers from npm and PyPI only", path, strings.Join(kinds, ", "))
	}
	if imp.Name == "" {
		imp.Name = lastSegment(sj.Name)
	}
	if imp.Summary == "" {
		imp.Summary = "TODO one line (at most 140 chars) saying what this BriefAgent does"
	}
	for _, r := range sj.Remotes {
		for _, m := range hostRE.FindAllStringSubmatch(strings.ToLower(r.URL), -1) {
			hints.Hosts = append(hints.Hosts, m[1])
		}
	}
	imp.Hints = hints
	return imp, nil
}

// lastSegment takes the trailing path segment of a package or reverse-DNS
// name and squashes it to the handle grammar.
func lastSegment(s string) string {
	if i := strings.LastIndexAny(s, "/."); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	s = invalidHandleChars.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// askCapabilities is the terminal wizard: every question is asked, hints
// are defaults the author confirms or replaces.
func askCapabilities(p setup.Prompter, imp *imported) (capAnswers, error) {
	p.Say("The capability contract cannot be read from the source; declare only what the agent really needs.")
	if len(imp.Servers) > 0 {
		p.Say(fmt.Sprintf("Declared MCP servers: %s (secret names taken from server.json)", describeServers(imp.Servers)))
	}
	var a capAnswers
	target := "the agent"
	if len(imp.Servers) > 0 {
		target = "its MCP server(s)"
	}
	def := strings.Join(imp.Hints.Hosts, ", ")
	if def == "" {
		def = "none"
	}
	eg, err := p.Ask(fmt.Sprintf("Network egress hosts %s may reach (comma-separated, \"*\" for any, none)", target), def)
	if err != nil {
		return a, err
	}
	a.Egress = splitAnswer(orDefault(eg, def))
	def = strings.Join(imp.Hints.Secrets, ", ")
	if def == "" {
		def = "none"
	}
	sec, err := p.Ask("Secret NAMES the agent itself needs (env vars, never values; none)", def)
	if err != nil {
		return a, err
	}
	a.Secrets = splitAnswer(orDefault(sec, def))
	def = strings.Join(imp.Hints.Exec, ", ")
	if def == "" {
		def = "none"
	}
	ex, err := p.Ask("Commands the agent executes (single tokens; none)", def)
	if err != nil {
		return a, err
	}
	a.Exec = splitAnswer(orDefault(ex, def))
	fr, err := p.Ask("Filesystem read (none | workspace)", "none")
	if err != nil {
		return a, err
	}
	a.FSRead = fsAnswer(orDefault(fr, "none"))
	fw, err := p.Ask("Filesystem write (none | workspace)", "none")
	if err != nil {
		return a, err
	}
	a.FSWrite = fsAnswer(orDefault(fw, "none"))
	return a, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// printHints shows what the source suggested, for the author to weigh.
func printHints(w io.Writer, h capHints) {
	if len(h.Hosts)+len(h.Secrets)+len(h.Exec) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "hints found in the source (not applied):")
	if len(h.Hosts) > 0 {
		_, _ = fmt.Fprintf(w, "  hosts mentioned: %s\n", strings.Join(h.Hosts, ", "))
	}
	if len(h.Secrets) > 0 {
		_, _ = fmt.Fprintf(w, "  secret-looking names: %s\n", strings.Join(h.Secrets, ", "))
	}
	if len(h.Exec) > 0 {
		_, _ = fmt.Fprintf(w, "  commands mentioned: %s\n", strings.Join(h.Exec, ", "))
	}
}

// importedYAML renders the manifest for an import.
func importedYAML(imp *imported) string {
	var b strings.Builder
	w := func(format string, args ...any) { _, _ = fmt.Fprintf(&b, format+"\n", args...) }
	q := func(s string) string {
		out, _ := yaml.Marshal(s)
		return strings.TrimSpace(string(out))
	}
	list := func(items []string) string {
		qs := make([]string, 0, len(items))
		for _, it := range items {
			qs = append(qs, q(it))
		}
		return "[" + strings.Join(qs, ", ") + "]"
	}
	version := imp.Version
	if version == "" {
		version = "0.1.0"
	}
	license := imp.License
	if license == "" {
		license = "Apache-2.0"
	}
	w(`schemaVersion: "2"`)
	w("name: %s", imp.Name)
	w("namespace: %s", imp.Namespace)
	w("# Run modes: an import starts autonomous; add interactive (and an")
	w("# interactive: block) once the instructions expect a conversation.")
	w("modes: [%s]", manifest.ModeAutonomous)
	w("version: %s", version)
	w("summary: %s", q(imp.Summary))
	w("license: %s", q(license))
	w("")
	w("# The imported SKILL.md is the instruction file; its frontmatter is")
	w("# stripped before the body reaches the harness.")
	w("entrypoints:")
	w("  instruction: SKILL.md")
	w("runtime:")
	w("  interfaces: [claude-skill]")
	w("")
	w("# capabilities is the security contract (closed vocabulary, PLAN 5.4),")
	w("# declared by the author at import time — never inferred from the source.")
	w("capabilities:")
	a := imp.Answers
	agentEgress := a.Egress
	if len(imp.Servers) > 0 {
		agentEgress = nil // the servers do the talking
	}
	w("  network:")
	switch {
	case len(agentEgress) == 1 && agentEgress[0] == "*":
		w(`    egress: "*"`)
	case len(agentEgress) > 0:
		w("    egress: %s", list(agentEgress))
	default:
		w("    egress: none")
	}
	w("  filesystem:")
	w("    read: %s", a.FSRead)
	w("    write: %s", a.FSWrite)
	if len(a.Exec) > 0 {
		w("  exec: { commands: %s }", list(a.Exec))
	} else {
		w("  exec: none")
	}
	if len(a.Secrets) > 0 {
		w("  secrets: %s", list(a.Secrets))
	}
	if len(imp.Servers) > 0 {
		w("  mcpServers:")
		for _, s := range imp.Servers {
			if s.NPM != "" {
				w("    - npm: %s", q(s.NPM))
			} else {
				w("    - pypi: %s", q(s.PyPI))
			}
			w("      version: %s", q(s.Version))
			if len(s.Secrets) > 0 {
				w("      secrets: %s", list(s.Secrets))
			}
			if len(a.Egress) > 0 {
				w("      egress: %s", list(a.Egress))
			}
		}
	}
	w("")
	w("publish:")
	w("  include: %s", list(imp.Include))
	return b.String()
}

func describeServers(servers []manifest.MCPServer) string {
	parts := make([]string, 0, len(servers))
	for _, s := range servers {
		parts = append(parts, s.SourceID()+"@"+s.Version)
	}
	return strings.Join(parts, ", ")
}

func describeAnswers(a capAnswers, servers []manifest.MCPServer) string {
	var parts []string
	eg := "none"
	if len(a.Egress) > 0 {
		eg = strings.Join(a.Egress, ",")
	}
	if len(servers) > 0 {
		parts = append(parts, "mcpServers "+describeServers(servers)+" (egress "+eg+")")
	} else {
		parts = append(parts, "egress "+eg)
	}
	if len(a.Secrets) > 0 {
		parts = append(parts, "secrets "+strings.Join(a.Secrets, ","))
	}
	if len(a.Exec) > 0 {
		parts = append(parts, "exec "+strings.Join(a.Exec, ","))
	}
	parts = append(parts, "fs read "+a.FSRead+", write "+a.FSWrite)
	return strings.Join(parts, "; ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// copyFile copies one regular file, creating parents.
func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from) //nolint:gosec // user-named source
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // workspace content
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
