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
		ReasonExecNotProvided, ReasonInputRefused, ReasonInputsNotDecoded, ReasonMCPInstallFailed,
		ReasonMCPNotResolved, ReasonMCPRuntimeExcluded, ReasonMCPStartFailed, ReasonMCPVulnerable,
		ReasonSecretMissing,
	}
	if got := ReasonCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonCodes() = %v, want %v", got, want)
	}
}
