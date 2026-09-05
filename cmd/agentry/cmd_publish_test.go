// `agentry publish` (§11, §6.5): both submission sources (git-tag and the
// locally packed cli-upload), the credential gate, and the §7 verdict
// mapping — published/needs_review end the poll cleanly, rejected and the
// containment states end it non-zero with the recorded reason.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/apiclient"
)

// publishWorkspace scaffolds a valid skill workspace to submit.
func publishWorkspace(t *testing.T) string {
	t.Helper()
	return writeTree(t, skillFiles("demo", ""))
}

func TestPublishRequiresCredential(t *testing.T) {
	isolateEnv(t)
	_, _, err := runCLI(t, nil, "--registry", "http://127.0.0.1:1", "publish", publishWorkspace(t))
	if err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Errorf("anonymous publish: %v", err)
	}
}

func TestPublishUploadToPublished(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.submitStatus = "published" // terminal on the submit response: no poll

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	// The cli-upload banner carries the client-computed identity the server
	// will cross-check (§6.5).
	if !strings.Contains(stdout, "(cli-upload source)") || !strings.Contains(stdout, "sha256:") {
		t.Errorf("upload banner = %s", stdout)
	}
	if !strings.Contains(stdout, "published acme/demo@0.1.0") {
		t.Errorf("verdict line = %s", stdout)
	}
}

func TestPublishTagSourceRejected(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.submitStatus = "rejected"
	fr.pollReason = "S1 found a binary"

	_, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t), "--tag", "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "rejected: S1 found a binary") {
		t.Errorf("rejected publish: %v", err)
	}
}

func TestPublishPollsToVerdict(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.submitStatus = "scanning" // not terminal: forces one poll round-trip
	fr.pollStatuses = []string{"published"}

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	// The status change is narrated, then the terminal verdict lands.
	if !strings.Contains(stdout, "status: published") || !strings.Contains(stdout, "published acme/demo@0.1.0") {
		t.Errorf("poll narration = %s", stdout)
	}
}

// TestVerdictTable: the §7 terminal-state mapping, without a poll loop.
func TestVerdictTable(t *testing.T) {
	reason := "why"
	digest := "sha256:abc"
	tests := []struct {
		status   string
		reason   *string
		done     bool
		wantErr  string
		wantText string
	}{
		{"published", nil, true, "", "published acme/demo@1.0.0 (sha256:abc)"},
		{"rejected", &reason, true, "rejected: why", ""},
		{"rejected", nil, true, "see the scan report", ""},
		{"needs_review", &reason, true, "", "needs human review (why)"},
		{"quarantined", &reason, true, "ended in state quarantined", ""},
		{"blocked", nil, true, "ended in state blocked", ""},
		{"yanked", nil, true, "ended in state yanked", ""},
		{"scanning", nil, false, "", ""},
		{"submitted", nil, false, "", ""},
		{"approved", nil, false, "", ""},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		v := &apiclient.AgentVersion{Version: "1.0.0", Status: apiclient.VersionStatus(tt.status), StatusReason: tt.reason}
		if tt.status == "published" {
			v.ArchiveDigest = &digest
		}
		done, err := verdict(&out, "acme", "demo", v)
		if done != tt.done {
			t.Errorf("%s: done = %v, want %v", tt.status, done, tt.done)
		}
		if tt.wantErr == "" && err != nil {
			t.Errorf("%s: err = %v", tt.status, err)
		}
		if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tt.status, err, tt.wantErr)
		}
		if tt.wantText != "" && !strings.Contains(out.String(), tt.wantText) {
			t.Errorf("%s: out = %q, want %q", tt.status, out.String(), tt.wantText)
		}
	}
}

func TestPublishEmptyIncludeRefused(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)

	// publish.include matching nothing is the V4 authoring error; the
	// upload path refuses before any bytes travel.
	dir := writeTree(t, map[string]string{"agent.yaml": `schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 0.1.0
summary: "s"
entrypoints:
  instruction: SKILL.md
publish:
  include: [does-not-exist/**]
`})
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", dir)
	if err == nil || !strings.Contains(err.Error(), "matches no files") {
		t.Errorf("empty include: %v", err)
	}
}

// A transport-level or 5xx failure is retried with the SAME Idempotency-Key,
// so the registry replays an already-accepted submission instead of the
// retry colliding on V3 "version exists" (agentry#22).
func TestPublishRetriesUploadWithSameKey(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.submitStatus = "published"
	fr.submitFails = 1

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "retrying with the same Idempotency-Key") {
		t.Errorf("no retry notice in %s", stdout)
	}
	fr.mu.Lock()
	keys := append([]string(nil), fr.submitKeys...)
	fr.mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("upload attempts = %d, want 2 (one 503, one 202)", len(keys))
	}
	if keys[0] == "" || keys[0] != keys[1] || !strings.HasPrefix(keys[0], "publish-") {
		t.Fatalf("Idempotency-Key per attempt = %q; want one non-empty key reused", keys)
	}
}
