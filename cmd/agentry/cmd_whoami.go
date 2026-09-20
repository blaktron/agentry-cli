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
			"overrides the stored one), and the organizations the account belongs\n" +
			"to with its role in each — a membership an admin has yet to approve\n" +
			"says so. A registry with subscription tiers (its /meta lists the\n" +
			"billing extension) adds the tier, the balance out of the allowance and\n" +
			"when the allowance resets; for a member of an organization the tier is\n" +
			"the organization's, the balance is its pool's, and its seats and\n" +
			"standing come with them. --json prints the same as one document.\n" +
			"Without a stored credential the command says so and exits 2.",
		Args: cobra.NoArgs,
		RunE: runWhoami,
	}
}

// billingAccount mirrors the billing extension's GET /api/v1/x/billing/account
// (agentry-enterprise/schemas/x-billing.yaml); the route is the closed
// build's, so it is not in the generated client. An active member of an
// organization is answered the organization's account — its tier, its
// standing and the pool their work spends from — with MemberOf naming it
// (plan organization-subscriptions.md D22); their own row stands
// underneath for the day they leave, and a pending fold-in is still
// answered their own.
type billingAccount struct {
	Kind           string     `json:"kind,omitempty"`
	Tier           string     `json:"tier"`
	Standing       string     `json:"standing,omitempty"`
	Unmetered      bool       `json:"unmetered,omitempty"`
	Seats          *seatsView `json:"seats,omitempty"`
	MemberOf       string     `json:"memberOf,omitempty"`
	Balance        int64      `json:"balance"`
	Allowance      int64      `json:"allowance"`
	SubmissionCost int64      `json:"submissionCost"`
	PeriodEnd      string     `json:"periodEnd"`
	PendingTier    string     `json:"pendingTier,omitempty"`
	PrivateAgents  struct {
		Max  int64 `json:"max"`
		Used int64 `json:"used"`
	} `json:"privateAgents"`
}

// seatsView is the organization's seats: held (active members plus open
// invitations) against the allowance, where 0 means no cap.
type seatsView struct {
	Used int64 `json:"used"`
	Max  int64 `json:"max"`
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
		if u.Orgs != nil {
			doc["orgs"] = *u.Orgs
		}
		if u.PendingOrg != nil {
			doc["pendingOrg"] = u.PendingOrg
		}
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
	for _, line := range orgLines(u) {
		_, _ = fmt.Fprintln(out, line)
	}
	switch {
	case acct != nil:
		writeSubscription(out, acct)
	case accountErr != nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the subscription could not be read: %v\n", accountErr)
	}
	return nil
}

// orgLines renders the memberships GET /user reports. They are the core's
// own facts, so a registry that sells nothing still says which
// organization a person belongs to and in what role; a membership an admin
// has yet to approve says so and names the door the person came through,
// because until then they hold no seat and spend from their own account.
func orgLines(u *apiclient.User) []string {
	if u.Orgs == nil {
		return nil
	}
	lines := make([]string, 0, len(*u.Orgs))
	for _, o := range *u.Orgs {
		line := fmt.Sprintf("org:      %s (%s)", o.Handle, o.Role)
		if o.State != nil && *o.State == apiclient.MembershipStatePending {
			line += " — pending: " + pendingNote(u.PendingOrg, o.Handle)
		}
		lines = append(lines, line)
	}
	return lines
}

// pendingNote says an admin has yet to approve the membership, and how the
// person arrived where GET /user named the door.
func pendingNote(p *apiclient.PendingOrg, handle string) string {
	const yet = "an admin has yet to approve you"
	if p == nil || p.Handle != handle {
		return yet
	}
	switch p.Via {
	case apiclient.PendingOrgViaDomain:
		return "your address is at the organization's domain; " + yet
	case apiclient.PendingOrgViaScim:
		return "the organization's directory provisioned you; " + yet
	}
	return yet
}

// writeSubscription renders the account the billing extension answered.
// For a member of an organization that is the organization's: the tier
// names it, the balance is the shared pool's rather than the person's own
// credits, and the seats and the standing come with them. The client shows
// and never sells, so nothing here offers an upgrade.
func writeSubscription(out io.Writer, a *billingAccount) {
	tier := tierLabel(a.Tier)
	if a.MemberOf != "" {
		tier += ", through " + a.MemberOf
	}
	_, _ = fmt.Fprintf(out, "tier:     %s\n", tier)
	switch pool := a.MemberOf != "" || a.Kind == "org"; {
	case pool && a.Unmetered:
		// Nothing is ever debited, so a balance out of an allowance would
		// be a number that means nothing.
		_, _ = fmt.Fprint(out, "pool:     not metered\n")
	case pool:
		_, _ = fmt.Fprintf(out, "pool:     %d of %d credits (a submission costs %d; resets on %s)\n",
			a.Balance, a.Allowance, a.SubmissionCost, dayOf(a.PeriodEnd))
	default:
		_, _ = fmt.Fprintf(out, "credits:  %d of %d (a submission costs %d; resets on %s)\n",
			a.Balance, a.Allowance, a.SubmissionCost, dayOf(a.PeriodEnd))
	}
	if a.Seats != nil {
		_, _ = fmt.Fprintf(out, "seats:    %s\n", seatsPhrase(*a.Seats))
	}
	if a.Standing != "" {
		_, _ = fmt.Fprintf(out, "standing: %s\n", a.Standing)
	}
	if a.PendingTier != "" {
		_, _ = fmt.Fprintf(out, "changes:  %s from %s\n", tierLabel(a.PendingTier), dayOf(a.PeriodEnd))
	}
	if a.PrivateAgents.Max > 0 {
		_, _ = fmt.Fprintf(out, "private:  %d of %d BriefAgents\n", a.PrivateAgents.Used, a.PrivateAgents.Max)
	}
}

// seatsPhrase renders held seats against the allowance; an allowance of 0
// is no cap, which is what an Enterprise organization is granted.
func seatsPhrase(s seatsView) string {
	if s.Max <= 0 {
		return fmt.Sprintf("%d (no cap)", s.Used)
	}
	return fmt.Sprintf("%d of %d", s.Used, s.Max)
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

// tierLabel is the product's name for a tier — the two a person may hold
// and the two an organization may (plan organization-subscriptions.md §4).
func tierLabel(t string) string {
	switch t {
	case "pro":
		return "Pro"
	case "standard":
		return "Standard"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
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
