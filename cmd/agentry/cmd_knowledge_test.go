package main

import (
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
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
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
		warning := strings.Index(stderr, "nothing it reads or writes is scanned")
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

// TestKnowledgeBuildOutputFormatJSON (agentry-cli#364): the form a sandbox
// host drives for a hosted build — the Creator run's envelope as the last
// JSON object on stdout with the build's report under knowledge, its usage
// narrated live on stderr, and one envelope still for a build that failed
// before the Creator ran or after it.
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
			made := knowledgetest.Make(t, name, map[string]string{
				"wiki/topics/a.md": "---\ntitle: A\ntype: topic\nsummary: A\nsources: [doc:notes.md]\nupdated: 2026-09-26\n---\n\nA [src: doc:notes.md].\n",
				"sources/notes.md": "# notes\n",
			})
			if err := copyTreeForTest(made.Dir, dir); err != nil {
				t.Fatal(err)
			}
			return &build.Result{Envelope: env, Applied: &knowledge.Result{Manifest: made.Manifest, Written: []string{"wiki/topics/a.md"}},
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
