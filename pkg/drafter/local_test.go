package drafter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// localKeys gives the test an in-memory keychain and no key in the
// environment, so it can never read or write a person's.
func localKeys(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Setenv(modelkey.EnvNoKeyring, "")
	t.Setenv(policy.DefaultJudgeKeyEnv, "")
	t.Setenv(policy.DefaultWebSearchKeyEnv, "")
	t.Setenv(policy.BraveSearchKeyEnv, "")
	t.Setenv(policy.TavilySearchKeyEnv, "")
	t.Setenv(policy.ExaSearchKeyEnv, "")
	t.Setenv(policy.PerplexitySearchKeyEnv, "")
}

// SearchKey refuses a provider the policy does not admit, rather than read
// the default variable for it: a policy a client built in memory is not
// validated as a loaded one is.
func TestSearchKeyRefusesAProviderThePolicyDoesNotAdmit(t *testing.T) {
	localKeys(t)
	t.Setenv(policy.DefaultWebSearchKeyEnv, "ydc-from-the-variable")
	pol := policy.Default()
	pol.WebSearch = policy.WebSearchKey{Provider: "bing"}
	if k, s, err := SearchKey(pol); err == nil || k != "" || s != "" {
		t.Fatalf("an unknown provider was read: %q %s %v", k, s, err)
	}
}

// TestLocalWebReadsThePersonsKeysAsThePolicySays: the variable wins; with
// apiKeySource keyring and the variable unset, the keychain entry
// provider:typesafe (the one the desktop writes); a keychain entry is never
// read for an env source; no Jev key is ErrNoJevKey. Search is on only with
// a provider named and its own key found: each provider's own variable or
// keychain entry, never another's (plan builder-rework.md D22, D25;
// agentry-cli#363, agentry-cli#369, agentry-cli#513).
func TestLocalWebReadsThePersonsKeysAsThePolicySays(t *testing.T) {
	localKeys(t)
	if err := modelkey.Store(policy.JudgeKeyProvider, "jev-from-the-keychain"); err != nil {
		t.Fatal(err)
	}
	if err := modelkey.Store("youcom", "ydc-from-the-keychain"); err != nil {
		t.Fatal(err)
	}
	if err := modelkey.Store("brave", "brave-from-the-keychain"); err != nil {
		t.Fatal(err)
	}
	if err := modelkey.Store("perplexity", "pplx-from-the-keychain"); err != nil {
		t.Fatal(err)
	}
	const reg = "https://registry.example"
	keyringJudge := policy.JudgeKey{APIKeySource: modelkey.SourceKeyring}
	keyringSearch := policy.WebSearchKey{Provider: "youcom", APIKeySource: modelkey.SourceKeyring}

	cases := []struct {
		name       string
		judge      policy.JudgeKey
		search     policy.WebSearchKey
		env        map[string]string
		wantErr    error
		wantJev    string
		wantSearch string
		wantSKey   string
		wantState  SearchState
	}{
		{name: "the keychain, for a keyring source", judge: keyringJudge, wantJev: "jev-from-the-keychain", wantState: SearchNoProvider},
		{name: "the variable wins over the keychain", judge: keyringJudge,
			env: map[string]string{policy.DefaultJudgeKeyEnv: "jev-from-the-variable"}, wantJev: "jev-from-the-variable", wantState: SearchNoProvider},
		{name: "a variable the policy names", judge: policy.JudgeKey{APIKeyEnv: "MY_JEV"},
			env: map[string]string{"MY_JEV": "jev-named"}, wantJev: "jev-named", wantState: SearchNoProvider},
		{name: "no source never reads the keychain", judge: policy.JudgeKey{}, wantErr: ErrNoJevKey},
		{name: "a blank variable is no key", judge: policy.JudgeKey{},
			env: map[string]string{policy.DefaultJudgeKeyEnv: "  "}, wantErr: ErrNoJevKey},
		{name: "search off with a provider and no key", judge: keyringJudge, search: policy.WebSearchKey{Provider: "youcom"},
			wantJev: "jev-from-the-keychain", wantState: SearchNoKey},
		{name: "search from the keychain", judge: keyringJudge, search: keyringSearch,
			wantJev: "jev-from-the-keychain", wantSearch: "youcom", wantSKey: "ydc-from-the-keychain", wantState: SearchOn},
		{name: "search from the variable", judge: keyringJudge, search: policy.WebSearchKey{Provider: "youcom"},
			env:     map[string]string{policy.DefaultWebSearchKeyEnv: "ydc-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantSearch: "youcom", wantSKey: "ydc-from-the-variable", wantState: SearchOn},
		{name: "Brave from its own variable", judge: keyringJudge, search: policy.WebSearchKey{Provider: "brave"},
			env:     map[string]string{policy.BraveSearchKeyEnv: "brave-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantSearch: "brave", wantSKey: "brave-from-the-variable", wantState: SearchOn},
		{name: "Brave from the keychain entry provider:brave", judge: keyringJudge,
			search:  policy.WebSearchKey{Provider: "brave", APIKeySource: modelkey.SourceKeyring},
			wantJev: "jev-from-the-keychain", wantSearch: "brave", wantSKey: "brave-from-the-keychain", wantState: SearchOn},
		{name: "a You.com key never serves Brave", judge: keyringJudge, search: policy.WebSearchKey{Provider: "brave"},
			env:     map[string]string{policy.DefaultWebSearchKeyEnv: "ydc-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantState: SearchNoKey},
		{name: "no Jev key means no search either", search: keyringSearch, wantErr: ErrNoJevKey},
		{name: "Tavily from its own variable", judge: keyringJudge, search: policy.WebSearchKey{Provider: "tavily"},
			env:     map[string]string{policy.TavilySearchKeyEnv: "tvly-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantSearch: "tavily", wantSKey: "tvly-from-the-variable", wantState: SearchOn},
		{name: "Exa from its own variable", judge: keyringJudge, search: policy.WebSearchKey{Provider: "exa"},
			env:     map[string]string{policy.ExaSearchKeyEnv: "exa-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantSearch: "exa", wantSKey: "exa-from-the-variable", wantState: SearchOn},
		{name: "Perplexity from the keychain entry provider:perplexity", judge: keyringJudge,
			search:  policy.WebSearchKey{Provider: "perplexity", APIKeySource: modelkey.SourceKeyring},
			wantJev: "jev-from-the-keychain", wantSearch: "perplexity", wantSKey: "pplx-from-the-keychain", wantState: SearchOn},
		{name: "a You.com key never serves Exa", judge: keyringJudge, search: policy.WebSearchKey{Provider: "exa"},
			env:     map[string]string{policy.DefaultWebSearchKeyEnv: "ydc-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantState: SearchNoKey},
		{name: "a Brave key never serves Tavily, by variable or keychain", judge: keyringJudge,
			search:  policy.WebSearchKey{Provider: "tavily", APIKeySource: modelkey.SourceKeyring},
			env:     map[string]string{policy.BraveSearchKeyEnv: "brave-from-the-variable"},
			wantJev: "jev-from-the-keychain", wantState: SearchNoKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			pol := policy.Default()
			pol.Judge, pol.WebSearch = c.judge, c.search
			w, state, err := LocalWeb(pol, reg)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || w != nil || state != "" {
					t.Fatalf("got %+v, %v; want %v", w, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if w.Scan != toolsrv.WebScanJev || w.JevKey != c.wantJev || w.Search != c.wantSearch || w.SearchKey != c.wantSKey ||
				w.Registry != reg || w.JobToken != "" || state != c.wantState {
				t.Fatalf("got %+v, %q", w, state)
			}
			// SearchKey is LocalWeb's search half, read alone.
			if k, s, serr := SearchKey(pol); serr != nil || s != c.wantState || k != c.wantSKey {
				t.Fatalf("SearchKey: %q %s %v, want %q %s", k, s, serr, c.wantSKey, c.wantState)
			}
		})
	}
}

// TestAKeychainThatCannotBeReadIsARefusal: a keyring source whose keychain
// cannot be asked, or whose presence marker outlived the key, is exit 11,
// never a quiet "no key"; an env source never asks the keychain at all, so
// a broken one does not stop it.
func TestAKeychainThatCannotBeReadIsARefusal(t *testing.T) {
	localKeys(t)
	refused := func(t *testing.T, err error) {
		t.Helper()
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || errors.Is(err, ErrNoJevKey) {
			t.Fatalf("got %v", err)
		}
	}
	pol := policy.Default()

	// A marker with no key behind it (a key removed outside Agentry).
	if err := keyring.Set(modelkey.Service, "stored:"+modelkey.Account(policy.JudgeKeyProvider), "stored"); err != nil {
		t.Fatal(err)
	}
	pol.Judge = policy.JudgeKey{APIKeySource: modelkey.SourceKeyring}
	_, _, err := LocalWeb(pol, "https://registry.example")
	refused(t, err)

	keyring.MockInitWithError(errors.New("the Secret Service is locked"))
	t.Cleanup(keyring.MockInit)
	_, _, err = LocalWeb(pol, "https://registry.example")
	refused(t, err)

	pol.Judge = policy.JudgeKey{APIKeySource: modelkey.SourceEnv}
	t.Setenv(policy.DefaultJudgeKeyEnv, "jev-from-the-variable")
	w, _, err := LocalWeb(pol, "https://registry.example")
	if err != nil || w.JevKey != "jev-from-the-variable" {
		t.Fatalf("an env source asked the keychain: %+v %v", w, err)
	}
	t.Setenv(policy.DefaultJudgeKeyEnv, "")
	if _, _, err := LocalWeb(pol, "https://registry.example"); !errors.Is(err, ErrNoJevKey) {
		t.Fatalf("an env source with no variable: %v", err)
	}
}

// TestFinish: the answer as a draft, named by what made it, with AI Review's
// record from the receipt and the ledger (never from what the model said),
// and the notes the Drafter wrote as its report followed by the withheld
// list; an answer that is not a draft is exit 17 (plan §7.5).
func TestFinish(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := `{"tool":"web_read","source":"page","url":"https://ok.example/","verdict":"pass"}
{"tool":"web_read","source":"page","url":"https://bad.example/","verdict":"withheld","score":3,"flagged":["injection"]}
{"tool":"web_read","url":"http://10.0.0.1/","refused":"not public"}
`
	for name, body := range map[string]string{
		filepath.Join("receipts", engine.WebLedgerFile): ledger,
		filepath.Join("reports", "AGENT.md"):            "# Meeting actions\n\nRead the notes.\n",
		filepath.Join("reports", "notes.md"):            "Pure text.\n",
	} {
		if err := os.WriteFile(filepath.Join(out, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The answer carries a review of its own that lists nothing withheld;
	// the record ignores it.
	answer := `{"manifest": {"schemaVersion": "2", "name": "meeting-actions", "summary": "s", "modes": ["autonomous"]},
	  "aiReview": {"description": "pass", "withheldSources": []}, "matches": [], "sources": [], "openQuestions": []}`
	env := &receipt.Envelope{RunID: "run-7", Result: json.RawMessage(answer), Harness: receipt.Harness{Model: "openai/qwen"},
		Web: &receipt.Web{Scan: toolsrv.WebScanJev, Read: 2, Passed: 1, Withheld: 1, Model: "jev-1.13.0", QuestionSet: "sha256:q"}}

	d, err := Finish(env, out, webscan.Result{Verdict: webscan.Pass}, "agentry/drafter@1.1.1", "agentry 0.6.4")
	if err != nil {
		t.Fatal(err)
	}
	if *d.Drafter != (Made{Ref: "agentry/drafter@1.1.1", Build: "agentry 0.6.4", Model: "openai/qwen", RunID: "run-7"}) {
		t.Fatalf("drafter %+v", d.Drafter)
	}
	a := d.AIReview
	if a.Description != webscan.Pass || a.Read != 2 || a.Withheld != 1 || len(a.WithheldSources) != 1 ||
		a.WithheldSources[0] != (WithheldSource{Tool: "web_read", URL: "https://bad.example/", Reason: "AI Review flagged it (injection)"}) {
		t.Fatalf("aiReview %+v", a)
	}
	const wantNotes = "Pure text.\n\n## Withheld by AI Review\n\nThe Drafter was given the address and the reason for each of these, never the text:\n\n" +
		"- https://bad.example/ (web_read): AI Review flagged it (injection)\n"
	if d.Instructions != "# Meeting actions\n\nRead the notes.\n" || d.Notes != wantNotes || d.ManifestError != "" {
		t.Fatalf("draft %+v", d)
	}

	// No instructions anywhere: kept, with why.
	if err := os.Remove(filepath.Join(out, "reports", "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	d, err = Finish(env, out, webscan.Result{Verdict: webscan.Pass}, "", "")
	if err != nil || d.ManifestError != "the Drafter wrote no instructions (AGENT.md)" {
		t.Fatalf("%v %+v", err, d)
	}

	env.Result = json.RawMessage(`{"sources": []}`)
	_, err = Finish(env, out, webscan.Result{Verdict: webscan.Pass}, "", "")
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitOutputSchema {
		t.Fatalf("not a draft: %v", err)
	}
}
