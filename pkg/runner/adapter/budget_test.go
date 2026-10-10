package adapter

import "testing"

// TestEveryAdapterHasAMeasuredResultBudget: a registered adapter without a
// measured row would run under a guess; each row sits under its harness's
// measured limit (DR-89).
func TestEveryAdapterHasAMeasuredResultBudget(t *testing.T) {
	limits := map[string]int64{
		DefaultName:             50 << 10,
		ClaudeAdapterName:       50000,
		ClaudeImageAdapterName:  50000,
		CodexAdapterName:        12000 / 2,
		CodexImageAdapterName:   12000 / 2,
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
	if got := ResultBudget("no-such-adapter"); got != codexBudget {
		t.Errorf("an unmeasured adapter gets %+v, not the tightest budget", got)
	}
}
