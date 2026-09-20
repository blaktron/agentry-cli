package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// whoami (agentry-cli#128): the account the credential belongs to; the
// subscription lines only where the registry mounts the billing extension;
// exit class 2 with no credential.
func TestWhoamiShowsTheAccountAndTheSubscriptionWhereThereIsOne(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.userHandle = "chris"
	t.Setenv(envToken, "agy_test")

	// A core registry: the account lines and nothing about a tier.
	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatalf("whoami: %v (stderr %q)", err, stderr)
	}
	for _, want := range []string{"registry: " + fr.url(), "handle:   chris", "email:    t@example.com"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "tier:") || strings.Contains(stdout, "credits:") {
		t.Errorf("a core registry showed a tier:\n%s", stdout)
	}

	// The same registry with the extension: the tier and the credits.
	fr.billing, fr.billingBalance = true, 40
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tier:     Standard", "credits:  40 of 50 (a submission costs 10; resets on 11 October 2026)", "private:  4 of 10 BriefAgents"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami lacks %q:\n%s", want, stdout)
		}
	}

	// --json carries the account object.
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Handle  string `json:"handle"`
		Account struct {
			Tier    string `json:"tier"`
			Balance int64  `json:"balance"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if doc.Handle != "chris" || doc.Account.Tier != "standard" || doc.Account.Balance != 40 {
		t.Errorf("json doc = %+v", doc)
	}
}

// A member of an organization (plan organization-subscriptions.md §8, M11):
// the tier is the organization's and says so, the balance is the pool's,
// and the seats and the standing come with it. The core's own facts — the
// organization and the role — are shown by a registry with no tiers at all.
func TestWhoamiShowsTheOrganizationAndThePool(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.userHandle = "chris"
	fr.orgs = []map[string]any{{"handle": "acme", "role": "admin", "state": "active"}}
	t.Setenv(envToken, "agy_test")

	// A core registry: the organization and the role, not a word about a tier.
	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatalf("whoami: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "org:      acme (admin)") {
		t.Errorf("whoami lacks the organization:\n%s", stdout)
	}
	if strings.Contains(stdout, "tier:") || strings.Contains(stdout, "Business") {
		t.Errorf("a core registry spoke of a tier:\n%s", stdout)
	}

	// The same registry with the extension: a member is answered the
	// organization's account, so the tier names it and the pool is the
	// organization's, with its seats and its standing.
	fr.billing = true
	fr.billingAccountDoc = map[string]any{
		"kind": "org", "tier": "business", "standing": "active", "memberOf": "acme",
		"seats":   map[string]any{"used": 3, "max": 10},
		"balance": 3960, "allowance": 5000, "submissionCost": 10,
		"periodStart": "2026-09-11T00:00:00Z", "periodEnd": "2026-10-11T00:00:00Z", "tierSince": "2026-09-11T00:00:00Z",
		"privateAgents": map[string]any{"max": 0, "used": 4}, "upgrade": map[string]any{"available": false},
	}
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"org:      acme (admin)",
		"tier:     Business, through acme",
		"pool:     3960 of 5000 credits (a submission costs 10; resets on 11 October 2026)",
		"seats:    3 of 10",
		"standing: active",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "credits:  ") {
		t.Errorf("a member's pool was labelled as their own credits:\n%s", stdout)
	}

	// An unmetered organization is never debited, and the line says so
	// rather than showing a balance that means nothing.
	fr.billingAccountDoc["unmetered"], fr.billingAccountDoc["allowance"] = true, 0
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "pool:     not metered") {
		t.Errorf("an unmetered pool showed a balance:\n%s", stdout)
	}

	// --json carries the organization and the account together.
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Orgs []struct {
			Handle, Role, State string
		} `json:"orgs"`
		Account struct {
			Tier     string `json:"tier"`
			MemberOf string `json:"memberOf"`
			Standing string `json:"standing"`
			Seats    struct {
				Used, Max int64
			} `json:"seats"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if len(doc.Orgs) != 1 || doc.Orgs[0].Handle != "acme" || doc.Orgs[0].Role != "admin" || doc.Orgs[0].State != "active" {
		t.Errorf("json orgs = %+v", doc.Orgs)
	}
	if doc.Account.MemberOf != "acme" || doc.Account.Tier != "business" || doc.Account.Standing != "active" || doc.Account.Seats.Max != 10 {
		t.Errorf("json account = %+v", doc.Account)
	}
}

// A pending fold-in (agentry#364): the membership is reported but not in
// force, so whoami says an admin has yet to approve them and names the
// door — and the subscription shown is still their own, because that is
// what they spend from until they are approved.
func TestWhoamiSaysAPendingFoldInIsNotYetAMembership(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.userHandle = "chris"
	fr.billing, fr.billingBalance = true, 40
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "pending"}}
	fr.pendingOrg = map[string]any{"handle": "acme", "displayName": "Acme", "via": "domain"}
	t.Setenv(envToken, "agy_test")

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"org:      acme (member) — pending:",
		"an admin has yet to approve you",
		"your address is at the organization's domain",
		"tier:     Standard",
		"credits:  40 of 50",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "through acme") {
		t.Errorf("a pending fold-in was shown as a membership:\n%s", stdout)
	}
}

func TestWhoamiWithoutACredentialExits2(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err == nil {
		t.Fatal("whoami with no credential succeeded")
	}
	if code, ok := exitCodeOf(err); !ok || code != 2 {
		t.Fatalf("exit class = %d (%v), want 2: %v", code, ok, err)
	}
	if !strings.Contains(err.Error(), "no credential stored") || !strings.Contains(err.Error(), "public agents only") {
		t.Fatalf("message = %q", err)
	}
}

// publish on 402 (the allowance spent): the problem's title and detail,
// the ordinary failure exit, no retry.
func TestPublishRendersCreditsExhausted(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.billing = true
	fr.submitProblem = &fakeProblem{Status: 402, Type: "https://agentry.dev/problems/credits-exhausted",
		Title: "Not enough credits for this submission", Detail: "A submission costs 10 credits; this account has 0, and its allowance resets on 11 October 2026."}
	t.Setenv(envToken, "agy_test")
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t))
	if err == nil {
		t.Fatal("publish succeeded on a 402")
	}
	if !strings.Contains(err.Error(), "Not enough credits for this submission: A submission costs 10 credits; this account has 0, and its allowance resets on 11 October 2026. (HTTP 402)") {
		t.Fatalf("message = %q", err)
	}
	if n := len(fr.submitKeys); n != 1 {
		t.Fatalf("submit attempts = %d, want 1 (no retry on a refusal)", n)
	}
}

// A 402 that names an account (plan organization-subscriptions.md §8, D22):
// the pool that is short is the organization's, not the publisher's, so the
// sentence says whose it is — and an organization whose subscription has not
// started is its own refusal, not an empty balance.
func TestPublishNamesTheAccountA402Refuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		problem *fakeProblem
		want    string
	}{
		{
			name: "credits-exhausted names the organization whose pool is short",
			problem: &fakeProblem{Status: 402, Type: "https://agentry.dev/problems/credits-exhausted",
				Title:   "Not enough credits for this submission",
				Detail:  "a submission costs 10 credits; the acme organization's pool has 0, and its allowance resets on 11 October 2026",
				Members: map[string]any{"account": "acme", "balance": 0, "tier": "business"}},
			want: "(account acme, HTTP 402)",
		},
		{
			name: "subscription-inactive names the organization and its standing",
			problem: &fakeProblem{Status: 402, Type: "https://agentry.dev/problems/subscription-inactive",
				Title:   "The organization's subscription is not active",
				Detail:  "the acme organization is awaiting payment: nothing is admitted until its subscription starts",
				Members: map[string]any{"account": "acme", "standing": "pending", "tier": "business"}},
			want: "(account acme, standing pending, HTTP 402)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			fr := newFakeRegistry(t)
			fr.billing = true
			fr.submitProblem = tc.problem
			t.Setenv(envToken, "agy_test")
			_, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t))
			if err == nil {
				t.Fatal("publish succeeded on a 402")
			}
			if !strings.Contains(err.Error(), tc.problem.Detail) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message = %q, want the detail and %q", err, tc.want)
			}
		})
	}
}
