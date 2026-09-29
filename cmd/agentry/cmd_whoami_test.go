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
	if strings.Contains(stdout, "org:") {
		t.Errorf("an account in no organization was given one:\n%s", stdout)
	}

	// The same registry with the extension: the tier and the credits.
	fr.billing, fr.billingBalance = true, 40
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tier:     Standard", "credits:  40 of 50 (a submission costs 10; resets on 11 October 2026)", "private:  4 of 10 QuickAgents"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("whoami lacks %q:\n%s", want, stdout)
		}
	}
	// An account in no organization spends its own credits, never a pool,
	// and nothing here is a seat or a standing.
	for _, never := range []string{"pool:", "seats:", "standing:", "org:", "through "} {
		if strings.Contains(stdout, never) {
			t.Errorf("whoami showed %q for an account in no organization:\n%s", never, stdout)
		}
	}
	// The credential is what this command reads, never what it prints.
	if strings.Contains(stdout, "agy_test") || strings.Contains(stderr, "agy_test") {
		t.Errorf("whoami echoed the token:\n%s\n%s", stdout, stderr)
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

	// An organization that is not paid for is granted no allowance, so the
	// pool says what is left and promises no reset: "0 of 0, resets on the
	// 11th" would be a promise the registry will not keep (D10).
	fr.billingAccountDoc["unmetered"] = false
	fr.billingAccountDoc["standing"], fr.billingAccountDoc["allowance"], fr.billingAccountDoc["balance"] = "restricted", 0, 4000
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "pool:     4000 credits, and no allowance while the subscription is not active") {
		t.Errorf("a restricted pool promised an allowance:\n%s", stdout)
	}
	if strings.Contains(stdout, "resets on") || strings.Contains(stdout, "of 0 credits") {
		t.Errorf("a restricted pool promised a reset:\n%s", stdout)
	}
	if !strings.Contains(stdout, "standing: restricted — the subscription has lapsed") {
		t.Errorf("the standing did not say what it means:\n%s", stdout)
	}

	// Back to active for the JSON check.
	fr.billingAccountDoc["standing"], fr.billingAccountDoc["allowance"], fr.billingAccountDoc["balance"] = "active", 5000, 3960

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
	// The account is the registry's own document, so a field this command
	// does not render still reaches --json.
	if !strings.Contains(stdout, "\"tierSince\"") || !strings.Contains(stdout, "\"upgrade\"") {
		t.Errorf("--json dropped fields the registry answered:\n%s", stdout)
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

	// A 402 that names an account (plan organization-subscriptions.md §8,
	// D22): a member publishes to their own namespace but spends from the
	// organization's pool, so the sentence says whose account it is —
	// including for an organization whose subscription never started,
	// which is its own refusal rather than an empty balance.
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
			want: "; the account is acme (HTTP 402)",
		},
		{
			name: "subscription-inactive names the organization it is about",
			problem: &fakeProblem{Status: 402, Type: "https://agentry.dev/problems/subscription-inactive",
				Title:   "The organization's subscription is not active",
				Detail:  "the acme organization is awaiting payment: nothing is admitted until its subscription starts",
				Members: map[string]any{"account": "acme", "standing": "pending", "tier": "business"}},
			want: "; the account is acme (HTTP 402)",
		},
		{
			name: "an account name is bounded and stripped before it reaches a terminal",
			problem: &fakeProblem{Status: 402, Type: "https://agentry.dev/problems/credits-exhausted",
				Title: "Not enough credits for this submission", Detail: "the pool is empty",
				Members: map[string]any{"account": "\x1b]0;pwned\x07" + strings.Repeat("a", 200)}},
			// The escape's control characters are dropped and what is left
			// is cut to 64 runes, so no registry can rewrite the terminal
			// its refusal is printed to, nor fill it.
			want: "; the account is ]0;pwned" + strings.Repeat("a", 56) + "… (HTTP 402)",
		},
		{
			name: "a status that is not a payment refusal carries no account",
			problem: &fakeProblem{Status: 422, Type: "https://agentry.dev/problems/validation",
				Title: "Validation failed", Detail: "the manifest is not valid",
				Members: map[string]any{"account": "acme"}},
			want: "the manifest is not valid (HTTP 422)",
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
				t.Fatal("publish succeeded on a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message = %q, want %q", err, tc.want)
			}
			if strings.ContainsAny(err.Error(), "\x1b\x07") {
				t.Fatalf("a control character reached the message: %q", err)
			}
		})
	}
}

// The private-agent line (agentry-cli#277, after agentry-enterprise#40):
// the registry's privateAgents.enabled says which of "none" and "no cap"
// a max of 0 means, so the line is printed for everyone it answers. A
// registry that predates the field keeps the old rule: the line only when
// max > 0, because 0 could mean either.
func TestWhoamiPrivateAgentLineReadsEnabled(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.userHandle = "chris"
	fr.billing = true
	t.Setenv(envToken, "agy_test")

	for _, tc := range []struct {
		name    string
		private map[string]any
		want    string // "" means no private line at all
	}{
		{"disabled", map[string]any{"enabled": false, "max": 0, "used": 0}, "private:  none\n"},
		{"disabled with agents held", map[string]any{"enabled": false, "max": 0, "used": 2}, "private:  none may be added (2 QuickAgents held)\n"},
		{"enabled, no cap", map[string]any{"enabled": true, "max": 0, "used": 4}, "private:  4 QuickAgents, no cap\n"},
		{"enabled, capped", map[string]any{"enabled": true, "max": 10, "used": 4}, "private:  4 of 10 QuickAgents\n"},
		{"absent field, capped", map[string]any{"max": 10, "used": 4}, "private:  4 of 10 QuickAgents\n"},
		{"absent field, max 0", map[string]any{"max": 0, "used": 4}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr.billingAccountDoc = map[string]any{
				"tier": "pro", "balance": 40, "allowance": 50, "submissionCost": 10,
				"periodEnd": "2026-10-11T00:00:00Z", "privateAgents": tc.private,
			}
			stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "whoami")
			if err != nil {
				t.Fatalf("whoami: %v (stderr %q)", err, stderr)
			}
			if tc.want == "" {
				if strings.Contains(stdout, "private:") {
					t.Errorf("a registry without enabled and max 0 was guessed at:\n%s", stdout)
				}
				return
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("whoami lacks %q:\n%s", tc.want, stdout)
			}
		})
	}
}
