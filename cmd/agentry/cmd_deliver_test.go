package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func TestChooseDeliverFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := t.TempDir()
	t.Chdir(work)
	abs := filepath.Join(t.TempDir(), "docs")

	for _, c := range []struct {
		name, flag, config, want string
		usage                    bool
	}{
		{name: "nothing set delivers nowhere"},
		{name: "the flag, absolute", flag: abs, want: abs},
		{name: "the flag, relative to the working folder", flag: "out/docs", want: filepath.Join(work, "out", "docs")},
		{name: "the flag, with the home folder", flag: "~/Documents/Agentry", want: filepath.Join(home, "Documents", "Agentry")},
		{name: "the config, absolute", config: abs, want: abs},
		{name: "the config, with the home folder", config: "~/Documents/Agentry", want: filepath.Join(home, "Documents", "Agentry")},
		{name: "the flag wins over the config", flag: abs, config: "~/else", want: abs},
		{name: "a relative config folder is refused", config: "Documents/Agentry", usage: true},
		{name: "a blank config folder is none", config: "   "},
	} {
		cfg := cliConfig{Deliver: deliverConfig{Folder: c.config}}
		got, err := chooseDeliverFolder(c.flag, cfg)
		if c.usage {
			wantClassified(t, err, engine.ExitUsage)
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: (%q, %v), want %q", c.name, got, err, c.want)
		}
	}
}

func TestARelativeDeliverFolderRefusesTheRunBeforeItStarts(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	if err := saveCLIConfig(cliConfig{Deliver: deliverConfig{Folder: "Documents/Agentry"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--policy", policyPath, "--yes", "--dry-run")
	wantClassified(t, err, engine.ExitUsage)
	if err == nil || !strings.Contains(err.Error(), "deliver.folder") {
		t.Errorf("error = %v", err)
	}
}

func TestTheConfigReadsTheDeliverFolder(t *testing.T) {
	dir := isolateConfig(t)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("deliver:\n  folder: /srv/agentry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCLIConfig()
	if err != nil || cfg.Deliver.Folder != "/srv/agentry" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}

// deliverRunFolder is a finished run's folder with the given reports and an
// envelope listing them.
func deliverRunFolder(t *testing.T, status string, reports map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: status, RunID: "3f9a1c2e", Ref: "acme/weekly@1.0.0", OutDir: dir}
	d := &receipt.Deliverables{}
	for rel, content := range reports {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		d.Files = append(d.Files, receipt.Deliverable{Path: rel, Title: "T", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Type: "md", Marked: true})
	}
	if status == "ok" {
		env.Deliverables = d
	}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAgentryDeliverCopiesFromARunFolder(t *testing.T) {
	isolateEnv(t)
	dir := deliverRunFolder(t, "ok", map[string]string{"reports/summary.md": "# Summary\n"})
	resultBefore, _ := os.ReadFile(filepath.Join(dir, "result.json"))
	to := filepath.Join(t.TempDir(), "Agentry")

	stdout, _, err := runCLI(t, nil, "deliver", dir, "--to", to)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(to, "weekly")
	if !strings.HasPrefix(stdout, "copied "+copied) || !strings.HasSuffix(strings.TrimSpace(stdout), "summary.md") {
		t.Errorf("stdout = %q", stdout)
	}
	matches, _ := filepath.Glob(filepath.Join(copied, "*", "summary.md"))
	if len(matches) != 1 {
		t.Fatalf("copies = %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "# Summary\n" {
		t.Errorf("the copy = %q", b)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "result.json")); string(after) != string(resultBefore) {
		t.Error("deliver rewrote result.json")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "receipts", "deliveries.ndjson")); err != nil || !strings.Contains(string(b), `"delivered"`) {
		t.Errorf("deliveries.ndjson = %q, %v", b, err)
	}

	// The folder comes from the config when --to is not given, and --json
	// prints the records.
	if err := saveCLIConfig(cliConfig{Deliver: deliverConfig{Folder: to}}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, nil, "deliver", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Deliveries []receipt.Delivery `json:"deliveries"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || len(got.Deliveries) != 1 || got.Deliveries[0].Status != receipt.DeliveryDelivered {
		t.Errorf("json = %s (%v)", stdout, err)
	}
}

func TestAgentryDeliverNeedsAFolderAndASuccessfulRun(t *testing.T) {
	isolateEnv(t)
	ok := deliverRunFolder(t, "ok", map[string]string{"reports/a.md": "a"})
	if _, _, err := runCLI(t, nil, "deliver", ok); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("no folder = %v", err)
	} else {
		wantClassified(t, err, engine.ExitUsage)
	}
	failed := deliverRunFolder(t, "error", map[string]string{"reports/a.md": "a"})
	_, _, err := runCLI(t, nil, "deliver", failed, "--to", t.TempDir())
	wantClassified(t, err, engine.ExitCLI)
	if !strings.Contains(err.Error(), "did not succeed") {
		t.Errorf("a failed run = %v", err)
	}
	_, _, err = runCLI(t, nil, "deliver", t.TempDir(), "--to", t.TempDir())
	wantClassified(t, err, engine.ExitCLI)
}

func TestAgentryDeliverExitsNonZeroWhenAFileIsRefused(t *testing.T) {
	isolateEnv(t)
	dir := deliverRunFolder(t, "ok", map[string]string{"reports/a.md": "original", "reports/b.md": "fine"})
	if err := os.WriteFile(filepath.Join(dir, "reports", "a.md"), []byte("changed!"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := t.TempDir()
	stdout, _, err := runCLI(t, nil, "deliver", dir, "--to", to)
	wantClassified(t, err, engine.ExitCLI)
	if !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(stdout, "not copied (refused: changed)") || !strings.Contains(stdout, "copied "+to) {
		t.Errorf("stdout %q, error %v", stdout, err)
	}
}

func TestTheRunSaysWhatItDelivered(t *testing.T) {
	got := strings.Join(deliverySummary([]receipt.Delivery{
		{Status: receipt.DeliveryDelivered, Object: "/d/weekly/2026-10-17 1504 x/a.md"},
		{Status: receipt.DeliveryDelivered, Object: "/d/weekly/2026-10-17 1504 x/b.md", Error: receipt.DeliveryErrNotMarked},
		{Status: receipt.DeliveryRefused, Error: receipt.DeliveryErrChanged},
		{Status: receipt.DeliveryFailed, Error: receipt.DeliveryErrWrite},
	}), "\n")
	for _, want := range []string{
		"deliverables: 2 files copied to /d/weekly/2026-10-17 1504 x\n",
		"deliverables: 1 copy could not be flagged as downloaded",
		"deliverables: 1 file not copied (changed)",
		"deliverables: 1 file not copied (write)",
		"the run itself is unaffected",
	} {
		if !strings.Contains(got+"\n", want) {
			t.Errorf("the summary lacks %q:\n%s", want, got)
		}
	}
	if deliverySummary(nil) != nil {
		t.Error("a run that delivered nothing says something")
	}
}
