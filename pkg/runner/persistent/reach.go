package persistent

import (
	"context"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The web tools' and the review grant's roster ids, for an agent that
// declares them.
const (
	EntryWeb    = "web"
	EntryReview = "review"
)

// The web tools' and the review grant's consent sentences.
const (
	webLine = "Searches the web and reads public pages (" + toolsrv.ToolWebSearch + ", " + toolsrv.ToolWebRead + ") from its tools container, which then reaches the public web: " +
		"every search result and page is read by AI Review with your own TypeSafe key before the agent sees it, and one it flags is withheld. " +
		"That container also holds your share, its own files and memory, and the knowledge packs it has, so what it reads there can be sent in an address it opens"
	reviewLine = "Asks AI Review's release rubric about a QuickAgent package in its folders (" + toolsrv.ToolAIReview + "), sending that package's files to TypeSafe with your own key; " +
		"its tools container then reaches TypeSafe's API alone"
)

// WebOpener and ReviewOpener read the keys a segment's web tools and review
// grant run with.
type (
	WebOpener    func(ctx context.Context) (*engine.WebRun, error)
	ReviewOpener func(ctx context.Context) (*engine.ReviewRun, error)
)

// reachEntries are the web tools' and the review grant's entries, for an
// agent that declares them: on, granted by what declares them.
func reachEntries(a Agent) []RosterEntry {
	var out []RosterEntry
	if a.Declared().Declares("web") {
		out = append(out, RosterEntry{ID: EntryWeb, Kind: KindTools, Name: "Web search and pages", GrantedBy: a.grantedBy(), Line: webLine})
	}
	if a.Declared().Declares("review") {
		out = append(out, RosterEntry{ID: EntryReview, Kind: KindTools, Name: "AI Review", GrantedBy: a.grantedBy(), Line: reviewLine})
	}
	return out
}

// LocalWebOpener and LocalReviewOpener read the keys from the runner
// policy as work on this computer does: the person's TypeSafe key, and for
// the web tools the search provider's key when the policy names one.
func LocalWebOpener(pol *policy.Policy) WebOpener {
	return func(context.Context) (*engine.WebRun, error) {
		w, _, err := drafter.LocalWeb(pol, "")
		return w, err
	}
}

func LocalReviewOpener(pol *policy.Policy) ReviewOpener {
	return func(context.Context) (*engine.ReviewRun, error) { return examiner.LocalReview(pol) }
}

// openReach is the segment's web tools and review grant: each opened when
// the agent declares it and its roster has it on.
func openReach(ctx context.Context, a Agent, on func(id string) bool, openers Openers) (*engine.WebRun, *engine.ReviewRun, error) {
	var w *engine.WebRun
	var r *engine.ReviewRun
	var err error
	if a.Declared().Declares("web") && on(EntryWeb) {
		if openers.Web == nil {
			return nil, nil, errors.New("the agent has the web tools on, and this client cannot read the keys they run with")
		}
		if w, err = openers.Web(ctx); err != nil {
			return nil, nil, fmt.Errorf("the web tools read every page through AI Review with your own TypeSafe key: %w (set it, or switch them off with agentry agent grant %s %s off)", err, a.Name, EntryWeb)
		}
	}
	if a.Declared().Declares("review") && on(EntryReview) {
		if openers.Review == nil {
			return nil, nil, errors.New("the agent has AI Review on, and this client cannot read your TypeSafe key")
		}
		if r, err = openers.Review(ctx); err != nil {
			return nil, nil, fmt.Errorf("AI Review runs with your own TypeSafe key: %w (set it, or switch it off with agentry agent grant %s %s off)", err, a.Name, EntryReview)
		}
	}
	return w, r, nil
}
