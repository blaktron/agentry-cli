package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
)

// newWhoamiCmd builds the whoami command (plan subscription-tiers.md §8,
// agentry-cli#128): the registry, the account the stored credential
// belongs to and, where the registry mounts the billing extension, the
// subscription tier and the credits. No client takes money: the upgrade
// is the site's, and this command only says where the account stands.
func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the account the stored credential belongs to, and its subscription where the registry has tiers",
		Long: "Prints the registry, the handle and the email of the credential agentry\n" +
			"would use (the --registry flag, else the default registry; " + envToken + "\n" +
			"overrides the stored one). A registry with subscription tiers (its /meta\n" +
			"lists the billing extension) adds the tier, the balance out of the\n" +
			"allowance and when the allowance resets. --json prints the same as one\n" +
			"document. Without a stored credential the command says so and exits 2.",
		Args: cobra.NoArgs,
		RunE: runWhoami,
	}
}

// billingAccount mirrors the billing extension's GET /api/v1/x/billing/account
// (agentry-enterprise/schemas/x-billing.yaml); the route is the closed
// build's, so it is not in the generated client.
type billingAccount struct {
	Tier           string `json:"tier"`
	Balance        int64  `json:"balance"`
	Allowance      int64  `json:"allowance"`
	SubmissionCost int64  `json:"submissionCost"`
	PeriodEnd      string `json:"periodEnd"`
	PendingTier    string `json:"pendingTier,omitempty"`
	PrivateAgents  struct {
		Max  int64 `json:"max"`
		Used int64 `json:"used"`
	} `json:"privateAgents"`
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	token := credentialFor(reg)
	if token == "" {
		return &exitError{code: 2, err: fmt.Errorf("no credential stored for %s; public agents only — run `agentry login`", reg)}
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	resp, err := client.GetCurrentUserWithResponse(cmd.Context())
	if err != nil {
		return fmt.Errorf("read account from %s: %w", reg, err)
	}
	if resp.JSON200 == nil {
		return apiProblem("read account", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	u := resp.JSON200

	acct, accountErr := fetchBillingAccount(cmd.Context(), client, reg, token)

	if flags.jsonOut {
		doc := map[string]any{"registry": reg, "handle": u.Handle, "email": u.Email, "displayName": u.DisplayName}
		if acct != nil {
			doc["account"] = acct
		}
		if accountErr != nil {
			doc["accountError"] = accountErr.Error()
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "registry: %s\nhandle:   %s\nemail:    %s\n", reg, u.Handle, u.Email)
	switch {
	case acct != nil:
		_, _ = fmt.Fprintf(out, "tier:     %s\ncredits:  %d of %d (a submission costs %d; resets on %s)\n",
			tierLabel(acct.Tier), acct.Balance, acct.Allowance, acct.SubmissionCost, dayOf(acct.PeriodEnd))
		if acct.PendingTier != "" {
			_, _ = fmt.Fprintf(out, "changes:  %s from %s\n", tierLabel(acct.PendingTier), dayOf(acct.PeriodEnd))
		}
		if acct.PrivateAgents.Max > 0 {
			_, _ = fmt.Fprintf(out, "private:  %d of %d BriefAgents\n", acct.PrivateAgents.Used, acct.PrivateAgents.Max)
		}
	case accountErr != nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the subscription could not be read: %v\n", accountErr)
	}
	return nil
}

// fetchBillingAccount returns nil, nil for a registry without the billing
// extension; an error only when the registry has it and the read failed.
func fetchBillingAccount(ctx context.Context, client *apiclient.ClientWithResponses, reg, token string) (*billingAccount, error) {
	meta, err := client.GetMetaWithResponse(ctx)
	if err != nil || meta.JSON200 == nil || meta.JSON200.Extensions == nil {
		return nil, nil
	}
	mounted := false
	for _, e := range *meta.JSON200.Extensions {
		if e == "billing" {
			mounted = true
		}
	}
	if !mounted {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg+"/api/v1/x/billing/account", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		var p apiclient.Problem
		if json.Unmarshal(body, &p) == nil && p.Title != "" {
			return nil, apiProblem("read subscription", res.StatusCode, &p, body)
		}
		return nil, apiProblem("read subscription", res.StatusCode, nil, body)
	}
	var acct billingAccount
	if err := json.Unmarshal(body, &acct); err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	return &acct, nil
}

func tierLabel(t string) string {
	switch t {
	case "pro":
		return "Pro"
	case "standard":
		return "Standard"
	}
	return t
}

// dayOf renders an RFC 3339 instant as its UTC day, the way the site does.
func dayOf(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("2 January 2006")
}

// exitError carries an exit class for a command outside `run` (whose
// classes are the engine's): 2 is the usage class the runner and the
// session commands already use for "cannot start".
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

var _ error = (*exitError)(nil)

// exitCodeOf is main's hook for exitError.
func exitCodeOf(err error) (int, bool) {
	var e *exitError
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}
