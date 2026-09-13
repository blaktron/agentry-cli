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
