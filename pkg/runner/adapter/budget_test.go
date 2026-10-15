package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryAdapterHasAMeasuredResultBudget: a registered adapter without a
// measured row would run under a guess; each row sits under its harness's
// measured limit.
func TestEveryAdapterHasAMeasuredResultBudget(t *testing.T) {
	limits := map[string]int64{
		DefaultName:            50 << 10,
		ClaudeAdapterName:      50000,
		ClaudeImageAdapterName: 50000,
		// Half the limit the adapter's own catalog sets (codexCatalog): a
		// resource is sent to the model as escaped JSON.
		CodexAdapterName:        codexToolResultBytes / 2,
		CodexImageAdapterName:   codexToolResultBytes / 2,
		CopilotImageAdapterName: 20480,
	}
	for _, name := range Names() {
		b, ok := resultBudgets[name]
		if !ok {
			t.Errorf("adapter %s has no measured result budget", name)
			continue
		}
		limit, ok := limits[name]
		if !ok {
			t.Errorf("adapter %s: no measured limit recorded in this test", name)
			continue
		}
		if b.Bytes >= limit || b.Bytes < limit*80/100 || b.Lines <= 0 || b.Lines >= 2000 {
			t.Errorf("adapter %s: budget %+v against a measured limit of %d bytes and 2,000 lines", name, b, limit)
		}
	}
	tightest := Budget{}
	for _, b := range resultBudgets {
		if tightest.Bytes == 0 || b.Bytes < tightest.Bytes {
			tightest = b
		}
	}
	if got := ResultBudget("no-such-adapter"); got != tightest {
		t.Errorf("an unmeasured adapter gets %+v, not the tightest budget %+v", got, tightest)
	}
}

// TestTheCodexCatalogKeepsAWholePage: the catalog the codex adapters render
// sets the limit codex holds an MCP result to, and a page at the codex
// budget fits it even as a resource of short lines, which codex sends as
// escaped JSON and which doubles in it.
func TestTheCodexCatalogKeepsAWholePage(t *testing.T) {
	raw, err := codexCatalog("gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Models []struct {
			TruncationPolicy struct {
				Mode  string `json:"mode"`
				Limit int64  `json:"limit"`
			} `json:"truncation_policy"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil || len(cat.Models) != 1 {
		t.Fatalf("catalog: %v %s", err, raw)
	}
	tp := cat.Models[0].TruncationPolicy
	if tp.Mode != "bytes" || tp.Limit != codexToolResultBytes {
		t.Fatalf("truncation_policy = %+v, want bytes %d", tp, codexToolResultBytes)
	}
	for _, b := range []Budget{ResultBudget(CodexAdapterName), ResultBudget(CodexImageAdapterName)} {
		if b.Bytes < 16<<10 {
			t.Errorf("codex budget %+v: a page this small starves a run reading a codebase", b)
		}
		page := strings.Repeat("\n", int(b.Bytes))[:b.Bytes]
		item, _ := json.Marshal(map[string]any{"type": "resource", "resource": map[string]any{"uri": "agentry://inputs", "mimeType": "text/plain", "text": page}})
		if int64(len(item)) > tp.Limit {
			t.Errorf("a %d-byte page of short lines is %d bytes as codex sends it, past the %d it keeps", b.Bytes, len(item), tp.Limit)
		}
	}
}
