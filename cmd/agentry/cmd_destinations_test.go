package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	got, fromDefault, err := chooseDestinations(nil, "acme/weekly@1.0.0", false)
	if err != nil || len(got) != 1 || got[0].Name != "b" || !fromDefault {
		t.Errorf("default set: %+v %v %v", got, fromDefault, err)
	}
	got, fromDefault, err = chooseDestinations([]string{"a"}, "acme/weekly@1.0.0", false)
	if err != nil || len(got) != 1 || got[0].Name != "a" || fromDefault {
		t.Errorf("named: %+v %v %v", got, fromDefault, err)
	}
	if got, _, err := chooseDestinations(nil, "acme/other@1", false); err != nil || got != nil {
		t.Errorf("a QuickAgent with no set: %+v %v", got, err)
	}
	// A folder on disk names itself in its own manifest, so it gets no default
	// set, and a name given for it is used as given.
	if got, fromDefault, err := chooseDestinations(nil, "acme/weekly@1.0.0", true); err != nil || got != nil || fromDefault {
		t.Errorf("a local folder took a default set: %+v %v %v", got, fromDefault, err)
	}
	if got, _, err := chooseDestinations([]string{"a"}, "acme/weekly@1.0.0", true); err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Errorf("a local folder with a named destination: %+v %v", got, err)
	}
	_, _, err = chooseDestinations([]string{"nope"}, "acme/weekly@1.0.0", false)
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

// TestChangingAnAddressToAnotherHostDropsTheSavedAuthorization: an Authorization
// value saved for one receiver is never sent to another; changing only the path
// keeps it.
func TestChangingAnAddressToAnotherHostDropsTheSavedAuthorization(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "s")
	t.Setenv("HOOK_AUTH", "Bearer FIRSTHOST")
	if _, _, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://one.example.com/a", "--secret-env", "HOOK_SECRET", "--authorization-env", "HOOK_AUTH"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://one.example.com/b"); err != nil {
		t.Fatal(err)
	}
	if _, a, _ := destination.Secrets("n8n"); a != "Bearer FIRSTHOST" {
		t.Errorf("a change of path dropped the value: %q", a)
	}
	_, errOut, err := runCLI(t, nil, "destinations", "add", "n8n", "--url", "https://two.example.com/b")
	if err != nil {
		t.Fatal(err)
	}
	if _, a, _ := destination.Secrets("n8n"); a != "" || !strings.Contains(errOut, "Authorization value is dropped") {
		t.Errorf("a change of host kept the value %q (%s)", a, errOut)
	}
}

// TestARefusedSaveLeavesNoCredentialBehind: an address the rules refuse is
// refused before the keychain is asked, and a full file leaves none of the
// credentials of the destination it did not save.
func TestARefusedSaveLeavesNoCredentialBehind(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "s")
	s, _ := destination.Load()
	for i := 0; i < destination.MaxDestinations; i++ {
		if err := s.Upsert(destination.Destination{Name: fmt.Sprintf("d%02d", i), Kind: destination.KindWebhook, URL: "https://hooks.example.com/x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "destinations", "add", "overflow", "--url", "https://hooks.example.com/x", "--secret-env", "HOOK_SECRET"); err == nil {
		t.Fatal("a destination past the limit was saved")
	}
	if destination.HasSecret("overflow") {
		t.Error("the keychain holds the secret of a destination that was not saved")
	}
}

func TestDestinationsAddSavesABucketAndKeepsItsKeyPairInTheKeychain(t *testing.T) {
	dir := destinationsEnv(t)
	t.Setenv("S3_KEY_ID", "AKIAPLANTEDKEYID0123")
	t.Setenv("S3_SECRET", "PLANTEDsecretACCESSkey/0123456789")
	out, errOut, err := runCLI(t, nil, "destinations", "add", "archive", "--bucket", "my-reports", "--prefix", "/agentry/out/", "--region", "eu-west-1", "--access-key-id-env", "S3_KEY_ID", "--secret-env", "S3_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saved destination archive: bucket s3://my-reports/agentry/out at https://my-reports.s3.eu-west-1.amazonaws.com:443") || strings.Contains(out+errOut, "PLANTED") {
		t.Errorf("add: %q %q", out, errOut)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, destination.File))
	if strings.Contains(string(raw), "PLANTED") || !strings.Contains(string(raw), "kind: s3") {
		t.Errorf("the file: %s", raw)
	}
	r, err := destination.Resolve([]string{"archive"})
	if err != nil || len(r) != 1 || r[0].AccessKeyID != "AKIAPLANTEDKEYID0123" || r[0].Secret != "PLANTEDsecretACCESSkey/0123456789" || r[0].Prefix != "agentry/out" {
		t.Fatalf("%+v %v", r, err)
	}
	// Saved again for another bucket of the same service, with no credentials
	// given: the pair is kept.
	if _, _, err := runCLI(t, nil, "destinations", "add", "archive", "--bucket", "other-reports", "--region", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := destination.Resolve([]string{"archive"}); len(r) != 1 || r[0].AccessKeyID == "" || r[0].Bucket != "other-reports" {
		t.Errorf("a change dropped the pair: %+v", r)
	}
	// Moved to another service: the saved pair is not kept for it, and a
	// change that gives none is refused.
	_, errOut, err = runCLI(t, nil, "destinations", "add", "archive", "--bucket", "other-reports", "--endpoint", "https://acct.r2.cloudflarestorage.com", "--region", "auto")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(errOut, "another service") {
		t.Errorf("stderr: %q", errOut)
	}
	if r, _ := destination.Resolve([]string{"archive"}); len(r) != 1 || r[0].Endpoint != "" {
		t.Errorf("a refused change was saved: %+v", r)
	}
	// A bucket on the person's network, with the defaults of an endpoint.
	t.Setenv("MINIO_USER", "minioadmin")
	t.Setenv("MINIO_PASSWORD", "minio-password-1")
	if _, _, err := runCLI(t, nil, "destinations", "add", "minio", "--bucket", "reports", "--endpoint", "http://192.168.1.20:9000", "--on-my-network", "--access-key-id-env", "MINIO_USER", "--secret-env", "MINIO_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if r, _ := destination.Resolve([]string{"minio"}); len(r) != 1 || r[0].Region != "us-east-1" || !r[0].OnMyNetwork {
		t.Errorf("minio: %+v", r)
	}
	out, _, _ = runCLI(t, nil, "destinations", "list")
	for _, want := range []string{"archive  s3  s3://other-reports at https://other-reports.s3.us-east-1.amazonaws.com:443", "minio  s3  s3://reports at http://192.168.1.20:9000  on my network"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PLANTED") || strings.Contains(out, "minio-password") {
		t.Errorf("list shows a credential:\n%s", out)
	}
	out, _, _ = runCLI(t, nil, "destinations", "list", "--json")
	var got struct{ Destinations []destinationRow }
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Destinations) != 2 || !got.Destinations[0].AccessKey || !got.Destinations[0].Secret || got.Destinations[0].Kind != "s3" {
		t.Errorf("json = %s (%v)", out, err)
	}
	if _, _, err := runCLI(t, nil, "destinations", "remove", "minio"); err != nil {
		t.Fatal(err)
	}
	if destination.HasAccessKey("minio") || destination.HasSecret("minio") {
		t.Error("remove left the key pair")
	}
}

func TestDestinationsAddRefusesABucketThatBreaksTheRules(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("S3_KEY_ID", "AKIAPLANTEDKEYID0123")
	t.Setenv("S3_SECRET", "secret")
	creds := []string{"--access-key-id-env", "S3_KEY_ID", "--secret-env", "S3_SECRET"}
	for _, args := range [][]string{
		{"b", "--bucket", "my-reports", "--url", "https://hooks.example.com/x"},
		{"b", "--bucket", "my-reports"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--prefix", "../x"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--endpoint", "https://10.0.0.5"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--endpoint", "http://169.254.169.254", "--on-my-network"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--endpoint", "https://minio.example.com/b"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--on-my-network"},
		{"b", "--bucket", "Bad_Bucket", "--region", "us-east-1"},
		{"b", "--bucket", "my.reports", "--region", "us-east-1"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--generate-secret"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--authorization-env", "S3_SECRET"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--access-key-id-env", "S3_KEY_ID", "--secret-env", "S3_SECRET", "--secret-stdin"},
		{"hook", "--url", "https://hooks.example.com/x", "--region", "us-east-1", "--secret-env", "S3_SECRET"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--access-key-id-env", "S3_KEY_ID"},
		{"b", "--bucket", "my-reports", "--region", "us-east-1", "--secret-env", "S3_SECRET"},
	} {
		a := append([]string{"destinations", "add"}, args...)
		if !strings.Contains(strings.Join(args, " "), "--access-key-id-env") && !strings.Contains(strings.Join(args, " "), "--secret-env") {
			a = append(a, creds...)
		}
		_, _, err := runCLI(t, nil, a...)
		if err == nil {
			t.Errorf("%v was taken", args)
			continue
		}
		wantClassified(t, err, engine.ExitUsage)
	}
	if s, _ := destination.Load(); len(s.Destinations) != 0 {
		t.Errorf("a refused add saved %+v", s.Destinations)
	}
}

func TestTheRunSaysWhatItPutInEachBucket(t *testing.T) {
	got := strings.Join(deliverySummary([]receipt.Delivery{
		{Destination: "archive", Kind: "s3", Object: "b/run/a.md", Status: receipt.DeliveryDelivered},
		{Destination: "archive", Kind: "s3", Object: "b/run/big.pdf", Status: receipt.DeliveryRefused, Error: receipt.DeliveryErrTooLarge},
		{Destination: "archive", Kind: "s3", Object: "b/run/c.md", Status: receipt.DeliveryFailed, Error: receipt.DeliveryErrStatus},
	}), "\n")
	for _, want := range []string{"destination archive: 1 file sent", "destination archive: 1 file not sent (over the 64 MiB an object may be)", "destination archive: 1 not sent (status)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "body cap") {
		t.Errorf("a bucket's file is not listed in a body:\n%s", got)
	}
}

func TestABucketsSecretIsNeverAWebhooksSigningSecret(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("S3_KEY_ID", "AKIAPLANTEDKEYID0123")
	t.Setenv("S3_SECRET", "PLANTEDsecretACCESSkey/0123456789")
	if _, _, err := runCLI(t, nil, "destinations", "add", "x", "--bucket", "my-reports", "--region", "eu-west-1", "--access-key-id-env", "S3_KEY_ID", "--secret-env", "S3_SECRET"); err != nil {
		t.Fatal(err)
	}
	// The same name pointed at a webhook, with no secret given, is refused: it
	// would sign that receiver's bodies with the person's secret access key.
	_, _, err := runCLI(t, nil, "destinations", "add", "x", "--url", "https://hooks.example.com/hook")
	wantClassified(t, err, engine.ExitUsage)
	if s, _ := destination.Load(); len(s.Destinations) != 1 || s.Destinations[0].Kind != destination.KindS3 {
		t.Errorf("a refused change was saved: %+v", s.Destinations)
	}
	// And a webhook saved over a bucket, given its own secret, keeps no key id.
	t.Setenv("HOOK_SECRET", "whsec_PLANTEDfromTHEenvironment")
	if _, _, err := runCLI(t, nil, "destinations", "add", "x", "--url", "https://hooks.example.com/hook", "--secret-env", "HOOK_SECRET"); err != nil {
		t.Fatal(err)
	}
	if destination.HasAccessKey("x") {
		t.Error("a webhook kept a bucket's key id")
	}
	if s, _, _ := destination.Secrets("x"); s != "whsec_PLANTEDfromTHEenvironment" {
		t.Errorf("secret %q", s)
	}
	// A webhook saved as a bucket with only a key id is refused: the webhook's
	// secret is not a secret access key.
	_, _, err = runCLI(t, nil, "destinations", "add", "x", "--bucket", "my-reports", "--region", "eu-west-1", "--access-key-id-env", "S3_KEY_ID")
	wantClassified(t, err, engine.ExitUsage)
}

func TestAPathTheStepWillNotNameReachesTheTerminalQuoted(t *testing.T) {
	if got := plainText("reports/a.md"); got != "reports/a.md" {
		t.Errorf("plain: %q", got)
	}
	if got := plainText("reports/a\x1b]0;pwned\x07.md"); strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("controls: %q", got)
	}
}

// TestASavedAuthorizationValueIsKeptOnlyForTheSameReceiver: a change of path
// keeps it; a downgrade to http, the On my network tick, another port or
// another host drops it and says so on stderr.
func TestASavedAuthorizationValueIsKeptOnlyForTheSameReceiver(t *testing.T) {
	destinationsEnv(t)
	t.Setenv("HOOK_SECRET", "s")
	t.Setenv("HOOK_AUTH", "Bearer PLANTEDauthorizationVALUE")
	save := func(args ...string) string {
		t.Helper()
		_, errOut, err := runCLI(t, nil, append([]string{"destinations", "add", "n8n"}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return errOut
	}
	save("--url", "https://hooks.example.com/a", "--secret-env", "HOOK_SECRET", "--authorization-env", "HOOK_AUTH")
	if errOut := save("--url", "https://hooks.example.com/b?x=1"); strings.Contains(errOut, "dropped") {
		t.Errorf("a change of path dropped the value: %q", errOut)
	}
	if _, a, _ := destination.Secrets("n8n"); a == "" {
		t.Fatal("a change of path lost the value")
	}
	for name, args := range map[string][]string{
		"scheme downgrade": {"--url", "http://hooks.example.com/b", "--on-my-network"},
		"the tick":         {"--url", "https://hooks.example.com/b", "--on-my-network"},
		"another port":     {"--url", "https://hooks.example.com:8443/b", "--on-my-network"},
		"another host":     {"--url", "https://other.example.com/b"},
	} {
		// Back to the saved receiver, with the value, then the change.
		save("--url", "https://hooks.example.com/a", "--authorization-env", "HOOK_AUTH")
		if _, a, _ := destination.Secrets("n8n"); a == "" {
			t.Fatalf("%s: the value was not saved again", name)
		}
		if errOut := save(args...); !strings.Contains(errOut, "dropped") {
			t.Errorf("%s: stderr %q does not say the value was dropped", name, errOut)
		}
		if _, a, _ := destination.Secrets("n8n"); a != "" {
			t.Errorf("%s: the value was kept", name)
		}
	}
}
