package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	voicebuild "github.com/blaktron/agentry-cli/pkg/voice/build"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

func sourceRow(t *testing.T, source string, locked bool) managedsettings.Setting {
	t.Helper()
	raw, err := json.Marshal(managedsettings.Source{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return managedsettings.Setting{Value: raw, Locked: locked}
}

// creditsRegistry holds this device to settings from a fake registry that
// opens AI Review operations, with no key of the person's own.
func creditsRegistry(t *testing.T, settings map[string]managedsettings.Setting) *fakeRegistry {
	t.Helper()
	isolateEnv(t)
	fr := newFakeRegistry(t)
	holdManagedFrom(t, fr, settings)
	t.Setenv(envRegistry, fr.url())
	t.Setenv(policy.DefaultJudgeKeyEnv, "")
	return fr
}

func TestADraftOnTheOrganizationsCreditsScansThroughOneOperation(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.ReviewSource: sourceRow(t, managedsettings.SourceAgentryCredits, true)})
	var errOut bytes.Buffer
	pay, end := creditsPay(&errOut)
	w, note, err := draftWeb(context.Background(), &policy.Policy{}, draftOpts{}, false, pay)
	if err != nil {
		t.Fatal(err)
	}
	if w.Scan != toolsrv.WebScanRegistry || w.Registry != fr.url() || w.JobToken != "agyr_op1" || w.JevKey != "" || w.Search != "" {
		t.Fatalf("web run %+v", w)
	}
	if !strings.Contains(note, "no web search") || len(fr.reviewOpened) != 1 || fr.reviewOpened[0] != managed.PurposeDraft {
		t.Fatalf("note %q, opened %v", note, fr.reviewOpened)
	}
	if !strings.Contains(errOut.String(), "AI Review is paid with your organization's credits: an operation on "+fr.url()+", 1 credit, spent whether or not the work finishes.") {
		t.Fatalf("said %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "agyr_") {
		t.Fatal("the operation's token was shown")
	}
	end()
	end()
	if len(fr.reviewEnded) != 1 || fr.reviewEnded[0] != "op-1" {
		t.Fatalf("ended %v", fr.reviewEnded)
	}
}

func TestSearchOnTheOrganizationsCreditsIsASearchOperationThroughTheRegistry(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.WebSearchSource: sourceRow(t, managedsettings.SourceAgentryCredits, true)})
	t.Setenv(policy.DefaultJudgeKeyEnv, "ts-own")
	var errOut bytes.Buffer
	pay, end := creditsPay(&errOut)
	defer end()
	w, note, err := draftWeb(context.Background(), &policy.Policy{}, draftOpts{}, false, pay)
	if err != nil {
		t.Fatal(err)
	}
	if w.Scan != toolsrv.WebScanRegistry || w.Search != toolsrv.WebScanRegistry || w.JobToken != "agyr_op1" || w.SearchKey != "" {
		t.Fatalf("web run %+v", w)
	}
	if len(fr.reviewOpened) != 1 || fr.reviewOpened[0] != managed.PurposeSearch || note != "The registry searches the web, paid with your organization's credits." {
		t.Fatalf("note %q, opened %v", note, fr.reviewOpened)
	}
}

func TestAMembersOwnKeySetsACreditsDefaultAside(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.ReviewSource: sourceRow(t, managedsettings.SourceAgentryCredits, false)})
	t.Setenv(policy.DefaultJudgeKeyEnv, "ts-own")
	pay, end := creditsPay(&bytes.Buffer{})
	defer end()
	w, _, err := draftWeb(context.Background(), &policy.Policy{}, draftOpts{}, false, pay)
	if err != nil {
		t.Fatal(err)
	}
	if w.Scan != toolsrv.WebScanJev || w.JevKey != "ts-own" || len(fr.reviewOpened) != 0 {
		t.Fatalf("web run %+v, opened %v", w, fr.reviewOpened)
	}
}

func TestAVoicesPagesAreReadThroughAnOperationOnTheOrganizationsCredits(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.ReviewSource: sourceRow(t, managedsettings.SourceAgentryCredits, true)})
	urls, err := voicebuild.ParseURLs([]string{"https://example.com/about"})
	if err != nil {
		t.Fatal(err)
	}
	pay, end := creditsPay(&bytes.Buffer{})
	defer end()
	sc, w, err := voicePageReview(context.Background(), &policy.Policy{}, urls, &bytes.Buffer{}, managed.PurposeVoice, pay)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := sc.(*webscan.Registry)
	if !ok || reg.Base != fr.url() || reg.JobToken != "agyr_op1" || w.Scan != toolsrv.WebScanRegistry || w.JobToken != "agyr_op1" {
		t.Fatalf("scanner %#v, web run %+v", sc, w)
	}
	if len(fr.reviewOpened) != 1 || fr.reviewOpened[0] != managed.PurposeVoice {
		t.Fatalf("opened %v", fr.reviewOpened)
	}
}

func TestAKnowledgeBuildWithNoSearchSpendsNoCredit(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.ReviewSource: sourceRow(t, managedsettings.SourceAgentryCredits, true)})
	var errOut bytes.Buffer
	pay, end := creditsPay(&errOut)
	defer end()
	w, err := knowledgeWeb(context.Background(), &policy.Policy{}, &knowledgeBuildOpts{}, true, &errOut, pay)
	if err != nil || w != nil {
		t.Fatalf("web run %+v: %v", w, err)
	}
	if len(fr.reviewOpened) != 0 || !strings.Contains(errOut.String(), "builds without the web") {
		t.Fatalf("opened %v, said %q", fr.reviewOpened, errOut.String())
	}
}

func TestAKnowledgeBuildSearchingOnTheOrganizationsCreditsOpensOneOperation(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{
		managedsettings.ReviewSource:    sourceRow(t, managedsettings.SourceAgentryCredits, true),
		managedsettings.WebSearchSource: sourceRow(t, managedsettings.SourceAgentryCredits, true),
	})
	var errOut bytes.Buffer
	pay, end := creditsPay(&errOut)
	defer end()
	w, err := knowledgeWeb(context.Background(), &policy.Policy{}, &knowledgeBuildOpts{}, true, &errOut, pay)
	if err != nil {
		t.Fatal(err)
	}
	if w.Scan != toolsrv.WebScanRegistry || w.Search != toolsrv.WebScanRegistry || w.JobToken != "agyr_op1" {
		t.Fatalf("web run %+v", w)
	}
	if len(fr.reviewOpened) != 1 || fr.reviewOpened[0] != managed.PurposeKnowledge || !strings.Contains(errOut.String(), "spent whether or not the work finishes") {
		t.Fatalf("opened %v, said %q", fr.reviewOpened, errOut.String())
	}
}

func TestAReviewOnTheOrganizationsCreditsAsksJevThroughTheRegistry(t *testing.T) {
	fr := creditsRegistry(t, map[string]managedsettings.Setting{managedsettings.ReviewSource: sourceRow(t, managedsettings.SourceAgentryCredits, true)})
	pay, end := creditsPay(&bytes.Buffer{})
	r, err := examiner.LocalReviewPaid(context.Background(), &policy.Policy{}, pay)
	if err != nil {
		t.Fatal(err)
	}
	if r.JevKey != "agyr_op1" || r.Base != fr.url()+"/api/v1/x/review" || len(fr.reviewOpened) != 1 || fr.reviewOpened[0] != managed.PurposeReview {
		t.Fatalf("review run %+v, opened %v", r, fr.reviewOpened)
	}
	end()
	if len(fr.reviewEnded) != 1 {
		t.Fatalf("ended %v", fr.reviewEnded)
	}
	if _, err := examiner.LocalReview(&policy.Policy{}); err == nil || !strings.Contains(err.Error(), "pay for AI Review with its credits (review.source), and this review on this computer cannot pay with them") {
		t.Fatalf("no payer: %v", err)
	}
}
