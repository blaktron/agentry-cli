package persistent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
)

// holdCreditsPaid holds a document of acme's on registry that pays for each
// of ids (review.source, webSearch.source) with its credits.
func holdCreditsPaid(t *testing.T, registry string, ids ...string) {
	t.Helper()
	raw, err := json.Marshal(managedsettings.Source{Source: managedsettings.SourceAgentryCredits})
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]managedsettings.Setting{}
	for _, id := range ids {
		settings[id] = managedsettings.Setting{Value: raw, Locked: true}
	}
	doc := managedsettings.Document{Schema: managedsettings.Schema, Registry: registry, Org: "acme", Version: 3,
		IssuedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), Settings: settings}
	prev := currentManaged
	t.Cleanup(func() { currentManaged = prev })
	currentManaged = func() managed.Effective {
		return managed.Effective{Documents: []managed.State{{Registry: registry, Document: doc}}}
	}
}

// TestTheReachLinesNameTheRegistryWhereCreditsPay: where an organization's
// credits pay for AI Review, the web tools' and the review grant's
// sentences say the pages and the package go through the registry, paid by
// the organization, in place of the person's own key; web search paid that
// way is said too.
func TestTheReachLinesNameTheRegistryWhereCreditsPay(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	both := installPublished(t, home, "acme", "both", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    review: true\n    web: true\n"})
	alone := installPublished(t, home, "acme", "rev", "1.0.0", publishedRelease{extra: "capabilities:\n  persistent:\n    review: true\n"})
	line := func(a Agent, id string) string {
		e, ok := BuildRoster(a, Config{Enabled: true}, nil).Entry(id)
		if !ok {
			t.Fatalf("%s has no %s entry", a.Name, id)
		}
		return e.Line
	}
	if got := line(both, EntryWeb); got != webLine {
		t.Fatalf("with no managed settings, the web line changed: %q", got)
	}

	holdCreditsPaid(t, "https://registry.example", managedsettings.ReviewSource)
	paid := "through registry.example, paid with acme's credits"
	if got := line(both, EntryWeb); !strings.Contains(got, "read by AI Review "+paid) || strings.Contains(got, "your own") {
		t.Errorf("web, review paid: %q", got)
	}
	if got := line(alone, EntryReview); !strings.Contains(got, "to AI Review "+paid) || !strings.Contains(got, "reaches registry.example alone") || strings.Contains(got, "TypeSafe") {
		t.Errorf("review alone, paid: %q", got)
	}
	if got := line(both, EntryReview); !strings.Contains(got, "asks for registry.example alone") || !strings.Contains(got, "the operation's token") || strings.Contains(got, "your key") {
		t.Errorf("review beside web, paid: %q", got)
	}

	holdCreditsPaid(t, "https://registry.example", managedsettings.WebSearchSource)
	if got := line(both, EntryWeb); !strings.HasPrefix(got, "Searches the web through registry.example, paid with acme's credits,") || !strings.Contains(got, "your own TypeSafe key") {
		t.Errorf("web, search alone paid: %q", got)
	}
}

// operations is a registry that opens and ends AI Review operations.
type operations struct {
	mu       sync.Mutex
	opened   []string
	ended    []string
	srv      *httptest.Server
	sequence int
}

func newOperations(t *testing.T) *operations {
	t.Helper()
	o := &operations{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/x/review/operations":
			var req struct{ Purpose string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			o.sequence++
			o.opened = append(o.opened, req.Purpose)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "agyr_seg", "credits": 1,
				"operation": map[string]any{"id": "op-" + string(rune('0'+o.sequence)), "purpose": req.Purpose, "expiresAt": time.Now().Add(2 * time.Hour)}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/x/review/operations/"):
			o.ended = append(o.ended, strings.TrimPrefix(r.URL.Path, "/api/v1/x/review/operations/"))
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *operations) pay(ctx context.Context, registry, purpose string) (*managed.Operation, error) {
	return managed.OpenOperation(ctx, o.srv.Client(), o.srv.URL, "session-token", purpose)
}

// TestASegmentOpensOneOperationAndEndsIt: a segment's web tools and review
// grant share one operation on the registry, opened for a run, and the
// segment's end ends it once; one that pays for web search alone opens a
// search operation; with no pay nothing is opened.
func TestASegmentOpensOneOperationAndEndsIt(t *testing.T) {
	o := newOperations(t)
	pay, end := SegmentPay(o.pay)
	a, err := pay(context.Background(), o.srv.URL, managed.PurposeRun)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pay(context.Background(), o.srv.URL, managed.PurposeReview)
	if err != nil || b != a {
		t.Fatalf("the review grant opened another operation: %v", err)
	}
	end()
	end()
	if len(o.opened) != 1 || o.opened[0] != managed.PurposeRun || len(o.ended) != 1 || o.ended[0] != a.ID {
		t.Fatalf("opened %v, ended %v", o.opened, o.ended)
	}

	pay, end = SegmentPay(o.pay)
	if _, err := pay(context.Background(), o.srv.URL, managed.PurposeSearch); err != nil {
		t.Fatal(err)
	}
	end()
	if len(o.opened) != 2 || o.opened[1] != managed.PurposeSearch || len(o.ended) != 2 {
		t.Fatalf("search alone: opened %v, ended %v", o.opened, o.ended)
	}

	if pay, end := SegmentPay(nil); pay != nil {
		t.Fatal("a nil pay paid")
	} else {
		end()
	}
}
