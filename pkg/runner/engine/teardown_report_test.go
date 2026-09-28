package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/removal"
)

func holdSecrets(t *testing.T) {
	t.Helper()
	old := removeSensitivePath
	removeSensitivePath = func(path string, all bool) error {
		if filepath.Base(path) == "secrets.env" {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return removal.Remove(path, all)
	}
	t.Cleanup(func() { removeSensitivePath = old })
}

// A secrets file teardown could not delete does not end the run as if it
// had been: stderr says so, the receipt says so (written again after
// teardown), and the run fails (agentry-cli#422).
func TestTeardownThatCannotDeleteSecretsSaysSo(t *testing.T) {
	holdSecrets(t)
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true // so the held file is still there when the run ends
	var stderr bytes.Buffer
	p.Stderr = &stderr
	env, err := Run(context.Background(), f, p)
	if err == nil {
		t.Fatal("the run ended without an error, with secrets.env left on disk")
	}
	if !strings.Contains(stderr.String(), "could not delete") || !strings.Contains(stderr.String(), "secrets.env") {
		t.Errorf("stderr does not name the file:\n%s", stderr.String())
	}
	raw, rerr := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var written struct {
		Status string            `json:"status"`
		Caps   map[string]string `json:"capabilities_enforced"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if written.Status != "error" || !strings.Contains(written.Caps["teardown"], "secrets.env") {
		t.Errorf("result.json: status %q, teardown line %q", written.Status, written.Caps["teardown"])
	}
	if env.Status != "error" {
		t.Errorf("envelope status %q", env.Status)
	}
}

// A delete that failed but was then taken by the run directory's own
// removal is not reported: nothing is left.
func TestTeardownDoesNotReportWhatTheRunDirRemovalTook(t *testing.T) {
	holdSecrets(t)
	p, f := fixture(t)
	happyFake(f)
	var stderr bytes.Buffer
	p.Stderr = &stderr
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(stderr.String(), "could not delete") {
		t.Errorf("reported a file that is gone:\n%s", stderr.String())
	}
}
