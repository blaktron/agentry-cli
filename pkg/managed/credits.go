package managed

// AI Review and web search paid with an organization's credits. When the
// documents say review.source or webSearch.source is agentry-credits (locked,
// or as a default the member has no key of their own to set aside), work on
// this computer asks Jev, and searches, through the registry: the client,
// with the person's sign-in, opens an operation (POST
// /api/v1/x/review/operations), which costs one credit from the pool
// however many requests it then carries, and is answered a token of its own
// for two hours. A Jev client pointed at OperationJevBase with that token
// asks Jev with the registry's key; the model-input routes (scan, search)
// take the same token as their job credential. Ending the operation
// (DELETE) refuses its token from then on; its credit is not handed back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// PaidWithCredits is ServiceKey's answer when the organization's credits
// pay for a service on this computer: work that can pay with them opens an
// operation on Registry; work that cannot says this sentence.
type PaidWithCredits struct {
	// Setting is review.source or webSearch.source.
	Setting string
	// Org is the organization whose document says so.
	Org string
	// Registry is the registry that document came from, where the
	// operation is opened.
	Registry string
	// Locked is a lock, which no key of the member's sets aside.
	Locked bool
}

func (p *PaidWithCredits) Error() string {
	return fmt.Sprintf("%s's managed settings pay for %s with its credits (%s)", p.Org, serviceWhat[p.Setting], p.Setting)
}

// CreditsPaid is the *PaidWithCredits in err's chain, or nil.
func CreditsPaid(err error) *PaidWithCredits {
	var p *PaidWithCredits
	if errors.As(err, &p) {
		return p
	}
	return nil
}

// The purposes an operation is opened for, as the registry's ledger
// records them. A search operation is charged under its own reason.
const (
	PurposeRun       = "run"
	PurposeDraft     = "draft"
	PurposeKnowledge = "knowledge"
	PurposeVoice     = "voice"
	PurposeReview    = "review"
	PurposeSearch    = "search"
)

// operationsPath is the registry's AI Review route; its Jev forward is at
// the core's judge path beneath it.
const operationsPath = "/api/v1/x/review"

// Operation is one open operation: its id, its token (never shown,
// planned or recorded), when the token lapses, and the registry it is
// open on.
type Operation struct {
	ID        string
	Token     string
	ExpiresAt time.Time
	Registry  string
	// Credits is what opening it cost.
	Credits int

	hc     *http.Client
	signIn string
}

// JevBase is the base a Jev client is pointed at to ask through this
// operation, with Token as its key.
func (o *Operation) JevBase() string { return o.Registry + operationsPath }

// OpenOperation opens an operation on reg for purpose with the person's
// sign-in (signIn, a session token or a token carrying credits:session or
// run:write). A refusal (no credits left, no seat in the pool, a
// deployment without it) is the registry's own sentence.
func OpenOperation(ctx context.Context, hc *http.Client, reg, signIn, purpose string) (*Operation, error) {
	reg = strings.TrimRight(reg, "/")
	if signIn == "" {
		return nil, fmt.Errorf("work paid with an organization's credits opens an operation on %s with your sign-in, and you are not signed in; run agentry login", reg)
	}
	body, err := json.Marshal(map[string]string{"purpose": purpose})
	if err != nil {
		return nil, err
	}
	res, err := send(ctx, hc, http.MethodPost, reg+operationsPath+"/operations", signIn, body)
	if err != nil {
		return nil, fmt.Errorf("open an AI Review operation on %s: %w", reg, err)
	}
	var out struct {
		Operation struct {
			ID        string    `json:"id"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"operation"`
		Token   string `json:"token"`
		Credits int    `json:"credits"`
	}
	if err := answer(res, http.StatusCreated, &out); err != nil {
		return nil, fmt.Errorf("the registry did not open an AI Review operation paid with credits: %w", err)
	}
	if out.Token == "" || out.Operation.ID == "" {
		return nil, errors.New("the registry opened an AI Review operation and answered no token")
	}
	return &Operation{ID: out.Operation.ID, Token: out.Token, ExpiresAt: out.Operation.ExpiresAt, Registry: reg, Credits: out.Credits,
		hc: hc, signIn: signIn}, nil
}

// End ends the operation: its token is refused from then on. A nil
// operation, or one already ended, is nothing to do.
func (o *Operation) End(ctx context.Context) error {
	if o == nil || o.signIn == "" {
		return nil
	}
	res, err := send(ctx, o.hc, http.MethodDelete, o.Registry+operationsPath+"/operations/"+o.ID, o.signIn, nil)
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := answer(res, http.StatusOK, &out); err != nil {
		return err
	}
	o.signIn = ""
	return nil
}
