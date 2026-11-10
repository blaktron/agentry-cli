package examiner

import (
	"context"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ErrNoJevKey is LocalReview's answer when the person has no TypeSafe key
// of their own: a review does not run without one, since AI Review is asked
// with it and every file the model reads is scanned with it first.
var ErrNoJevKey = drafter.ErrNoJevKey

// LocalReview is a review's key on this computer: the person's own, read as
// a draft reads it (drafter.JevKey). Without one it is ErrNoJevKey. Where an
// organization's credits pay for AI Review, it refuses with the
// organization's sentence: work that can pay with them calls
// LocalReviewPaid.
func LocalReview(pol *policy.Policy) (*engine.ReviewRun, error) {
	return LocalReviewPaid(context.Background(), pol, nil)
}

// LocalReviewPaid is LocalReview for work that can pay with an
// organization's credits: where the managed settings say they pay for AI
// Review, it opens an operation with pay (managed.PurposeReview) and the
// review asks Jev through the registry with the operation's token. A nil
// pay refuses such work with the organization's sentence.
func LocalReviewPaid(ctx context.Context, pol *policy.Policy, pay drafter.Pay) (*engine.ReviewRun, error) {
	key, err := drafter.JevKey(pol)
	if paid := managed.CreditsPaid(err); paid != nil {
		if pay == nil {
			return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("%w, and this review on this computer cannot pay with them", paid))
		}
		op, err := pay(ctx, paid.Registry, managed.PurposeReview)
		if err != nil {
			return nil, engine.Classify(engine.ExitPolicy, err)
		}
		return &engine.ReviewRun{JevKey: op.Token, Base: op.JevBase()}, nil
	}
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrNoJevKey
	}
	return &engine.ReviewRun{JevKey: key}, nil
}
