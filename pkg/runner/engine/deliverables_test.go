package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// agentWrites plays an agent that wrote files into reports/ and a ledger of
// marks (lines are the ledger's JSON), then ended with its final answer.
func agentWrites(p Params, f *dockerx.Fake, files map[string]string, ledger ...string) {
	f.OnForeground = func(r dockerx.RunSpec) {
		if !strings.HasSuffix(r.Name, "-agent") {
			return
		}
		for rel, content := range files {
			path := filepath.Join(p.OutDir, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.WriteFile(path, []byte(content), 0o644)
		}
		if len(ledger) > 0 {
			_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", DeliverablesLedgerFile), []byte(strings.Join(ledger, "\n")+"\n"), 0o644)
		}
	}
}

func paths(d *receipt.Deliverables) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, f := range d.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestTheRunsToolsSpecCarriesTheDeliverablesLedger(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Deliverables.Ledger != "/out/receipts/"+DeliverablesLedgerFile {
		t.Errorf("the tools spec's deliverables = %+v", ts.Deliverables)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", DeliverablesLedgerFile)); err != nil {
		t.Errorf("the ledger was not made: %v", err)
	}
}

// TestTheEnvelopeListsWhatTheAgentMarked: each marked file by the digest and
// size the runner measured, with its title, in the order marked; a file left
// unmarked is not listed.
func TestTheEnvelopeListsWhatTheAgentMarked(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{
		"reports/notes.md":       "working notes",
		"reports/final/brief.md": "# Brief\n",
		"reports/data.csv":       "a,b\n1,2\n",
	},
		`{"path":"reports/final/brief.md","title":"The brief","marked":true}`,
		`{"path":"reports/data.csv","marked":true}`,
		`{"path":"reports/data.csv","marked":false}`,
		`{"path":"reports/data.csv","title":"Data","marked":true}`,
	)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []receipt.Deliverable{
		{Path: "reports/final/brief.md", Title: "The brief", SHA256: sha([]byte("# Brief\n")), Bytes: 8, Type: "md", Marked: true},
		{Path: "reports/data.csv", Title: "Data", SHA256: sha([]byte("a,b\n1,2\n")), Bytes: 8, Type: "csv", Marked: true},
	}
	if env.Deliverables == nil || len(env.Deliverables.Files) != 2 || env.Deliverables.Files[0] != want[0] || env.Deliverables.Files[1] != want[1] {
		t.Fatalf("deliverables = %+v, want %+v", env.Deliverables, want)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
		t.Error("the final message was written down although the agent marked files")
	}
	onDisk, err := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if err != nil || !strings.Contains(string(onDisk), `"deliverables"`) || strings.Contains(string(onDisk), `"deliveries"`) {
		t.Errorf("result.json: %v\n%s", err, onDisk)
	}
}

// TestAForgedLedgerNamesNothingTheRunnerDoesNotStandBehind: the ledger is the
// agent's word. A path outside reports/ (the run's own envelope among them),
// one reached through a link, a file that is gone, a folder, and a PDF the PDF
// ledger does not vouch for are all left out; a title that is not one line of
// plain text becomes the file's name; and what is hashed is the file's bytes,
// never a digest the ledger could carry.
func TestAForgedLedgerNamesNothingTheRunnerDoesNotStandBehind(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("not the run's"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.OnForeground = nil
	agentWrites(p, f, map[string]string{
		"reports/real.md":        "real",
		"reports/odd.md":         "odd title",
		"reports/forged.pdf":     "%PDF-1.7 not exported by export_pdf",
		"reports/dir/inner.md":   "x",
		"receipts/events.ndjson": "evidence",
	},
		`{"path":"reports/real.md","title":"Real","marked":true,"sha256":"`+strings.Repeat("0", 64)+`"}`,
		`{"path":"result.json","marked":true}`,
		`{"path":"reports/../result.json","marked":true}`,
		`{"path":"../../etc/hostname","marked":true}`,
		`{"path":"receipts/events.ndjson","marked":true}`,
		`{"path":"reports/gone.md","marked":true}`,
		`{"path":"reports/dir","marked":true}`,
		`{"path":"reports/forged.pdf","marked":true}`,
		`{"path":"reports/browser/shot.png","marked":true}`,
		`{"path":"reports/odd.md","title":"a`+"\\u202e"+`b","marked":true}`,
		`not json at all`,
	)
	if runtime.GOOS != "windows" {
		inner := f.OnForeground
		f.OnForeground = func(r dockerx.RunSpec) {
			inner(r)
			if strings.HasSuffix(r.Name, "-agent") {
				_ = os.Symlink(outside, filepath.Join(p.OutDir, "reports", "link.md"))
				_ = os.Symlink(outside, filepath.Join(p.OutDir, "reports", "dir", "link.md"))
				ledger, _ := os.OpenFile(filepath.Join(p.OutDir, "receipts", DeliverablesLedgerFile), os.O_APPEND|os.O_WRONLY, 0o644)
				_, _ = ledger.WriteString(`{"path":"reports/link.md","marked":true}` + "\n" + `{"path":"reports/dir/link.md","marked":true}` + "\n")
				_ = ledger.Close()
			}
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	got := paths(env.Deliverables)
	if len(got) != 2 || got[0] != "reports/real.md" || got[1] != "reports/odd.md" {
		t.Fatalf("deliverables = %v, want real.md and odd.md alone", got)
	}
	real, odd := env.Deliverables.Files[0], env.Deliverables.Files[1]
	if real.SHA256 != sha([]byte("real")) || real.Title != "Real" {
		t.Errorf("real.md = %+v: the digest is the runner's own", real)
	}
	if odd.Title != "odd" {
		t.Errorf("a title with a bidirectional override became %q, want the file's name", odd.Title)
	}
}

// TestAPDFIsListedOnlyWhenThePDFLedgerVouchesForIt.
func TestAPDFIsListedOnlyWhenThePDFLedgerVouchesForIt(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	exported, swapped := []byte("%PDF-1.7 exported"), []byte("%PDF-1.7 swapped later")
	agentWrites(p, f, map[string]string{"reports/a.pdf": string(exported), "reports/b.pdf": string(swapped), "reports/c.pdf": "%PDF-1.7 never exported"},
		`{"path":"reports/a.pdf","title":"A","marked":true}`,
		`{"path":"reports/b.pdf","title":"B","marked":true}`,
		`{"path":"reports/c.pdf","title":"C","marked":true}`,
	)
	inner := f.OnForeground
	f.OnForeground = func(r dockerx.RunSpec) {
		inner(r)
		if strings.HasSuffix(r.Name, "-agent") {
			lines := []string{
				`{"path":"reports/a.pdf","sha256":"` + sha(exported) + `","pages":1,"bytes":17}`,
				`{"path":"reports/b.pdf","sha256":"` + sha([]byte("%PDF-1.7 as exported")) + `","pages":1,"bytes":20}`,
			}
			_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", PDFLedgerFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(env.Deliverables); len(got) != 1 || got[0] != "reports/a.pdf" {
		t.Errorf("deliverables = %v, want a.pdf alone", got)
	}
}

// TestNothingMarkedTheRunnerPicks covers the fallback's three cases.
func TestNothingMarkedTheRunnerPicks(t *testing.T) {
	t.Run("the exported PDFs", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		pdf := []byte("%PDF-1.7 exported")
		agentWrites(p, f, map[string]string{"reports/summary.pdf": string(pdf), "reports/summary.md": "# Summary"})
		inner := f.OnForeground
		f.OnForeground = func(r dockerx.RunSpec) {
			inner(r)
			if strings.HasSuffix(r.Name, "-agent") {
				_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", PDFLedgerFile), []byte(`{"path":"reports/summary.pdf","sha256":"`+sha(pdf)+`","pages":1,"bytes":17}`+"\n"), 0o644)
			}
		}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if got := env.Deliverables; got == nil || len(got.Files) != 1 || got.Files[0].Path != "reports/summary.pdf" || got.Files[0].Marked || got.Files[0].Title != "summary" || got.Files[0].Type != "pdf" {
			t.Errorf("deliverables = %+v, want the PDF alone, unmarked", got)
		}
	})
	t.Run("the files in reports", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		agentWrites(p, f, map[string]string{
			"reports/b.md":             "b",
			"reports/a/first.txt":      "first",
			"reports/browser/shot.png": "screenshot",
			"reports/loose.pdf":        "%PDF-1.7 no ledger vouches for it",
		})
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		got := paths(env.Deliverables)
		if len(got) != 2 || got[0] != "reports/a/first.txt" || got[1] != "reports/b.md" {
			t.Fatalf("deliverables = %v, want a/first.txt and b.md (sorted; the browser's folder and an unvouched PDF aside)", got)
		}
		for _, d := range env.Deliverables.Files {
			if d.Marked {
				t.Errorf("%s is marked, though the runner chose it", d.Path)
			}
		}
		if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
			t.Error("the final message was written down although reports/ held files")
		}
	})
	t.Run("the final message", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if got := env.Deliverables; got == nil || len(got.Files) != 1 {
			t.Fatalf("deliverables = %+v", got)
		} else if d := got.Files[0]; d.Path != "reports/deliverable.md" || d.Title != "Result" || d.Marked || d.Type != "md" || d.SHA256 != sha([]byte("final answer\n")) || d.Bytes != 13 {
			t.Errorf("the final message's entry = %+v", d)
		}
		if b, err := os.ReadFile(filepath.Join(p.OutDir, "reports", "deliverable.md")); err != nil || string(b) != "final answer\n" {
			t.Errorf("deliverable.md = %q, %v", b, err)
		}
	})
	t.Run("only the browser's files and no message", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		f.Foreground["-agent"].Stdout = `{"type":"agent_choice","message":{"content":"  "}}` + "\n"
		agentWrites(p, f, map[string]string{"reports/browser/shot.png": "screenshot"})
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if env.Deliverables != nil {
			t.Errorf("deliverables = %+v, want none", env.Deliverables)
		}
		if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
			t.Error("a blank message was written down")
		}
	})
}

func TestAStructuredResultIsNotWrittenDown(t *testing.T) {
	var d *receipt.Deliverables
	rd := &runDirs{outDir: t.TempDir()}
	if got := plainResultText(json.RawMessage(`{"answer":42}`)); got != "" {
		t.Errorf("plainResultText(object) = %q", got)
	}
	if got := plainResultText(json.RawMessage(`"text"`)); got != "text" {
		t.Errorf("plainResultText(text) = %q", got)
	}
	if d = deliverablesRecord(rd, nil, plainResultText(json.RawMessage(`{"answer":42}`))); d != nil {
		t.Errorf("a structured result listed %+v", d)
	}
	if _, err := os.Stat(filepath.Join(rd.outDir, "reports")); err == nil {
		t.Error("a structured result made reports/")
	}
}

func TestAFailedRunListsNoDeliverablesAndWritesNoMessage(t *testing.T) {
	p, f := fixture(t)
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: `{"type":"agent_choice","message":{"content":"partial"}}` + "\n", ExitCode: 3},
	}
	agentWrites(p, f, map[string]string{"reports/half.md": "half"}, `{"path":"reports/half.md","marked":true}`)
	p.DeliverTo = filepath.Join(t.TempDir(), "docs")
	env, err := Run(context.Background(), f, p)
	if err == nil || env == nil {
		t.Fatalf("run = %v, %v", env, err)
	}
	if env.Deliverables != nil || env.Deliveries != nil {
		t.Errorf("a failed run lists deliverables %+v, deliveries %+v", env.Deliverables, env.Deliveries)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
		t.Error("a failed run's message was written down")
	}
	if _, err := os.Stat(p.DeliverTo); err == nil {
		t.Error("a failed run delivered")
	}
	if b, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json")); strings.Contains(string(b), "deliverables") {
		t.Errorf("a failed run's result.json names deliverables:\n%s", b)
	}
}

// TestARunWithoutAFolderDeliversNothing: no destination, no delivery, and no
// record of one.
func TestARunWithoutAFolderDeliversNothing(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Deliverables == nil || env.Deliveries != nil {
		t.Errorf("deliverables %+v, deliveries %+v", env.Deliverables, env.Deliveries)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", "deliveries.ndjson")); err == nil {
		t.Error("a run with no folder wrote a deliveries record")
	}
}

// TestARunCopiesItsDeliverablesToTheFolder: the copy, its record in the
// envelope and receipts/deliveries.ndjson, and the output folder as the run
// left it.
func TestARunCopiesItsDeliverablesToTheFolder(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/brief.md": "# Brief\n", "reports/notes.md": "notes"}, `{"path":"reports/brief.md","title":"The brief","marked":true}`)
	p.DeliverTo = filepath.Join(t.TempDir(), "Documents", "Agentry")
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Deliveries) != 1 {
		t.Fatalf("deliveries = %+v", env.Deliveries)
	}
	d := env.Deliveries[0]
	if d.Status != receipt.DeliveryDelivered || d.Kind != "folder" || d.Destination != p.DeliverTo || d.SHA256 != sha([]byte("# Brief\n")) {
		t.Errorf("delivery = %+v", d)
	}
	// The folder is named for the QuickAgent and the one deliverable's title.
	rel, _ := filepath.Rel(p.DeliverTo, d.Object)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "engine-test" || !strings.HasSuffix(parts[1], " The brief") || parts[2] != "brief.md" {
		t.Errorf("the copy is at %s", rel)
	}
	if b, err := os.ReadFile(d.Object); err != nil || string(b) != "# Brief\n" {
		t.Errorf("the copy = %q, %v", b, err)
	}
	// The output folder keeps what the agent wrote, and the record is in
	// the envelope and the ledger.
	if b, _ := os.ReadFile(filepath.Join(p.OutDir, "reports", "brief.md")); string(b) != "# Brief\n" {
		t.Errorf("the original = %q", b)
	}
	ledger, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "deliveries.ndjson"))
	if err != nil || strings.Count(string(ledger), "\n") != 1 || !strings.Contains(string(ledger), `"status":"delivered"`) {
		t.Errorf("deliveries.ndjson = %q, %v", ledger, err)
	}
	onDisk, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if !strings.Contains(string(onDisk), `"deliveries"`) || strings.Contains(string(onDisk), "sekret-value-1234") {
		t.Errorf("result.json:\n%s", onDisk)
	}
}

// TestAFailedDeliveryNeverChangesTheRunsOutcome: a folder that cannot be made
// is a failed delivery in the record, and the run still succeeded.
func TestAFailedDeliveryNeverChangesTheRunsOutcome(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.DeliverTo = filepath.Join(file, "under")
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("a failed delivery failed the run: %v", err)
	}
	if env.Status != "ok" || env.ExitCode != ExitOK || env.ExitClass != "" && env.ExitClass != "ok" {
		t.Errorf("status %q exit %d class %q", env.Status, env.ExitCode, env.ExitClass)
	}
	if len(env.Deliveries) != 1 || env.Deliveries[0].Status != receipt.DeliveryFailed || env.Deliveries[0].Error != receipt.DeliveryErrDestination {
		t.Errorf("deliveries = %+v", env.Deliveries)
	}
}

// TestASessionListsItsDeliverablesButNeverWritesItsChatDown: a session lists
// the files it marked and, with none marked, the files in reports/; its last
// message is a turn of a chat and never becomes deliverable.md.
func TestASessionListsItsDeliverablesButNeverWritesItsChatDown(t *testing.T) {
	t.Run("marked", func(t *testing.T) {
		s, _, p, _ := startFixture(t)
		get, done := collect(s)
		_ = get
		if err := os.MkdirAll(filepath.Join(p.OutDir, "reports"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(p.OutDir, "reports", "plan.md"), []byte("the plan"), 0o644)
		_ = os.WriteFile(filepath.Join(p.OutDir, "reports", "scratch.md"), []byte("notes"), 0o644)
		_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", DeliverablesLedgerFile), []byte(`{"path":"reports/plan.md","title":"Plan","marked":true}`+"\n"), 0o644)
		env, err := s.End(context.Background(), true)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if got := env.Deliverables; got == nil || len(got.Files) != 1 || got.Files[0].Path != "reports/plan.md" || !got.Files[0].Marked || got.Files[0].Title != "Plan" {
			t.Errorf("deliverables = %+v", got)
		}
	})
	t.Run("chat only", func(t *testing.T) {
		s, _, p, _ := startFixture(t)
		_, done := collect(s)
		env, err := s.End(context.Background(), true)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if env.Deliverables != nil {
			t.Errorf("a session with no files listed %+v", env.Deliverables)
		}
		if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
			t.Error("a session's last message was written down")
		}
	})
}

// TestAHugeReportsFolderIsListedByTheFirstFiles: the block names at most
// MaxDeliverables files, the first in path order, and says it stopped.
func TestAHugeReportsFolderIsListedByTheFirstFiles(t *testing.T) {
	rd := &runDirs{outDir: t.TempDir()}
	for i := range receipt.MaxDeliverables + 5 {
		name := filepath.Join(rd.outDir, "reports", fmt.Sprintf("f%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := deliverablesRecord(rd, nil, "")
	if d == nil || len(d.Files) != receipt.MaxDeliverables || !d.Truncated {
		t.Fatalf("listed %d, truncated %v", len(d.Files), d != nil && d.Truncated)
	}
	if d.Files[0].Path != "reports/f0000.txt" || d.Files[len(d.Files)-1].Path != fmt.Sprintf("reports/f%04d.txt", receipt.MaxDeliverables-1) {
		t.Errorf("first %s, last %s", d.Files[0].Path, d.Files[len(d.Files)-1].Path)
	}
	few := &runDirs{outDir: t.TempDir()}
	_ = os.MkdirAll(filepath.Join(few.outDir, "reports"), 0o755)
	_ = os.WriteFile(filepath.Join(few.outDir, "reports", "a.txt"), []byte("x"), 0o644)
	if d := deliverablesRecord(few, nil, ""); d == nil || d.Truncated {
		t.Errorf("a short list = %+v", d)
	}
}

// TestARunTeardownFailsListsAndDeliversNothing: what teardown could not
// delete turns a successful run into a failed one, and a failed run lists no
// deliverables, writes no deliverable.md and delivers nothing, because the
// listing and the copy wait for teardown.
func TestARunTeardownFailsListsAndDeliversNothing(t *testing.T) {
	holdSecrets(t)
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	p.DeliverTo = filepath.Join(t.TempDir(), "docs")
	env, err := Run(context.Background(), f, p)
	if err == nil {
		t.Fatal("the run ended without an error, with secrets.env left on disk")
	}
	if env.Status != "error" || env.Deliverables != nil || env.Deliveries != nil {
		t.Errorf("status %q, deliverables %+v, deliveries %+v", env.Status, env.Deliverables, env.Deliveries)
	}
	if b, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json")); strings.Contains(string(b), `"deliverables"`) {
		t.Errorf("result.json names deliverables:\n%s", b)
	}
	if _, err := os.Stat(p.DeliverTo); err == nil {
		t.Error("a run teardown failed delivered")
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "reports", "deliverable.md")); err == nil {
		t.Error("a run teardown failed wrote its message down")
	}
}

// TestAHostileFileNameIsOneLineOfPlainText: the title a file's name gives and
// the type its extension gives are held to the same rule as an agent's title.
func TestAHostileFileNameIsOneLineOfPlainText(t *testing.T) {
	rd := &runDirs{outDir: t.TempDir()}
	bidi := string(rune(0x202e))
	name := "q3" + bidi + "fdp\nline" + string(rune(0x200b)) + "break.md"
	for _, n := range []string{name, "x.m" + bidi + "d", "y.a b", "z." + strings.Repeat("e", 40), strings.Repeat("é", 300) + ".md"} {
		p := filepath.Join(rd.outDir, "reports", n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Skipf("this file system takes no name like %q: %v", n, err)
		}
	}
	d := deliverablesRecord(rd, nil, "")
	if d == nil || len(d.Files) != 5 {
		t.Fatalf("deliverables = %+v", d)
	}
	for _, f := range d.Files {
		if err := toolsrv.CheckDeliverableTitle(f.Title); err != nil {
			t.Errorf("%s: title %q: %v", f.Path, f.Title, err)
		}
		if f.Type != "" && !fileTypeRE.MatchString(f.Type) {
			t.Errorf("%s: type %q", f.Path, f.Type)
		}
	}
	for _, f := range d.Files {
		if strings.HasPrefix(f.Path, "reports/q3") && f.Title != "q3_fdp_line_break" {
			t.Errorf("title = %q", f.Title)
		}
		if strings.HasPrefix(f.Path, "reports/z.") && f.Type != "" {
			t.Errorf("a 40-letter extension gave the type %q", f.Type)
		}
	}
}

// TestAFileNamedWithASecretIsNotListed: a secret's value in a file's name never
// reaches the envelope, as it reaches no ledger.
func TestAFileNamedWithASecretIsNotListed(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/notes-sekret-value-1234.md": "x", "reports/fine.md": "y"})
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(env.Deliverables); len(got) != 1 || got[0] != "reports/fine.md" {
		t.Errorf("deliverables = %v, want fine.md alone", got)
	}
	if b, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json")); strings.Contains(string(b), "sekret-value-1234") {
		t.Errorf("result.json holds a secret's value:\n%s", b)
	}
}

// TestTheRunnersPickIsBounded: a reports/ folder bigger than what the runner
// will look at is read in path order to its bounds, and the ledger is not read
// when it is anything but a regular file, nor past its bounds.
func TestTheRunnersPickIsBounded(t *testing.T) {
	oldPaths, oldVisits := maxPickedPaths, maxPickedVisits
	maxPickedPaths, maxPickedVisits = 7, 12
	t.Cleanup(func() { maxPickedPaths, maxPickedVisits = oldPaths, oldVisits })
	rd := &runDirs{outDir: t.TempDir()}
	for i := range 30 {
		dir := filepath.Join(rd.outDir, "reports", fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if d := deliverablesRecord(rd, nil, ""); d != nil {
		t.Errorf("30 empty folders listed %+v", d)
	}
	// The walk stops at its budget of entries, folders included.
	for i := range 30 {
		_ = os.WriteFile(filepath.Join(rd.outDir, "reports", fmt.Sprintf("a%02d.txt", i)), []byte("x"), 0o644)
	}
	d := deliverablesRecord(rd, nil, "")
	if d == nil || len(d.Files) != 7 || d.Files[0].Path != "reports/a00.txt" {
		t.Fatalf("deliverables = %+v, want the first 7 files in path order", d)
	}

	if runtime.GOOS == "windows" {
		return
	}
	ledger := filepath.Join(t.TempDir(), "deliverables.ndjson")
	if err := os.Symlink("/dev/zero", ledger); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadDeliverablesLedger(ledger); err != nil || got != nil {
		t.Errorf("a link as the ledger = %v, %v", got, err)
	}
	big := filepath.Join(t.TempDir(), "big.ndjson")
	line := `{"path":"reports/a.md","marked":true}` + "\n"
	if err := os.WriteFile(big, []byte(strings.Repeat(line, maxLedgerLines+500)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadDeliverablesLedger(big); len(got) != maxLedgerLines {
		t.Errorf("a ledger of %d lines was read as %d", maxLedgerLines+500, len(got))
	}
}
