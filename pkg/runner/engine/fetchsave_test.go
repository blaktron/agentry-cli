package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheReceiptCountsFetchSaves: a run whose fetch can save into the
// scratch folder gets the fetch ledger in its own tools spec, the receipt
// counts the saves the ledger records, those the agent asked for among
// them, and a path the agent chose that holds a run secret's value is
// masked in the ledger.
func TestTheReceiptCountsFetchSaves(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	t.Setenv("ENGINE_TEST_TOKEN", "engine-secret-value-123")
	ledger := filepath.Join(p.OutDir, "receipts", FetchLedgerFile)
	f.OnForeground = func(r dockerx.RunSpec) {
		if !strings.HasSuffix(r.Name, "-agent") {
			return
		}
		lines := []string{
			`{"path":"fetch/1-big.txt","host":"api.example.com","bytes":100000,"sha256":"aa"}`,
			`{"path":"repo.zip","host":"api.example.com","bytes":5000000,"sha256":"bb","asked":true}`,
			`not json`,
			`{"path":"engine-secret-value-123.png","host":"api.example.com","bytes":7,"sha256":"cc","asked":true}`,
		}
		_ = os.WriteFile(ledger, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if env.FetchSaves == nil || env.FetchSaves.Files != 3 || env.FetchSaves.Asked != 2 || env.FetchSaves.Bytes != 5100007 || env.FetchSaves.Ledger != "receipts/"+FetchLedgerFile {
		t.Fatalf("fetchSaves = %+v", env.FetchSaves)
	}
	if b, _ := os.ReadFile(ledger); strings.Contains(string(b), "engine-secret-value-123") {
		t.Errorf("the ledger holds a secret's value:\n%s", b)
	}
	result, err := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if err != nil || !strings.Contains(string(result), `"fetchSaves"`) {
		t.Errorf("result.json lacks the fetchSaves block: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Fetch.Ledger != "/out/receipts/"+FetchLedgerFile {
		t.Errorf("the run's tools spec fetch = %+v", ts.Fetch)
	}
}

// A run that saved nothing has no fetchSaves block but its empty ledger,
// and a run that cannot fetch gets no ledger.
func TestNoSaveNoFetchSavesBlock(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if env.FetchSaves != nil {
		t.Errorf("fetchSaves = %+v", env.FetchSaves)
	}
	if fi, err := os.Stat(filepath.Join(p.OutDir, "receipts", FetchLedgerFile)); err != nil || fi.Size() != 0 {
		t.Errorf("the fetching run's ledger: %v", err)
	}

	p, f = fixtureEdited(t, nil, func(m *manifest.Manifest) { m.Capabilities.Network.Egress = manifest.Egress{} })
	happyFake(f)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", FetchLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a run without fetch has a fetch ledger: %v", err)
	}
}
