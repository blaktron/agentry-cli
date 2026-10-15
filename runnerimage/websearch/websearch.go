// Package websearch is web search behind one small interface. The registry
// brokers the Drafter's `web_search` through the deployment's provider
// with the platform's key, so no sandbox host holds one; a person's local
// draft uses the same clients with their own key. Every result is scanned
// as model input before a model reads it (pkg/judge), which is the
// caller's job, not this package's: here a result is only fetched and
// bounded.
package websearch

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Result is one web search result: what a model is shown of it.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Provider searches the web.
type Provider interface {
	// Name is the provider's short id, as configuration names it.
	Name() string
	// Search asks for at most count results for query. An error is the
	// provider's failure; its text never carries the key.
	Search(ctx context.Context, query string, count int) ([]Result, error)
}

// The bounds every result and query is held to.
const (
	// MaxQuery is the longest query, in characters.
	MaxQuery = 400
	// MaxCount is the most results one search returns; DefaultCount is
	// what a search asks for when the caller names none.
	MaxCount     = 10
	DefaultCount = 5
	// maxTitle and maxSnippet bound what a result carries, in characters;
	// maxURL its address, in bytes.
	maxTitle   = 300
	maxSnippet = 1000
	maxURL     = 2048
)

// ErrBadQuery is a query that is empty, too long, or not text.
var ErrBadQuery = errors.New("websearch: the query must be 1 to 400 characters of text")

// CleanQuery trims a query and checks it: 1 to MaxQuery characters of
// valid UTF-8, with no control characters.
func CleanQuery(q string) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" || !utf8.ValidString(q) || utf8.RuneCountInString(q) > MaxQuery {
		return "", ErrBadQuery
	}
	for _, r := range q {
		if unicode.IsControl(r) {
			return "", ErrBadQuery
		}
	}
	return q, nil
}

// ClampCount holds a requested result count to 1..MaxCount, with 0 or less
// meaning DefaultCount.
func ClampCount(n int) int {
	switch {
	case n <= 0:
		return DefaultCount
	case n > MaxCount:
		return MaxCount
	}
	return n
}

// Clean bounds what a provider returned: an absolute http or https address
// with a host and no user information, at most maxURL bytes; a title and a
// snippet of valid UTF-8 with white space folded, cut to their bounds; no
// address twice; at most count results. A result without a usable address
// is dropped, since nothing could name it.
func Clean(rs []Result, count int) []Result {
	count = ClampCount(count)
	out := make([]Result, 0, min(len(rs), count))
	seen := map[string]bool{}
	for _, r := range rs {
		if len(out) == count {
			break
		}
		u, ok := cleanURL(r.URL)
		if !ok || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, Result{Title: cleanText(r.Title, maxTitle), URL: u, Snippet: cleanText(r.Snippet, maxSnippet)})
	}
	return out
}

// Origin is an address's scheme and host alone, what a caller shows of a
// result that did not pass its scan: a path or a query can carry an
// instruction as readily as a snippet can.
func Origin(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

func cleanURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURL || !utf8.ValidString(raw) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.String(), true
}

// cleanText keeps printable text: invalid UTF-8 and control characters
// go, runs of white space fold to one space, and the text is cut to n
// characters. What a result says is left to the scan; only its shape is
// held here.
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	space := false
	count := 0
	for _, r := range s {
		if count == n {
			break
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if space {
			b.WriteByte(' ')
			count++
			space = false
			if count == n {
				break
			}
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// Fake answers every search from a fixed list, for tests. No deployment
// configuration can name it.
type Fake struct {
	mu      sync.Mutex
	Results []Result
	Err     error
	queries []string
}

// Name implements Provider.
func (*Fake) Name() string { return "fake" }

// Queries is what was asked, in order.
func (f *Fake) Queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// Search implements Provider.
func (f *Fake) Search(_ context.Context, query string, count int) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if f.Err != nil {
		return nil, f.Err
	}
	if count > len(f.Results) {
		count = len(f.Results)
	}
	return append([]Result(nil), f.Results[:count]...), nil
}
