package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// --review-inputs: nothing without it; with it, the
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
	// A bad address is a usage error before the credential is looked for,
	// and the error does not echo the address (it could carry a password).
	if _, err := inputReview("https://u:p@x"); !errors.As(err, &ce) || ce.Code != engine.ExitUsage || strings.Contains(err.Error(), "u:p") {
		t.Errorf("a bad address with no credential: %v", err)
	}
	t.Setenv(envJobToken, "  \t ")
	if _, err := inputReview("https://dev.secureagentry.net"); !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "$"+envJobToken) {
		t.Errorf("a credential of spaces: %v", err)
	}
	t.Setenv(envJobToken, " agyj_abc ")
	for _, bad := range []string{"dev.secureagentry.net", "ftp://x", "https://u:p@x", "https://x/?q=1", "https://", "http://dev.secureagentry.net",
		"https://x/#f", "http://10.0.0.1", "http://[2001:db8::1]:8080", "file:///etc"} {
		if _, err := inputReview(bad); !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "https://dev.secureagentry.net/path"} {
		if _, err := inputReview(ok); err != nil {
			t.Errorf("%q is a registry the credential may travel to: %v", ok, err)
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
	// The run itself checks the flag before anything is pulled or written.
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	t.Setenv(envJobToken, "")
	pure := repoPath(t, "testdata/runner/pure-text")
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--policy", policyPath, "--review-inputs", "https://dev.secureagentry.net")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "$"+envJobToken) {
		t.Errorf("err = %v", err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", pure, "--allow-unsigned", "--yes", "--policy", policyPath, "--review-inputs", "ftp://x")
	wantClassified(t, err, engine.ExitUsage)
	if entries, _ := os.ReadDir(filepath.Join(home, "agents")); len(entries) != 0 {
		t.Errorf("a refused run installed %v", entries)
	}
}
