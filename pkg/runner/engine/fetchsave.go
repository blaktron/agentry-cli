package engine

// The fetch ledger (plan input-sources.md §6, agentry-cli#832). The tools
// server saves a fetched body in the scratch folder when the agent names a
// path for it (saveTo) or when it is larger than one result, and records
// each save in the ledger the engine stamps into the run's own tools spec.
// The scratch folder is gone with the run, so the ledger is the record of
// what was saved: the receipt's fetchSaves block counts it, and the ledger
// names each file, its host, its size and its SHA-256.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// FetchLedgerFile is the fetch ledger under the run's receipts.
const FetchLedgerFile = "fetches.ndjson"

// savesFetches says whether a plan's tools can save a fetched body: fetch
// beside the scratch folder.
func savesFetches(tools []string) bool {
	return slices.Contains(tools, toolsrv.ToolFetch) && slices.Contains(tools, toolsrv.ToolScratchWrite)
}

// grantFetchLedger stamps the fetch ledger prepareRunDirs made into the
// run's rendered tools spec.
func grantFetchLedger(gatewayDir string, rd *runDirs) error {
	if rd.fetchLedger == "" {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if !ts.Fetch.Enabled || ts.ScratchDir == "" {
		return nil
	}
	ts.Fetch.Ledger = path.Join(spec.OutMount, "receipts", FetchLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// ReadFetchLedger reads a run's fetch ledger. A missing ledger is none; a
// line that does not decode is skipped.
func ReadFetchLedger(file string) ([]toolsrv.SaveEntry, error) {
	f, err := os.Open(file) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.SaveEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e toolsrv.SaveEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// fetchSavesRecord is the receipt's fetchSaves block: how many bodies fetch
// saved in the scratch folder, how many of them because the agent asked,
// and their bytes. Nil when the run saved none.
func fetchSavesRecord(rd *runDirs) *receipt.FetchSaves {
	if rd.fetchLedger == "" {
		return nil
	}
	// A ledger that stops reading part-way still counts what it holds.
	entries, _ := ReadFetchLedger(rd.fetchLedger)
	if len(entries) == 0 {
		return nil
	}
	rec := &receipt.FetchSaves{Ledger: "receipts/" + FetchLedgerFile}
	for _, e := range entries {
		rec.Files++
		rec.Bytes += e.Bytes
		if e.Asked {
			rec.Asked++
		}
	}
	return rec
}
