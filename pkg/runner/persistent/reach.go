package persistent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/managed"
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

// operationEndWait bounds ending one operation when a segment ends.
const operationEndWait = 15 * time.Second

// The web tools' and the review grant's consent sentences.
const (
	webLine = "Searches the web and reads public pages (" + toolsrv.ToolWebSearch + ", " + toolsrv.ToolWebRead + ") from its tools container, which then reaches the public web: " +
		"every search result and page is read by AI Review with your own TypeSafe key before the agent sees it, and one it flags is withheld. " +
		"That container also holds your share, its own files and memory, and the knowledge packs it has, so what it reads there can be sent in an address it opens"
	reviewLine = "Asks AI Review's release rubric about a QuickAgent package in its folders (" + toolsrv.ToolAIReview + "), sending that package's files to TypeSafe with your own key; " +
		"its tools container then reaches TypeSafe's API alone"
	// reviewBesideWebLine is the review grant's sentence beside the web
	// tools, whose container it shares: that container reaches the public
	// web, and ai_review's client still asks for TypeSafe's API alone.
	reviewBesideWebLine = "Asks AI Review's release rubric about a QuickAgent package in its folders (" + toolsrv.ToolAIReview + "), sending that package's files to TypeSafe with your own key; " +
		"it asks for TypeSafe's API alone, from the tools container the web tools also use, which reaches the public web, and your key is in that container too"
)

// WebOpener and ReviewOpener read the keys a segment's web tools and review
// grant run with.
type (
	WebOpener    func(ctx context.Context) (*engine.WebRun, error)
	ReviewOpener func(ctx context.Context) (*engine.ReviewRun, error)
)

// currentManaged is the managed settings held on this computer; a test
// replaces it.
var currentManaged = managed.Current

// paidBy names the registry and the organization whose credits pay for the
// service setting id (review.source, webSearch.source) on this computer, or
// "" when the person's own keys do.
func paidBy(held managed.Effective, id string) (registry, org string) {
	ks := held.KeySourceOf(id)
	if !ks.Decided || ks.Source != managedsettings.SourceAgentryCredits {
		return "", ""
	}
	return hostOf(ks.Registry), ks.Org
}

// hostOf is a registry's address without its scheme, as a sentence names it.
func hostOf(registry string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://"), "/")
}

// reachLines are the web tools' and the review grant's sentences as the
// held managed settings pay for them: where an organization's credits pay
// for AI Review (or web search), the sentence names the registry it goes
// through and the organization that pays, in place of the person's key.
func reachLines(a Agent, held managed.Effective) (web, review string) {
	web, review = webLine, reviewLine
	if a.Declared().Declares("web") {
		review = reviewBesideWebLine
	}
	reg, org := paidBy(held, managedsettings.ReviewSource)
	if reg != "" {
		paid := "through " + reg + ", paid with " + org + "'s credits"
		web = strings.Replace(web, "with your own TypeSafe key", paid+",", 1)
		review = strings.Replace(review, "sending that package's files to TypeSafe with your own key", "sending that package's files to AI Review "+paid, 1)
		review = strings.Replace(review, "reaches TypeSafe's API alone", "reaches "+reg+" alone", 1)
		review = strings.Replace(review, "asks for TypeSafe's API alone", "asks for "+reg+" alone", 1)
		review = strings.Replace(review, ", and your key is in that container too", ", and the operation's token is in that container too", 1)
	}
	if sreg, sorg := paidBy(held, managedsettings.WebSearchSource); sreg != "" {
		web = strings.Replace(web, "Searches the web", "Searches the web through "+sreg+", paid with "+sorg+"'s credits,", 1)
	}
	return web, review
}

// reachEntries are the web tools' and the review grant's entries, for an
// agent that declares them: on, granted by what declares them.
func reachEntries(a Agent) []RosterEntry {
	web, review := reachLines(a, currentManaged())
	var out []RosterEntry
	if a.Declared().Declares("web") {
		out = append(out, RosterEntry{ID: EntryWeb, Kind: KindTools, Name: "Web search and pages", GrantedBy: a.grantedBy(), Line: web})
	}
	if a.Declared().Declares("review") {
		out = append(out, RosterEntry{ID: EntryReview, Kind: KindTools, Name: "AI Review", GrantedBy: a.grantedBy(), Line: review})
	}
	return out
}

// LocalWebOpener and LocalReviewOpener read the keys from the runner
// policy as work on this computer does: the person's TypeSafe key, and for
// the web tools the search provider's key when the policy names one. Where
// an organization's credits pay for AI Review or web search they refuse
// with the organization's sentence; LocalWebOpenerPaid and
// LocalReviewOpenerPaid pay instead.
func LocalWebOpener(pol *policy.Policy) WebOpener { return LocalWebOpenerPaid(pol, nil) }

func LocalReviewOpener(pol *policy.Policy) ReviewOpener { return LocalReviewOpenerPaid(pol, nil) }

// LocalWebOpenerPaid is LocalWebOpener for a client that can pay with an
// organization's credits: where they pay for AI Review or web search, the
// segment's web tools work through an operation pay opens on the registry.
func LocalWebOpenerPaid(pol *policy.Policy, pay drafter.Pay) WebOpener {
	return func(ctx context.Context) (*engine.WebRun, error) {
		w, _, err := drafter.LocalWebPaid(ctx, pol, "", managed.PurposeRun, pay)
		return w, err
	}
}

// LocalReviewOpenerPaid is LocalReviewOpener for a client that can pay
// with an organization's credits, as LocalWebOpenerPaid is.
func LocalReviewOpenerPaid(pol *policy.Policy, pay drafter.Pay) ReviewOpener {
	return func(ctx context.Context) (*engine.ReviewRun, error) { return examiner.LocalReviewPaid(ctx, pol, pay) }
}

// SegmentPay is pay for one segment of a persistent agent: the first operation it opens on a
// registry serves the segment's web tools and its review grant alike,
// opened for a run (managed.PurposeRun) unless it pays for web search
// alone. end ends every operation it opened, once; a nil pay is nil.
func SegmentPay(pay drafter.Pay) (drafter.Pay, func()) {
	if pay == nil {
		return nil, func() {}
	}
	var mu sync.Mutex
	ops := map[string]*managed.Operation{}
	var opened []*managed.Operation
	once := sync.Once{}
	return func(ctx context.Context, registry, purpose string) (*managed.Operation, error) {
			mu.Lock()
			defer mu.Unlock()
			if op, ok := ops[registry]; ok {
				return op, nil
			}
			if purpose != managed.PurposeSearch {
				purpose = managed.PurposeRun
			}
			op, err := pay(ctx, registry, purpose)
			if err != nil {
				return nil, err
			}
			ops[registry] = op
			opened = append(opened, op)
			return op, nil
		}, func() {
			once.Do(func() {
				mu.Lock()
				defer mu.Unlock()
				for _, op := range opened {
					ctx, cancel := context.WithTimeout(context.Background(), operationEndWait)
					_ = op.End(ctx)
					cancel()
				}
			})
		}
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
			if errors.Is(err, drafter.ErrNoJevKey) {
				return nil, nil, fmt.Errorf("the web tools read every page through AI Review with your own TypeSafe key: %w (set it, or switch them off with agentry agent grant %s %s off)", err, a.Name, EntryWeb)
			}
			return nil, nil, fmt.Errorf("the web tools read every page through AI Review: %w (switch them off with agentry agent grant %s %s off)", err, a.Name, EntryWeb)
		}
	}
	if a.Declared().Declares("review") && on(EntryReview) {
		if openers.Review == nil {
			return nil, nil, errors.New("the agent has AI Review on, and this client cannot read your TypeSafe key")
		}
		if r, err = openers.Review(ctx); err != nil {
			if errors.Is(err, drafter.ErrNoJevKey) {
				return nil, nil, fmt.Errorf("AI Review runs with your own TypeSafe key: %w (set it, or switch it off with agentry agent grant %s %s off)", err, a.Name, EntryReview)
			}
			return nil, nil, fmt.Errorf("AI Review: %w (switch it off with agentry agent grant %s %s off)", err, a.Name, EntryReview)
		}
	}
	return w, r, nil
}
