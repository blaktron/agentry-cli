// `agentry fork`: the request it sends, the terms and the typed "yes"
// before it, --wait over the fork's copy, and every refusal in the site's
// and the desktop's words.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/apiclient"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// forkServer is a registry that answers a fork as the test says: the
// account route with userStatus (the signed-in handle when 200), and the
// fork route with the fork agent document that forkDoc adds its members to.
func forkServer(t *testing.T, userStatus int, forkDoc map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/meta", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "v1"})
	})
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		if userStatus != http.StatusOK {
			writeProblem(w, userStatus, "no account", "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "00000000-0000-0000-0000-000000000002", "handle": "tester", "displayName": "Tester",
			"email": "t@example.com", "emailVerified": true, "totpEnabled": false, "createdAt": "2026-01-02T03:04:05Z"})
	})
	mux.HandleFunc("POST /api/v1/agents/{ns}/{name}/fork", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{"id": "00000000-0000-0000-0000-000000000001", "namespace": "tester", "name": "demo", "modes": []string{"autonomous"},
			"visibility": "private", "quarantineState": "none", "defaultBranch": "main", "gitHttpUrl": "http://x/git", "createdAt": "2026-01-02T03:04:05Z"}
		for k, v := range forkDoc {
			doc[k] = v
		}
		writeJSON(w, http.StatusAccepted, doc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The server is the self-hosted registry a token authenticates to.
	t.Setenv(envRegistry, srv.URL)
	return srv
}

func forkRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	old := forkWaitPoll
	forkWaitPoll = time.Millisecond
	t.Cleanup(func() { forkWaitPoll = old })
	return fr
}

func forkBody(t *testing.T, fr *fakeRegistry, i int) map[string]any {
	t.Helper()
	if len(fr.forkBodies) <= i {
		t.Fatalf("fork requests = %v, want at least %d", fr.forkBodies, i+1)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(fr.forkBodies[i]), &b); err != nil {
		t.Fatalf("fork body %q: %v", fr.forkBodies[i], err)
	}
	return b
}

func TestForkGoesIntoYourOwnNamespaceUnderTheOriginsName(t *testing.T) {
	fr := forkRegistry(t)
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Forked acme/demo into tester/demo.") || !strings.Contains(stdout, "Its copy is under way") {
		t.Errorf("stdout = %q", stdout)
	}
	b := forkBody(t, fr, 0)
	if b["namespace"] != "tester" {
		t.Errorf("namespace = %v, want the signed-in handle", b["namespace"])
	}
	if _, ok := b["name"]; ok {
		t.Errorf("a fork under the origin's name sent a name: %v", b)
	}
}

func TestForkSendsIntoAndName(t *testing.T) {
	fr := forkRegistry(t)
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--into", "myorg", "--name", "demo-two", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if b := forkBody(t, fr, 0); b["namespace"] != "myorg" || b["name"] != "demo-two" {
		t.Errorf("body = %v", b)
	}
	if !strings.Contains(stdout, "Forked acme/demo into myorg/demo-two.") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestForkWaitFollowsTheCopy(t *testing.T) {
	fr := forkRegistry(t)
	fr.forkStates = []string{"copying", "copying", "ready"}
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--yes", "--wait")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "The copy is made, and its first version 1.0.0 is in the pipeline.") {
		t.Errorf("stdout = %q", stdout)
	}

	// A registry that answers no fork state cannot be waited on: --wait
	// says so at once rather than polling to its deadline.
	fr3 := forkRegistry(t)
	fr3.forkStates = []string{""}
	_, _, err = runCLI(t, nil, "--registry", fr3.url(), "fork", "acme/demo", "--yes", "--wait")
	if err == nil || !strings.Contains(err.Error(), "does not say how the copy") {
		t.Errorf("no fork state: %v", err)
	}

	// A copy that never finishes is given up on, naming the fork and the wait.
	oldLimit := forkWaitLimit
	t.Cleanup(func() { forkWaitLimit = oldLimit })
	forkWaitLimit = 20 * time.Millisecond
	fr4 := forkRegistry(t)
	fr4.forkStates = []string{"copying"}
	_, _, err = runCLI(t, nil, "--registry", fr4.url(), "fork", "acme/demo", "--yes", "--wait")
	if err == nil || err.Error() != "the fork tester/demo is still being copied after 20ms; `agentry info tester/demo` shows when it is ready" {
		t.Errorf("a copy that never finishes: %v", err)
	}
	forkWaitLimit = oldLimit

	// A cancelled wait stops at once, and a read of the fork that fails is the registry's problem.
	copying := apiclient.AgentForkStateCopying
	made := &apiclient.Agent{Namespace: "nobody", Name: "gone", ForkState: &copying}
	client, err := newAPIClient(fr.url(), "agy_testtoken")
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForFork(stubCmd(t), client, made)
	if err == nil || !strings.Contains(err.Error(), "read the fork nobody/gone") {
		t.Errorf("a failed read of the fork: %v", err)
	}
	oldPoll := forkWaitPoll
	forkWaitPoll = time.Hour
	t.Cleanup(func() { forkWaitPoll = oldPoll })
	forkWaitLimit = time.Hour
	cancelled := stubCmd(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled.SetContext(ctx)
	if _, err := waitForFork(cancelled, client, made); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
	forkWaitPoll, forkWaitLimit = oldPoll, oldLimit

	fr2 := forkRegistry(t)
	fr2.forkStates = []string{"copying", "failed"}
	_, _, err = runCLI(t, nil, "--registry", fr2.url(), "fork", "acme/demo", "--yes", "--wait")
	if err == nil || !strings.Contains(err.Error(), "the fork tester/demo was made, but its copy failed: the origin's repository could not be read") {
		t.Errorf("a failed copy: %v", err)
	}
}

func TestForkJSONPrintsTheForksRecord(t *testing.T) {
	fr := forkRegistry(t)
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "fork", "acme/demo", "--yes", "--wait")
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Namespace, Name, ForkState string
		ForkedFrom                 struct{ Namespace, Name, Version string }
	}
	if err := json.Unmarshal([]byte(stdout), &a); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if a.Namespace != "tester" || a.ForkState != "ready" || a.ForkedFrom.Namespace != "acme" || a.ForkedFrom.Version != "1.0.0" {
		t.Errorf("record = %+v", a)
	}

	// Without --wait the record is the registry's first answer, still being
	// copied, and still the only thing on stdout.
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "fork", "acme/demo", "--yes", "--name", "demo-copy")
	if err != nil {
		t.Fatal(err)
	}
	a.Name, a.ForkState = "", ""
	if err := json.Unmarshal([]byte(stdout), &a); err != nil || a.Namespace != "tester" || a.Name != "demo-copy" || a.ForkState != "copying" || strings.Contains(stdout, "Forked ") {
		t.Errorf("record without --wait = %+v (%v)\n%s", a, err, stdout)
	}
}

// Off a terminal, without --yes, nothing is sent: a piped "yes" is not a
// person agreeing to a charged submission.
func TestForkOffATerminalNeedsYes(t *testing.T) {
	fr := forkRegistry(t)
	_, stderr, err := runCLI(t, strings.NewReader("yes\n"), "--registry", fr.url(), "fork", "acme/demo")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "pass --yes") {
		t.Errorf("err = %v", err)
	}
	for _, term := range forkTerms {
		if !strings.Contains(stderr, term) {
			t.Errorf("the terms were not said before asking: missing %q", term)
		}
	}
	if len(fr.forkBodies) != 0 {
		t.Errorf("a fork was sent without consent: %v", fr.forkBodies)
	}
}

func TestForkChecksItsArgumentsBeforeSending(t *testing.T) {
	fr := forkRegistry(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"acme/demo@1.0.0"}, "without a version"},
		{[]string{"demo"}, "is not a QuickAgent's name"},
		{[]string{"Not_A_Namespace/demo"}, "is not a QuickAgent's name"},
		{[]string{"acme/Demo Two"}, "is not a QuickAgent's name"},
		{[]string{"acme/demo", "--into", "Not_A_Namespace"}, "--into"},
		{[]string{"acme/demo", "--name", "Demo Two"}, "--name"},
	} {
		args := append([]string{"--registry", fr.url(), "fork"}, append(tc.args, "--yes")...)
		_, _, err := runCLI(t, nil, args...)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if len(fr.forkBodies) != 0 {
		t.Errorf("a refused argument still sent a fork: %v", fr.forkBodies)
	}

	t.Setenv(envToken, "")
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--yes")
	if err == nil || !strings.Contains(err.Error(), "no credential for "+fr.url()) || !strings.Contains(err.Error(), "agentry login") {
		t.Errorf("no credential: %v", err)
	}
	if len(fr.forkBodies) != 0 {
		t.Errorf("a fork was sent without a credential: %v", fr.forkBodies)
	}
}

func TestForkSaysTheRegistrysRefusalInWords(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	// The account that names the namespace cannot be read.
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "Not forked: the registry did not accept your credential. Run `agentry login`, then try again."},
		{http.StatusInternalServerError, "read your account to fork into your namespace (or pass --into)"},
	} {
		srv := forkServer(t, tc.status, nil)
		_, _, err := runCLI(t, nil, "--registry", srv.URL, "fork", "acme/demo", "--yes")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("account answered %d: err = %v, want %q", tc.status, err, tc.want)
		}
	}
	// A registry that does not answer is said to be so, whichever request it was.
	down := forkServer(t, http.StatusOK, nil)
	down.Close()
	for _, args := range [][]string{{"fork", "acme/demo", "--yes"}, {"fork", "acme/demo", "--yes", "--into", "myorg"}} {
		_, _, err := runCLI(t, nil, append([]string{"--registry", down.URL}, args...)...)
		if err == nil || err.Error() != "Not forked: the registry did not answer. Try again in a moment." {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	// A copy that failed without a reason says so.
	noReason := forkServer(t, http.StatusOK, map[string]any{"forkState": "failed", "forkedFrom": map[string]any{"namespace": "acme", "name": "demo", "version": "1.0.0", "available": true}})
	_, _, err := runCLI(t, nil, "--registry", noReason.URL, "fork", "acme/demo", "--yes", "--wait")
	if err == nil || err.Error() != "the fork tester/demo was made, but its copy failed: the registry gave no reason" {
		t.Errorf("a copy that failed without a reason: %v", err)
	}

	fr := forkRegistry(t)
	fr.forkProblem = &fakeProblem{Status: http.StatusConflict, Type: "https://agentry.dev/problems/name-taken", Title: "Name taken", Detail: "taken"}
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--yes")
	if err == nil || err.Error() != "Not forked: tester already has a QuickAgent called demo. Choose another name for the fork with --name." {
		t.Errorf("name-taken: %v", err)
	}
}

// The refusals read as the site's forkRefusal and the desktop's do.
func TestForkRefusalWords(t *testing.T) {
	const o, tg, n = "acme/demo", "tester", "demo"
	for _, tc := range []struct {
		status       int
		slug, detail string
		want         string
	}{
		{422, "account-unactivated", "Confirm your email first.", "Confirm your email first."},
		{401, "", "", "Not forked: the registry did not accept your credential. Run `agentry login`, then try again."},
		{422, "validation", "namespace agent quota reached", "Not forked: tester already holds as many QuickAgents as it may. Delete one there, then try again."},
		{409, "name-taken", "", "Not forked: tester already has a QuickAgent called demo. Choose another name for the fork with --name."},
		{409, "name-held", "", "Not forked: demo belonged to a QuickAgent deleted from tester, and that name is never used again. Choose another name for the fork with --name."},
		{409, "fork-source", "", "Not forked: only a public QuickAgent that is not quarantined or blocked can be forked, and acme/demo is not one now."},
		{409, "nothing-to-fork", "", "Not forked: acme/demo has no published version yet, and a fork starts from one."},
		{422, "private-agent-cap", "", "Not forked: tester already holds as many private QuickAgents as its subscription allows, and a fork is private. Retire one, or upgrade, then try again."},
		{403, "forbidden", "Only an organization's members publish to it.", "Not forked: Only an organization's members publish to it."},
		{403, "", "", "Not forked: you cannot create QuickAgents in tester."},
		{404, "not-found", "agent not found", "Not forked: acme/demo or tester is not there any more."},
		{429, "", "", "Not forked: too many requests. Wait a minute, then try again."},
		{503, "", "", "Not forked: the registry did not answer. Try again in a moment."},
		{500, "", "", "Not forked: the registry did not answer. Try again in a moment."},
		{422, "account-unactivated", "", "Not forked: the registry refused it."},
		{422, "validation", "Namespace Agent QUOTA reached", "Not forked: tester already holds as many QuickAgents as it may. Delete one there, then try again."},
		{422, "", "quota exceeded", "Not forked: quota exceeded"},
		{404, "gone", "the origin was deleted", "Not forked: the origin was deleted"},
		{400, "bad", "something odd", "Not forked: something odd"},
		{400, "", "", "Not forked: the registry refused it."},
	} {
		if got := forkRefusal(o, tg, n, tc.status, tc.slug, tc.detail); got != tc.want {
			t.Errorf("%d %s: got %q\nwant %q", tc.status, tc.slug, got, tc.want)
		}
	}

	// A refusal's slug is the last segment of its problem's type. For a status
	// the client does not type itself the problem is read from the body, when
	// the body is one for that status.
	detail := "its detail"
	if slug, got := problemParts(409, &apiclient.Problem{Type: "https://agentry.dev/problems/name-taken", Detail: &detail}, nil); slug != "name-taken" || got != detail {
		t.Errorf("a parsed problem: %q %q", slug, got)
	}
	body := []byte(`{"type":"https://agentry.dev/problems/fork-source","title":"t","status":409,"detail":"d"}`)
	for _, tc := range []struct {
		status           int
		body             []byte
		wantSlug, wantDt string
	}{
		{409, body, "fork-source", "d"},
		{403, body, "", ""},
		{409, []byte("not json"), "", ""},
		{409, []byte(`{"type":"x","status":409}`), "", ""},
	} {
		if slug, got := problemParts(tc.status, nil, tc.body); slug != tc.wantSlug || got != tc.wantDt {
			t.Errorf("problemParts(%d, %s) = %q, %q, want %q, %q", tc.status, tc.body, slug, got, tc.wantSlug, tc.wantDt)
		}
	}
}
