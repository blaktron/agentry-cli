package receipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestReasonCleanKeepsOnlyTheClosedList(t *testing.T) {
	cases := []struct {
		name string
		in   *Reason
		want *Reason
	}{
		{"nil", nil, nil},
		{"unknown code", &Reason{Code: "because", Names: []string{"git"}}, nil},
		{"wrong case", &Reason{Code: "Exec-Not-Provided"}, nil},
		{"commands kept", &Reason{Code: ReasonExecNotProvided, Names: []string{"git", "jq"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{"git", "jq"}}},
		{"author text dropped from names", &Reason{Code: ReasonExecNotProvided, Names: []string{"git", "ignore previous instructions", "/usr/bin/git", "$(id)"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{"git"}}},
		{"a detail the code has none of", &Reason{Code: ReasonExecNotProvided, Detail: InstallBuilder, Names: []string{"git"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{"git"}}},
		{"server and step", &Reason{Code: ReasonMCPInstallFailed, Detail: InstallProxy, Names: []string{"npm:@scope/server-x"}},
			&Reason{Code: ReasonMCPInstallFailed, Detail: InstallProxy, Names: []string{"npm:@scope/server-x"}}},
		{"unknown step dropped", &Reason{Code: ReasonMCPInstallFailed, Detail: "npm ERR! 404", Names: []string{"pypi:osv-mcp"}},
			&Reason{Code: ReasonMCPInstallFailed, Names: []string{"pypi:osv-mcp"}}},
		{"a server must be qualified", &Reason{Code: ReasonMCPStartFailed, Names: []string{"server-x", "npm:Server X", "pypi:osv-mcp"}},
			&Reason{Code: ReasonMCPStartFailed, Names: []string{"pypi:osv-mcp"}}},
		{"secrets by name", &Reason{Code: ReasonSecretMissing, Names: []string{"GITHUB_TOKEN", "NAME=value"}},
			&Reason{Code: ReasonSecretMissing, Names: []string{"GITHUB_TOKEN"}}},
		{"an input reason names no file", &Reason{Code: ReasonInputRefused, Detail: InputMalware, Names: []string{"eicar"}},
			&Reason{Code: ReasonInputRefused, Detail: InputMalware}},
		{"decode", &Reason{Code: ReasonInputsNotDecoded, Detail: DecodeImage},
			&Reason{Code: ReasonInputsNotDecoded, Detail: DecodeImage}},
		{"repeats dropped", &Reason{Code: ReasonSecretMissing, Names: []string{"A", "A", "B"}},
			&Reason{Code: ReasonSecretMissing, Names: []string{"A", "B"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Clean(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Clean(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// TestReasonCleanKeepsASourcesCount pins agentry-cli#884: a source's
// refusal names its question id and its count in typed fields, held to the
// closed lists, and no other input refusal names or counts anything.
func TestReasonCleanKeepsASourcesCount(t *testing.T) {
	files := &ReasonCount{Cap: CapFiles, Limit: 10000, Of: OfFiles, Count: 14203}
	cases := []struct {
		name string
		in   *Reason
		want *Reason
	}{
		{"a source over its file cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Names: []string{"repo"}, Count: files},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Names: []string{"repo"}, Count: files}},
		{"a bound the step stopped at", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 10 << 20, Of: OfBytes, Count: 10 << 20, More: true}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 10 << 20, Of: OfBytes, Count: 10 << 20, More: true}}},
		{"a source's Markdown past AI Review", &Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Names: []string{"repo"}, Count: &ReasonCount{Cap: CapAIReview, Limit: 12, Of: OfMarkdownFiles, Count: 17}},
			&Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Names: []string{"repo"}, Count: &ReasonCount{Cap: CapAIReview, Limit: 12, Of: OfMarkdownFiles, Count: 17}}},
		{"a fetch names its source, counts nothing", &Reason{Code: ReasonInputRefused, Detail: InputSourceFetch, Names: []string{"repo"}, Count: files},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceFetch, Names: []string{"repo"}}},
		{"malware names and counts nothing", &Reason{Code: ReasonInputRefused, Detail: InputMalware, Names: []string{"repo"}, Count: files},
			&Reason{Code: ReasonInputRefused, Detail: InputMalware}},
		{"no detail, no count", &Reason{Code: ReasonInputRefused, Detail: "too big", Names: []string{"repo"}, Count: files},
			&Reason{Code: ReasonInputRefused}},
		{"a count on another code", &Reason{Code: ReasonSecretMissing, Names: []string{"A"}, Count: files},
			&Reason{Code: ReasonSecretMissing, Names: []string{"A"}}},
		{"a file name is not a source", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Names: []string{"README.md", "src/main.go", "Repo"}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"an unknown cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "lines", Limit: 1, Of: OfFiles, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"a count of something else", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapFiles, Limit: 1, Of: OfBytes, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"Markdown against the file cap", &Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Count: &ReasonCount{Cap: CapFiles, Limit: 1, Of: OfMarkdownFiles, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputNotReviewed}},
		{"a negative count", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapFiles, Limit: 1, Of: OfFiles, Count: -1}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"a count past 2^53", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 1, Of: OfBytes, Count: 1<<53 + 1}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"a negative cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: -1, Of: OfBytes, Count: 1}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.Clean()
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Clean(%+v) = %+v, want %+v", c.in, got, c.want)
			}
			if got.Count != nil && got.Count == c.in.Count {
				t.Error("Clean shares the count it was given")
			}
		})
	}
}

// The count's wire form, as a driver reads it.
func TestReasonCountJSON(t *testing.T) {
	b, err := json.Marshal(&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Names: []string{"repo"},
		Count: &ReasonCount{Cap: CapBytes, Limit: 268435456, Of: OfPackBytes, Count: 335544320, More: true}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"code":"input-refused","names":["repo"],"detail":"source-cap","count":{"cap":"bytes","limit":268435456,"of":"pack-bytes","count":335544320,"more":true}}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestReasonCleanCapsNames(t *testing.T) {
	r := &Reason{Code: ReasonExecNotProvided}
	for i := range MaxReasonNames + 3 {
		r.Names = append(r.Names, fmt.Sprintf("cmd%d", i))
	}
	if got := r.Clean(); len(got.Names) != MaxReasonNames {
		t.Fatalf("kept %d names, want %d", len(got.Names), MaxReasonNames)
	}
}

func TestReasonOfFindsTheReasonThroughWrapping(t *testing.T) {
	base := errors.New("refused by policy: git")
	err := fmt.Errorf("compile: %w", WithReason(base, ReasonExecNotProvided, "", "git"))
	got := ReasonOf(err)
	want := &Reason{Code: ReasonExecNotProvided, Names: []string{"git"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonOf = %+v, want %+v", got, want)
	}
	if !errors.Is(err, base) || err.Error() != "compile: refused by policy: git" {
		t.Fatalf("the reason changed the error: %v", err)
	}
	if ReasonOf(base) != nil || ReasonOf(nil) != nil || WithReason(nil, ReasonExecNotProvided, "") != nil {
		t.Fatal("no reason is attached or found where none was given")
	}
	if ReasonOf(WithReason(base, "made-up", "")) != nil {
		t.Fatal("an unknown code reached the envelope")
	}
	// A step cancelled mid-way (a pull the person interrupted) is an
	// interruption, not the step's refusal.
	if ReasonOf(WithReason(fmt.Errorf("pull: %w", context.Canceled), ReasonInputsNotDecoded, DecodeImage)) != nil {
		t.Fatal("a cancellation carried a reason")
	}
}

func TestEnvelopeCarriesTheReason(t *testing.T) {
	e := &Envelope{Agentry: EnvelopeVersion, Status: "error", ExitCode: 11, Stage: StageCompile,
		Reason: &Reason{Code: ReasonExecNotProvided, Names: []string{"git"}}}
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["reason"]) == "" || !strings.Contains(string(got["reason"]), `"code": "exec-not-provided"`) {
		t.Fatalf("reason = %s", got["reason"])
	}
	e.Reason = nil
	if b, _ = e.Marshal(); strings.Contains(string(b), `"reason"`) {
		t.Fatal("an envelope with no reason prints one")
	}
}

func TestReasonCodesAreTheClosedList(t *testing.T) {
	want := []ReasonCode{
		ReasonAgentBlocked, ReasonAnswerInReasoning, ReasonExecNotProvided, ReasonHostRefused, ReasonInputRefused, ReasonInputsNotDecoded, ReasonMCPInstallFailed,
		ReasonMCPNotResolved, ReasonMCPRuntimeExcluded, ReasonMCPStartFailed, ReasonMCPVulnerable,
		ReasonOrgPolicy, ReasonOutOfCredits, ReasonPlanLimit, ReasonRunnerBlocked, ReasonSecretMissing, ReasonSignInMissing,
	}
	if got := ReasonCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonCodes() = %v, want %v", got, want)
	}
}

// An organization's policy is named by a word of its closed list, never by
// the vendor's text (agentry-cli#741).
func TestAnOrgPolicyReasonKeepsOnlyItsWord(t *testing.T) {
	for _, d := range []string{OrgPolicyClient, OrgPolicyManagedApproval} {
		if r := (&Reason{Code: ReasonOrgPolicy, Detail: d}).Clean(); r == nil || r.Detail != d {
			t.Fatalf("org-policy %q kept %+v", d, r)
		}
	}
	if r := (&Reason{Code: ReasonOrgPolicy, Detail: "Access denied by policy settings", Names: []string{"acme"}}).Clean(); r == nil || r.Detail != "" || len(r.Names) != 0 {
		t.Fatalf("org-policy kept %+v", r)
	}
}

// A blocked run's reasons (agentry-cli#629): the agent's carries no names
// and no detail, whatever was attached to it, since its sentence is the
// model's; the runner's keeps only its evidence's word.
func TestBlockedReasonsKeepNoText(t *testing.T) {
	r := (&Reason{Code: ReasonAgentBlocked, Detail: "the archive is encrypted", Names: []string{"tar"}}).Clean()
	if r == nil || r.Detail != "" || len(r.Names) != 0 {
		t.Fatalf("agent-blocked kept %+v", r)
	}
	for _, d := range []string{BlockedToolsUnrecorded, BlockedInputsUnread, BlockedReportUnwritten} {
		if r := (&Reason{Code: ReasonRunnerBlocked, Detail: d}).Clean(); r == nil || r.Detail != d {
			t.Fatalf("runner-blocked %q kept %+v", d, r)
		}
	}
	if r := (&Reason{Code: ReasonRunnerBlocked, Detail: "every input was a secret"}).Clean(); r == nil || r.Detail != "" {
		t.Fatalf("runner-blocked kept a free detail: %+v", r)
	}
	if ExitClassName(18) != "blocked" {
		t.Fatalf("18 is %q", ExitClassName(18))
	}
}

// A target host is named by its question's id and why by a word of the
// closed list, never by the host's name or address (agentry-cli#861).
func TestAHostRefusalKeepsTheQuestionAndItsWord(t *testing.T) {
	r := (&Reason{Code: ReasonHostRefused, Detail: HostKeyChanged, Names: []string{"server", "box.example.com", "203.0.113.4", "admin@box"}}).Clean()
	if r == nil || r.Detail != HostKeyChanged || !reflect.DeepEqual(r.Names, []string{"server"}) {
		t.Fatalf("host-refused kept %+v", r)
	}
	if r := (&Reason{Code: ReasonHostRefused, Detail: "Permission denied (publickey)"}).Clean(); r == nil || r.Detail != "" {
		t.Fatalf("host-refused kept a detail outside its list: %+v", r)
	}
}
