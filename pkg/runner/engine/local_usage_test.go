package engine

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// TestALocalRunsOutputIsInTokens (plan scan-and-run-tweaks.md invariant 10;
// agentry-cli#500): a whole run on the fake docker client, to the person's
// own model provider. Its envelope and the usage it reports as it goes
// count tokens, and nothing it writes — the narration, the envelope, a
// file in the output folder or its receipts — is worded in credits.
func TestALocalRunsOutputIsInTokens(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	var narration bytes.Buffer
	var reported []receipt.Usage
	p.Stderr = &narration
	p.OnUsage = func(u receipt.Usage) { reported = append(reported, u) }
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if env.Usage.InputTokens != 100 || env.Usage.OutputTokens != 40 || env.Usage.Total() != 140 {
		t.Fatalf("usage = %+v", env.Usage)
	}
	if len(reported) == 0 || reported[len(reported)-1].InputTokens != 100 || reported[len(reported)-1].OutputTokens != 40 {
		t.Fatalf("the usage reported as the run went: %+v", reported)
	}
	credits := func(where string, b []byte) {
		if strings.Contains(strings.ToLower(string(b)), "credit") {
			t.Errorf("%s is worded in credits:\n%s", where, b)
		}
	}
	// A clean run may narrate nothing of its own; whatever it did say is read.
	credits("the narration", narration.Bytes())
	raw, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	credits("the envelope", raw)
	files := 0
	err = filepath.WalkDir(p.OutDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		credits(path, b)
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("the output folder: %d file(s), %v", files, err)
	}
}
