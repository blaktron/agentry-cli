package examiner

import (
	"errors"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ErrNoJevKey is LocalReview's answer when the person has no TypeSafe key
// of their own: a review does not run without one (plan D28), since AI
// Review is asked with it and every file the model reads is scanned with it
// first.
var ErrNoJevKey = errors.New("no TypeSafe Jev key of your own was found")

// LocalReview is a review's key on this computer: the person's own, read as
// a draft reads it (drafter.JevKey). Without one it is ErrNoJevKey.
func LocalReview(pol *policy.Policy) (*engine.ReviewRun, error) {
	key, err := drafter.JevKey(pol)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrNoJevKey
	}
	return &engine.ReviewRun{JevKey: key}, nil
}
