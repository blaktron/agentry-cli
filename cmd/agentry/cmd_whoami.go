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

// newWhoamiCmd builds the whoami command: the registry, the account the
// stored credential belongs to and, where the registry mounts the
// billing extension, the subscription tier and the credits. No client
// takes money: the upgrade is the site's, and this command only says
// where the account stands.
func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the account the stored credential belongs to, and its subscription where the registry has tiers",
		Long: "Prints the registry, the handle and the email of the credential agentry\n" +
			"would use (the --registry flag, else the default registry; an\n" +
			"environment token scoped to that registry, " + envSecureAgentryToken + " or\n" +
			envToken + ", is used in place of the stored one), and the organizations\n" +
			"the account belongs to with its role in each — a membership an admin\n" +
			"has yet to approve says so. A registry with subscription tiers (its\n" +
			"/meta lists the billing extension) adds the tier, the balance out of\n" +
			"the allowance and when the allowance resets; for a member of an\n" +
			"organization the tier is the organization's, the balance is its\n" +
			"pool's, and its seats and standing come with them. --json prints the\n" +
			"same as one document. Without a stored credential the command says\n" +
			"so and exits 2.",
		Args: cobra.NoArgs,
		RunE: runWhoami,
	}
}

// billingAccount mirrors the billing extension's GET /api/v1/x/billing/account; the
// route is the closed build's, so it is not in the generated client. An active member of an
// organization is answered the organization's account — its tier, its standing
// and the pool their work spends from — with MemberOf naming it; their own row
// stands underneath for the day they leave, and a pending fold-in is still
// answered their own.
type billingAccount struct {
	Tier           string            `json:"tier"`
	Standing       string            `json:"standing,omitempty"`
	Unmetered      bool              `json:"unmetered,omitempty"`
	Seats          *seatsView        `json:"seats,omitempty"`
	MemberOf       string            `json:"memberOf,omitempty"`
	Balance        int64             `json:"balance"`
	Allowance      int64             `json:"allowance"`
	SubmissionCost int64             `json:"submissionCost"`
	PeriodEnd      string            `json:"periodEnd"`
	PendingTier    string            `json:"pendingTier,omitempty"`
	PrivateAgents  privateAgentsView `json:"privateAgents"`
	Promo          *promoView        `json:"promo,omitempty"`
}

// promoView is a promo trial on a person's own account: state "running"
// while the trial runs, "eligible" while the account may still redeem a
// code. During a trial the code's credits are the account's only
// credits, so they, not the allowance (0 for Promo Pro), are what the
// balance is of.
type promoView struct {
	State   string `json:"state"`
	Code    string `json:"code,omitempty"`
	Credits int64  `json:"credits,omitempty"`
	EndsAt  string `json:"endsAt,omitempty"`
}

// runningPromo is the account's promo trial while it runs, else nil.
func (a *billingAccount) runningPromo() *promoView {
	if a.Promo != nil && a.Promo.State == "running" {
		return a.Promo
	}
	return nil
}

// privateAgentsView is the private-agent cap as the registry applies it.
// Enabled is a pointer because an older registry does not send it, and
// absence must not read as false: there a max of 0 meant both "none" and
// "no cap".
type privateAgentsView struct {
	Enabled *bool `json:"enabled,omitempty"`
	Max     int64 `json:"max"`
	Used    int64 `json:"used"`
}

// seatsView is the organization's seats: held (active members plus open
// invitations) against the allowance, where 0 means no cap.
type seatsView struct {
	Used int64 `json:"used"`
	Max  int64 `json:"max"`
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	reg := registryURL()
	token, err := credentialFor(reg)
	if err != nil {
		return err
	}
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

	acct, raw, accountErr := fetchBillingAccount(cmd.Context(), client, reg, token)

	if flags.jsonOut {
		doc := map[string]any{"registry": reg, "handle": u.Handle, "email": u.Email, "displayName": u.DisplayName}
		if u.Orgs != nil {
			doc["orgs"] = *u.Orgs
		}
		if u.PendingOrg != nil {
			doc["pendingOrg"] = *u.PendingOrg
		}
		if raw != nil {
			// The registry's own account document, not this command's
			// copy of the fields it renders: a field the extension adds
			// reaches --json without an edit here, and none is silently
			// dropped.
			doc["account"] = raw
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
	writeOrgs(out, u)
	switch {
	case acct != nil:
		writeSubscription(out, acct)
	case accountErr != nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the subscription could not be read: %v\n", accountErr)
	}
	return nil
}

// writeOrgs renders the memberships GET /user reports. They are the core's
// own facts, so a registry that sells nothing still says which
// organization a person belongs to and in what role; a membership an admin
// has yet to approve says so and names the door the person came through,
// because until then they hold no seat and spend from their own account.
func writeOrgs(out io.Writer, u *apiclient.User) {
	if u.Orgs == nil {
		return
	}
	for _, o := range *u.Orgs {
		line := fmt.Sprintf("org:      %s (%s)", o.Handle, o.Role)
		if o.State != nil && *o.State == apiclient.MembershipStatePending {
			line += " — pending: " + pendingNote(u.PendingOrg, o.Handle)
		}
		_, _ = fmt.Fprintln(out, line)
	}
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
	pool := a.MemberOf != ""
	promo := a.runningPromo()
	switch {
	case pool && a.Unmetered:
		// Nothing is ever debited, so a balance out of an allowance would
		// be a number that means nothing.
		_, _ = fmt.Fprint(out, "pool:     not metered\n")
	case pool && a.Standing != standingActive:
		// The extension grants no allowance to an organization that is
		// not paid for, so the balance is whatever the ledger last left
		// and there is no reset to promise: saying "0 of 0, resets on the
		// 11th" would be a promise the registry will not keep.
		_, _ = fmt.Fprintf(out, "pool:     %d credits, and no allowance while the subscription is not active\n", a.Balance)
	case pool:
		_, _ = fmt.Fprintf(out, "pool:     %d of %d credits (a submission costs %d; resets on %s)\n",
			a.Balance, a.Allowance, a.SubmissionCost, dayOf(a.PeriodEnd))
	case promo != nil && promo.Credits > 0:
		// Nothing resets: the promo credits expire at the trial's end and
		// Standard's allowance starts then.
		_, _ = fmt.Fprintf(out, "credits:  %d of %d promo credits (a submission costs %d; they expire on %s)\n",
			a.Balance, promo.Credits, a.SubmissionCost, dayOf(a.promoEnd()))
	default:
		_, _ = fmt.Fprintf(out, "credits:  %d of %d (a submission costs %d; resets on %s)\n",
			a.Balance, a.Allowance, a.SubmissionCost, dayOf(a.PeriodEnd))
	}
	if promo != nil {
		_, _ = fmt.Fprintf(out, "promo:    code %s, until %s; you will not be charged\n", promo.Code, dayOf(a.promoEnd()))
	}
	if a.Seats != nil {
		_, _ = fmt.Fprintf(out, "seats:    %s\n", seatsPhrase(*a.Seats))
	}
	if a.Standing != "" {
		_, _ = fmt.Fprintf(out, "standing: %s\n", standingPhrase(a.Standing))
	}
	if a.PendingTier != "" {
		_, _ = fmt.Fprintf(out, "changes:  %s from %s\n", tierLabel(a.PendingTier), dayOf(a.PeriodEnd))
	}
	if line := privatePhrase(a.PrivateAgents); line != "" {
		_, _ = fmt.Fprintf(out, "private:  %s\n", line)
	}
}

// privatePhrase renders the private-agent cap. Enabled is read first:
// false is "none", and max then says nothing (an organization that is not
// paid for, and its members); enabled with max 0 is no cap; otherwise at
// most max. A registry that does not send enabled gets the old rule, a
// line only when max > 0, because its 0 is ambiguous and the client does
// not guess. Agents already held while none may be added are named, so a
// person is not told "none" beside agents they own.
func privatePhrase(p privateAgentsView) string {
	switch {
	case p.Enabled == nil && p.Max <= 0:
		return ""
	case p.Enabled != nil && !*p.Enabled && p.Used > 0:
		return fmt.Sprintf("none may be added (%d QuickAgents held)", p.Used)
	case p.Enabled != nil && !*p.Enabled:
		return "none"
	case p.Max <= 0:
		return fmt.Sprintf("%d QuickAgents, no cap", p.Used)
	}
	return fmt.Sprintf("%d of %d QuickAgents", p.Used, p.Max)
}

// seatsPhrase renders held seats against the allowance. The extension's
// seat maximum is 0 for no cap, which is what an Enterprise organization
// is configured with by default (AGENTRY_BILLING_ORG_ENTERPRISE_SEATS_MAX);
// a Business one is validated to at least 1, so the unbounded phrase is
// only ever the registry's own meaning, never this client's guess.
func seatsPhrase(s seatsView) string {
	if s.Max <= 0 {
		return fmt.Sprintf("%d (no cap)", s.Used)
	}
	return fmt.Sprintf("%d of %d", s.Used, s.Max)
}

// standingActive is the one standing that admits work; the extension applies
// the same test.
const standingActive = "active"

// standingPhrase glosses an organization's standing with the consequence
// the registry's own refusal gives it, so "restricted" reaches a person as
// what it means rather than as a column value.
func standingPhrase(s string) string {
	switch s {
	case standingActive:
		return "active"
	case "pending":
		return "pending — awaiting payment; nothing is admitted until the subscription starts"
	case "restricted":
		return "restricted — the subscription has lapsed; nothing is admitted until it is paid again"
	case "decommissioning":
		return "decommissioning — the organization is closing and admits nothing"
	}
	return s
}

// fetchBillingAccount returns nil for a registry without the billing
// extension; an error only when the registry has it and the read failed.
// The raw document comes back beside the parsed one so --json can print
// what the registry said rather than this command's copy of it.
func fetchBillingAccount(ctx context.Context, client *apiclient.ClientWithResponses, reg, token string) (*billingAccount, json.RawMessage, error) {
	meta, err := client.GetMetaWithResponse(ctx)
	if err != nil || meta.JSON200 == nil || meta.JSON200.Extensions == nil {
		return nil, nil, nil
	}
	mounted := false
	for _, e := range *meta.JSON200.Extensions {
		if e == "billing" {
			mounted = true
		}
	}
	if !mounted {
		return nil, nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg+"/api/v1/x/billing/account", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		var p apiclient.Problem
		if json.Unmarshal(body, &p) == nil && p.Title != "" {
			return nil, nil, apiProblem("read subscription", res.StatusCode, &p, body)
		}
		return nil, nil, apiProblem("read subscription", res.StatusCode, nil, body)
	}
	var acct billingAccount
	if err := json.Unmarshal(body, &acct); err != nil {
		return nil, nil, fmt.Errorf("read subscription: %w", err)
	}
	return &acct, json.RawMessage(body), nil
}

// promoEnd is the day the running promo trial ends: its own endsAt, else
// the period's end, which billing sets to the same instant.
func (a *billingAccount) promoEnd() string {
	if a.Promo != nil && a.Promo.EndsAt != "" {
		return a.Promo.EndsAt
	}
	return a.PeriodEnd
}

// tierLabel is the product's name for a tier — the three a person may
// hold and the two an organization may.
func tierLabel(t string) string {
	switch t {
	case "pro":
		return "Pro"
	case "promo_pro":
		return "Promo Pro"
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
