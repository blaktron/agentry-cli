package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/build"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

func TestKnowledgeCommands(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("the warning comes before anything is built", func(t *testing.T) {
		// The fake refuses the first image build: whatever the build does
		// first, the person has already been told the pack is not scanned.
		prev := knowledgeDocker
		t.Cleanup(func() { knowledgeDocker = prev })
		knowledgeDocker = func() dockerx.Client {
			return &dockerx.Fake{FailBuild: map[string]error{}, FailPull: map[string]error{}, AvailableErr: errors.New("no docker here")}
		}
		dir := filepath.Join(t.TempDir(), "notes")
		_, stderr, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", dir, "--file", doc, "--policy", policyPath)
		if err == nil {
			t.Fatal("the build went through a docker client that refuses")
		}
		warning := strings.Index(stderr, "neither the documents nor the pack it makes are scanned")
		if warning < 0 {
			t.Fatalf("no warning in stderr:\n%s", stderr)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Fatal("a failed build left a pack directory")
		}
	})
	t.Run("create refuses a name that is not a pack name", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "create", "Bad Name", "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("create refuses a directory that holds a pack", func(t *testing.T) {
		p := knowledgetest.Make(t, "notes", nil)
		_, _, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", p.Dir, "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "knowledge add") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("add refuses a directory with no pack", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "add", t.TempDir(), "--file", doc, "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("a build names its documents", func(t *testing.T) {
		_, _, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "n"), "--policy", policyPath)
		wantClassified(t, err, engine.ExitUsage)
	})
}

// layDownTestPack lays a one-page pack named name down in dir, as a build
// that ran would leave it, and answers its manifest.
func layDownTestPack(t *testing.T, name, dir string) knowledge.Manifest {
	t.Helper()
	made := knowledgetest.Make(t, name, map[string]string{
		"wiki/topics/a.md": "---\ntitle: A\ntype: topic\nsummary: A\nsources: [doc:notes.md]\nupdated: 2026-09-26\n---\n\nA [src: doc:notes.md].\n",
		"sources/notes.md": "# notes\n",
	})
	if err := copyTreeForTest(made.Dir, dir); err != nil {
		t.Fatal(err)
	}
	return made.Manifest
}

// TestKnowledgeBuildOutputFormatJSON: the form a sandbox host drives for a
// hosted build — the Creator run's envelope as the last JSON object on
// stdout with the build's report under knowledge, its usage narrated live
// on stderr, and one envelope still for a build that failed before the
// Creator ran or after it.
func TestKnowledgeBuildOutputFormatJSON(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	creatorEnv := func(status string, code int) *receipt.Envelope {
		return &receipt.Envelope{
			Agentry: receipt.EnvelopeVersion, Status: status, ExitCode: code, RunID: "r1",
			Ref: localrun.KnowledgeCreator().Ref(), Attestation: receipt.Attestation{State: "builtin"},
			CapabilitiesEnforced: map[string]string{"filesystem": "none"},
			Usage:                receipt.Usage{InputTokens: 120_000, OutputTokens: 9_000, Turns: 12},
			Result:               receipt.TextResult("done"),
		}
	}
	// builder stands in for the build: it narrates usage through the hook
	// the command hands it, lays a pack down in dir, and answers what the
	// case needs.
	builder := func(env *receipt.Envelope, runErr, err error) func(*cobra.Command, string, string, *knowledgeBuildOpts) (*build.Result, string, error) {
		return func(_ *cobra.Command, dir, name string, o *knowledgeBuildOpts) (*build.Result, string, error) {
			o.stage = receipt.StagePrepare
			if o.onUsage != nil {
				o.onUsage(receipt.Usage{InputTokens: 60_000, OutputTokens: 4_000, Turns: 5})
			}
			if env == nil {
				return nil, "", err
			}
			made := layDownTestPack(t, name, dir)
			return &build.Result{Envelope: env, Applied: &knowledge.Result{Manifest: made, Written: []string{"wiki/topics/a.md"}},
				Documents: []string{"notes.md"}, RunErr: runErr}, o.out, err
		}
	}
	type decoded struct {
		Status    string `json:"status"`
		ExitClass string `json:"exitClass"`
		Stage     string `json:"stage"`
		Ref       string `json:"ref"`
		Usage     struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"usage"`
		Attestation struct {
			State string `json:"state"`
		} `json:"attestation"`
		Knowledge *struct {
			Manifest struct {
				Name  string `json:"name"`
				Pages int    `json:"pages"`
			} `json:"manifest"`
			Written []string `json:"written"`
			Stopped string   `json:"stopped"`
		} `json:"knowledge"`
	}
	run := func(t *testing.T, b func(*cobra.Command, string, string, *knowledgeBuildOpts) (*build.Result, string, error), args ...string) (decoded, string, error) {
		t.Helper()
		prev := knowledgeBuilder
		t.Cleanup(func() { knowledgeBuilder = prev })
		knowledgeBuilder = b
		dir := filepath.Join(t.TempDir(), "notes")
		out, stderr, err := runCLI(t, nil, append([]string{"knowledge", "create", "notes", "--dir", dir, "--file", doc, "--policy", policyPath, "--output-format", "json"}, args...)...)
		raw, ok := runexecLastJSONObject(out)
		if !ok {
			t.Fatalf("no envelope on stdout:\n%s", out)
		}
		var d decoded
		if jerr := json.Unmarshal(raw, &d); jerr != nil {
			t.Fatal(jerr)
		}
		return d, stderr, err
	}

	t.Run("a build that ran: the Creator's envelope, the report, the narration", func(t *testing.T) {
		d, stderr, err := run(t, builder(creatorEnv("ok", 0), nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != "ok" || d.ExitClass != "ok" || d.Attestation.State != "builtin" || d.Usage.InputTokens != 120_000 || d.Usage.OutputTokens != 9_000 {
			t.Fatalf("envelope = %+v", d)
		}
		if d.Knowledge == nil || d.Knowledge.Manifest.Name != "notes" || d.Knowledge.Manifest.Pages != 1 || len(d.Knowledge.Written) != 1 {
			t.Fatalf("knowledge = %+v", d.Knowledge)
		}
		if !strings.Contains(stderr, "usage: input=60000 output=4000 total=64000 turns=5") {
			t.Fatalf("no narration on stderr:\n%s", stderr)
		}
	})
	t.Run("a build the run stopped short keeps the run's class, and what it wrote", func(t *testing.T) {
		env := creatorEnv("error", engine.ExitBudget)
		env.Error = "budget"
		stopped := engine.Classify(engine.ExitBudget, errors.New("the run passed its budget"))
		d, _, err := run(t, builder(env, stopped, nil))
		wantClassified(t, err, engine.ExitBudget)
		if d.ExitClass != "budget" || d.Knowledge == nil || d.Knowledge.Stopped == "" || d.Knowledge.Manifest.Pages != 1 {
			t.Fatalf("envelope = %+v knowledge %+v", d, d.Knowledge)
		}
	})
	t.Run("a build that failed after the run says so over the run's ok", func(t *testing.T) {
		failed := engine.Classify(engine.ExitHarness, errors.New("the Knowledge Creator wrote no page"))
		d, _, err := run(t, builder(creatorEnv("ok", 0), nil, failed))
		wantClassified(t, err, engine.ExitHarness)
		if d.Status != "error" || d.ExitClass != "harness" || d.Stage != receipt.StageOutput || d.Usage.InputTokens != 120_000 {
			t.Fatalf("envelope = %+v", d)
		}
	})
	t.Run("a build that failed before the Creator ran still writes one envelope", func(t *testing.T) {
		unread := engine.Classify(engine.ExitUsage, errors.New("no document could be read: scan.pdf"))
		d, _, err := run(t, builder(nil, nil, unread))
		wantClassified(t, err, engine.ExitUsage)
		if d.Status != "error" || d.ExitClass != "usage" || d.Stage != receipt.StagePrepare || d.Ref != localrun.KnowledgeCreator().Ref() || d.Knowledge != nil {
			t.Fatalf("envelope = %+v", d)
		}
	})
	t.Run("a build interrupted says so, over what the run reported, and still reports what it applied", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		b := builder(creatorEnv("ok", 0), nil, nil)
		interrupting := func(cmd *cobra.Command, dir, name string, o *knowledgeBuildOpts) (*build.Result, string, error) {
			res, out, err := b(cmd, dir, name, o)
			cancel() // the driver's SIGINT arrived while the build ran
			return res, out, err
		}
		prev := knowledgeBuilder
		t.Cleanup(func() { knowledgeBuilder = prev })
		knowledgeBuilder = interrupting
		root := newRootCmd()
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetArgs([]string{"knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "notes"), "--file", doc, "--policy", policyPath, "--output-format", "json"})
		_ = root.ExecuteContext(ctx)
		raw, ok := runexecLastJSONObject(out.String())
		if !ok {
			t.Fatalf("no envelope:\n%s", out.String())
		}
		var d struct {
			Status    string          `json:"status"`
			Error     string          `json:"error"`
			Knowledge json.RawMessage `json:"knowledge"`
		}
		_ = json.Unmarshal(raw, &d)
		if d.Status != "error" || d.Error != "interrupted" || len(d.Knowledge) == 0 {
			t.Fatalf("envelope = %s", raw)
		}
	})
	t.Run("an unknown output format is refused", func(t *testing.T) {
		prev := knowledgeBuilder
		t.Cleanup(func() { knowledgeBuilder = prev })
		knowledgeBuilder = builder(creatorEnv("ok", 0), nil, nil)
		_, _, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "n"), "--file", doc, "--policy", policyPath, "--output-format", "yaml")
		wantClassified(t, err, engine.ExitUsage)
	})
}

// runexecLastJSONObject finds an envelope in stdout as a sandbox host's
// runexec does: the last JSON object that starts at the start of a line.
func runexecLastJSONObject(out string) ([]byte, bool) {
	b := []byte(strings.TrimSpace(out))
	for start := len(b) - 1; start >= 0; start-- {
		if b[start] == '{' && (start == 0 || b[start-1] == '\n') && json.Valid(b[start:]) {
			return b[start:], true
		}
	}
	return nil, false
}

// TestKnowledgeBuildBriefAndWeb: a build takes documents, a brief, or
// both; the web is on by default with no documents and off with them, and
// a build that cannot have AI Review read every result runs without it and
// says why; a hosted build searches through the registry with the job's
// credential.
func TestKnowledgeBuildBriefAndWeb(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.WebSearch = policy.WebSearchKey{Provider: "brave"}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	env := &receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "r1", Ref: localrun.KnowledgeCreator().Ref(),
		Attestation: receipt.Attestation{State: "builtin"}, Result: receipt.TextResult("done")}
	create := func(args ...string) (build.Options, string, error) {
		t.Helper()
		got := creatorBuilds(t, env, nil)
		base := []string{"knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "n"), "--policy", policyPath}
		_, stderr, err := runCLI(t, nil, append(base, args...)...)
		if len(*got) == 0 {
			return build.Options{}, stderr, err
		}
		return (*got)[0], stderr, err
	}

	t.Run("neither documents nor a brief", func(t *testing.T) {
		_, _, err := create()
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "documents, a brief, or both") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a brief past 8 KiB, from a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "brief.txt")
		_ = os.WriteFile(f, []byte(strings.Repeat("b", build.MaxBriefBytes+1)), 0o644)
		_, _, err := create("--brief-file", f)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("--brief and --brief-file together", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "brief.txt")
		_ = os.WriteFile(f, []byte("x"), 0o644)
		_, _, err := create("--brief", "y", "--brief-file", f)
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("--web and --no-web together", func(t *testing.T) {
		_, _, err := create("--brief", "y", "--web", "--no-web")
		wantClassified(t, err, engine.ExitUsage)
	})

	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	t.Setenv(policy.BraveSearchKeyEnv, "brave-person-search-key")
	t.Run("a brief alone searches the web, warned", func(t *testing.T) {
		o, stderr, err := create("--brief", "  Cold-chain alarms, for night operators. ")
		if err != nil {
			t.Fatal(err)
		}
		if o.Brief != "Cold-chain alarms, for night operators." || len(o.Documents) != 0 {
			t.Fatalf("options %+v", o)
		}
		if o.Web == nil || o.Web.Scan != toolsrv.WebScanJev || o.Web.JevKey != testJev || o.Web.Search != "brave" ||
			o.Web.SearchKey != "brave-person-search-key" || o.Web.Registry != "" {
			t.Fatalf("web %+v", o.Web)
		}
		for _, want := range []string{"Warning: no documents. Specific documents make the best knowledge.", "which can be wrong or out of date",
			"may search the web with Brave", "Searching can raise what a build costs"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr lacks %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stderr, testJev) || strings.Contains(stderr, "brave-person-search-key") {
			t.Fatal("a key on stderr")
		}
	})
	t.Run("documents are built without the web unless it is asked for", func(t *testing.T) {
		o, stderr, err := create("--file", doc)
		if err != nil || o.Web != nil || strings.Contains(stderr, "Warning: no documents") {
			t.Fatalf("%v web %+v\n%s", err, o.Web, stderr)
		}
		o, _, err = create("--file", doc, "--web", "--brief", "Also the day shift.")
		if err != nil || o.Web == nil || o.Brief != "Also the day shift." {
			t.Fatalf("%v %+v", err, o)
		}
		o, _, err = create("--brief", "x", "--no-web")
		if err != nil || o.Web != nil {
			t.Fatalf("%v web %+v", err, o.Web)
		}
	})
	t.Run("an add with a brief alone is built without the web unless asked", func(t *testing.T) {
		// The pack being extended holds documents, which searches would
		// carry to the search provider.
		dir := filepath.Join(t.TempDir(), "p")
		layDownTestPack(t, "notes", dir)
		stop := errors.New("the test stops the build here")
		add := func(args ...string) build.Options {
			got := creatorBuilds(t, env, stop)
			if _, stderr, err := runCLI(t, nil, append([]string{"knowledge", "add", dir, "--policy", policyPath}, args...)...); !errors.Is(err, stop) {
				t.Fatalf("%v\n%s", err, stderr)
			}
			return (*got)[0]
		}
		if o := add("--brief", "Also the day shift."); o.Web != nil || o.Brief != "Also the day shift." {
			t.Fatalf("%+v", o)
		}
		if o := add("--brief", "Also the day shift.", "--web"); o.Web == nil {
			t.Fatal("--web was not honoured on add")
		}
	})
	t.Run("without a search key it builds without the web, and says why", func(t *testing.T) {
		t.Setenv(policy.BraveSearchKeyEnv, "")
		o, stderr, err := create("--brief", "x")
		if err != nil || o.Web != nil || !strings.Contains(stderr, "builds without the web: no Brave key in $BRAVE_API_KEY") {
			t.Fatalf("%v web %+v\n%s", err, o.Web, stderr)
		}
	})
	t.Run("without a Jev key it builds without the web, and says why", func(t *testing.T) {
		t.Setenv(policy.DefaultJudgeKeyEnv, "")
		o, stderr, err := create("--brief", "x")
		if err != nil || o.Web != nil || !strings.Contains(stderr, "builds without the web: AI Review reads every result first") {
			t.Fatalf("%v web %+v\n%s", err, o.Web, stderr)
		}
	})
	t.Run("hosted: the registry's search and scan, with the job's credential", func(t *testing.T) {
		t.Setenv(envJobToken, "")
		_, _, err := create("--brief", "x", "--hosted")
		wantClassified(t, err, engine.ExitPolicy)
		t.Setenv(envJobToken, "agyj_the-job")
		o, _, err := create("--brief", "x", "--hosted")
		if err != nil || o.Web == nil || o.Web.Scan != toolsrv.WebScanRegistry || o.Web.Search != toolsrv.WebScanRegistry ||
			o.Web.JobToken != "agyj_the-job" || o.Web.JevKey != "" || o.Web.SearchKey != "" || o.Web.Registry == "" {
			t.Fatalf("%v web %+v", err, o.Web)
		}
		// A hosted build with documents and no --web needs no credential.
		t.Setenv(envJobToken, "")
		if o, _, err = create("--file", doc, "--hosted"); err != nil || o.Web != nil {
			t.Fatalf("%v web %+v", err, o.Web)
		}
	})
}

// TestKnowledgeBuildSources: --repo and --url reach the build as sources,
// each with its access token from --source-token's variable for its host;
// a token flag for a host no source is on, a variable that is unset, and a
// token flag with no source are refused before anything runs; and a build
// given a repository alone has something to build.
func TestKnowledgeBuildSources(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	pol := policy.Default()
	pol.WebSearch = policy.WebSearchKey{Provider: "brave"}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	env := &receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "r1", Ref: localrun.KnowledgeCreator().Ref(),
		Attestation: receipt.Attestation{State: "builtin"}, Result: receipt.TextResult("done")}
	create := func(args ...string) (build.Options, string, error) {
		t.Helper()
		got := creatorBuilds(t, env, nil)
		base := []string{"knowledge", "create", "notes", "--dir", filepath.Join(t.TempDir(), "n"), "--policy", policyPath}
		_, stderr, err := runCLI(t, nil, append(base, args...)...)
		if len(*got) == 0 {
			return build.Options{}, stderr, err
		}
		return (*got)[0], stderr, err
	}
	t.Setenv("GH_TOKEN_FOR_TEST", "ghp_build-token")
	t.Run("a repository and an address, with a token for the repository's host", func(t *testing.T) {
		o, stderr, err := create("--repo", "https://github.com/o/r@main", "--url", "https://example.org/spec.pdf", "--source-token", "github.com=GH_TOKEN_FOR_TEST")
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr)
		}
		if len(o.Sources) != 2 || o.Sources[0].ID != "repo-github.com-o-r" || o.Sources[0].Ref != "main" || o.Sources[1].ID != "url-1" {
			t.Fatalf("sources %+v", o.Sources)
		}
		if o.Sources[0].Token != "ghp_build-token" || o.Sources[0].TokenFrom != sources.TokenFromBuildFlag || o.Sources[1].Token != "" {
			t.Fatalf("tokens %+v %+v", o.Sources[0].TokenFrom, o.Sources[1].TokenFrom)
		}
		if strings.Contains(stderr, "ghp_build-token") || strings.Contains(stderr, "Warning: no documents") {
			t.Fatalf("stderr:\n%s", stderr)
		}
	})
	t.Run("a token for a host no source is on", func(t *testing.T) {
		_, _, err := create("--repo", "https://github.com/o/r", "--source-token", "gitlab.com=GH_TOKEN_FOR_TEST")
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "no repository or address given is on that host") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a token variable that is unset", func(t *testing.T) {
		_, _, err := create("--repo", "https://github.com/o/r", "--source-token", "github.com=UNSET_FOR_TEST")
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "--source-token github.com=UNSET_FOR_TEST: the environment variable UNSET_FOR_TEST is unset or empty") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a token with no source", func(t *testing.T) {
		_, _, err := create("--brief", "x", "--source-token", "github.com=GH_TOKEN_FOR_TEST")
		wantClassified(t, err, engine.ExitUsage)
	})
	t.Run("an address that is not https", func(t *testing.T) {
		_, _, err := create("--url", "http://example.org/spec.pdf")
		wantClassified(t, err, engine.ExitUsage)
		if !strings.Contains(err.Error(), "--url http://example.org/spec.pdf:") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a repository alone is something to build, and is built without the web", func(t *testing.T) {
		// A repository is documents: the web stays off unless asked, even
		// with both keys present.
		t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
		t.Setenv(policy.BraveSearchKeyEnv, "brave-person-search-key")
		o, stderr, err := create("--repo", "https://github.com/o/r")
		if err != nil || len(o.Sources) != 1 || o.Brief != "" || o.Web != nil || strings.Contains(stderr, "Warning: no documents") {
			t.Fatalf("%v %+v\n%s", err, o, stderr)
		}
		if o, _, err = create("--url", "https://example.org/a.pdf", "--web"); err != nil || o.Web == nil {
			t.Fatalf("--web with an address: %v %+v", err, o.Web)
		}
	})
	t.Run("the report names each source, never its token", func(t *testing.T) {
		r := buildReport("/p", "", &build.Result{Sources: []build.FetchedSource{
			{Kind: "repository", URL: "https://github.com/o/r", Ref: "main", Commit: "0123456789abcdef0123456789abcdef01234567", Document: "repo-github.com-o-r/", Files: 4, Bytes: 32, LeftOut: map[string]int{"link": 1}, Token: sources.TokenFromBuildFlag},
		}}, nil)
		b, _ := json.Marshal(r)
		for _, want := range []string{`"sources":[{"kind":"repository","url":"https://github.com/o/r","ref":"main","commit":"0123456789abcdef0123456789abcdef01234567","document":"repo-github.com-o-r/","files":4,"bytes":32,"leftOut":{"link":1},"token":"source-token"}]`} {
			if !strings.Contains(string(b), want) {
				t.Fatalf("report %s lacks %s", b, want)
			}
		}
	})
}
