// Package sources is the input-source seam (plan input-sources.md §5,
// D2–D8; agentry-cli#833). An input source is where a run's inputs come
// from beyond what was uploaded: a repository today, a file at a web
// address next (agentry-cli#834), a SharePoint library later. A QuickAgent
// asks for one with an intake question of the kind's type; the answer is
// fetched before the run, by the runner image's `source` role in the files
// container behind a proxy to the source's host alone, into
// /inputs/<question id>/, and decoded like any input.
//
// Each kind is one entry in a registry and gives: how its answer is parsed
// and checked, the host its fetch reaches, how it uses a credential, the
// fetch step's argv (the fetch itself is the runner image's), its caps
// (D7), its malware-check rule (D8) and its receipt line. The engine runs
// every source through the same steps (engine/sources.go), so a new kind is
// a new entry here and in the runner image, with nothing else changed.
//
// A source is outside the contract (D4): giving it is the grant, and its
// access token is used by the fetch step alone (D5). The token is never
// part of an intake document, a receipt or an error.
package sources

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/blaktron/agentry/pkg/manifest"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// Caps bound one source (D7): a source over either is refused with its
// count and never staged in part.
type Caps struct {
	Files int
	Bytes int64
}

// LocalCaps are a local run's: 20,000 files and 1 GiB.
var LocalCaps = Caps{Files: source.DefaultMaxFiles, Bytes: source.DefaultMaxBytes}

// Kind is one source kind on the seam.
type Kind struct {
	// Type is the intake question type that asks for it.
	Type manifest.IntakeType
	// Noun names the kind in what a person reads.
	Noun string
	// Parse checks an answer and answers the source it names.
	Parse func(answer string) (Source, error)
	// Argv is the fetch step's argv after the runner binary: the role, the
	// source, where it writes and through which proxy, and its caps.
	Argv func(s Source, caps Caps, proxy string) []string
	// Prompt is what a terminal asks for it.
	Prompt string
}

// Source is one source a run fetches: a question's answer, parsed.
type Source struct {
	// ID is the intake question's id, which names the folder.
	ID   string
	Kind manifest.IntakeType
	// URL is the address, with no credentials; Ref the ref asked for,
	// empty for the default.
	URL string
	Ref string
	// Host is the address's host, lower-case: the one host the fetch step
	// reaches, and the saved key's name.
	Host string
	// Token is the access token, empty for anonymous. It goes to the
	// fetch step's stdin and nowhere else.
	Token string `json:"-"`
	// TokenFrom says where the token came from (TokenFrom*), for the
	// receipt; empty when there is none.
	TokenFrom string `json:"-"`
}

// Where a token came from.
const (
	TokenFromFlag     = "flag"
	TokenFromKeychain = "keychain"
	TokenFromPrompt   = "prompt"
)

var registry = map[manifest.IntakeType]Kind{
	manifest.IntakeRepository: repository,
}

// Lookup answers the kind an intake type asks for, false for a type that
// asks for no source or a kind this CLI cannot fetch.
func Lookup(t manifest.IntakeType) (Kind, bool) {
	k, ok := registry[t]
	return k, ok
}

// Unfetched says why an intake type that asks for a source cannot be
// answered by this CLI.
func Unfetched(id string, t manifest.IntakeType) error {
	if t == manifest.IntakeURL {
		return fmt.Errorf("intake question %q asks for a file at a web address (a url source), which this version of agentry cannot fetch yet (agentry-cli#834)", id)
	}
	return fmt.Errorf("intake question %q asks for a %s source, which this version of agentry cannot fetch", id, t)
}

// repository is the repository kind: a git repository at a ref, its
// answer an https address with the ref after #, as Docker's build-context
// addresses carry one.
var repository = Kind{
	Type:   manifest.IntakeRepository,
	Noun:   "repository",
	Prompt: "(the repository's https address; add #branch, #tag or #commit for one other than the default branch)",
	Parse:  parseRepository,
	Argv: func(s Source, caps Caps, proxy string) []string {
		argv := []string{"source", "--kind", source.KindRepository, "--url", s.URL,
			"--out", "/out", "--work", "/work", "--proxy", proxy,
			"--max-files", strconv.Itoa(caps.Files), "--max-bytes", strconv.FormatInt(caps.Bytes, 10)}
		if s.Ref != "" {
			argv = append(argv, "--ref", s.Ref)
		}
		if s.Token != "" {
			argv = append(argv, "--token-stdin")
		}
		return argv
	},
}

func parseRepository(answer string) (Source, error) {
	raw, ref, _ := strings.Cut(strings.TrimSpace(answer), "#")
	u, err := source.CheckURL(raw)
	if err != nil {
		return Source{}, err
	}
	if err := checkRef(ref); err != nil {
		return Source{}, err
	}
	host := strings.ToLower(u.Hostname())
	return Source{Kind: manifest.IntakeRepository, URL: raw, Ref: ref, Host: host}, nil
}

// checkRef holds a ref to git's own rules for a ref name, as far as an
// answer needs (git-check-ref-format): no spaces or control characters,
// none of ~^:?*[\, no "..", no "@{", no leading "-" or "/", and no
// trailing "/" or ".lock". A colon would be Docker's folder within the
// repository, which a source does not take.
func checkRef(ref string) error {
	if ref == "" {
		return nil
	}
	if len(ref) > 255 {
		return errors.New("the ref after # is longer than a ref can be")
	}
	if strings.Contains(ref, ":") {
		return errors.New("the ref after # names a folder (ref:folder); a source takes the whole repository")
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`~^?*[\`, r) {
			return fmt.Errorf("the ref after # holds %q, which no git ref holds", r)
		}
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") ||
		strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, ".") {
		return errors.New("the ref after # is not a valid git ref name")
	}
	return nil
}

// Fetched is what a source's fetch step reported and the host checked.
type Fetched struct {
	Source source.Report
	// Took is how long the fetch step ran.
	Took time.Duration
}

// CheckReport reads the fetch step's report and holds it to the source it
// was asked for and its caps. Everything in it was written by a process
// that read a host's answer, so it is held to shapes, never trusted for
// more than counts and words in a closed vocabulary.
func CheckReport(raw []byte, s Source, caps Caps) (source.Report, error) {
	var rep source.Report
	if err := jsonStrict(raw, &rep); err != nil {
		return rep, fmt.Errorf("the fetch step's report: %w", err)
	}
	if rep.Schema != source.Schema || rep.Kind != source.KindRepository {
		return rep, fmt.Errorf("the fetch step's report has the schema %q and kind %q", rep.Schema, rep.Kind)
	}
	if rep.Files < 0 || rep.Bytes < 0 || rep.Files > caps.Files || rep.Bytes > caps.Bytes || rep.LFSPointers < 0 || rep.LFSPointers > rep.Files {
		return rep, errors.New("the fetch step's report gives counts outside the source's caps")
	}
	if rep.Commit != "" && !validCommit(rep.Commit) {
		return rep, errors.New("the fetch step's report names a commit that is not a SHA-1")
	}
	if rep.Protocol != "" && rep.Protocol != "v0" && rep.Protocol != "v2" {
		return rep, errors.New("the fetch step's report names an unknown protocol")
	}
	for why, n := range rep.Skipped {
		if !knownSkip[why] || n < 0 {
			return rep, fmt.Errorf("the fetch step's report gives the unknown reason %q", why)
		}
	}
	for why := range rep.SkippedPaths {
		if !knownSkip[why] {
			return rep, fmt.Errorf("the fetch step's report gives the unknown reason %q", why)
		}
	}
	// Words shown to the person: printable and bounded.
	rep.Refusal = printable(rep.Refusal, 300)
	rep.Error = printable(rep.Error, 600)
	rep.Ref = printable(rep.Ref, 300)
	if s.Token != "" {
		for _, p := range []*string{&rep.Refusal, &rep.Error, &rep.Ref} {
			*p = strings.ReplaceAll(*p, s.Token, "[token]")
		}
	}
	return rep, nil
}

var knownSkip = map[string]bool{
	source.SkipLink: true, source.SkipSubmodule: true, source.SkipGit: true,
	source.SkipName: true, source.SkipCase: true, source.SkipDeep: true,
}

var skipWords = map[string]string{
	source.SkipLink:      "links",
	source.SkipSubmodule: "submodules",
	source.SkipGit:       ".git folders",
	source.SkipName:      "names no file system shares",
	source.SkipCase:      "names differing only in case",
	source.SkipDeep:      "folders nested too deep",
}

// Document is the source's entry in intake.json:
// {"repository": {"url", "ref", "commit", "path", "files"}}. It never
// holds the token.
func Document(s Source, rep source.Report, inputsMount string) map[string]any {
	entry := map[string]any{
		"url":    s.URL,
		"commit": rep.Commit,
		"path":   inputsMount + "/" + s.ID + "/",
		"files":  rep.Files,
	}
	if s.Ref != "" {
		entry["ref"] = s.Ref
	}
	return map[string]any{string(s.Kind): entry}
}

// Line is the source's receipt line: its kind, the address, the ref asked
// for and resolved, the commit, the files and bytes, whether a token was
// used (never its value), the time it took, what was left out, and the
// malware check of its files (checks, from the decode).
func Line(s Source, f Fetched, checks string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", nounOf(s.Kind), s.URL)
	switch {
	case s.Ref != "":
		fmt.Fprintf(&b, " at %q", s.Ref)
	default:
		b.WriteString(" at its default branch")
	}
	if f.Source.Ref != "" && f.Source.Ref != s.Ref {
		fmt.Fprintf(&b, " (%s)", f.Source.Ref)
	}
	fmt.Fprintf(&b, ", commit %s: %d files, %d bytes, fetched in %s at depth 1 through a proxy to %s alone", f.Source.Commit, f.Source.Files, f.Source.Bytes, f.Took.Round(100*time.Millisecond), s.Host)
	switch s.TokenFrom {
	case TokenFromFlag:
		b.WriteString(", with an access token from the environment variable named by --intake-token")
	case TokenFromKeychain:
		b.WriteString(", with the access token saved in the system keychain for " + s.Host)
	case TokenFromPrompt:
		b.WriteString(", with an access token given at the prompt")
	default:
		b.WriteString(", with no access token")
	}
	if left := skippedWords(f.Source.Skipped); left != "" {
		b.WriteString("; left out: " + left)
	}
	if f.Source.LFSPointers > 0 {
		fmt.Fprintf(&b, "; %d Git LFS files staged as their pointers", f.Source.LFSPointers)
	}
	if checks != "" {
		b.WriteString("; " + checks)
	}
	return b.String()
}

func nounOf(t manifest.IntakeType) string {
	if k, ok := registry[t]; ok {
		return k.Noun
	}
	return string(t)
}

func skippedWords(m map[string]int) string {
	var parts []string
	for why, n := range m {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, skipWords[why]))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// Checks is the malware-check clause of a source's receipt line, from the
// decode's report of its files: how many executable or binary risky files
// ClamAV checked and what it made of them, and how many were left
// unchecked by the source rule (D8).
func Checks(id string, files []runnerdecode.File) string {
	clean, found, other, notRisky := 0, 0, 0, 0
	prefix := id + "/"
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		switch {
		case f.Check == runnerdecode.CheckClean:
			clean++
		case f.Check == runnerdecode.CheckFound:
			found++
		case f.Unchecked == runnerdecode.UncheckedNotRisky:
			notRisky++
		case f.Check == runnerdecode.CheckUnchecked:
			other++
		}
	}
	risky := clean + found + other
	s := fmt.Sprintf("%d executable or binary risky files went to the malware check (%d clean", risky, clean)
	if found > 0 {
		s += fmt.Sprintf(", %d found", found)
	}
	if other > 0 {
		s += fmt.Sprintf(", %d not checked", other)
	}
	s += fmt.Sprintf("), and %d others were not checked, as the source rule leaves code, text and data", notRisky)
	return s
}

func validCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func printable(s string, n int) string {
	var b []byte
	for i := 0; i < len(s) && len(b) < n; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b = append(b, c)
		}
	}
	return string(b)
}
