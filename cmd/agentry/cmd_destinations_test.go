package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/send"
)

// destinationsEnv isolates the config folder and gives the keychain a mock, never
// the person's.
func destinationsEnv(t *testing.T) string {
	t.Helper()
	isolateEnv(t)
	t.Setenv(modelkey.EnvNoKeyring, "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	dir, err := configDirForTest()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func configDirForTest() (string, error) { return configDir() }

func TestDestinationsAddListRemoveAndDefault(t *testing.T) {
	dir := destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "whsec_PLANTEDfromTHEenvironment")
	t.Setenv("HOOK_AUTH", "Bearer PLANTEDauthorizationVALUE")
	out, errOut, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://hooks.example.com/webhook/abc?token=URLTOKEN", "--secret-env", "HOOK_SECRET", "--authorization-env", "HOOK_AUTH")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saved destination n8n: webhook to https://hooks.example.com") || strings.Contains(out+errOut, "PLANTED") || strings.Contains(out+errOut, "URLTOKEN") {
		t.Errorf("add: %q %q", out, errOut)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, destination.File))
	if strings.Contains(string(raw), "PLANTED") {
		t.Errorf("the file holds a credential: %s", raw)
	}
	if s, a, _ := destination.Secrets("n8n"); s != "whsec_PLANTEDfromTHEenvironment" || a != "Bearer PLANTEDauthorizationVALUE" {
		t.Errorf("keychain: %q %q", s, a)
	}
	if _, _, err := runCLI(t, nil, "destinations", "add", "lan", "--url", "http://192.168.1.20:5678/x", "--on-my-network", "--generate-secret"); err != nil {
		t.Fatal(err)
	}
	// A saved destination changed again without a secret keeps both credentials.
	if _, _, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://hooks.example.com/webhook/def"); err != nil {
		t.Fatal(err)
	}
	if s, a, _ := destination.Secrets("n8n"); s == "" || a == "" {
		t.Errorf("a change dropped a credential: %q %q", s, a)
	}
	if _, _, err := runCLI(t, nil, "destinations", "default", "acme/weekly", "n8n", "lan"); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, nil, "destinations", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"n8n  webhook  https://hooks.example.com  default for acme/weekly", "lan  webhook  http://192.168.1.20:5678  on my network  default for acme/weekly"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PLANTED") || strings.Contains(out, "URLTOKEN") || strings.Contains(out, "/webhook/") {
		t.Errorf("list shows a secret or a path:\n%s", out)
	}
	out, _, _ = runCLI(t, nil, "destinations", "list", "--json")
	var got struct{ Destinations []destinationRow }
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Destinations) != 2 || !got.Destinations[1].Secret || !got.Destinations[1].Authorization || got.Destinations[0].Authorization {
		t.Errorf("json = %s (%v)", out, err)
	}
	if _, _, err := runCLI(t, nil, "destinations", "remove", "n8n"); err != nil {
		t.Fatal(err)
	}
	if s, a, _ := destination.Secrets("n8n"); s != "" || a != "" {
		t.Errorf("remove left credentials: %q %q", s, a)
	}
	out, _, _ = runCLI(t, nil, "destinations", "list")
	if strings.Contains(out, "n8n") || !strings.Contains(out, "default for acme/weekly") {
		t.Errorf("after remove:\n%s", out)
	}
	_, _, err = runCLI(t, nil, "destinations", "remove", "n8n")
	wantClassified(t, err, engine.ExitUsage)
}

func TestDestinationsAddRefusesWhatTheAddressRuleRefuses(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "s")
	for _, args := range [][]string{
		{"n8n", "--url", "http://hooks.example.com/x", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://10.0.0.5/x", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://user:pw@hooks.example.com/x", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://hooks.example.com:8443/x", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://127.0.0.1:8443/x", "--on-my-network", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://169.254.169.254/x", "--on-my-network", "--secret-env", "HOOK_SECRET"},
		{"Bad Name", "--url", "https://hooks.example.com/x", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://hooks.example.com/x", "--generate-secret", "--secret-env", "HOOK_SECRET"},
		{"n8n", "--url", "https://hooks.example.com/x"},
	} {
		_, _, err := runCLI(t, nil, append([]string{"destinations", "add"}, args...)...)
		if err == nil {
			t.Errorf("%v was taken", args)
			continue
		}
		wantClassified(t, err, engine.ExitUsage)
	}
	s, _ := destination.Load()
	if len(s.Destinations) != 0 {
		t.Errorf("a refused add saved %+v", s.Destinations)
	}
}

func TestAGeneratedSecretIsShownOnceOnStderr(t *testing.T) {
	destinationsEnv(t)
	out, errOut, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://hooks.example.com/x", "--generate-secret")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := destination.Secrets("n8n")
	if !strings.HasPrefix(s, "whsec_") || len(s) < 40 || !strings.Contains(errOut, s) || strings.Contains(out, s) {
		t.Errorf("secret %q, stdout %q, stderr %q", s, out, errOut)
	}
	if out, _, _ := runCLI(t, nil, "destinations", "list"); strings.Contains(out, s) {
		t.Error("list shows the secret")
	}
}

func TestASecretCanComeOnStdin(t *testing.T) {
	destinationsEnv(t)
	if _, _, err := runCLI(t, strings.NewReader("whsec_fromSTDIN\n"), "destinations", "add", "n8n", "--url", "https://hooks.example.com/x", "--secret-stdin"); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := destination.Secrets("n8n"); s != "whsec_fromSTDIN" {
		t.Errorf("secret %q", s)
	}
}

func TestARunNamesDestinationsThatAreSavedOrItsDefaultSet(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "s")
	for _, n := range []string{"a", "b"} {
		if _, _, err := runCLI(t, nil, "destinations", "add", n, "--url", "https://hooks.example.com/"+n, "--secret-env", "HOOK_SECRET"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := runCLI(t, nil, "destinations", "default", "acme/weekly", "b"); err != nil {
		t.Fatal(err)
	}
	got, err := chooseDestinations(nil, "acme/weekly@1.0.0")
	if err != nil || len(got) != 1 || got[0].Name != "b" {
		t.Errorf("default set: %+v %v", got, err)
	}
	got, err = chooseDestinations([]string{"a"}, "acme/weekly@1.0.0")
	if err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Errorf("named: %+v %v", got, err)
	}
	if got, err := chooseDestinations(nil, "acme/other@1"); err != nil || got != nil {
		t.Errorf("a QuickAgent with no set: %+v %v", got, err)
	}
	_, err = chooseDestinations([]string{"nope"}, "acme/weekly@1.0.0")
	wantClassified(t, err, engine.ExitUsage)
	if _, _, err := runCLI(t, nil, "destinations", "default", "acme/weekly", "nope"); err == nil {
		t.Error("a default naming nothing was taken")
	}
}

func TestTheRunSaysWhatItSentToEachDestination(t *testing.T) {
	got := strings.Join(deliverySummary([]receipt.Delivery{
		{Destination: "n8n", Kind: "webhook", Object: "reports/a.md", Status: receipt.DeliveryDelivered},
		{Destination: "n8n", Kind: "webhook", Object: "reports/b.bin", Status: receipt.DeliveryRefused, Error: receipt.DeliveryErrTooLarge},
		{Destination: "lan", Kind: "webhook", Object: "reports/a.md", Status: receipt.DeliveryFailed, Error: receipt.DeliveryErrStatus},
		{Destination: "res", Kind: "webhook", Object: "result", Status: receipt.DeliveryDelivered},
	}), "\n")
	for _, want := range []string{
		"destination n8n: 1 file sent",
		"destination n8n: 1 file listed without content (over the 10 MiB body cap)",
		"destination lan: 1 not sent (status); the run itself is unaffected",
		"agentry deliver RUN-FOLDER --destination lan tries again",
		"destination res: the result sent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary lacks %q:\n%s", want, got)
		}
	}
}

// TestAgentryDeliverToADestinationRetriesAFailedRun: the retry sends the run
// folder through the delivery step, adds to the ledger, and leaves the folder as
// it was.
func TestAgentryDeliverToADestinationRetriesAFailedRun(t *testing.T) {
	destinationsEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	t.Setenv("HOOK_SECRET", "whsec_PLANTED")
	if _, _, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://hooks.example.com/x", "--secret-env", "HOOK_SECRET"); err != nil {
		t.Fatal(err)
	}
	dir := deliverRunFolder(t, "ok", map[string]string{"reports/summary.md": "# Summary\n"})
	resultBefore, _ := os.ReadFile(filepath.Join(dir, "result.json"))
	f := &dockerx.Fake{}
	f.Foreground = map[string]*dockerx.FakeForeground{"-delivery": {Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
		b, _ := io.ReadAll(stdin)
		if !strings.Contains(string(b), "whsec_PLANTED") {
			t.Errorf("the step's stdin: %q", b)
		}
		var work, jobDir string
		for _, m := range r.Mounts {
			switch m.Target {
			case "/work":
				work = m.Source
			case "/job":
				jobDir = m.Source
			}
		}
		job, err := send.ReadJob(jobDir)
		if err != nil || len(job.Files) != 1 || job.Files[0].Staged == "" || job.RunID != "3f9a1c2e" {
			t.Errorf("job %+v %v", job, err)
		}
		rep, _ := json.Marshal(send.Report{Schema: send.ReportSchema, Kind: "webhook", Status: send.StatusDelivered, Files: []send.FileResult{{Path: "reports/summary.md", Status: send.StatusDelivered}}})
		_ = os.WriteFile(filepath.Join(work, send.ReportFile), rep, 0o644)
		return 0, nil
	}}}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	out, _, err := runCLI(t, nil, "deliver", dir, "--destination", "n8n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sent reports/summary.md to n8n") {
		t.Errorf("stdout %q", out)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "result.json")); string(after) != string(resultBefore) {
		t.Error("deliver rewrote result.json")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "receipts", "deliveries.ndjson")); err != nil || !strings.Contains(string(b), `"destination":"n8n"`) || strings.Contains(string(b), "PLANTED") {
		t.Errorf("deliveries.ndjson = %q, %v", b, err)
	}
	// A destination that is not saved refuses before anything is run.
	_, _, err = runCLI(t, nil, "deliver", dir, "--destination", "nope")
	wantClassified(t, err, engine.ExitUsage)
}
