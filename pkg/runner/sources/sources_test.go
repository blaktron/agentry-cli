package sources

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

func TestARepositoryAnswerIsParsed(t *testing.T) {
	k, ok := Lookup(manifest.IntakeRepository)
	if !ok {
		t.Fatal("no repository kind")
	}
	s, err := k.Parse(" https://GitHub.com/o/r#release/1.2 ")
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://GitHub.com/o/r" || s.Ref != "release/1.2" || s.Host != "github.com" || s.Kind != manifest.IntakeRepository {
		t.Errorf("parsed %+v", s)
	}
	for _, bad := range []string{
		"http://github.com/o/r", "ssh://git@github.com/o/r", "git://github.com/o/r", "git@github.com:o/r.git",
		"https://x:tok@github.com/o/r", "https://github.com/o/r#main:subdir", "https://github.com/o/r#a..b",
		"https://github.com/o/r#-x", "https://github.com/o/r#has space", "https://github.com:22/o/r",
	} {
		if _, err := k.Parse(bad); err == nil {
			t.Errorf("Parse(%q) passed", bad)
		}
	}
}

func TestAURLAnswerIsParsed(t *testing.T) {
	k, ok := Lookup(manifest.IntakeURL)
	if !ok || !k.AsUpload || k.Caps != URLCaps {
		t.Fatalf("the url kind: %+v %v", k, ok)
	}
	s, err := k.Parse(" https://Files.Example.com/get?id=7&sig=s3cr3t#page=2 ")
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://Files.Example.com/get?id=7&sig=s3cr3t" || s.Host != "files.example.com" || s.Ref != "" || s.Kind != manifest.IntakeURL {
		t.Errorf("parsed %+v", s)
	}
	if s.Shown() != "https://Files.Example.com/get?…" {
		t.Errorf("shown %q", s.Shown())
	}
	for _, bad := range []string{
		"http://example.com/f.pdf", "ftp://example.com/f", "https://u:p@example.com/f", "https://example.com:8080/f", "file:///etc/passwd", "",
	} {
		if _, err := k.Parse(bad); err == nil {
			t.Errorf("Parse(%q) passed", bad)
		}
	}
	argv := strings.Join(k.Argv(Source{URL: s.URL, Token: "tok"}, URLCaps, "http://p:1"), " ")
	if !strings.Contains(argv, "--kind url") || !strings.Contains(argv, "--max-files 1 --max-bytes 268435456") || !strings.Contains(argv, "--token-stdin") || strings.Contains(argv, "tok ") || strings.Contains(argv, "--ref") {
		t.Errorf("argv %s", argv)
	}
}

func TestAURLReportIsOneFile(t *testing.T) {
	s := Source{Kind: manifest.IntakeURL, Token: "tok123"}
	good := `{"schema":"source/v1","kind":"url","files":1,"bytes":10,"name":"report.pdf"}`
	if rep, err := CheckReport([]byte(good), s, URLCaps); err != nil || rep.Name != "report.pdf" {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := CheckReport([]byte(`{"schema":"source/v1","kind":"url","files":0,"bytes":0,"error":"the host answered 404"}`), s, URLCaps); err != nil {
		t.Errorf("a failed fetch's report: %v", err)
	}
	for name, raw := range map[string]string{
		"a repository's report": `{"schema":"source/v1","kind":"repository","files":1,"bytes":1}`,
		"two files":             `{"schema":"source/v1","kind":"url","files":2,"bytes":10,"name":"a"}`,
		"an unsafe name":        `{"schema":"source/v1","kind":"url","files":1,"bytes":10,"name":"../a"}`,
		"a device name":         `{"schema":"source/v1","kind":"url","files":1,"bytes":10,"name":"CON"}`,
		"a commit":              `{"schema":"source/v1","kind":"url","files":1,"bytes":10,"name":"a","commit":"0123456789abcdef0123456789abcdef01234567"}`,
		"a file with no name":   `{"schema":"source/v1","kind":"url","files":1,"bytes":10}`,
		"over the cap":          `{"schema":"source/v1","kind":"url","files":1,"bytes":268435457,"name":"a"}`,
	} {
		if _, err := CheckReport([]byte(raw), s, URLCaps); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
	if _, err := CheckReport([]byte(`{"schema":"source/v1","kind":"repository","files":1,"bytes":1,"name":"a"}`), Source{Kind: manifest.IntakeRepository}, LocalCaps); err == nil {
		t.Error("a repository's report naming a file passed")
	}
}

func TestAURLDocumentAndLineHideTheQuery(t *testing.T) {
	s := Source{ID: "doc", Kind: manifest.IntakeURL, URL: "https://example.com/f?sig=s3cr3t", Host: "example.com", Token: "tok_PLANTED", TokenFrom: TokenFromKeychain}
	rep := source.Report{Kind: source.KindURL, Files: 1, Bytes: 42, Name: "f.pdf"}
	doc := Document(s, rep, "/inputs")
	r := doc["url"].(map[string]any)
	if r["url"] != "https://example.com/f?…" || r["file"] != "/inputs/doc/f.pdf" || r["path"] != "/inputs/doc/" || r["files"] != 1 || r["bytes"] != int64(42) {
		t.Errorf("document %v", doc)
	}
	files := []runnerdecode.File{
		{Path: "doc/f.pdf", Check: runnerdecode.CheckClean},
		{Path: "doc2/x.txt", Check: runnerdecode.CheckFound},
	}
	line := Line(s, Fetched{Source: rep}, ChecksFor(s, files))
	for _, want := range []string{"file https://example.com/f?…: f.pdf, 42 bytes", "through a proxy to example.com alone", "saved in the system keychain for example.com", "decoded as an upload is: the malware check read 1 file (1 clean)"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not say %q", line, want)
		}
	}
	if strings.Contains(line, "s3cr3t") || strings.Contains(line, "tok_PLANTED") {
		t.Errorf("line %q carries the query or the token", line)
	}
	// A file the check could not read is said apart, never counted as read.
	files = append(files, runnerdecode.File{Path: "doc/f.pdf/inner.bin", Check: runnerdecode.CheckUnchecked})
	if got := ChecksAsUpload("doc", files); got != "decoded as an upload is: the malware check read 1 file (1 clean), and 1 was not checked" {
		t.Errorf("ChecksAsUpload = %q", got)
	}
}

// TestAURLSourceFitsWhatTheDecodeReadsBack: a url source's byte cap is the
// most of one document the host reads back from a decode, so a file within
// it is never refused later by the read-back.
func TestAURLSourceFitsWhatTheDecodeReadsBack(t *testing.T) {
	if URLCaps.Bytes != decode.MaxDocumentBytes || URLCaps.Files != 1 {
		t.Errorf("URLCaps %+v, decode.MaxDocumentBytes %d", URLCaps, decode.MaxDocumentBytes)
	}
}

func TestTheFetchStepsArgvCarriesNoToken(t *testing.T) {
	s := Source{ID: "repo", Kind: manifest.IntakeRepository, URL: "https://github.com/o/r", Ref: "main", Token: "secret-token"}
	argv := strings.Join(repository.Argv(s, LocalCaps, "http://source-proxy:3128"), " ")
	if strings.Contains(argv, "secret-token") || !strings.Contains(argv, "--token-stdin") || !strings.Contains(argv, "--ref main") || !strings.Contains(argv, "--max-files 20000") {
		t.Errorf("argv %s", argv)
	}
	s.Token = ""
	if strings.Contains(strings.Join(repository.Argv(s, LocalCaps, "p"), " "), "--token-stdin") {
		t.Error("an anonymous fetch reads stdin")
	}
}

func TestTheReportIsHeldToItsShape(t *testing.T) {
	s := Source{Kind: manifest.IntakeRepository, Token: "tok123"}
	good := `{"schema":"source/v1","kind":"repository","commit":"0123456789abcdef0123456789abcdef01234567","files":3,"bytes":10,"skipped":{"link":1},"error":"refused tok123"}`
	rep, err := CheckReport([]byte(good), s, LocalCaps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.Error, "tok123") {
		t.Error("the token survived in the report's words")
	}
	for name, raw := range map[string]string{
		"unknown field":  `{"schema":"source/v1","kind":"repository","files":0,"bytes":0,"x":1}`,
		"wrong schema":   `{"schema":"source/v9","kind":"repository","files":0,"bytes":0}`,
		"over the cap":   `{"schema":"source/v1","kind":"repository","files":20001,"bytes":0}`,
		"bad commit":     `{"schema":"source/v1","kind":"repository","commit":"zz","files":0,"bytes":0}`,
		"unknown reason": `{"schema":"source/v1","kind":"repository","files":0,"bytes":0,"skipped":{"why":1}}`,
		"trailing data":  `{"schema":"source/v1","kind":"repository","files":0,"bytes":0}{}`,
	} {
		if _, err := CheckReport([]byte(raw), s, LocalCaps); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestTheDocumentNamesNoToken(t *testing.T) {
	s := Source{ID: "repo", Kind: manifest.IntakeRepository, URL: "https://github.com/o/r", Ref: "v1", Token: "tok"}
	doc := Document(s, source.Report{Commit: "c0ffee", Files: 4}, "/inputs")
	r := doc["repository"].(map[string]any)
	if r["url"] != s.URL || r["ref"] != "v1" || r["commit"] != "c0ffee" || r["path"] != "/inputs/repo/" || r["files"] != 4 || len(r) != 5 {
		t.Errorf("document %v", doc)
	}
}

func TestChecksCountsOnlyTheSourcesFiles(t *testing.T) {
	files := []runnerdecode.File{
		{Path: "repo/a.go", Check: runnerdecode.CheckUnchecked, Unchecked: runnerdecode.UncheckedNotRisky},
		{Path: "repo/b.exe", Check: runnerdecode.CheckClean},
		{Path: "repo/c.dll", Check: runnerdecode.CheckFound},
		{Path: "repository-notes.txt", Check: runnerdecode.CheckClean},
	}
	got := Checks("repo", files)
	if !strings.Contains(got, "2 executable or binary risky files went to the malware check (1 clean, 1 found)") || !strings.Contains(got, "1 other was not checked") {
		t.Errorf("Checks = %q", got)
	}
}

type fakeAsker struct {
	answer string
	save   bool
	asked  []string
}

func (f *fakeAsker) AskSecret(p string) (string, error) {
	f.asked = append(f.asked, p)
	return f.answer, nil
}
func (f *fakeAsker) Confirm(p string, _ bool) (bool, error) {
	f.asked = append(f.asked, p)
	return f.save, nil
}

func withKeychain(t *testing.T, saved map[string]string) map[string]string {
	t.Helper()
	t.Setenv("AGENTRY_NO_KEYRING", "")
	oldGet, oldSet := keyringGet, keyringSet
	t.Cleanup(func() { keyringGet, keyringSet = oldGet, oldSet })
	keyringGet = func(_, account string) (string, error) {
		if v, ok := saved[account]; ok {
			return v, nil
		}
		return "", keyring.ErrNotFound
	}
	keyringSet = func(_, account, v string) error { saved[account] = v; return nil }
	return saved
}

func TestATokenComesFromTheFlagThenTheKeychainThenThePrompt(t *testing.T) {
	saved := withKeychain(t, map[string]string{KeyAccount("gitlab.com"): "saved-gl"})
	srcs := []*Source{
		{ID: "a", Kind: manifest.IntakeRepository, Host: "github.com"},
		{ID: "b", Kind: manifest.IntakeRepository, Host: "gitlab.com"},
		{ID: "c", Kind: manifest.IntakeRepository, Host: "codeberg.org"},
	}
	ask := &fakeAsker{answer: "typed", save: true}
	env := map[string]string{"GH_TOKEN": "from-env"}
	if err := ResolveTokens(srcs, map[string]string{"a": "GH_TOKEN"}, func(k string) string { return env[k] }, ask, nil); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ token, from string }{{"from-env", TokenFromFlag}, {"saved-gl", TokenFromKeychain}, {"typed", TokenFromPrompt}} {
		if srcs[i].Token != want.token || srcs[i].TokenFrom != want.from {
			t.Errorf("%s: %q from %q, want %q from %q", srcs[i].ID, srcs[i].Token, srcs[i].TokenFrom, want.token, want.from)
		}
	}
	if saved[KeyAccount("codeberg.org")] != "typed" {
		t.Error("a typed token the person chose to save was not saved")
	}
	if len(ask.asked) != 2 {
		t.Errorf("asked %v, want one token and one save", ask.asked)
	}
	// With no terminal and nothing saved, the fetch is anonymous.
	anon := []*Source{{ID: "d", Kind: manifest.IntakeRepository, Host: "example.org"}}
	if err := ResolveTokens(anon, nil, func(string) string { return "" }, nil, nil); err != nil || anon[0].Token != "" || anon[0].TokenFrom != "" {
		t.Errorf("anonymous: %v %+v", err, anon[0])
	}
}

func TestATokenFlagNamingAnEmptyVariableFails(t *testing.T) {
	withKeychain(t, map[string]string{})
	srcs := []*Source{{ID: "a", Host: "github.com"}}
	err := ResolveTokens(srcs, map[string]string{"a": "NOPE"}, func(string) string { return "" }, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "NOPE is unset or empty") {
		t.Errorf("err %v", err)
	}
	if err := ResolveTokens(srcs, map[string]string{"zz": "X"}, func(string) string { return "v" }, nil, nil); err == nil {
		t.Error("a flag for no source passed")
	}
	for _, bad := range []string{"a", "a=", "=X", "a=ghp_value with space", "a=X=Y"} {
		if _, err := ParseTokenFlags([]string{bad}); err == nil {
			t.Errorf("ParseTokenFlags(%q) passed", bad)
		}
	}
}

func TestAKeychainThatCannotBeReadIsSaid(t *testing.T) {
	withKeychain(t, map[string]string{})
	keyringGet = func(string, string) (string, error) { return "", errors.New("no secret service") }
	var warned []string
	srcs := []*Source{{ID: "a", Host: "github.com"}}
	if err := ResolveTokens(srcs, nil, func(string) string { return "" }, nil, func(m string) { warned = append(warned, m) }); err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "no secret service") {
		t.Errorf("warned %v", warned)
	}
}

// TestASignedOutFireReadsItsSealedSourceKey (agentry-cli#854): a fire of a
// schedule that runs while its person is signed out has no keychain
// (AGENTRY_NO_KEYRING) and its saved keys sealed beside it. A source's
// saved key is read from that credential and the receipt says so; a host
// with no key sealed is fetched anonymously, and the keychain is never
// asked.
func TestASignedOutFireReadsItsSealedSourceKey(t *testing.T) {
	withKeychain(t, map[string]string{KeyAccount("gitlab.com"): "keychain-only"})
	asked := false
	keyringGet = func(string, string) (string, error) { asked = true; return "", keyring.ErrNotFound }
	plain, err := modelkey.SealPlaintext(map[string]string{KeyAccount("github.com"): "sealed-tok\n"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modelkey.CredentialName), plain, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(modelkey.EnvCredentialsDirectory, dir)
	t.Setenv("AGENTRY_NO_KEYRING", "1")

	srcs := []*Source{{ID: "code", Host: "github.com"}, {ID: "other", Host: "gitlab.com"}}
	if err := ResolveTokens(srcs, nil, func(string) string { return "" }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if srcs[0].Token != "sealed-tok" || srcs[0].TokenFrom != TokenFromSealed {
		t.Errorf("the sealed host: %q from %q", srcs[0].Token, srcs[0].TokenFrom)
	}
	if srcs[1].Token != "" || srcs[1].TokenFrom != "" {
		t.Errorf("a host with nothing sealed: %q from %q", srcs[1].Token, srcs[1].TokenFrom)
	}
	if asked {
		t.Error("the keychain was asked while sealed keys were present")
	}
	if w := tokenWords(*srcs[0]); !strings.Contains(w, "saved for github.com") || !strings.Contains(w, "sealed beside this schedule") {
		t.Errorf("receipt words %q", w)
	}
}
