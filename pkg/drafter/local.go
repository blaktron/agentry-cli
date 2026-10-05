package drafter

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// What a client needs around a Drafter run, shared by `agentry draft` and
// the desktop's Describe it, on this computer (plan builder-rework.md D22,
// D25; agentry-cli#363): for a draft on this computer, the person's own keys
// read as the runner policy says (LocalWeb); for any draft, the post-step
// that turns the Drafter's answer into a draft with AI Review's record
// (Finish). The order is the caller's to keep: Prescan passes before the
// run starts. Each client words its own refusals and renders the draft its
// own way.

// ErrNoJevKey is LocalWeb's answer when the person has no Jev key of their
// own: a draft on this computer does not run without AI Review reading
// everything first (D22), and there it takes the person's key (D25).
var ErrNoJevKey = errors.New("no TypeSafe Jev key of your own was found")

// SearchState says whether a local draft has web search, and why not.
type SearchState string

const (
	// SearchOn: the policy names a provider and its key was found.
	SearchOn SearchState = "on"
	// SearchNoProvider: the policy's webSearch block names no provider.
	SearchNoProvider SearchState = "no-provider"
	// SearchNoKey: a provider is named, and no key for it was found.
	SearchNoKey SearchState = "no-key"
)

// LocalWeb is a local draft's web configuration from the person's own keys
// (D25): their Jev key, which every result is scanned with, and their search
// key when the policy names a provider and the key is found, with which of
// those it is. Without a Jev key it is ErrNoJevKey and nothing runs. The
// registry is the one registry_search reads. The Jev key in the answer is
// exactly the one the draft scans with, so a client that tests a key tests
// that one.
func LocalWeb(pol *policy.Policy, registry string) (*engine.WebRun, SearchState, error) {
	jev, err := JevKey(pol)
	if err != nil {
		return nil, "", err
	}
	if jev == "" {
		return nil, "", ErrNoJevKey
	}
	w := &engine.WebRun{Scan: toolsrv.WebScanJev, JevKey: jev, Registry: registry}
	key, state, err := SearchKey(pol)
	if err != nil {
		return nil, "", err
	}
	if state == SearchOn {
		w.Search, w.SearchKey = pol.WebSearch.Provider, key
	}
	return w, state, nil
}

// JevKey is the person's own TypeSafe key as work on this computer reads
// it: the variable the policy's judge block names, else, for a keyring
// source, the keychain entry provider:typesafe. "" is no key. A draft scans
// with it (LocalWeb), and the Examiner reviews with it (agentry-cli#504).
func JevKey(pol *policy.Policy) (string, error) {
	return personalKey(pol.Judge.KeyEnv(), pol.Judge.APIKeySource, policy.JudgeKeyProvider)
}

// SearchKey is the person's own search key as a local draft reads it, and
// whether search is on: the policy's provider, its key from the variable
// the policy names or the provider's own, else, for a keyring source, the
// keychain entry provider:<provider>. It needs no Jev key, so a client can
// test the search key alone, and it is the one LocalWeb searches with.
func SearchKey(pol *policy.Policy) (string, SearchState, error) {
	if !pol.WebSearch.On() {
		return "", SearchNoProvider, nil
	}
	// A loaded policy is validated; one a client built in memory may not
	// be, and an unknown provider would read the default variable.
	if !slices.Contains(policy.WebSearchProviders(), pol.WebSearch.Provider) {
		return "", "", engine.Classify(engine.ExitPolicy, fmt.Errorf("webSearch.provider %q is not a provider the runner policy admits", pol.WebSearch.Provider))
	}
	key, err := personalKey(pol.WebSearch.KeyEnv(), pol.WebSearch.APIKeySource, pol.WebSearch.Provider)
	if err != nil {
		return "", "", err
	}
	if key == "" {
		return "", SearchNoKey, nil
	}
	return key, SearchOn, nil
}

// personalKey reads one of the person's own keys as the runner policy says
// to: the variable env names, else, with source keyring, the OS keychain
// entry provider:<provider>. "" is no key; an error is a keychain the
// policy names that could not be read (exit 11).
func personalKey(env, source, provider string) (string, error) {
	v, err := modelkey.Personal(env, source, provider)
	if err != nil {
		return "", engine.Classify(engine.ExitPolicy, err)
	}
	return v, nil
}

// Finish is the post-step's first half (plan §7.5): the Drafter's answer in
// env as a draft, named by the Drafter's ref and the build that ran it (the
// model and the run id are the envelope's), its long texts from the reports
// under outDir, AI Review's record from the receipt and the run's ledger,
// its manifest's mechanical slips corrected (Correct), and the notes
// followed by what was corrected and a list of every text AI Review
// withheld (its address, tool and reason, never the text). A draft whose reports were
// refused, or that has no instructions, carries why in ManifestError; a
// caller renders and checks only a draft whose ManifestError is empty. An
// answer that is not a draft is exit 17.
func Finish(env *receipt.Envelope, outDir string, desc webscan.Result, ref, build string) (*Draft, error) {
	d, err := Parse(env.Result)
	if err != nil {
		return nil, engine.Classify(engine.ExitOutputSchema, err)
	}
	d.Drafter = made(env, ref, build)
	d.AIReview = reviewOf(env.Web, desc, filepath.Join(outDir, "receipts", engine.WebLedgerFile))
	// The reports first: a report is read only into an empty field, so the
	// withheld list, added after, never takes the notes' place.
	switch err := FromReports(d, filepath.Join(outDir, "reports")); {
	case err != nil:
		d.ManifestError = err.Error()
	case strings.TrimSpace(d.Instructions) == "":
		d.ManifestError = "the Drafter wrote no instructions (AGENT.md)"
	}
	d.ownNotes = d.Notes
	d.Manifest, d.corrections = Correct(d.Manifest)
	d.composeNotes("")
	return d, nil
}

// reviewOf is the draft's AI Review record: the description's verdict, the
// receipt's counts, and each text the model was not given, read from the
// run's ledger rather than from what the model said.
func reviewOf(w *receipt.Web, desc webscan.Result, ledger string) *AIReview {
	a := &AIReview{Description: desc.Verdict, WithheldSources: []WithheldSource{}}
	if w != nil {
		a.Web = *w
	}
	if a.Model == "" {
		a.Model, a.QuestionSet = desc.Model, desc.QuestionSet
	}
	entries, _, _ := engine.ReadWebLedger(ledger)
	for _, e := range entries {
		if e.Refused != "" || e.Search || e.Passed() {
			continue
		}
		a.WithheldSources = append(a.WithheldSources, WithheldSource{Tool: e.Tool, URL: e.URL, Reason: e.Reason()})
	}
	return a
}
