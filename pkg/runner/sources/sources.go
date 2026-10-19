// Package sources is the input-source seam. An input source is where a
// run's inputs come from beyond what was uploaded: a repository, a file at
// a web address, a SharePoint library later. A QuickAgent
// asks for one with an intake question of the kind's type; the answer is
// fetched before the run, by the runner image's `source` role in the files
// container behind a proxy to the source's host alone, into
// /inputs/<question id>/, and decoded like any input.
//
// Each kind is one entry in a registry and gives: how its answer is parsed
// and checked, the host its fetch reaches, how it uses a credential, the
// fetch step's argv (the fetch itself is the runner image's), its caps,
// its malware-check rule (the source rule, or decoded as an
// upload) and its receipt line. The engine runs every source through the
// same steps (engine/sources.go). A new kind is a new entry here and in the
// runner image, and a branch in Line, Document and CheckReport when its
// report has a shape of its own, as a url source's one file does.
//
// A source is outside the contract: giving it is the grant, and its
// access token is used by the fetch step alone. The token is never
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

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// Caps bound one source: a source over either is refused with its
// count and never staged in part.
type Caps struct {
	Files int
	Bytes int64
}

// LocalCaps are a local repository's: 20,000 files and 1 GiB.
var LocalCaps = Caps{Files: source.DefaultMaxFiles, Bytes: source.DefaultMaxBytes}

// URLCaps are a local url source's: the uploads' caps, so one file within
// the most of one document the host reads back from a decode (256 MiB),
// its archive members counted against the uploads' 2,000 in the decode.
var URLCaps = Caps{Files: 1, Bytes: source.DefaultURLMaxBytes}

// HostedRepositoryCaps are a hosted run's repository caps: 10,000 files and 256
// MiB, on a run host shared by every account.
var HostedRepositoryCaps = Caps{Files: 10_000, Bytes: 256 << 20}

// HostedURLCaps are a hosted run's url source caps: the hosted upload cap, 10
// MiB, since its file is decoded as an upload is.
var HostedURLCaps = Caps{Files: 1, Bytes: 10 << 20}

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
	// Runner is the kind's word in the runner image's source role (its
	// --kind) and in the role's report.
	Runner string
	// Caps are a local run's caps for it; HostedCaps a hosted run's.
	Caps       Caps
	HostedCaps Caps
	// AsUpload decodes it as an upload is decoded: every file checked by
	// ClamAV, images read with OCR, and its files counted against the
	// uploads' cap. Otherwise the source rule holds: only executable
	// and binary risky files are checked, images are not read with OCR,
	// and the source has a cap of its own.
	AsUpload bool
}

// CapsFor is the kind's caps for a local run or, hosted, for a run on a
// sandbox host shared by every account.
func (k Kind) CapsFor(hosted bool) Caps {
	if hosted {
		return k.HostedCaps
	}
	return k.Caps
}

// Within is the caps held to a caller's own bounds: a knowledge build's
// pack cap, say, which a source may not pass however large the kind
// allows. A bound of zero or less leaves that cap as it is.
func (c Caps) Within(files int, bytes int64) Caps {
	if files > 0 && files < c.Files {
		c.Files = files
	}
	if bytes > 0 && bytes < c.Bytes {
		c.Bytes = bytes
	}
	return c
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
	TokenFromFlag = "flag"
	// TokenFromBuildFlag is a knowledge build's --source-token.
	TokenFromBuildFlag = "source-token"
	TokenFromKeychain  = "keychain"
	// TokenFromSealed is the saved key, read from the keys sealed beside
	// a schedule that runs while its person is signed out.
	TokenFromSealed = "sealed"
	TokenFromPrompt = "prompt"
)

var registry = map[manifest.IntakeType]Kind{
	manifest.IntakeRepository: repository,
	manifest.IntakeURL:        fileURL,
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
	return fmt.Errorf("intake question %q asks for a %s source, which this version of agentry cannot fetch", id, t)
}

// repository is the repository kind: a git repository at a ref, its
// answer an https address with the ref after #, as Docker's build-context
// addresses carry one.
var repository = Kind{
	Type:       manifest.IntakeRepository,
	Noun:       "repository",
	Prompt:     "(the repository's https address; add #branch, #tag or #commit for one other than the default branch)",
	Runner:     source.KindRepository,
	Caps:       LocalCaps,
	HostedCaps: HostedRepositoryCaps,
	Parse:      parseRepository,
	Argv: func(s Source, caps Caps, proxy string) []string {
		return fetchArgv(source.KindRepository, s, caps, proxy)
	},
}

// fetchArgv is the source role's argv: the kind, the address and the ref,
// where it writes, its proxy and caps, and --token-stdin when there is a
// token (never the token itself).
func fetchArgv(kind string, s Source, caps Caps, proxy string) []string {
	argv := []string{"source", "--kind", kind, "--url", s.URL,
		"--out", "/out", "--work", "/work", "--proxy", proxy,
		"--max-files", strconv.Itoa(caps.Files), "--max-bytes", strconv.FormatInt(caps.Bytes, 10)}
	if s.Ref != "" {
		argv = append(argv, "--ref", s.Ref)
	}
	if s.Token != "" {
		argv = append(argv, "--token-stdin")
	}
	return argv
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

// fileURL is the url kind: one file at a web address, fetched with one https
// GET and decoded as an upload.
var fileURL = Kind{
	Type:       manifest.IntakeURL,
	Noun:       "file",
	Prompt:     "(the file's https address)",
	Runner:     source.KindURL,
	Caps:       URLCaps,
	HostedCaps: HostedURLCaps,
	AsUpload:   true,
	Parse:      parseFileURL,
	Argv: func(s Source, caps Caps, proxy string) []string {
		return fetchArgv(source.KindURL, s, caps, proxy)
	},
}

// parseFileURL takes a file's https address. A fragment is never sent to a
// host, so it is dropped (a PDF's #page=2); a query is kept.
func parseFileURL(answer string) (Source, error) {
	raw, _, _ := strings.Cut(strings.TrimSpace(answer), "#")
	u, err := source.CheckFileURL(raw)
	if err != nil {
		return Source{}, err
	}
	return Source{Kind: manifest.IntakeURL, URL: raw, Host: strings.ToLower(u.Hostname())}, nil
}

// Shown is the address as a person and the agent read it: a query, which
// can carry a signed download's credential, is shown as "?…" and never
// written to intake.json, a receipt or an error.
func (s Source) Shown() string {
	if base, _, ok := strings.Cut(s.URL, "?"); ok {
		return base + "?…"
	}
	return s.URL
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
	if want := registry[s.Kind].Runner; rep.Schema != source.Schema || rep.Kind != want || want == "" {
		return rep, fmt.Errorf("the fetch step's report has the schema %q and kind %q", rep.Schema, rep.Kind)
	}
	// A url source is one file under a portable name, or a refusal or an error
	// with none; nothing of a repository's.
	if rep.Kind == source.KindURL {
		ok := rep.Commit == "" && rep.Protocol == "" && rep.Ref == "" && rep.LFSPointers == 0 && len(rep.Skipped) == 0 && len(rep.SkippedPaths) == 0
		if rep.Name == "" {
			ok = ok && rep.Files == 0 && rep.Bytes == 0
		} else {
			ok = ok && rep.Files == 1 && source.PortableFileName(rep.Name) == rep.Name
		}
		if !ok {
			return rep, errors.New("the fetch step's report is not one file's")
		}
	} else if rep.Name != "" {
		return rep, errors.New("the fetch step's report names a file for a repository")
	}
	if rep.Files < 0 || rep.Bytes < 0 || rep.Files > caps.Files || rep.Bytes > caps.Bytes || rep.LFSPointers < 0 || rep.LFSPointers > rep.Files {
		return rep, errors.New("the fetch step's report gives counts outside the source's caps")
	}
	// A refusal's count is held to the closed words and must be over the
	// cap the caller fetched with, whose value is never the report's.
	if o := rep.Over; o != nil && !overTheCap(rep, caps) {
		return rep, errors.New("the fetch step's report gives a count that is not a refusal's")
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
	for _, f := range source.TokenForms(s.Token) {
		for _, p := range []*string{&rep.Refusal, &rep.Error, &rep.Ref} {
			*p = strings.ReplaceAll(*p, f, "[token]")
		}
	}
	return rep, nil
}

// overTheCap says whether a report's count is a refusal's: with its
// sentence, in the closed words (a url source's in bytes alone), and over
// the cap. A bound the step stopped counting at (entries, a pack's sizes)
// says so with More, and may equal a url source's cap, past which it
// stopped reading.
func overTheCap(rep source.Report, caps Caps) bool {
	o := rep.Over
	var limit int64
	switch o.Cap {
	case source.CapFiles:
		limit = int64(caps.Files)
	case source.CapBytes:
		limit = caps.Bytes
	default:
		return false
	}
	bound := o.Of == source.OfEntries || o.Of == source.OfPackBytes || o.Of == source.OfInflatedBytes
	switch {
	case rep.Refusal == "" || !receipt.CountsAgainst(o.Cap, o.Of) || (bound && !o.More):
		return false
	case rep.Kind == source.KindURL && (o.Cap != source.CapBytes || o.Of != source.OfBytes):
		return false
	case o.More:
		return o.Count >= limit
	}
	return o.Count > limit
}

var knownSkip = map[string]bool{
	source.SkipLink: true, source.SkipSubmodule: true, source.SkipGit: true,
	source.SkipName: true, source.SkipCase: true, source.SkipDeep: true,
}

var skipWords = map[string][2]string{
	source.SkipLink:      {"link", "links"},
	source.SkipSubmodule: {"submodule", "submodules"},
	source.SkipGit:       {".git folder", ".git folders"},
	source.SkipName:      {"name no file system shares", "names no file system shares"},
	source.SkipCase:      {"name differing only in case", "names differing only in case"},
	source.SkipDeep:      {"folder nested too deep", "folders nested too deep"},
}

// Document is the source's entry in intake.json:
// {"repository": {"url", "ref", "commit", "path", "files"}}. It never
// holds the token.
func Document(s Source, rep source.Report, inputsMount string) map[string]any {
	entry := map[string]any{
		"url":  s.Shown(),
		"path": inputsMount + "/" + s.ID + "/",
	}
	// A url source names its one file and its size once it is fetched.
	if rep.Name != "" {
		entry["file"], entry["files"], entry["bytes"] = inputsMount+"/"+s.ID+"/"+rep.Name, rep.Files, rep.Bytes
	}
	// The commit and the count once the fetch made them, never a blank.
	if rep.Commit != "" {
		entry["commit"], entry["files"] = rep.Commit, rep.Files
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
	if s.Kind == manifest.IntakeURL {
		fmt.Fprintf(&b, "file %s: %s, %d bytes, fetched in %s with one GET (redirects followed on that host alone) through a proxy to %s alone", s.Shown(), f.Source.Name, f.Source.Bytes, f.Took.Round(100*time.Millisecond), s.Host)
		b.WriteString(tokenWords(s))
		if checks != "" {
			b.WriteString("; " + checks)
		}
		return b.String()
	}
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
	fmt.Fprintf(&b, ", commit %s: %d %s, %d bytes, fetched in %s at depth 1 through a proxy to %s alone", f.Source.Commit, f.Source.Files, plural(f.Source.Files, "file", "files"), f.Source.Bytes, f.Took.Round(100*time.Millisecond), s.Host)
	b.WriteString(tokenWords(s))
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

// tokenWords says whether a token was used and where it came from, never
// its value.
func tokenWords(s Source) string {
	switch s.TokenFrom {
	case TokenFromFlag:
		return ", with an access token from the environment variable named by " + TokenFlag
	case TokenFromBuildFlag:
		return ", with an access token from the environment variable named by " + BuildTokenFlag
	case TokenFromKeychain:
		return ", with the access token saved in the system keychain for " + s.Host
	case TokenFromSealed:
		return ", with the access token saved for " + s.Host + ", from the keys sealed beside this schedule"
	case TokenFromPrompt:
		return ", with an access token given at the prompt"
	}
	return ", with no access token"
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
			w := skipWords[why]
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, w[0], w[1])))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// Checks is the malware-check clause of a source's receipt line, from the
// decode's report of its files: how many executable or binary risky files
// ClamAV checked and what it made of them, and how many were left
// unchecked by the source rule.
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
	s := fmt.Sprintf("%d executable or binary risky %s went to the malware check (%d clean", risky, plural(risky, "file", "files"), clean)
	if found > 0 {
		s += fmt.Sprintf(", %d found", found)
	}
	if other > 0 {
		s += fmt.Sprintf(", %d not checked", other)
	}
	s += fmt.Sprintf("), and %d %s not checked, as the source rule leaves code, text and data", notRisky, plural(notRisky, "other was", "others were"))
	return s
}

// ChecksAsUpload is the malware-check clause of a source decoded as an
// upload (Kind.AsUpload): every file of it, an archive's members included,
// was sent to the check as an upload's is; the clause counts those it
// checked and, apart, those it could not.
func ChecksAsUpload(id string, files []runnerdecode.File) string {
	prefix := id + "/"
	return checksAsUpload(files, func(p string) bool { return strings.HasPrefix(p, prefix) })
}

// ChecksAsUploadNamed is ChecksAsUpload for a file staged under its own
// name rather than in a folder of the source's id (a knowledge build's): the
// file itself and the members of an archive it may be.
func ChecksAsUploadNamed(name string, files []runnerdecode.File) string {
	return checksAsUpload(files, func(p string) bool { return p == name || strings.HasPrefix(p, name+"/") })
}

func checksAsUpload(files []runnerdecode.File, ofSource func(string) bool) string {
	clean, found, other := 0, 0, 0
	for _, f := range files {
		if !ofSource(f.Path) {
			continue
		}
		switch f.Check {
		case runnerdecode.CheckClean:
			clean++
		case runnerdecode.CheckFound:
			found++
		default:
			other++
		}
	}
	n := clean + found
	s := fmt.Sprintf("decoded as an upload is: the malware check read %d %s (%d clean", n, plural(n, "file", "files"), clean)
	if found > 0 {
		s += fmt.Sprintf(", %d found", found)
	}
	s += ")"
	if other > 0 {
		s += fmt.Sprintf(", and %d %s not checked", other, plural(other, "was", "were"))
	}
	return s
}

// ChecksFor is the malware-check clause for a source by its kind's rule.
func ChecksFor(s Source, files []runnerdecode.File) string {
	if registry[s.Kind].AsUpload {
		return ChecksAsUpload(s.ID, files)
	}
	return Checks(s.ID, files)
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

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
