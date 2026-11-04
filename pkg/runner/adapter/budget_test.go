package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
	raw, err := codexCatalog("gpt-6.1-sol", 0)
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

// TestTheWindowSizesThePage: a page is a sixteenth of a known window, at
// four bytes a token, between 16 KiB and the adapter's own budget; an
// endpoint of the person's own with no window gets 32 KiB, and a hosted
// model with none keeps the adapter's budget, which its harness sizes from
// its own catalogue.
func TestTheWindowSizesThePage(t *testing.T) {
	own := func(window int64) policy.Model {
		return policy.Model{Provider: "custom", Name: "m", BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: window}
	}
	hosted := policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-5-5"}
	da := ResultBudget(DefaultName)
	for _, c := range []struct {
		name    string
		adapter string
		model   policy.Model
		bytes   int64
	}{
		{"hosted, no window", DefaultName, hosted, da.Bytes},
		{"hosted, a small window", DefaultName, policy.Model{Provider: policy.ProviderAnthropic, Name: "m", ContextWindow: 65536}, 16 << 10},
		{"own, 262k", DefaultName, own(262144), da.Bytes},
		{"own, 200k", DefaultName, own(200000), da.Bytes},
		{"own, 128k", DefaultName, own(131072), 32 << 10},
		{"own, 100k, a whole KiB", DefaultName, own(100000), 24 << 10},
		{"own, 32k", DefaultName, own(32768), 16 << 10},
		{"own, the smallest window", DefaultName, own(policy.MinContextWindow), 16 << 10},
		{"own, no window", DefaultName, own(0), 32 << 10},
		{"openai at a base URL, no window", DefaultName, policy.Model{Provider: policy.ProviderOpenAI, Name: "m", BaseURL: "http://127.0.0.1:1234/v1"}, 32 << 10},
		{"never above a tighter adapter's", CopilotImageAdapterName, own(0), ResultBudget(CopilotImageAdapterName).Bytes},
	} {
		got := PageBudget(c.adapter, c.model)
		if got.Bytes != c.bytes || got.Lines != ResultBudget(c.adapter).Lines {
			t.Errorf("%s: page %+v, want %d bytes and the adapter's %d lines", c.name, got, c.bytes, ResultBudget(c.adapter).Lines)
		}
	}
}
