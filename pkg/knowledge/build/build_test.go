package build

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge/creator"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
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

// The signatures a build is given: a stand-in bundle, never the registry.
func init() {
	// No lookup leaves the machine, whatever the shell holds.
	hybridClient = func(*policy.Policy) (*hybrid.Client, error) { return &hybrid.Client{}, nil }
	ensureSignatures = func(_ context.Context, home, _ string, _ io.Writer) clamdb.DB {
		return clamdb.DB{Dir: filepath.Join(home, "clamav", "db", "84cbc016d58b4b70"), Ref: policy.DefaultClamAVSignatures, Digest: "sha256:84cbc016d58b4b70b7337b78cfd01929d6ad2646ac5d773be66efab8b3e2d10c", Daily: clamdb.Version{Number: 28143}}
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

// printScan is the scan record the decode container prints before it
// decodes, of the report playDecode wrote.
func printScan(r dockerx.RunSpec) string {
	var rep runnerdecode.Report
	b, _ := os.ReadFile(filepath.Join(mountOf(r, "/out"), runnerdecode.ReportFile))
	_ = json.Unmarshal(b, &rep)
	return string(runnerdecode.RecordOf(rep.Files, mountOf(r, "/clamav") != ""))
}

// playDecode is the decode container: every .pdf becomes its text, every
// .md passes as text, a .com is malware ClamAV finds, anything else is
// undecodable. Each file is clean when the signatures are mounted, and
// unchecked for want of them otherwise.
func playDecode(t *testing.T) func(dockerx.RunSpec) {
	return func(r dockerx.RunSpec) {
		in, out := mountOf(r, "/in"), mountOf(r, "/out")
		check := func(f runnerdecode.File) runnerdecode.File {
			switch {
			case filepath.Ext(f.Path) == ".com":
				f.Family, f.Tool, f.Outcome, f.Output, f.Bytes, f.Reason = "unknown", "", runnerdecode.OutcomeRefused, "", 0, ""
				f.Why, f.Check, f.Signature = runnerdecode.WhyFound, runnerdecode.CheckFound, "Eicar-Test-Signature"
			case mountOf(r, "/clamav") != "":
				f.Check = runnerdecode.CheckClean
			default:
				f.Check, f.Unchecked = runnerdecode.CheckUnchecked, runnerdecode.UncheckedNoSignatures
			}
			return f
		}
		rep := runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{}}
		_ = filepath.WalkDir(in, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(in, p)
			rel = filepath.ToSlash(rel)
			switch filepath.Ext(rel) {
			case ".zip":
				rep.Files = append(rep.Files, playArchive(t, p, rel, out, slices.Contains(r.Argv, "--keep-members"))...)
				return nil
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
		for i := range rep.Files {
			if rep.Files[i].Check == "" {
				rep.Files[i] = check(rep.Files[i])
			}
		}
		b, _ := json.Marshal(rep)
		writeFile(t, filepath.Join(out, runnerdecode.ReportFile), string(b))
	}
}

// playArchive is the decode container unpacking a zip, as a decode asked to
// keep members does (keepMembers): the archive unpacked, each .pdf member
// decoded and each .md member read as text, both with their own bytes kept
// under originals/ when asked, anything else undecodable. Every entry is checked clean.
func playArchive(t *testing.T, src, rel, out string, keepMembers bool) []runnerdecode.File {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	whole, _ := os.ReadFile(src) //nolint:gosec // the test's own staged file
	files := []runnerdecode.File{{Path: rel, Family: runnerdecode.FamilyArchive, Tool: "agentry-runner", Outcome: runnerdecode.OutcomeUnpacked,
		Check: runnerdecode.CheckClean, MIME: "application/zip", SHA256: sha256Hex(whole)}}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		path := rel + "/" + zf.Name
		f := runnerdecode.File{Path: path, In: rel, Size: int64(len(body)), Check: runnerdecode.CheckClean, SHA256: sha256Hex(body), MIME: "text/plain"}
		keep := func() {
			if !keepMembers {
				return
			}
			f.Original = runnerdecode.OriginalsDir + "/" + path
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Original)), string(body))
		}
		switch filepath.Ext(zf.Name) {
		case ".pdf":
			f.Family, f.Tool, f.Outcome, f.Output, f.MIME = "pdf", "pdftotext", runnerdecode.OutcomeDecoded, path+".txt", "application/pdf"
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Output)), "text of "+path+"\n")
			f.Bytes = int64(len("text of " + path + "\n"))
			keep()
		case ".md":
			f.Family, f.Outcome, f.Output = "text", runnerdecode.OutcomeText, path+".txt"
			writeFile(t, filepath.Join(out, filepath.FromSlash(f.Output)), string(body))
			f.Bytes = int64(len(body))
			keep()
		default:
			f.Family, f.Outcome, f.Reason, f.MIME = "unknown", runnerdecode.OutcomeUndecodable, "not text", "application/octet-stream"
		}
		files = append(files, f)
	}
	return files
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
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
			"agentry-decode-": {Before: playDecode(t), Print: printScan},
			"-probe":          {ExitCode: 0},
			"-agent":          agent,
		},
		ImageFiles: map[string]string{
			decode.ImageSpec(runnerTag, policy.DefaultFilesImage).Tag + ":" + decode.FilesPackagesFile: "pandoc-cli-3.6.4-r0\npoppler-utils-25.04.0-r0\n",
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

// TestABuildReadsAnArchivesMembers: given an archive, the Creator reads
// each file it holds by its own name, <archive>/<member>; the pack keeps
// each member as it was given, under sources/<archive>/, with a decoded
// member's text beside it; pages cite the members and the pack lints
// clean; a member no decoder reads is listed unread and not kept, and the
// archive itself is not a document.
func TestABuildReadsAnArchivesMembers(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"reports/outage.pdf": "%PDF outage", "notes/shift.md": "# Shift\n", "tools/run.bin": "\x00\x01"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	writeFile(t, filepath.Join(fx.docs, "docs.zip"), buf.String())
	fx.opts.Documents = []string{filepath.Join(fx.docs, "docs.zip")}
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/topics/outage.md": page("Outage", "docs.zip/reports/outage.pdf"),
		"wiki/topics/shift.md":  page("Shift", "docs.zip/notes/shift.md"),
	})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Join(res.Documents, ",") != "docs.zip/notes/shift.md,docs.zip/reports/outage.pdf" || strings.Join(res.Skipped, ",") != "docs.zip/tools/run.bin" {
		t.Fatalf("documents %v skipped %v", res.Documents, res.Skipped)
	}
	for rel, want := range map[string]string{
		"sources/docs.zip/reports/outage.pdf":     "%PDF outage",
		"sources/docs.zip/reports/outage.pdf.txt": "text of docs.zip/reports/outage.pdf\n",
		"sources/docs.zip/notes/shift.md":         "# Shift\n",
	} {
		got, err := os.ReadFile(filepath.Join(fx.opts.PackDir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"sources/docs.zip/tools/run.bin"} {
		if _, err := os.Stat(filepath.Join(fx.opts.PackDir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s was kept", rel)
		}
	}
	if fi, err := os.Stat(filepath.Join(fx.opts.PackDir, "sources", "docs.zip")); err != nil || !fi.IsDir() {
		t.Fatalf("sources/docs.zip is not the folder of its members: %v", err)
	}
	p, err := knowledge.Load(fx.opts.PackDir)
	if err != nil {
		t.Fatalf("the pack does not load: %v", err)
	}
	findings, err := knowledge.Lint(fx.opts.PackDir, p.Manifest.Name)
	if err != nil || len(findings) != 0 {
		t.Fatalf("the pack does not lint clean: %+v (%v)", findings, err)
	}
	dec, ok := fx.fake.FindRun("agentry-decode-")
	if !ok || !strings.Contains(strings.Join(dec.Argv, " "), "--keep-members") {
		t.Fatalf("the decode was not asked to keep members: %v", dec.Argv)
	}
}

// A member whose name a citation cannot carry is listed unread, never
// given to the Creator to cite; and what the pack keeps of an archive's
// members is held to the pack's cap before anything runs, though the
// archive as staged was within it.
func TestABuildHoldsAnArchivesMembersToTheirNamesAndTheCap(t *testing.T) {
	zipOf := func(files map[string]string) string {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range files {
			w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
			_, _ = w.Write([]byte(body))
		}
		_ = zw.Close()
		return buf.String()
	}
	t.Run("a name a citation cannot carry", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		writeFile(t, filepath.Join(fx.docs, "docs.zip"), zipOf(map[string]string{"Q3 report.md": "# Q3\n", "plan.md": "# Plan\n"}))
		fx.opts.Documents = []string{filepath.Join(fx.docs, "docs.zip")}
		fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{"wiki/topics/plan.md": page("Plan", "docs.zip/plan.md")})
		res, err := Build(context.Background(), fx.fake, fx.opts)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if strings.Join(res.Documents, ",") != "docs.zip/plan.md" || strings.Join(res.Skipped, ",") != "docs.zip/Q3 report.md" {
			t.Fatalf("documents %v skipped %v", res.Documents, res.Skipped)
		}
	})
	t.Run("past the pack's cap once unpacked", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		big := strings.Repeat("a long line of notes\n", 2000)
		writeFile(t, filepath.Join(fx.docs, "docs.zip"), zipOf(map[string]string{"a.md": big, "b.md": big}))
		fx.opts.Documents = []string{filepath.Join(fx.docs, "docs.zip")}
		fx.opts.MaxBytes = int64(len(big)) + 100
		_, err := Build(context.Background(), fx.fake, fx.opts)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "a knowledge pack's cap") {
			t.Fatalf("err %v", err)
		}
		if _, ok := fx.fake.FindRun("-agent"); ok {
			t.Fatal("the Creator ran on documents past the pack's cap")
		}
	})
}

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
	// The decode container binds from under the runner home, which the
	// daemon sees wherever the runner runs, and the work is gone after.
	dec, ok := fx.fake.FindRun("agentry-decode-")
	if !ok {
		t.Fatal("no decode container ran")
	}
	for _, m := range dec.Mounts {
		// The signatures are the runner home's own bundle, read-only.
		if m.Target == "/clamav" && m.ReadOnly && strings.HasPrefix(m.Source, filepath.Join(fx.opts.Home, "clamav")+string(filepath.Separator)) {
			continue
		}
		if !strings.HasPrefix(m.Source, filepath.Join(fx.opts.Home, "work")+string(filepath.Separator)) {
			t.Errorf("the decode container binds %s, outside the runner home's work directory", m.Source)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(fx.opts.Home, "work")); len(entries) != 0 {
		t.Errorf("the build left its work behind: %v", entries)
	}
	if strings.Join(res.Documents, ",") != "incident-report.pdf,runbook.md" || strings.Join(res.Skipped, ",") != "blob.bin" {
		t.Fatalf("documents %v skipped %v", res.Documents, res.Skipped)
	}
	env := res.Envelope
	if env.Attestation.State != "builtin" || env.Ref != "agentry/knowledge-creator@1.5.0" {
		t.Fatalf("attestation %+v ref %s", env.Attestation, env.Ref)
	}
	if !strings.Contains(env.CapabilitiesEnforced["decode"], "1 decoded, 1 read as text, 1 not decoded (blob.bin)") {
		t.Fatalf("decode line %q", env.CapabilitiesEnforced["decode"])
	}
	// A new pack's build attaches no pack but the Creator's own guide.
	if line := env.CapabilitiesEnforced["knowledge"]; !strings.HasPrefix(line, creator.GuideName+" shipped with the release") || strings.Count(line, "shipped with the release") != 1 || strings.Contains(line, "scan:") {
		t.Fatalf("a new pack's build attached %q", line)
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

// A build on this computer takes documents past Agentry's 10 MB when its
// caller names this computer's cap, and makes a pack past it.
func TestABuildOnThisComputerTakesMoreThanAgentry(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	writeFile(t, filepath.Join(fx.docs, "runbook.md"), "# Runbook\n"+strings.Repeat("A long runbook line.\n", (knowledge.MaxBytes+(1<<20))/21))
	fx.opts.Documents = []string{filepath.Join(fx.docs, "runbook.md")}
	fx.opts.MaxBytes = knowledge.MaxLocalBytes
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/procedures/night.md": page("Night shift", "runbook.md"),
	})
	if _, err := Build(context.Background(), fx.fake, fx.opts); err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, err := knowledge.Load(fx.opts.PackDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Bytes <= knowledge.MaxBytes || p.Manifest.Pages != 1 {
		t.Fatalf("manifest %+v", p.Manifest)
	}
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
	log, _ := os.ReadFile(filepath.Join(fx.opts.PackDir, "wiki", "log.md"))
	if !strings.Contains(string(log), "The Creator run stopped before it finished") || !strings.Contains(string(log), "a further add continues") {
		t.Fatalf("the log does not say the ingest stopped:\n%s", log)
	}

	// The further add, with the same documents, finishes what was left.
	fx2 := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx2.opts.PackDir, fx2.opts.Name, fx2.opts.Documents = fx.opts.PackDir, "", fx.opts.Documents
	fx2.fake.Foreground["-agent"].Before = writes(fx2.opts.OutDir, map[string]string{
		"wiki/topics/power-loss.md": page("Power loss", "incident-report.pdf"),
		"wiki/procedures/night.md":  page("Night shift", "runbook.md"),
	})
	res2, err := Build(context.Background(), fx2.fake, fx2.opts)
	if err != nil || res2.RunErr != nil {
		t.Fatalf("the continuing add: %v %v", err, res2.RunErr)
	}
	if res2.Applied.Manifest.Pages != 2 || res2.Applied.Manifest.ID != res.Applied.Manifest.ID {
		t.Fatalf("after the continuing add: %+v", res2.Applied.Manifest)
	}
	if !strings.Contains(res2.Envelope.CapabilitiesEnforced["knowledge"], "heron mounted read-only") {
		t.Fatal("the continuing add did not read the pack as it stood")
	}
}

// A page the Creator writes and then lists under its changes.json's delete,
// leaving the file in its reports, is deleted: the build does not name it
// twice and fail as a whole. A delete the Creator lists twice is one
// delete.
func TestAPageWrittenAndDeletedInOneRunIsDeleted(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/topics/power-loss.md":   page("Power loss", "incident-report.pdf"),
		"wiki/topics/cold-storage.md": page("Cold storage", "incident-report.pdf"),
		"wiki/procedures/night.md":    page("Night shift", "runbook.md"),
		"changes.json":                `{"schema":"knowledge/changes/v1","delete":["wiki/topics/cold-storage.md","wiki/topics/cold-storage.md"],"summary":"a stray duplicate slug"}`,
	})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Applied.Manifest.Pages != 2 {
		t.Fatalf("manifest %+v", res.Applied.Manifest)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.PackDir, "wiki", "topics", "cold-storage.md")); err == nil {
		t.Fatal("the page the Creator deleted is in the pack")
	}
	for _, rel := range []string{"wiki/topics/power-loss.md", "wiki/procedures/night.md"} {
		if _, err := os.Stat(filepath.Join(fx.opts.PackDir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s is not in the pack: %v", rel, err)
		}
	}

	t.Run("a page the pack holds, rewritten and deleted, is deleted", func(t *testing.T) {
		fx2 := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx2.opts.PackDir, fx2.opts.Name = fx.opts.PackDir, ""
		writeFile(t, filepath.Join(fx2.docs, "memo.md"), "# Memo\n")
		fx2.opts.Documents = []string{filepath.Join(fx2.docs, "memo.md")}
		fx2.fake.Foreground["-agent"].Before = writes(fx2.opts.OutDir, map[string]string{
			"wiki/procedures/night.md": page("Night shift", "memo.md"),
			"wiki/topics/memo.md":      page("Memo", "memo.md"),
			"changes.json":             `{"schema":"knowledge/changes/v1","delete":["wiki/procedures/night.md"]}`,
		})
		res, err := Build(context.Background(), fx2.fake, fx2.opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(res.Applied.Deleted, ",") != "wiki/procedures/night.md" || res.Applied.Manifest.Pages != 2 {
			t.Fatalf("deleted %v, manifest %+v", res.Applied.Deleted, res.Applied.Manifest)
		}
		if _, err := os.Stat(filepath.Join(fx.opts.PackDir, "wiki", "procedures", "night.md")); err == nil {
			t.Fatal("the page the Creator deleted is still in the pack")
		}
	})
}

// TestABuildFromABriefAlone: a build with no document and a brief makes a
// pack whose pages cite the brief, the web and the model, and pass the lint;
// the brief is kept as sources/brief.md, and a later brief beside it as
// brief-2.md.
func TestABuildFromABriefAlone(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.opts.Documents = nil
	fx.opts.Brief = "  Cold-chain alarms in our depots, for night-shift operators.  "
	body := "---\ntitle: Cold chain\ntype: topic\nsummary: Alarms\nsources: [doc:brief.md, web:https://example.org/cold, model]\nupdated: 2026-09-25\n---\n\n# Cold chain\n\n" +
		"Operators work nights [src: doc:brief.md]. An alarm fires at 8 C [src: web:https://example.org/cold]. Ice melts [src: model].\n"
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{"wiki/topics/cold-chain.md": body})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Applied.Manifest.Pages != 1 || len(res.Applied.Dropped) != 0 {
		t.Fatalf("applied %+v", res.Applied)
	}
	if got, _ := os.ReadFile(filepath.Join(fx.opts.PackDir, "sources", "brief.md")); string(got) != "Cold-chain alarms in our depots, for night-shift operators.\n" {
		t.Fatalf("sources/brief.md = %q", got)
	}
	if strings.Join(res.Documents, ",") != "brief.md" {
		t.Fatalf("documents %v", res.Documents)
	}
	if fs, err := knowledge.Lint(fx.opts.PackDir, "heron"); err != nil || len(fs) != 0 {
		t.Fatalf("lint: %v %v", fs, err)
	}
	// Without the web the plan grants no web tool.
	if _, ok := res.Envelope.CapabilitiesEnforced["web"]; ok {
		t.Fatalf("a build without the web was granted it: %v", res.Envelope.CapabilitiesEnforced)
	}

	t.Run("a second brief, with a document named brief.md, takes the next name", func(t *testing.T) {
		fx2 := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx2.opts.PackDir, fx2.opts.Name = fx.opts.PackDir, ""
		writeFile(t, filepath.Join(fx2.docs, "brief.md"), "# A document of that name\n")
		fx2.opts.Documents = []string{filepath.Join(fx2.docs, "brief.md")}
		fx2.opts.Brief = "Add the day shift."
		fx2.fake.Foreground["-agent"].Before = writes(fx2.opts.OutDir, map[string]string{"wiki/topics/day.md": page("Day shift", "brief-2.md")})
		res, err := Build(context.Background(), fx2.fake, fx2.opts)
		if err != nil {
			t.Fatal(err)
		}
		// The document keeps the name it was given, as any document added
		// again does; the brief never takes a name already in use.
		got, _ := os.ReadFile(filepath.Join(fx.opts.PackDir, "sources", "brief-2.md"))
		if string(got) != "Add the day shift.\n" || res.Applied.Manifest.Pages != 2 {
			t.Fatalf("brief-2.md = %q, manifest %+v", got, res.Applied.Manifest)
		}
	})
}

// TestTheTaskSaysWhatTheBuildHas: the Creator is told which document is the
// brief, that a brief alone is researched, and whether it may search.
func TestTheTaskSaysWhatTheBuildHas(t *testing.T) {
	brief := []decode.Document{{Name: "brief.md"}}
	got := task("heron", nil, brief, "brief.md", true, now)
	for _, want := range []string{"brief.md is the brief", "It is the only document: research what it asks for with web_search and web_read",
		"Search only for what the documents and the pack as it stands do not already hold"} {
		if !strings.Contains(got, want) {
			t.Errorf("task lacks %q:\n%s", want, got)
		}
	}
	got = task("heron", nil, append(brief, decode.Document{Name: "a.pdf"}), "brief.md", false, now)
	if strings.Contains(got, "only document") || !strings.Contains(got, "This build has no web tools.") {
		t.Errorf("task:\n%s", got)
	}
	if got = task("heron", nil, []decode.Document{{Name: "a.pdf"}}, "", false, now); strings.Contains(got, "brief") {
		t.Errorf("a build with no brief is told of one:\n%s", got)
	}
}

// TestABuildWithTheWebGrantsItsToolsAlone: a build given a web
// configuration runs the Creator with web_search and web_read, AI Review
// reading every result first, and never registry_search; the keys reach
// the run's gateway secrets, not the plan.
func TestABuildWithTheWebGrantsItsToolsAlone(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.opts.Documents = nil
	fx.opts.Brief = "Cold-chain alarms."
	fx.opts.Web = &engine.WebRun{Scan: "jev", JevKey: "not-a-real-jev-key", Search: "brave", SearchKey: "not-a-real-brave-key"}
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{"wiki/topics/cold.md": page("Cold", "brief.md")})
	res, err := Build(context.Background(), fx.fake, fx.opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	web := res.Envelope.CapabilitiesEnforced["web"]
	if !strings.Contains(web, "web_search") || !strings.Contains(web, "web_read") || strings.Contains(web, "registry_search") ||
		!strings.Contains(web, "read by AI Review") {
		t.Fatalf("web enforcement %q", web)
	}
	b, _ := json.Marshal(res.Envelope)
	for _, k := range []string{"not-a-real-jev-key", "not-a-real-brave-key"} {
		if strings.Contains(string(b), k) {
			t.Fatal("a key in the envelope")
		}
	}
}

func TestBuildRefusals(t *testing.T) {
	t.Run("neither documents nor a brief", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx.opts.Documents, fx.opts.Brief = nil, "   "
		var ce *engine.ClassifiedError
		if _, err := Build(context.Background(), fx.fake, fx.opts); !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "documents, a brief, or both") {
			t.Fatalf("err = %v", err)
		}
		if _, ok := fx.fake.FindRun("agentry-decode-"); ok {
			t.Fatal("a refused build ran the decode step")
		}
	})
	t.Run("a directory with no document in it, and no brief", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx.opts.Documents = []string{t.TempDir()}
		if _, err := Build(context.Background(), fx.fake, fx.opts); err == nil || !strings.Contains(err.Error(), "no document was found in") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a brief past 8 KiB", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		fx.opts.Brief = strings.Repeat("b", MaxBriefBytes+1)
		var ce *engine.ClassifiedError
		if _, err := Build(context.Background(), fx.fake, fx.opts); !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "at most 8192") {
			t.Fatalf("err = %v", err)
		}
	})
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
	t.Run("documents past the pack's cap, alone or together", func(t *testing.T) {
		fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
		big := t.TempDir()
		writeFile(t, filepath.Join(big, "a.txt"), strings.Repeat("a", 600<<10))
		writeFile(t, filepath.Join(big, "b.txt"), strings.Repeat("b", 600<<10))
		fx.opts.Documents = []string{big}
		fx.opts.MaxBytes = 1 << 20 // each fits; together they do not
		var ce *engine.ClassifiedError
		if _, err := Build(context.Background(), fx.fake, fx.opts); !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "is at most 1 MB") {
			t.Fatalf("err = %v", err)
		}
		fx.opts.MaxBytes = 0 // Agentry's cap when none is named
		writeFile(t, filepath.Join(big, "c.txt"), strings.Repeat("c", knowledge.MaxBytes))
		if _, err := Build(context.Background(), fx.fake, fx.opts); err == nil || !strings.Contains(err.Error(), "is at most 10 MB") {
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

// The Creator's reports are written by the run, so a link among them is
// refused, never followed: a wiki/ that is a link to a directory outside,
// or a changes.json that is a link to a file outside, contributes nothing.
// os.Root refuses such escapes on Linux; where it does not, the build
// checks each part itself.
func TestCollectRefusesLinksInTheReports(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "wiki", "secret.md"), []byte("# Host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, knowledge.ChangesFile), []byte(`{"schema":"`+knowledge.ChangesSchema+`","delete":["wiki/keep.md"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("a linked wiki", func(t *testing.T) {
		reports, applyDir := t.TempDir(), t.TempDir()
		if err := os.Symlink(filepath.Join(outside, "wiki"), filepath.Join(reports, "wiki")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := collect(reports, &decode.Result{}, nil, applyDir, nil); err == nil {
			t.Error("collect read a wiki/ that is a link out of the reports")
		}
		if _, err := os.Stat(filepath.Join(applyDir, "wiki", "secret.md")); err == nil {
			t.Error("a page from outside the reports reached the apply step")
		}
	})
	t.Run("a linked changes.json", func(t *testing.T) {
		reports, applyDir := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(reports, "wiki"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(reports, "wiki", "page.md"), []byte("# Page\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, knowledge.ChangesFile), filepath.Join(reports, knowledge.ChangesFile)); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := collect(reports, &decode.Result{}, nil, applyDir, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(applyDir, knowledge.ChangesFile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "keep.md") {
			t.Errorf("a changes.json outside the reports was read:\n%s", raw)
		}
	})
}

// A work directory the build could not delete (Windows keeps a handle for
// a moment after the decode container) is named to the person, never
// passed over: it holds the documents and the pages in the clear.
func TestBuildNamesAWorkDirectoryItCouldNotDelete(t *testing.T) {
	old := removeWork
	removeWork = func(string, bool) error { return errors.New("in use by another process") }
	t.Cleanup(func() { removeWork = old })
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	fx.fake.Foreground["-agent"].Before = writes(fx.opts.OutDir, map[string]string{
		"wiki/topics/power-loss.md": page("Power loss", "incident-report.pdf"),
	})
	var stderr strings.Builder
	fx.opts.Stderr = &stderr
	if _, err := Build(context.Background(), fx.fake, fx.opts); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(stderr.String(), "could not delete") || !strings.Contains(stderr.String(), filepath.Join(fx.opts.Home, "work")) {
		t.Errorf("stderr does not name the work directory:\n%s", stderr.String())
	}
}

// A document ClamAV finds refuses the build before the Creator starts,
// with the policy class, naming it.
func TestADocumentClamAVFindsRefusesTheBuild(t *testing.T) {
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	writeFile(t, filepath.Join(fx.docs, "eicar.com"), "X5O EICAR")
	fx.opts.Documents = append(fx.opts.Documents, filepath.Join(fx.docs, "eicar.com"))
	_, err := Build(context.Background(), fx.fake, fx.opts)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "ClamAV found malware in eicar.com (Eicar-Test-Signature)") {
		t.Fatalf("err %v", err)
	}
	if _, ok := fx.fake.FindRun("-agent"); ok {
		t.Fatal("the Creator ran on a document ClamAV found")
	}
}

// maliciousHA stands for Hybrid Analysis with every hash malicious, for
// this test alone.
func maliciousHA(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"verdict":"malicious","threat_score":100}`)
	}))
	t.Cleanup(srv.Close)
	saved := hybridClient
	t.Cleanup(func() { hybridClient = saved })
	hybridClient = func(*policy.Policy) (*hybrid.Client, error) {
		return &hybrid.Client{Key: "ha-test-key", Base: srv.URL, HTTP: srv.Client()}, nil
	}
}

// A risky document Hybrid Analysis calls malicious refuses the build with
// the policy class, before the Creator starts.
func TestADocumentHybridAnalysisCallsMaliciousRefusesTheBuild(t *testing.T) {
	maliciousHA(t)
	fx := newFixture(t, &dockerx.FakeForeground{Stdout: finalMessage})
	writeFile(t, filepath.Join(fx.docs, "macro.docm"), "PK not really")
	fx.opts.Documents = append(fx.opts.Documents, filepath.Join(fx.docs, "macro.docm"))
	_, err := Build(context.Background(), fx.fake, fx.opts)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "Hybrid Analysis's verdict on macro.docm is malicious (threat score 100)") {
		t.Fatalf("err %v", err)
	}
	if _, ok := fx.fake.FindRun("-agent"); ok {
		t.Fatal("the Creator ran on a document Hybrid Analysis called malicious")
	}
}
