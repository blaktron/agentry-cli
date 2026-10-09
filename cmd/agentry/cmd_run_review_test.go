package main

import (
	"errors"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// --review-inputs (agentry-cli#321): nothing without it; with it, the
// registry named and the job's credential from the environment, or a
// refusal before anything is pulled.
func TestReviewInputsTakesTheRegistryAndTheJobsCredential(t *testing.T) {
	if sc, err := inputReview(""); sc != nil || err != nil {
		t.Fatalf("without the flag: %v, %v", sc, err)
	}
	t.Setenv(envJobToken, "")
	_, err := inputReview("https://dev.secureagentry.net")
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy {
		t.Fatalf("no credential: %v", err)
	}
	t.Setenv(envJobToken, " agyj_abc ")
	for _, bad := range []string{"dev.secureagentry.net", "ftp://x", "https://u:p@x", "https://x/?q=1", "https://"} {
		if _, err := inputReview(bad); !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
			t.Errorf("%q: %v", bad, err)
		}
	}
	sc, err := inputReview("https://dev.secureagentry.net/")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := sc.(*webscan.Registry)
	if !ok || r.Base != "https://dev.secureagentry.net" || r.JobToken != "agyj_abc" {
		t.Fatalf("scanner %#v", sc)
	}
}
