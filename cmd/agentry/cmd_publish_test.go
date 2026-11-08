// `agentry publish`: both submission sources (git-tag and the locally
// packed cli-upload), the credential gate, and the verdict mapping —
// published/needs_review end the poll cleanly, rejected and the
// containment states end it non-zero with the recorded reason.
package main

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
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
	// will cross-check.
	if !strings.Contains(stdout, "(cli-upload source)") || !strings.Contains(stdout, "sha256:") {
		t.Errorf("upload banner = %s", stdout)
	}
	banner := regexp.MustCompile(`submitting acme/demo 1\.0\.0 \(cli-upload source\): 2 file\(s\), ([1-9][0-9]*) bytes, (sha256:[0-9a-f]{64})\n`).FindStringSubmatch(stdout)
	if banner == nil {
		t.Fatalf("the banner does not give the file count, the size and the digest: %s", stdout)
	}
	// The digest the banner names is the one the upload claims.
	fr.mu.Lock()
	parts := fr.submitParts
	fr.mu.Unlock()
	if len(parts) != 1 || parts[0]["digest"].body != banner[2] {
		t.Errorf("the upload claims %v, the banner says %s", parts, banner[2])
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

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t), "--tag", "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "rejected: S1 found a binary") {
		t.Errorf("rejected publish: %v", err)
	}
	// The tag is what is submitted, and the output says that is what it is.
	if !strings.Contains(stdout, "submitting acme/demo tag v0.1.0 (git-tag source)\n") {
		t.Errorf("tag banner = %s", stdout)
	}
	fr.mu.Lock()
	tags := fr.submitTags
	fr.mu.Unlock()
	if len(tags) != 1 || !strings.Contains(tags[0], `"tag":"v0.1.0"`) {
		t.Errorf("tag submissions %v", tags)
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
	if !strings.Contains(stdout, "version 0.1.0 accepted (status scanning) — waiting for the pipeline verdict\n") {
		t.Errorf("the accepted version is not announced: %s", stdout)
	}
}

// TestVerdictTable: the terminal-state mapping, without a poll loop.
func TestVerdictTable(t *testing.T) {
	reason := "why"
	digest := "sha256:abc"
	skipped := apiclient.AgentVersionScanStatusSkipped
	tests := []struct {
		status   string
		reason   *string
		done     bool
		wantErr  string
		wantText string
		scan     *apiclient.AgentVersionScanStatus
	}{
		{"published", nil, true, "", "published acme/demo@1.0.0 (sha256:abc)", nil},
		{"published", nil, true, "", "published acme/demo@1.0.0 (sha256:abc) — unscanned: the registry admitted it without a scan, and installs and runs will ask for --allow-unscanned\n", &skipped},
		{"rejected", &reason, true, "rejected: why", "", nil},
		{"rejected", nil, true, "see the scan report", "", nil},
		{"needs_review", &reason, true, "", "needs human review (why)", nil},
		{"quarantined", &reason, true, "ended in state quarantined", "", nil},
		{"blocked", nil, true, "ended in state blocked", "", nil},
		{"yanked", nil, true, "ended in state yanked", "", nil},
		{"scanning", nil, false, "", "", nil},
		{"submitted", nil, false, "", "", nil},
		{"approved", nil, false, "", "", nil},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		v := &apiclient.AgentVersion{Version: "1.0.0", Status: apiclient.VersionStatus(tt.status), StatusReason: tt.reason}
		if tt.status == "published" {
			v.ArchiveDigest = &digest
		}
		v.ScanStatus = tt.scan
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
		if tt.scan == nil && strings.Contains(out.String(), "unscanned") {
			t.Errorf("%s: a scanned version is said to be unscanned: %q", tt.status, out.String())
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

	// A release past what the registry takes through an upload is refused
	// the same way, with the tag named as the way round.
	files := skillFiles("big", "")
	blob := make([]byte, 17<<20)
	rand.New(rand.NewSource(1)).Read(blob) // incompressible
	files["blob.dat"] = string(blob)
	files["agent.yaml"] = strings.Replace(files["agent.yaml"], "include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, blob.dat]", 1)
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "publish", writeTree(t, files))
	if err == nil || !strings.Contains(err.Error(), "this release's upload is 17.0 MiB compressed; the registry takes at most 16.0 MiB through an upload") ||
		!strings.Contains(err.Error(), "publish it from its tag instead") {
		t.Errorf("an upload past the cap: %v", err)
	}
	fr.mu.Lock()
	sent := len(fr.submitKeys)
	fr.mu.Unlock()
	if sent != 0 {
		t.Errorf("%d upload(s) sent of a release past the cap", sent)
	}
}

// A transport-level or 5xx failure is retried with the SAME Idempotency-Key,
// so the registry replays an already-accepted submission instead of the
// retry colliding on V3 "version exists".
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
	if !strings.Contains(stdout, "upload attempt 1 failed (upload version: registry answered 503); retrying with the same Idempotency-Key\n") {
		t.Errorf("the retry notice: %s", stdout)
	}

	// Another publish is another key.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", publishWorkspace(t)); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	keys = append([]string(nil), fr.submitKeys...)
	fr.mu.Unlock()
	if len(keys) != 3 || keys[2] == keys[0] {
		t.Errorf("Idempotency-Key of a second publish = %q; want a key of its own", keys)
	}

	// Three attempts are made and no more: the third failure is the error.
	third := newFakeRegistry(t)
	third.submitFails = 3
	_, _, err = runCLI(t, nil, "--registry", third.url(), "publish", publishWorkspace(t))
	if err == nil || err.Error() != "upload version: registry answered 503" {
		t.Errorf("an upload that fails every time: %v", err)
	}
	third.mu.Lock()
	attempts := len(third.submitKeys)
	third.mu.Unlock()
	if attempts != 3 {
		t.Errorf("%d upload attempts, want 3", attempts)
	}

	// An answer that is a refusal is not retried.
	refused := newFakeRegistry(t)
	refused.submitProblem = &fakeProblem{Status: 422, Type: "about:blank", Title: "Unprocessable", Detail: "no"}
	_, _, err = runCLI(t, nil, "--registry", refused.url(), "publish", publishWorkspace(t))
	refused.mu.Lock()
	attempts = len(refused.submitKeys)
	refused.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "Unprocessable") || attempts != 1 {
		t.Errorf("a refused upload: %v after %d attempt(s)", err, attempts)
	}
}

// TestPublishRefusesAScaffoldLineBeforeUploading: an upload whose files
// still carry a line a scaffold wrote for the author is refused by name, and
// nothing reaches the registry; once the line is written, it uploads.
func TestPublishRefusesAScaffoldLineBeforeUploading(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.submitStatus = "published"
	dir := publishWorkspace(t)
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	placeholder := strings.Replace(string(raw), "namespace: acme", "namespace: your-namespace", 1)
	if placeholder == string(raw) {
		t.Fatalf("the fixture's namespace moved:\n%s", raw)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(placeholder), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "publish", dir)
	if err == nil || !strings.Contains(err.Error(), "publish refused") || !strings.Contains(stderr, "agent.yaml: placeholder namespace") {
		t.Fatalf("err = %v\nstderr %s", err, stderr)
	}
	if fr.submitParts != nil {
		t.Fatal("a refused publish reached the registry")
	}

	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "publish", dir); err != nil {
		t.Fatalf("the finished package: %v", err)
	}
}
