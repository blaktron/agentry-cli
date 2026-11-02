package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeTokenRegistry answers a token revoking itself, and records each
// revocation as "<id> <bearer>".
type fakeTokenRegistry struct {
	srv     *httptest.Server
	mu      sync.Mutex
	revoked []string
	status  int
}

func newFakeTokenRegistry(t *testing.T) *fakeTokenRegistry {
	t.Helper()
	fr := &fakeTokenRegistry{status: http.StatusNoContent}
	fr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/user/tokens/")
		if r.Method != http.MethodDelete || !ok {
			http.NotFound(w, r)
			return
		}
		fr.mu.Lock()
		fr.revoked = append(fr.revoked, id+" "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		st := fr.status
		fr.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(fr.srv.Close)
	return fr
}

func (fr *fakeTokenRegistry) seen() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]string(nil), fr.revoked...)
}

// askStore answers one store verb in this process.
func askStore(t *testing.T, args []string, in string) runhost.Answer {
	t.Helper()
	var buf bytes.Buffer
	fw := runhost.NewFrameWriter(&buf)
	code, err := runhostStoreVerb(context.Background(), runhost.Request{Verb: "store", Args: args}, strings.NewReader(in), fw)
	_ = fw.Exit(code, err)
	a, rerr := runhost.ReadAnswer(&buf, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return a
}

const (
	testCreditsID    = "0f0e0d0c-0b0a-4908-8706-050403020100"
	testCreditsID2   = "1f1e1d1c-1b1a-4918-8716-151413121110"
	testCreditsToken = "agy_runhost-credits-token-0001"
)

func creditsDoc(reg, id, token string) string {
	b, _ := json.Marshal(runhostCredits{Registry: reg, TokenID: id, Name: "Runhost lab (credits only)", Token: token, Device: testDevice})
	return string(b)
}

// The runhost keeps its credits token encrypted, says what it holds without
// the token, revokes the token it replaces, and revokes and removes it on
// request; a run paid with credits takes it in its own variable alone.
func TestARunhostKeepsItsCreditsTokenAndRevokesIt(t *testing.T) {
	newRunhostServer(t)
	fr := newFakeTokenRegistry(t)
	reg := fr.srv.URL

	for name, doc := range map[string]string{
		"not a token":        creditsDoc(reg, testCreditsID, "sk-not-ours-0000000000"),
		"no id":              creditsDoc(reg, "", testCreditsToken),
		"a path":             creditsDoc(reg+"/api", testCreditsID, testCreditsToken),
		"plain http":         creditsDoc("http://registry.test", testCreditsID, testCreditsToken),
		"an unknown field":   `{"registry":"` + reg + `","surprise":1}`,
		"an unpaired device": strings.Replace(creditsDoc(reg, testCreditsID, testCreditsToken), testDevice, "bbbbbbbbbbbbbbbb", 1),
	} {
		if a := askStore(t, []string{"put-credits"}, doc); a.Code != engine.ExitUsage {
			t.Errorf("%s: code %d %s, want usage", name, a.Code, a.Error)
		}
	}

	if a := askStore(t, []string{"put-credits"}, creditsDoc(reg, testCreditsID, testCreditsToken)); a.Code != 0 {
		t.Fatalf("put-credits: %d %s", a.Code, a.Error)
	}
	a := askStore(t, []string{"list"}, "")
	if a.Code != 0 || strings.Contains(string(a.Result), testCreditsToken) || !strings.Contains(string(a.Result), testCreditsID) {
		t.Fatalf("list: %d %s", a.Code, a.Result)
	}
	if b, err := os.ReadFile(filepath.Join(runhostStoreDir, "credits.entry")); err != nil || bytes.Contains(b, []byte(testCreditsToken)) {
		t.Fatalf("the credits entry is plaintext on disk, or missing: %v", err)
	}

	// A credits model takes the stored token for its own registry alone.
	model := policy.Model{Provider: policy.ProviderAgentry, Name: "m", BaseURL: reg + policy.AgentryInferencePath}
	if tok, err := runhostJobCredits(model); err != nil || tok != testCreditsToken {
		t.Fatalf("job credits: %q, %v", tok, err)
	}
	if _, err := runhostJobCredits(policy.Model{Provider: policy.ProviderAgentry, Name: "m", BaseURL: "https://other.test" + policy.AgentryInferencePath}); err == nil {
		t.Fatal("another registry's run took the runhost's credits token")
	}
	j := &job.Job{Run: job.Run{Model: job.ModelOf(model)}}
	env := runhostChildEnv([]string{"PATH=/bin", envRunhostCreditsToken + "=stale"}, j, testCreditsToken)
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, envRunhostCreditsToken+"=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != envRunhostCreditsToken+"="+testCreditsToken {
		t.Fatalf("the run's credits variable: %v", got)
	}
	if env := runhostChildEnv([]string{envRunhostCreditsToken + "=stale"}, j, ""); strings.Contains(strings.Join(env, "\n"), envRunhostCreditsToken) {
		t.Fatalf("a run took the service's own credits variable: %v", env)
	}

	// A new token replaces the old one, which revokes itself.
	if a := askStore(t, []string{"put-credits"}, creditsDoc(reg, testCreditsID2, testCreditsToken+"x")); a.Code != 0 {
		t.Fatalf("second put-credits: %d %s", a.Code, a.Error)
	}
	if s := fr.seen(); len(s) != 1 || s[0] != testCreditsID+" "+testCreditsToken {
		t.Fatalf("the replaced token's revocation: %v", s)
	}

	a = askStore(t, []string{"remove-credits"}, "")
	var removed runhostCreditsRemoved
	if a.Code != 0 || json.Unmarshal(a.Result, &removed) != nil || !removed.Removed || !removed.Revoked {
		t.Fatalf("remove-credits: %d %s", a.Code, a.Result)
	}
	if s := fr.seen(); len(s) != 2 || s[1] != testCreditsID2+" "+testCreditsToken+"x" {
		t.Fatalf("the removed token's revocation: %v", s)
	}
	if _, err := runhostJobCredits(model); err == nil || !strings.Contains(err.Error(), "agentry runhost model") {
		t.Fatalf("a credits run with no token: %v", err)
	}

	// A registry that cannot revoke it: the token still leaves the store,
	// and the answer says so.
	_ = askStore(t, []string{"put-credits"}, creditsDoc(reg, testCreditsID, testCreditsToken))
	fr.mu.Lock()
	fr.status = http.StatusInternalServerError
	fr.mu.Unlock()
	a = askStore(t, []string{"remove-credits"}, "")
	removed = runhostCreditsRemoved{}
	if a.Code != 0 || json.Unmarshal(a.Result, &removed) != nil || !removed.Removed || removed.Revoked || removed.RevokeError == "" {
		t.Fatalf("remove-credits, the registry refusing: %d %s", a.Code, a.Result)
	}
	if a := askStore(t, []string{"list"}, ""); strings.Contains(string(a.Result), "credits") {
		t.Fatalf("the token stayed in the store: %s", a.Result)
	}
}

// The token a runhost is given must carry the credits scope alone and an id
// it can revoke itself by; a registry that mints anything else is refused,
// and nothing reaches the runhost.
func TestARunhostsTokenMustBeCreditsOnly(t *testing.T) {
	isolateEnv(t)
	var answer string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/desktop/exchange" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
	}))
	defer reg.Close()
	prev := openBrowser
	t.Cleanup(func() { openBrowser = prev })
	var asked string
	openBrowser = func(raw string) error {
		asked = raw
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		go func() {
			res, err := http.Get("http://127.0.0.1:" + q.Get("port") + "/callback?code=agd_x&state=" + url.QueryEscape(q.Get("state")))
			if err == nil {
				_ = res.Body.Close()
			}
		}()
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetErr(io.Discard)

	answer = `{"id":"` + testCreditsID + `","name":"Runhost lab (credits only)","token":"` + testCreditsToken + `","scopes":["credits:session"]}`
	pat, err := mintRunhostCreditsToken(cmd, reg.URL, "lab")
	if err != nil || pat.ID != testCreditsID || pat.Token != testCreditsToken {
		t.Fatalf("a credits-only token: %+v, %v", pat, err)
	}
	if !strings.Contains(asked, "/ui/authorize?") || !strings.Contains(asked, "&runhost=lab") {
		t.Fatalf("the authorize page asked: %s", asked)
	}
	for name, a := range map[string]string{
		"the sign-in set":  `{"id":"` + testCreditsID + `","name":"Agentry Desktop (2026-11-02)","token":"` + testCreditsToken + `","scopes":["agent:read","user:profile"]}`,
		"credits and more": `{"id":"` + testCreditsID + `","name":"x","token":"` + testCreditsToken + `","scopes":["credits:session","user:profile"]}`,
		"no id":            `{"name":"x","token":"` + testCreditsToken + `","scopes":["credits:session"]}`,
	} {
		answer = a
		if _, err := mintRunhostCreditsToken(cmd, reg.URL, "lab"); err == nil || !strings.Contains(err.Error(), "revoke it") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A job paid with Agentry credits carries no key: the runhost's run gets the
// stored credits token in its own variable, and a runhost with none refuses
// the job in the policy class.
func TestACreditsJobRunsWithTheRunhostsToken(t *testing.T) {
	srv := newRunhostServer(t)
	fr := newFakeTokenRegistry(t)
	credits := func(j *job.Job) {
		j.Run.ModelKey = ""
		j.Run.Model = job.Model{Provider: policy.ProviderAgentry, Name: "claude-x", BaseURL: fr.srv.URL + policy.AgentryInferencePath,
			ProxyAllow: []string{strings.TrimPrefix(fr.srv.URL, "http://")}}
	}
	j, archive := unsignedJob(t, "c0ffee0000000001")
	credits(j)
	if ans := sendJob(t, j, archive); ans.code != engine.ExitPolicy || ans.err == nil || !strings.Contains(ans.err.Error(), "no Agentry credits token") {
		t.Fatalf("a credits job with no token: %d %v", ans.code, ans.err)
	}
	if a := askStore(t, []string{"put-credits"}, creditsDoc(fr.srv.URL, testCreditsID, testCreditsToken)); a.Code != 0 {
		t.Fatalf("put-credits: %d %s", a.Code, a.Error)
	}
	j, archive = unsignedJob(t, "c0ffee0000000002")
	credits(j)
	if ans := sendJob(t, j, archive); ans.code != 0 {
		t.Fatalf("a credits job: %d %v %v", ans.code, ans.err, ans.out)
	}
	env, err := os.ReadFile(filepath.Join(srv.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), envRunhostCreditsToken+"="+testCreditsToken+"\n") || strings.Contains(string(env), "TEST_MODEL_API_KEY=") {
		t.Fatalf("the credits run's environment:\n%s", env)
	}
}

// A run a runhost started with its credits token, refused by the registry,
// says to give the runhost a new token, not to sign in on a device; a local
// run's advice is unchanged.
func TestARevokedRunhostTokenSaysToGiveTheRunhostANewOne(t *testing.T) {
	isolateEnv(t)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"title":"Unauthorized","detail":"This token is invalid or expired."}`)
	}))
	defer reg.Close()
	pol := &policy.Policy{Model: policy.Model{Provider: policy.ProviderAgentry, Name: "m", BaseURL: reg.URL + policy.AgentryInferencePath}}
	t.Setenv(envRunhostCreditsToken, testCreditsToken)
	c := creditsClient(pol)
	if c == nil {
		t.Fatal("no credits client for a runhost's token")
	}
	_, err := c.Open(context.Background(), "run")
	if err == nil || !strings.Contains(err.Error(), "agentry runhost model") || !strings.Contains(err.Error(), "--provider agentry") || strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("a revoked runhost token: %v", err)
	}
	if !strings.Contains(runhostCreditsAdvice("lab"), "agentry runhost model lab --provider agentry") {
		t.Fatalf("advice: %s", runhostCreditsAdvice("lab"))
	}
}
