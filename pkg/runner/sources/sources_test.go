package sources

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry/pkg/manifest"

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
	if _, ok := Lookup(manifest.IntakeURL); ok {
		t.Error("the url kind is fetched before agentry-cli#834")
	}
	if err := Unfetched("doc", manifest.IntakeURL); err == nil || !strings.Contains(err.Error(), "agentry-cli#834") {
		t.Errorf("Unfetched: %v", err)
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
	s := Source{Token: "tok123"}
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
