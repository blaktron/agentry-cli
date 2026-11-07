package receipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestReasonCleanKeepsOnlyTheClosedList(t *testing.T) {
	var (
		cmd64     = strings.Repeat("a", 64)
		srv230    = "npm:" + strings.Repeat("a", 226)
		secret128 = strings.Repeat("S", 128)
		q64       = strings.Repeat("q", 64)
	)
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
		{"a command is at most 64 characters", &Reason{Code: ReasonExecNotProvided, Names: []string{cmd64, cmd64 + "a", "git"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{cmd64, "git"}}},
		{"a command starts with a letter or digit", &Reason{Code: ReasonExecNotProvided, Names: []string{".git", "-rf", "_x", "7z"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{"7z"}}},
		{"a command is a name, not a path", &Reason{Code: ReasonExecNotProvided, Names: []string{"bin/git", "g++", "python3.12", "git_x"}},
			&Reason{Code: ReasonExecNotProvided, Names: []string{"g++", "python3.12", "git_x"}}},
		{"a server id is at most 230 characters", &Reason{Code: ReasonMCPStartFailed, Names: []string{srv230, srv230 + "a", "pypi:" + strings.Repeat("a", 225)}},
			&Reason{Code: ReasonMCPStartFailed, Names: []string{srv230, "pypi:" + strings.Repeat("a", 225)}}},
		{"npm ids are lower case throughout", &Reason{Code: ReasonMCPStartFailed, Names: []string{"npm:Server", "npm:serVer", "npm:@Scope/x", "npm:@scope/X", "npm:a.b-c_d"}},
			&Reason{Code: ReasonMCPStartFailed, Names: []string{"npm:a.b-c_d"}}},
		{"pypi ids start and end with a letter or digit", &Reason{Code: ReasonMCPStartFailed, Names: []string{"pypi:Osv", "pypi:osv-", "pypi:-osv", "pypi:o", "pypi:osv_mcp.x-1"}},
			&Reason{Code: ReasonMCPStartFailed, Names: []string{"pypi:o", "pypi:osv_mcp.x-1"}}},
		{"a server is named with its ecosystem", &Reason{Code: ReasonMCPVulnerable, Names: []string{"osv-mcp", "npm:osv-mcp"}},
			&Reason{Code: ReasonMCPVulnerable, Names: []string{"npm:osv-mcp"}}},
		{"a secret starts with a letter or underscore", &Reason{Code: ReasonSecretMissing, Names: []string{"1TOKEN", "_TOKEN", "-X", "a-b", "TOKEN_2"}},
			&Reason{Code: ReasonSecretMissing, Names: []string{"_TOKEN", "TOKEN_2"}}},
		{"a secret name is at most 128 characters", &Reason{Code: ReasonSecretMissing, Names: []string{secret128, secret128 + "A"}},
			&Reason{Code: ReasonSecretMissing, Names: []string{secret128}}},
		{"a question id is lower case words of 1 to 64", &Reason{Code: ReasonHostRefused, Detail: HostAuth, Names: []string{"", "a", "repo_2", q64, q64 + "a", "my-repo", "Repo", "a b"}},
			&Reason{Code: ReasonHostRefused, Detail: HostAuth, Names: []string{"a", "repo_2", q64}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Clean(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Clean(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}

	// Every code keeps exactly the details and the kind of names its closed list
	// names, spelled out here: a word a client matches on is part of the
	// contract. A name of another kind is dropped, a name of its own kind kept.
	t.Run("every code keeps only its own details and names", func(t *testing.T) {
		const (
			command  = "git-1.0"
			server   = "npm:x"
			secret   = "GIT_TOKEN"
			question = "repo_1"
		)
		all := []string{command, server, secret, question}
		codes := []struct {
			code    ReasonCode
			details []string
			kept    []string // the names of all that survive: a command may hold the letters and underscore a secret or a question does
		}{
			{"exec-not-provided", nil, []string{command, secret, question}},
			{"mcp-vulnerable", nil, []string{server}},
			{"mcp-not-resolved", nil, []string{server}},
			{"mcp-runtime-excluded", nil, []string{server}},
			{"mcp-install-failed", []string{"pin-changed", "store-changed", "image", "install-proxy", "builder", "runtime-check", "record"}, []string{server}},
			{"mcp-start-failed", nil, []string{server}},
			{"secret-missing", nil, []string{secret, question}},
			{"input-refused", []string{"malware", "name-clash", "unreadable", "ai-review", "not-reviewed", "source-cap", "source-fetch"}, []string{question}},
			{"inputs-not-decoded", []string{"image", "run"}, nil},
			{"agent-blocked", nil, nil},
			{"runner-blocked", []string{"tools-unrecorded", "inputs-unread", "report-unwritten"}, nil},
			{"signin-missing", nil, nil},
			{"plan-limit", nil, nil},
			{"org-policy", []string{"client-disabled", "managed-approval"}, nil},
			{"out-of-credits", nil, nil},
			{"answer-in-reasoning", nil, nil},
			{"tool-call-as-text", nil, nil},
			{"host-refused", []string{"host-address", "host-key-unknown", "host-key-changed", "host-auth", "host-credential", "host-unreachable", "hosted"}, []string{question}},
			{"desktop-refused", []string{"desktop-unavailable", "desktop-not-installed", "desktop-stopped", "desktop-paused", "desktop-declined", "desktop-unreachable", "hosted", "outside-window", "desktop-not-granted"}, []string{"blender", "unreal-engine"}},
		}
		var listed []ReasonCode
		for _, c := range codes {
			listed = append(listed, c.code)
		}
		slices.Sort(listed)
		if got := ReasonCodes(); !slices.Equal(got, listed) {
			t.Fatalf("ReasonCodes() = %v, the contract has %v", got, listed)
		}
		for _, c := range codes {
			t.Run(string(c.code), func(t *testing.T) {
				if !slices.Contains(ReasonCodes(), c.code) {
					t.Fatalf("%q is not in the closed list", c.code)
				}
				for _, d := range c.details {
					want := &Reason{Code: c.code, Detail: d}
					if got := (&Reason{Code: c.code, Detail: d}).Clean(); !reflect.DeepEqual(got, want) {
						t.Errorf("detail %q: Clean = %+v, want %+v", d, got, want)
					}
				}
				if got := (&Reason{Code: c.code, Detail: "not-a-detail"}).Clean(); !reflect.DeepEqual(got, &Reason{Code: c.code}) {
					t.Errorf("a detail outside the list was kept: %+v", got)
				}
				// A word that is another code's detail is no detail of this one.
				for _, other := range codes {
					for _, d := range other.details {
						if slices.Contains(c.details, d) {
							continue
						}
						if got := (&Reason{Code: c.code, Detail: d}).Clean(); got == nil || got.Detail != "" {
							t.Errorf("%s kept %q, which is %s's detail: %+v", c.code, d, other.code, got)
						}
					}
				}
				// input-refused names a question only for a source's refusals.
				detail := ""
				if c.code == "input-refused" {
					detail = "source-fetch"
				}
				var wantNames []string
				for _, n := range all {
					if slices.Contains(c.kept, n) {
						wantNames = append(wantNames, n)
					}
				}
				got := (&Reason{Code: c.code, Detail: detail, Names: all}).Clean()
				if got == nil || !slices.Equal(got.Names, wantNames) {
					t.Errorf("names %v kept as %+v, want %v", all, got, wantNames)
				}
			})
		}
		// Only a source's refusals name a question; the others name nothing.
		for _, d := range []string{"malware", "name-clash", "unreadable", "ai-review"} {
			if got := (&Reason{Code: "input-refused", Detail: d, Names: []string{"repo"}}).Clean(); got == nil || got.Names != nil || got.Detail != d {
				t.Errorf("input-refused %q kept names: %+v", d, got)
			}
		}
		for _, d := range []string{"source-cap", "source-fetch", "not-reviewed"} {
			if got := (&Reason{Code: "input-refused", Detail: d, Names: []string{"repo"}}).Clean(); got == nil || !slices.Equal(got.Names, []string{"repo"}) {
				t.Errorf("input-refused %q lost its source: %+v", d, got)
			}
		}
	})
}

// TestReasonCleanKeepsASourcesCount pins that a source's
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
		{"a count and a cap of zero", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 0, Of: "files", Count: 0}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 0, Of: "files", Count: 0}}},
		{"a count and a cap of exactly 2^53", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 1 << 53, Of: "bytes", Count: 1 << 53}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 1 << 53, Of: "bytes", Count: 1 << 53}}},
		{"a cap past 2^53", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 1<<53 + 1, Of: OfBytes, Count: 1}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"objects against the file cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 10, Of: "objects", Count: 11, More: true}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 10, Of: "objects", Count: 11, More: true}}},
		{"entries against the file cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 10, Of: "entries", Count: 11}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "files", Limit: 10, Of: "entries", Count: 11}}},
		{"the pack's bytes against the byte cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 10, Of: "pack-bytes", Count: 11}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 10, Of: "pack-bytes", Count: 11}}},
		{"the inflated bytes against the byte cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 10, Of: "inflated-bytes", Count: 11}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: "bytes", Limit: 10, Of: "inflated-bytes", Count: 11}}},
		{"Markdown files against AI Review's cap", &Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Count: &ReasonCount{Cap: "ai-review", Limit: 12, Of: "markdown-files", Count: 13}},
			&Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Count: &ReasonCount{Cap: "ai-review", Limit: 12, Of: "markdown-files", Count: 13}}},
		{"files against AI Review's cap", &Reason{Code: ReasonInputRefused, Detail: InputNotReviewed, Count: &ReasonCount{Cap: CapAIReview, Limit: 1, Of: OfFiles, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputNotReviewed}},
		{"Markdown files against the byte cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 1, Of: OfMarkdownFiles, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"the pack's bytes against the file cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapFiles, Limit: 1, Of: OfPackBytes, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"objects against the byte cap", &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapBytes, Limit: 1, Of: OfObjects, Count: 2}},
			&Reason{Code: ReasonInputRefused, Detail: InputSourceCap}},
		{"no name for a flagged input", &Reason{Code: ReasonInputRefused, Detail: InputFlagged, Names: []string{"repo"}, Count: files},
			&Reason{Code: ReasonInputRefused, Detail: InputFlagged}},
		{"no name for an unreadable input", &Reason{Code: ReasonInputRefused, Detail: InputUnreadable, Names: []string{"repo"}},
			&Reason{Code: ReasonInputRefused, Detail: InputUnreadable}},
		{"no name for a clash", &Reason{Code: ReasonInputRefused, Detail: InputNameClash, Names: []string{"repo"}},
			&Reason{Code: ReasonInputRefused, Detail: InputNameClash}},
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

// TestReasonCountJSON pins the count's wire form, as a driver reads it.
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
	// A reason prints only what it holds, and a count only what it stopped at.
	for _, c := range []struct {
		in   *Reason
		want string
	}{
		{&Reason{Code: ReasonExecNotProvided}, `{"code":"exec-not-provided"}`},
		{&Reason{Code: ReasonOrgPolicy, Detail: OrgPolicyClient}, `{"code":"org-policy","detail":"client-disabled"}`},
		{&Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Count: &ReasonCount{Cap: CapFiles, Limit: 10, Of: OfFiles, Count: 11}},
			`{"code":"input-refused","detail":"source-cap","count":{"cap":"files","limit":10,"of":"files","count":11}}`},
	} {
		if b, err := json.Marshal(c.in); err != nil || string(b) != c.want {
			t.Errorf("got %s (%v)\nwant %s", b, err, c.want)
		}
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
	// Eight names are kept, in the order given; a ninth is dropped.
	if MaxReasonNames != 8 {
		t.Fatalf("MaxReasonNames = %d, want 8", MaxReasonNames)
	}
	eight := []string{"cmd0", "cmd1", "cmd2", "cmd3", "cmd4", "cmd5", "cmd6", "cmd7"}
	if got := (&Reason{Code: ReasonExecNotProvided, Names: eight}).Clean(); !reflect.DeepEqual(got.Names, eight) {
		t.Fatalf("eight names kept as %v", got.Names)
	}
	if got := (&Reason{Code: ReasonExecNotProvided, Names: append(slices.Clone(eight), "cmd8")}).Clean(); !reflect.DeepEqual(got.Names, eight) {
		t.Fatalf("nine names kept as %v", got.Names)
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
	if got, want := ReasonOf(fmt.Errorf("install: %w", WithReason(base, ReasonMCPInstallFailed, InstallProxy, "npm:x"))),
		(&Reason{Code: ReasonMCPInstallFailed, Detail: InstallProxy, Names: []string{"npm:x"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonOf = %+v, want %+v", got, want)
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
		ReasonAgentBlocked, ReasonAnswerInReasoning, ReasonDesktopRefused, ReasonExecNotProvided, ReasonHostRefused, ReasonInputRefused, ReasonInputsNotDecoded, ReasonMCPInstallFailed,
		ReasonMCPNotResolved, ReasonMCPRuntimeExcluded, ReasonMCPStartFailed, ReasonMCPVulnerable,
		ReasonOrgPolicy, ReasonOutOfCredits, ReasonPlanLimit, ReasonRunnerBlocked, ReasonSecretMissing, ReasonSignInMissing, ReasonToolCallAsText,
	}
	if got := ReasonCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonCodes() = %v, want %v", got, want)
	}
}

// An organization's policy is named by a word of its closed list, never by
// the vendor's text.
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

// A blocked run's reasons: the agent's carries no names
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
// closed list, never by the host's name or address.
func TestAHostRefusalKeepsTheQuestionAndItsWord(t *testing.T) {
	r := (&Reason{Code: ReasonHostRefused, Detail: HostKeyChanged, Names: []string{"server", "box.example.com", "203.0.113.4", "admin@box"}}).Clean()
	if r == nil || r.Detail != HostKeyChanged || !reflect.DeepEqual(r.Names, []string{"server"}) {
		t.Fatalf("host-refused kept %+v", r)
	}
	if r := (&Reason{Code: ReasonHostRefused, Detail: "Permission denied (publickey)"}).Clean(); r == nil || r.Detail != "" {
		t.Fatalf("host-refused kept a detail outside its list: %+v", r)
	}
}

// A cap's count travels with the error that stopped the run and comes back
// from ReasonOf, cleaned, through any wrapping; a nil error stays nil.
func TestWithCountCarriesTheCountThroughReasonOf(t *testing.T) {
	if WithCount(nil, ReasonInputRefused, InputSourceCap, &ReasonCount{Cap: CapFiles, Limit: 1, Of: OfFiles, Count: 2}, "repo") != nil {
		t.Fatal("a nil error gained a reason")
	}
	base := errors.New("repo is over its file cap")
	count := &ReasonCount{Cap: CapFiles, Limit: 10000, Of: OfFiles, Count: 14203}
	err := fmt.Errorf("fetch: %w", WithCount(base, ReasonInputRefused, InputSourceCap, count, "repo", "bad name!"))
	want := &Reason{Code: ReasonInputRefused, Detail: InputSourceCap, Names: []string{"repo"}, Count: count}
	if got := ReasonOf(err); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReasonOf = %+v, want %+v", got, want)
	}
	if !errors.Is(err, base) || err.Error() != "fetch: repo is over its file cap" {
		t.Fatalf("the count changed the error: %v", err)
	}
	// A count the closed lists refuse does not come back.
	bad := WithCount(base, ReasonInputRefused, InputSourceCap, &ReasonCount{Cap: "lines", Limit: 1, Of: OfFiles, Count: 2}, "repo")
	if got := ReasonOf(bad); got == nil || got.Count != nil || got.Detail != InputSourceCap {
		t.Fatalf("ReasonOf kept a count outside the lists: %+v", got)
	}
}
