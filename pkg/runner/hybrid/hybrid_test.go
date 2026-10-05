package hybrid

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

const (
	eicar  = "275a021bbfb6489e54d471899f7db9d1663fc695ec2fe2a2c4538aabf651fd0f"
	unseen = "0000000000000000000000000000000000000000000000000000000000000000"
	sus    = "1111111111111111111111111111111111111111111111111111111111111111"
	white  = "2222222222222222222222222222222222222222222222222222222222222222"
)

// fakeHA answers the overview route as Hybrid Analysis did on 2026-10-02:
// EICAR is malicious with threat score 100, an unseen hash is 404.
type fakeHA struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	answer   func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	if f.answer != nil {
		f.answer(w, r)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/api/v2/overview/") {
	case eicar:
		_, _ = io.WriteString(w, `{"sha256":"`+eicar+`","verdict":"malicious","threat_score":100,"multiscan_result":40,"vx_family":"EICAR"}`)
	case sus:
		_, _ = io.WriteString(w, `{"verdict":"suspicious","threat_score":45}`)
	case white:
		_, _ = io.WriteString(w, `{"verdict":"whitelisted","threat_score":null}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

func client(t *testing.T, h http.Handler) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var waited []time.Duration
	c := &Client{Key: "ha-test-key", Base: srv.URL, HTTP: srv.Client()}
	c.sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	return c, &waited
}

// The request is a GET of the overview of one SHA-256, the key in the
// api-key header, Hybrid Analysis's own User-Agent, and no body: nothing
// of a file but its hash leaves the machine (plan §3 invariant 5).
func TestTheLookupSendsAHashAndNothingElse(t *testing.T) {
	f := &fakeHA{}
	c, _ := client(t, f)
	c.Look(context.Background(), []string{eicar})
	if len(f.requests) != 1 {
		t.Fatalf("%d requests", len(f.requests))
	}
	r := f.requests[0]
	if r.Method != http.MethodGet || r.URL.Path != "/api/v2/overview/"+eicar || r.URL.RawQuery != "" {
		t.Fatalf("request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	if r.Header.Get("api-key") != "ha-test-key" || r.Header.Get("User-Agent") != "Falcon Sandbox" {
		t.Fatalf("headers: %v", r.Header)
	}
	if f.bodies[0] != "" || r.ContentLength > 0 {
		t.Fatalf("a body was sent: %q", f.bodies[0])
	}
}

// The verdict rule (D14): malicious refuses, suspicious is noted with its
// threat score, anything else and an unseen hash are clean. Each hash is
// asked once however many files share it.
func TestTheVerdictRule(t *testing.T) {
	f := &fakeHA{}
	c, _ := client(t, f)
	got := c.Look(context.Background(), []string{eicar, sus, white, unseen, eicar})
	want := map[string]Outcome{eicar: Refused, sus: Noted, white: Clean, unseen: Clean}
	for h, o := range want {
		if got[h].Outcome != o {
			t.Errorf("%s…: %+v, want %s", h[:8], got[h], o)
		}
	}
	if got[eicar].ThreatScore != 100 || got[sus].ThreatScore != 45 {
		t.Errorf("threat scores: %d %d", got[eicar].ThreatScore, got[sus].ThreatScore)
	}
	if len(f.requests) != 4 {
		t.Errorf("%d requests for 4 distinct hashes", len(f.requests))
	}
}

// No key: nothing is asked, and every hash is not checked, no key (D8).
func TestNoKeyAsksNothing(t *testing.T) {
	f := &fakeHA{}
	c, _ := client(t, f)
	c.Key = ""
	got := c.Look(context.Background(), []string{eicar})
	if len(f.requests) != 0 || got[eicar] != (Result{Outcome: NotChecked, Reason: ReasonNoKey}) {
		t.Fatalf("no key: %d requests, %+v", len(f.requests), got[eicar])
	}
	var nilClient *Client
	if got := nilClient.Look(context.Background(), []string{eicar}); got[eicar].Reason != ReasonNoKey {
		t.Fatalf("a nil client: %+v", got[eicar])
	}
}

// A 429 waits, then asks again; past a minute of waiting in all, the
// hash and every one after it are not checked, quota (D13).
func TestARateLimitWaitsThenGivesUp(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if strings.HasSuffix(r.URL.Path, eicar) {
			_, _ = io.WriteString(w, `{"verdict":"malicious","threat_score":100}`)
			return
		}
		w.Header().Set("Retry-After", "50")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, waited := client(t, h)
	got := c.Look(context.Background(), []string{eicar, sus, white})
	if got[eicar].Outcome != Refused {
		t.Fatalf("after one 429 the lookup was %+v", got[eicar])
	}
	for _, x := range []string{sus, white} {
		if got[x] != (Result{Outcome: NotChecked, Reason: ReasonQuota}) {
			t.Errorf("%s…: %+v, want not checked: quota", x[:8], got[x])
		}
	}
	var total time.Duration
	for _, d := range *waited {
		total += d
	}
	if total > MaxWait {
		t.Fatalf("waited %s in all, past %s", total, MaxWait)
	}
}

// An outage, a server error or a refused key goes on: the hash and the
// rest are not checked, with the reason, and nothing more is asked.
func TestWhenTheServiceCannotAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		reason Reason
	}{
		"server error": {http.StatusBadGateway, ReasonUnavailable},
		"key refused":  {http.StatusUnauthorized, ReasonKeyRefused},
		"forbidden":    {http.StatusForbidden, ReasonKeyRefused},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeHA{answer: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }}
			c, _ := client(t, f)
			got := c.Look(context.Background(), []string{eicar, sus})
			for _, x := range []string{eicar, sus} {
				if got[x] != (Result{Outcome: NotChecked, Reason: tc.reason}) {
					t.Errorf("%s…: %+v", x[:8], got[x])
				}
			}
			if len(f.requests) != 1 {
				t.Errorf("%d requests after the first failed", len(f.requests))
			}
		})
	}
	c := &Client{Key: "k", Base: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}
	if got := c.Look(context.Background(), []string{eicar}); got[eicar].Reason != ReasonUnavailable {
		t.Fatalf("unreachable: %+v", got[eicar])
	}
}

// A malformed answer or hash is never taken for a verdict: an answer that
// is not JSON is unavailable, and a string that is not a SHA-256 is never
// put in a URL.
func TestOnlyAHashIsAskedAndOnlyAVerdictIsTaken(t *testing.T) {
	f := &fakeHA{answer: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") }}
	c, _ := client(t, f)
	if got := c.Look(context.Background(), []string{eicar}); got[eicar].Reason != ReasonUnavailable {
		t.Fatalf("a page that is not JSON: %+v", got[eicar])
	}
	f2 := &fakeHA{}
	c2, _ := client(t, f2)
	bad := "../../api/v2/submit/file"
	if got := c2.Look(context.Background(), []string{bad, strings.ToUpper(eicar)}); len(f2.requests) != 0 || got[bad].Outcome != NotChecked {
		t.Fatalf("a malformed hash was asked: %d requests, %+v", len(f2.requests), got[bad])
	}
}

// The words the receipt shows, from a fixed vocabulary.
func TestResultWords(t *testing.T) {
	for r, want := range map[Result]string{
		{Outcome: Clean}:                           "clean",
		{Outcome: Noted, ThreatScore: 45}:          "noted (suspicious, threat score 45)",
		{Outcome: Refused, ThreatScore: 100}:       "refused (malicious, threat score 100)",
		{Outcome: NotChecked, Reason: ReasonQuota}: "not checked: quota",
	} {
		if got := r.String(); got != want {
			t.Errorf("%+v: %q, want %q", r, got, want)
		}
	}
}

// The key comes from the variable the policy names, HYBRID_API_KEY by
// default; a source of env never reads the keychain.
func TestKeyFromThePolicy(t *testing.T) {
	t.Setenv("HYBRID_API_KEY", " ha-env ")
	if k, err := Key(nil); err != nil || k != "ha-env" {
		t.Fatalf("no policy: %q %v", k, err)
	}
	pol := policy.Default()
	if k, err := Key(pol); err != nil || k != "ha-env" {
		t.Fatalf("the default variable: %q %v", k, err)
	}
	pol.HybridAnalysis.APIKeyEnv = "OTHER_HA_KEY"
	t.Setenv("OTHER_HA_KEY", "")
	if k, err := Key(pol); err != nil || k != "" {
		t.Fatalf("a named variable that is empty, source env: %q %v", k, err)
	}
	c, err := FromPolicy(pol)
	if err != nil || c.Key != "" {
		t.Fatalf("FromPolicy with no key: %+v %v", c, err)
	}
}

// A redirect is never followed: the key rides in a header Go would carry
// to whatever host a 3xx names, so the lookup stops, unavailable, and the
// other host is never asked.
func TestARedirectIsNeverFollowed(t *testing.T) {
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Header.Get("api-key"))
	}))
	t.Cleanup(other.Close)
	f := &fakeHA{answer: func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}}
	c, _ := client(t, f)
	got := c.Look(context.Background(), []string{eicar})
	if len(elsewhere) != 0 {
		t.Fatalf("the redirect was followed, carrying %v", elsewhere)
	}
	if got[eicar] != (Result{Outcome: NotChecked, Reason: ReasonUnavailable}) {
		t.Fatalf("a redirect: %+v", got[eicar])
	}
}

// A 429 that names no wait, or a wait of nothing, still spends the minute,
// so a server that always answers 429 ends in quota, never a loop.
func TestARetryOfNothingStillSpendsTheBudget(t *testing.T) {
	f := &fakeHA{answer: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}}
	c, waited := client(t, f)
	got := c.Look(context.Background(), []string{eicar})
	if got[eicar].Reason != ReasonQuota || len(*waited) > 61 {
		t.Fatalf("%+v after %d waits", got[eicar], len(*waited))
	}
}
