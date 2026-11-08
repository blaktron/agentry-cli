package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// publishAgainst runs `agentry publish` on a fresh package against fr.
func publishAgainst(t *testing.T, fr *fakeRegistry, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, nil, append([]string{"--registry", fr.url()}, append(args, "publish", publishWorkspace(t))...)...)
}

func newPublishRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	return newFakeRegistry(t)
}

// TestPublishSaysWhatHappenedAndWhereToLook: each of the four outcomes says
// what happened and where to look, and held and rejected exit with their own
// codes.
func TestPublishSaysWhatHappenedAndWhereToLook(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		fr := newPublishRegistry(t)
		stdout, _, err := publishAgainst(t, fr)
		if err != nil || !strings.Contains(stdout, "published acme/demo@") ||
			!strings.Contains(stdout, "report: "+fr.url()+"/api/v1/agents/acme/demo/versions/0.1.0/report") {
			t.Fatalf("%v\n%s", err, stdout)
		}
	})
	t.Run("held for review", func(t *testing.T) {
		fr := newPublishRegistry(t)
		fr.submitStatus = "needs_review"
		fr.pollReason = "a high finding"
		stdout, _, err := publishAgainst(t, fr)
		wantClassified(t, err, exitPublishHeld)
		if !strings.Contains(stdout, "needs human review (a high finding)") || !strings.Contains(stdout, "report: ") {
			t.Fatalf("%s", stdout)
		}
	})
	t.Run("rejected, with its findings", func(t *testing.T) {
		fr := newPublishRegistry(t)
		fr.submitStatus = "rejected"
		fr.pollReason = "a critical finding"
		fr.reportFindings = []map[string]any{{
			"scanner": "S4", "severity": "critical", "confidence": "high", "title": "an instruction override",
			"filePath": "SKILL.md", "lineStart": 3, "verdict": "open", "detail": map[string]any{"ruleId": "S4-017"},
		}}
		stdout, _, err := publishAgainst(t, fr)
		wantClassified(t, err, exitPublishRejected)
		if !strings.Contains(stdout, "findings (1):") || !strings.Contains(stdout, "CRITICAL S4-017 SKILL.md:3 — an instruction override") || !strings.Contains(stdout, "report: ") {
			t.Fatalf("%s", stdout)
		}
		if !strings.Contains(err.Error(), "rejected: a critical finding") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no record", func(t *testing.T) {
		fr := newPublishRegistry(t)
		fr.submitProblem = &fakeProblem{Status: 404, Type: "https://agentry.dev/problems/not-found", Title: "Not found"}
		_, _, err := publishAgainst(t, fr)
		if err == nil || !strings.Contains(err.Error(), "no QuickAgent acme/demo this credential can publish to") || !strings.Contains(err.Error(), "POST "+fr.url()+"/api/v1/agents") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestPublishRefusesAVersionThatIsNotNewer: a version the registry already
// has, or one not above its channel's newest, is refused before anything is
// uploaded, naming the version it must pass.
func TestPublishRefusesAVersionThatIsNotNewer(t *testing.T) {
	for _, existing := range []string{"1.0.0", "2.0.0"} {
		fr := newPublishRegistry(t)
		fr.existingVersions = []map[string]any{fr.versionDoc(existing, "published")}
		_, _, err := publishAgainst(t, fr)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "nothing was uploaded") || !strings.Contains(err.Error(), existing) {
			t.Fatalf("existing %s: %v", existing, err)
		}
		if fr.submitParts != nil {
			t.Fatalf("existing %s: an upload reached the registry", existing)
		}
	}
	fr := newPublishRegistry(t)
	fr.existingVersions = []map[string]any{fr.versionDoc("0.9.0", "published")}
	if _, _, err := publishAgainst(t, fr); err != nil {
		t.Fatalf("a newer version: %v", err)
	}
}

// TestPublishJSON: --json prints one result object on stdout, the narration
// on stderr, for a published and a held version alike.
func TestPublishJSON(t *testing.T) {
	fr := newPublishRegistry(t)
	stdout, stderr, err := publishAgainst(t, fr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var r publishResult
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("stdout is not one result: %v\n%s", err, stdout)
	}
	if r.Outcome != "published" || r.Namespace != "acme" || r.Name != "demo" || r.Version != "0.1.0" || r.Status != "published" ||
		!strings.HasSuffix(r.ReportURL, "/versions/0.1.0/report") || r.Findings == nil {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(stderr, "published acme/demo@") {
		t.Fatalf("the narration is not on stderr: %s", stderr)
	}
	var keys map[string]any
	_ = json.Unmarshal([]byte(stdout), &keys)
	for _, k := range []string{"namespace", "name", "version", "outcome", "status", "reportUrl", "findings"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("the result lacks %q: %s", k, stdout)
		}
	}

	fr = newPublishRegistry(t)
	fr.submitStatus = "needs_review"
	stdout, _, err = publishAgainst(t, fr, "--json")
	wantClassified(t, err, exitPublishHeld)
	if err := json.Unmarshal([]byte(stdout), &r); err != nil || r.Outcome != "held" {
		t.Fatalf("held: %v %+v", err, r)
	}
}
