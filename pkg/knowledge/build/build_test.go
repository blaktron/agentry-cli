package build

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

var now = time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mountOf(r dockerx.RunSpec, target string) string {
	for _, m := range r.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

// playDecode is the decode container: every .pdf becomes its text, every
// .md passes as text, anything else is undecodable.
func playDecode(t *testing.T) func(dockerx.RunSpec) {
	return func(r dockerx.RunSpec) {
		in, out := mountOf(r, "/in"), mountOf(r, "/out")
		rep := runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{}}
		_ = filepath.WalkDir(in, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(in, p)
			rel = filepath.ToSlash(rel)
			switch filepath.Ext(rel) {
			case ".pdf":
				writeFile(t, filepath.Join(out, rel+".txt"), "text of "+rel+"\n")
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: rel + ".txt", Bytes: 20})
			case ".md":
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "text", Outcome: runnerdecode.OutcomeText})
			default:
				rep.Files = append(rep.Files, runnerdecode.File{Path: rel, Family: "unknown", Outcome: runnerdecode.OutcomeUndecodable, Reason: "not text"})
			}
			return nil
		})
		b, _ := json.Marshal(rep)
		writeFile(t, filepath.Join(out, runnerdecode.ReportFile), string(b))
	}
}

func page(title, cite string) string {
	return "---\ntitle: " + title + "\ntype: topic\nsummary: About " + title + "\nsources: [doc:" + cite + "]\nupdated: 2026-09-25\n---\n\n# " + title + "\n\nA fact [src: doc:" + cite + "].\n"
}

type fixture struct {
	opts Options
	fake *dockerx.Fake
	docs string
}

func newFixture(t *testing.T, agent *dockerx.FakeForeground) *fixture {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Model.Name = "test-model"
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-decode-": {Before: playDecode(t)},
			"-probe":          {ExitCode: 0},
			"-agent":          agent,
		},
		ImageFiles: map[string]string{
			decode.ImageSpec(runnerTag, "").Tag + ":" + gateway.ExecPackagesFile: "pandoc-cli-3.6.4-r0\npoppler-utils-25.04.0-r0\n",
		},
	}
	docs := t.TempDir()
	writeFile(t, filepath.Join(docs, "incident report.pdf"), "%PDF bytes")
	writeFile(t, filepath.Join(docs, "runbook.md"), "# Runbook\n")
	writeFile(t, filepath.Join(docs, "blob.bin"), "\x00\x01")
	return &fixture{fake: f, docs: docs, opts: Options{
		PackDir: filepath.Join(t.TempDir(), "heron"), Name: "heron",
		Documents: []string{filepath.Join(docs, "incident report.pdf"), filepath.Join(docs, "runbook.md"), filepath.Join(docs, "blob.bin")},
		Home:      t.TempDir(), Policy: pol, PolicySHA: "sha", Build: "agentry 9.9.9 (test)", Now: now,
		OutDir: t.TempDir(), Stderr: os.Stderr, RunID: "cafe0002",
	}}
}

// writes plays the Creator: it writes the given files under the run's
// reports/, as write_report would.
func writes(outDir string, files map[string]string) func(dockerx.RunSpec) {
	return func(dockerx.RunSpec) {
		for rel, body := range files {
			p := filepath.Join(outDir, "reports", filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(body), 0o644)
		}
	}
}

const finalMessage = `{"type":"agent_choice","message":{"content":"wrote pages"}}` + "\n" + `{"type":"stream_stopped","usage":{"input_tokens":900,"output_tokens":300}}` + "\n"

func TestBuildMakesAPackFromDocuments(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/topics/power-loss.md": page("Power loss", "incident-report.pdf"),
		"wiki/procedures/night.md":  page("Night shift", "runbook.md"),
	})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, err := knowledge.Load(fx.opts.PackDir)
	if err != nil {
		t.Fatalf("the pack does not load: %v", err)
	}
	m := p.Manifest
	if m.Name != "heron" || m.Pages != 2 || m.Scan.Scanned || m.Creator.CLI != "agentry 9.9.9 (test)" || m.Creator.Model != "test-model" {
		t.Fatalf("manifest %+v", m)
	}
	// The documents, and the text decoded from one, are the pack's sources,
	// put there by the build; the name a citation cannot carry was made one
	// it can.
	for rel, want := range map[string]string{
		"sources/incident-report.pdf":     "%PDF bytes",
		"sources/incident-report.pdf.txt": "text of incident-report.pdf\n",
		"sources/runbook.md":              "# Runbook\n",
	} {
		got, err := os.ReadFile(filepath.Join(fx.opts.PackDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", rel, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.opts.PackDir, "sources", "blob.bin")); err == nil {
		t.Error("an undecodable document went into the pack")
	}
	if strings.Join(res.Documents, ",") != "incident-report.pdf,runbook.md" || strings.Join(res.Skipped, ",") != "blob.bin" {
		t.Fatalf("documents %v skipped %v", res.Documents, res.Skipped)
	}
	env := res.Envelope
	if env.Attestation.State != "builtin" || env.Ref != "agentry/knowledge-creator@1.0.0" {
		t.Fatalf("attestation %+v ref %s", env.Attestation, env.Ref)
	}
	if !strings.Contains(env.CapabilitiesEnforced["decode"], "1 decoded, 1 read as text, 1 not decoded (blob.bin)") {
		t.Fatalf("decode line %q", env.CapabilitiesEnforced["decode"])
	}
	if _, ok := env.CapabilitiesEnforced["knowledge"]; ok {
		t.Fatal("a new pack's build attached a pack")
	}
	log, _ := os.ReadFile(filepath.Join(fx.opts.PackDir, "wiki", "log.md"))
	if !strings.Contains(string(log), "## [2026-09-25] ingest | incident-report.pdf, runbook.md") {
		t.Fatalf("log:\n%s", log)
	}

	t.Run("the model's changes.json removes pages, never sources", func(t *testing.T) {
		fx3 := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx3.opts.PackDir, fx3.opts.Name = fx.opts.PackDir, ""
		writeFile(t, filepath.Join(fx3.docs, "extra.md"), "# Extra\n")
		fx3.opts.Documents = []string{filepath.Join(fx3.docs, "extra.md")}
		fx3.fake.Foreground["-agent"].Before = writes(fx3.opts.OutDir, map[string]string{
			"wiki/topics/extra.md": page("Extra", "extra.md"),
			"changes.json":         `{"schema":"knowledge/changes/v1","delete":["sources/runbook.md","wiki/procedures/night.md"],"summary":"trim"}`,
		})
		res, err := Build(context.Background(), fx3.fake, fx3.opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(res.Applied.Deleted, ",") != "wiki/procedures/night.md" {
			t.Fatalf("deleted %v", res.Applied.Deleted)
		}
		if _, err := os.Stat(filepath.Join(fx.opts.PackDir, "sources", "runbook.md")); err != nil {
			t.Fatal("the model removed a source")
		}
	})
	t.Run("a second build extends it with the pack attached", func(t *testing.T) {
		fx2 := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx2.opts.PackDir, fx2.opts.Name = fx.opts.PackDir, ""
		writeFile(t, filepath.Join(fx2.docs, "memo.md"), "# Memo\n")
		fx2.opts.Documents = []string{filepath.Join(fx2.docs, "memo.md")}
		fx2.fake.Foreground["-agent"].Before = writes(fx2.opts.OutDir, map[string]string{"wiki/topics/memo.md": page("Memo", "memo.md")})
		res, err := Build(context.Background(), fx2.fake, fx2.opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Applied.Manifest.Pages != 3 || res.Applied.Manifest.ID != m.ID {
			t.Fatalf("second manifest %+v", res.Applied.Manifest)
		}
		if !strings.Contains(res.Envelope.CapabilitiesEnforced["knowledge"], "heron mounted read-only at /knowledge/heron") {
			t.Fatalf("the pack was not attached: %q", res.Envelope.CapabilitiesEnforced["knowledge"])
		}
	})
}

func TestBuildStoppedAtABudgetKeepsWhatItWrote(t *testing.T) {
	// Three tool calls against a budget of one turn: the engine stops the
	// run (exit 16) after the Creator wrote one page.
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: strings.Repeat(`{"type":"tool_call","tool":"write_report"}`+"\n", 3)})
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{"wiki/topics/power-loss.md": page("Power loss", "incident-report.pdf")})
	fx.opts.Budgets = &spec.Budgets{TimeoutSeconds: 30, MaxTurns: 1}
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("a stopped run's pages were not applied: %v", err)
	}
	var ce *engine.ClassifiedError
	if !errors.As(res.RunErr, &ce) || ce.Code != engine.ExitBudget {
		t.Fatalf("RunErr = %v, want the budget class", res.RunErr)
	}
	if res.Applied == nil || res.Applied.Manifest.Pages != 1 {
		t.Fatalf("applied %+v", res.Applied)
	}
	if _, err := knowledge.Load(fx.opts.PackDir); err != nil {
		t.Fatalf("the pack a stopped run left does not load: %v", err)
	}
}

func TestBuildRefusals(t *testing.T) {
	t.Run("a run that writes nothing is an error and makes no pack", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		_, err := Build(context.Background(), fx.fake, fx.opts)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitHarness || !strings.Contains(err.Error(), "wrote no page") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(fx.opts.PackDir); err == nil {
			t.Fatal("a pack was made from nothing")
		}
	})
	t.Run("a document named as the Creator's own list", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		writeFile(t, filepath.Join(fx.docs, "documents.json"), "{}")
		fx.opts.Documents = []string{filepath.Join(fx.docs, "documents.json")}
		if _, err := Build(context.Background(), fx.fake, fx.opts); err == nil || !strings.Contains(err.Error(), "may not be named documents.json") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("two documents one name", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		other := t.TempDir()
		writeFile(t, filepath.Join(other, "runbook.md"), "# another\n")
		fx.opts.Documents = []string{filepath.Join(fx.docs, "runbook.md"), filepath.Join(other, "runbook.md")}
		if _, err := Build(context.Background(), fx.fake, fx.opts); err == nil || !strings.Contains(err.Error(), "would both be the document runbook.md") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a directory that is not a pack", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		writeFile(t, filepath.Join(fx.opts.PackDir, "something.txt"), "x")
		var ce *engine.ClassifiedError
		if _, err := Build(context.Background(), fx.fake, fx.opts); !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCreatorInputs(t *testing.T) {
	docs, dec, in := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(docs, "a.md"), "# a\n")
	writeFile(t, filepath.Join(dec, "b.pdf.txt"), "b text\n")
	res := &decode.Result{Out: dec, Report: runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{
		{Path: "a.md", Family: "text", Outcome: runnerdecode.OutcomeText},
		{Path: "b.pdf", Family: "pdf", Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "b.pdf.txt"},
		{Path: "c.bin", Family: "unknown", Outcome: runnerdecode.OutcomeUndecodable},
	}}}
	got, inputs, err := creatorInputs(res, map[string]string{"a.md": "/given/a.md", "b.pdf": "/given/b.pdf"}, docs, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Read != "a.md" || got[1].Read != "b.pdf.txt" || got[1].DecodedBy != "pdftotext" || got[1].GivenAs != "/given/b.pdf" {
		t.Fatalf("documents %+v", got)
	}
	var names []string
	for _, p := range inputs {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, ",") != "a.md,b.pdf.txt,documents.json" {
		t.Fatalf("inputs %v", names)
	}
	raw, _ := os.ReadFile(filepath.Join(in, DocumentsFile))
	want := `{
  "documents": [
    {
      "name": "a.md",
      "givenAs": "/given/a.md",
      "read": "a.md",
      "family": "text"
    },
    {
      "name": "b.pdf",
      "givenAs": "/given/b.pdf",
      "read": "b.pdf.txt",
      "family": "pdf",
      "decodedBy": "pdftotext"
    }
  ]
}
`
	if string(raw) != want {
		t.Fatalf("documents.json =\n%s", raw)
	}
	t.Run("a text document named as another's decoded text is refused", func(t *testing.T) {
		clash := &decode.Result{Out: dec, Report: runnerdecode.Report{Files: []runnerdecode.File{
			{Path: "b.pdf", Outcome: runnerdecode.OutcomeDecoded, Output: "b.pdf.txt"},
			{Path: "b.pdf.txt", Outcome: runnerdecode.OutcomeText},
		}}}
		if _, _, err := creatorInputs(clash, nil, docs, t.TempDir()); err == nil || !strings.Contains(err.Error(), "would both be read as b.pdf.txt") {
			t.Fatalf("err = %v", err)
		}
	})
}
