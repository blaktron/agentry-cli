// `agentry fork`: the request it sends, the terms and the typed "yes"
// before it, --wait over the fork's copy, and every refusal in the site's
// and the desktop's words.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

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
	if err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Errorf("no credential: %v", err)
	}
}

func TestForkSaysTheRegistrysRefusalInWords(t *testing.T) {
	fr := forkRegistry(t)
	fr.forkProblem = &fakeProblem{Status: http.StatusConflict, Type: "https://agentry.dev/problems/name-taken", Title: "Name taken", Detail: "taken"}
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "fork", "acme/demo", "--yes")
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
		{400, "bad", "something odd", "Not forked: something odd"},
		{400, "", "", "Not forked: the registry refused it."},
	} {
		if got := forkRefusal(o, tg, n, tc.status, tc.slug, tc.detail); got != tc.want {
			t.Errorf("%d %s: got %q\nwant %q", tc.status, tc.slug, got, tc.want)
		}
	}
}
