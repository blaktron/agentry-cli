package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/send"
)

const (
	plantedSigningSecret = "whsec_PLANTEDsigningSECRET0123456789abcdef"
	plantedAuthorization = "Bearer PLANTEDauthorizationVALUE0123456789abcd"
)

func hook(name, url string, onMyNetwork bool) destination.Resolved {
	return destination.Resolved{
		Destination: destination.Destination{Name: name, Kind: destination.KindWebhook, URL: url, OnMyNetwork: onMyNetwork},
		Secret:      plantedSigningSecret, Authorization: plantedAuthorization,
	}
}

// deliveryStepPlays scripts the delivery step as the runner image's deliver role
// would behave: it records what it was given (the credential on stdin, the job,
// the staged files and the mounts) and writes the report that answer makes.
type deliveryStepSaw struct {
	stdin  string
	job    send.Job
	staged map[string]string
	spec   dockerx.RunSpec
}

func deliveryStepPlays(t *testing.T, f *dockerx.Fake, answer func(job send.Job) send.Report) *deliveryStepSaw {
	t.Helper()
	saw := &deliveryStepSaw{staged: map[string]string{}}
	if f.Foreground == nil {
		f.Foreground = map[string]*dockerx.FakeForeground{}
	}
	f.Foreground["-delivery"] = &dockerx.FakeForeground{Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
		saw.spec = r
		b, _ := io.ReadAll(stdin)
		saw.stdin = string(b)
		var jobDir, stagedDir, work string
		for _, m := range r.Mounts {
			switch m.Target {
			case "/job":
				jobDir = m.Source
			case "/staged":
				stagedDir = m.Source
			case "/work":
				work = m.Source
			}
		}
		job, err := send.ReadJob(jobDir)
		if err != nil {
			t.Errorf("the step cannot read its job: %v", err)
			return 1, nil
		}
		saw.job = job
		entries, _ := os.ReadDir(stagedDir)
		for _, e := range entries {
			body, _ := os.ReadFile(filepath.Join(stagedDir, e.Name()))
			saw.staged[e.Name()] = string(body)
		}
		rep := answer(job)
		rep.Schema = send.ReportSchema
		out, _ := json.Marshal(rep)
		if err := os.WriteFile(filepath.Join(work, send.ReportFile), out, 0o644); err != nil {
			t.Errorf("the step cannot write its report: %v", err)
		}
		if rep.Status != send.StatusDelivered {
			return 1, nil
		}
		return 0, nil
	}}
	return saw
}

// delivered answers every file of the job delivered.
func delivered(job send.Job) send.Report {
	rep := send.Report{Kind: send.KindWebhook, Host: "hooks.example.com:443", Status: send.StatusDelivered, Result: send.StatusDelivered}
	for _, f := range job.Files {
		st := send.FileResult{Path: f.Path, Status: send.StatusDelivered}
		if f.Omitted != "" {
			st.Status, st.Error = send.StatusRefused, f.Omitted
		}
		rep.Files = append(rep.Files, st)
	}
	return rep
}

// stepRun is the call that ran the delivery step (not its proxy).
func stepRun(f *dockerx.Fake) (dockerx.RunSpec, bool) {
	for _, c := range f.Calls {
		if c.Op == "RunForeground" && strings.HasSuffix(c.Run.Name, "-delivery") {
			return c.Run, true
		}
	}
	return dockerx.RunSpec{}, false
}

func proxyRun(f *dockerx.Fake) (dockerx.RunSpec, bool) {
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-delivery-proxy") && strings.HasPrefix(c.Op, "Run") {
			return c.Run, true
		}
	}
	return dockerx.RunSpec{}, false
}

// outputHolds says whether any file of the output folder holds the text.
func outputHolds(t *testing.T, dir, text string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, []byte(text)) {
			found = append(found, p)
		}
		return nil
	})
	return found
}

// TestARunWithNoDestinationMakesNoDeliveryStep: a release that was never given
// a destination delivers nowhere but the run folder: no network, no proxy and no
// container is made for a delivery.
func TestARunWithNoDestinationMakesNoDeliveryStep(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if strings.Contains(c.Run.Name, "delivery") || strings.Contains(c.Arg, "delivery") || strings.Contains(c.Arg, "-dlv") {
			t.Errorf("a delivery object was made for a run with no destination: %s %s %s", c.Op, c.Arg, c.Run.Name)
		}
	}
}

// TestADestinationGetsAStepOfItsOwn: the step runs the runner image on an
// internal network of its own, behind a proxy to the destination's host on 443
// at public addresses alone, with copies of the deliverables read-only and the
// credential on stdin; it mounts nothing of the run, and the credential is in no
// container's argument or environment, the receipts or the envelope.
func TestADestinationGetsAStepOfItsOwn(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/brief.md": "# Brief\n"}, `{"path":"reports/brief.md","title":"The brief","marked":true}`)
	saw := deliveryStepPlays(t, f, delivered)
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := stepRun(f)
	if !ok {
		t.Fatal("no delivery step ran")
	}
	if step.Image != p.Plan.Proxy.Image {
		t.Errorf("the step ran %s, not the runner image %s", step.Image, p.Plan.Proxy.Image)
	}
	if !step.ReadOnly || step.User == "" && os.Getuid() >= 0 || !step.Interactive {
		t.Errorf("the step's posture: %+v", step)
	}
	proxy, ok := proxyRun(f)
	if !ok {
		t.Fatal("no delivery proxy ran")
	}
	if got := strings.Join(proxy.Argv, " "); !strings.Contains(got, "--allow hooks.example.com:443") || !strings.Contains(got, "--public-only") || strings.Contains(got, "--network-ok") || strings.Contains(got, "plain-http") {
		t.Errorf("the proxy's argv: %s", got)
	}
	// An internal network of its own, the step on it and nothing else but
	// the proxy.
	var created bool
	for _, c := range f.Calls {
		if c.Op == "NetworkCreate" && strings.Contains(c.Arg, "-dlv(internal=true)") {
			created = true
		}
	}
	if !created || step.Network == "" || len(step.ExtraNetworks) != 0 || step.Network != proxy.Network {
		t.Errorf("networks: step %q %v, proxy %q", step.Network, step.ExtraNetworks, proxy.Network)
	}
	// The credential reached the step on stdin, as JSON, and nowhere else.
	var cred send.Credential
	if err := json.Unmarshal([]byte(saw.stdin), &cred); err != nil || cred.Secret != plantedSigningSecret || cred.Authorization != plantedAuthorization {
		t.Errorf("the step's stdin: %q (%v)", saw.stdin, err)
	}
	for _, c := range f.Calls {
		r := c.Run
		if strings.Contains(strings.Join(r.Argv, " "), "PLANTED") || strings.Contains(c.Arg, "PLANTED") {
			t.Errorf("%s %s carries a credential in its argv", c.Op, r.Name)
		}
		for k, v := range r.Env {
			if strings.Contains(v, "PLANTED") || strings.Contains(os.Getenv(k), "PLANTED") {
				t.Errorf("%s carries a credential in its environment (%s)", r.Name, k)
			}
		}
		for _, m := range r.Mounts {
			if strings.Contains(m.Source, "PLANTED") {
				t.Errorf("%s mounts a path with a credential", r.Name)
			}
		}
		if r.Name != step.Name && r.Name != proxy.Name && (r.Network == step.Network || slices.Contains(r.ExtraNetworks, step.Network)) {
			t.Errorf("%s is on the delivery network", r.Name)
		}
	}
	// The step mounts its copies and job read-only and its report folder, and
	// nothing of the run: not the output folder, the workspace or the run's
	// own folders.
	targets := map[string]bool{}
	for _, m := range step.Mounts {
		targets[m.Target] = m.ReadOnly
		if strings.HasPrefix(m.Source, p.OutDir) || strings.Contains(m.Source, filepath.Join(p.RunRoot, "runs")) {
			t.Errorf("the step mounts %s, a folder of the run", m.Source)
		}
	}
	if ro, ok := targets["/staged"]; !ok || !ro {
		t.Errorf("/staged: %v", targets)
	}
	if ro, ok := targets["/job"]; !ok || !ro {
		t.Errorf("/job: %v", targets)
	}
	if ro, ok := targets["/work"]; !ok || ro || len(targets) != 3 {
		t.Errorf("mounts: %v", targets)
	}
	// What the step was given: the staged copy of the file, the run's identity
	// and result, the destination's address.
	if len(saw.job.Files) != 1 || saw.staged[saw.job.Files[0].Staged] != "# Brief\n" || saw.job.RunID != env.RunID || saw.job.Digest != env.Digest || saw.job.URL != "https://hooks.example.com/in/abc" {
		t.Errorf("job: %+v staged %v", saw.job, saw.staged)
	}
	// The record, in the envelope and the ledger.
	if len(env.Deliveries) != 1 {
		t.Fatalf("deliveries = %+v", env.Deliveries)
	}
	d := env.Deliveries[0]
	if d.Destination != "n8n" || d.Kind != receipt.DeliveryKindWebhook || d.Host != "hooks.example.com:443" || d.Object != "reports/brief.md" || d.Status != receipt.DeliveryDelivered || d.OnMyNetwork {
		t.Errorf("delivery = %+v", d)
	}
	if hits := outputHolds(t, p.OutDir, "PLANTED"); len(hits) != 0 {
		t.Errorf("the credential is in %v", hits)
	}
	if hits := outputHolds(t, p.RunRoot, "PLANTED"); len(hits) != 0 {
		t.Errorf("the credential is in the runner home: %v", hits)
	}
	// The step's containers and network are gone, and so are its folders.
	left, _ := filepath.Glob(filepath.Join(p.RunRoot, "delivery-*"))
	if len(left) != 0 {
		t.Errorf("the step's folders remain: %v", left)
	}
	var removedNet, removedStep bool
	for _, c := range f.Calls {
		removedNet = removedNet || c.Op == "NetworkRemove" && strings.HasSuffix(c.Arg, "-dlv")
		removedStep = removedStep || c.Op == "RemoveForce" && c.Arg == step.Name
	}
	if !removedNet || !removedStep {
		t.Errorf("cleanup: network %v step %v", removedNet, removedStep)
	}
}

// TestOnMyNetworkWidensTheProxyAndIsRecorded: a destination saved with "On my
// network" runs behind a proxy that also admits private-network addresses (and
// a plain-http one for an http address), and the receipt says so.
func TestOnMyNetworkWidensTheProxyAndIsRecorded(t *testing.T) {
	for _, c := range []struct {
		url, allow string
		plain      bool
	}{
		{"https://n8n.lan:5678/webhook/x", "n8n.lan:5678", false},
		{"http://192.168.1.20:5678/x", "192.168.1.20:5678", true},
	} {
		p, f := fixture(t)
		happyFake(f)
		agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
		deliveryStepPlays(t, f, delivered)
		p.Destinations = []destination.Resolved{hook("lan", c.url, true)}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		proxy, _ := proxyRun(f)
		got := strings.Join(proxy.Argv, " ")
		if !strings.Contains(got, "--allow "+c.allow) || !strings.Contains(got, "--network-ok") || strings.Contains(got, "--public-only") || strings.Contains(got, "--plain-http "+c.allow) != c.plain {
			t.Errorf("%s: the proxy's argv: %s", c.url, got)
		}
		if len(env.Deliveries) != 1 || !env.Deliveries[0].OnMyNetwork || env.Deliveries[0].Host != c.allow {
			t.Errorf("%s: deliveries = %+v", c.url, env.Deliveries)
		}
	}
}

// TestADestinationThatBreaksTheAddressRuleMakesNoContainer: a private address
// without "On my network", an http address, a credential-bearing address and a
// secret that could split a header are each refused before anything is made.
func TestADestinationThatBreaksTheAddressRuleMakesNoContainer(t *testing.T) {
	for _, d := range []destination.Resolved{
		hook("private", "https://10.0.0.5/x", false),
		hook("metadata", "https://169.254.169.254/latest/meta-data/", false),
		hook("metadata-lan", "https://169.254.169.254/latest/meta-data/", true),
		hook("loopback", "https://127.0.0.1:8443/x", true),
		hook("plain", "http://hooks.example.com/x", false),
		hook("userinfo", "https://user:pw@hooks.example.com/x", false),
		hook("encoded", "https://2130706433/x", true),
		hook("port", "https://hooks.example.com:8443/x", false),
	} {
		p, f := fixture(t)
		happyFake(f)
		agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
		deliveryStepPlays(t, f, delivered)
		p.Destinations = []destination.Resolved{d}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stepRun(f); ok {
			t.Errorf("%s: a step ran", d.Name)
		}
		if _, ok := proxyRun(f); ok {
			t.Errorf("%s: a proxy ran", d.Name)
		}
		if env.Status != "ok" || len(env.Deliveries) != 1 || env.Deliveries[0].Status != receipt.DeliveryFailed || env.Deliveries[0].Error != receipt.DeliveryErrDestination {
			t.Errorf("%s: %s, deliveries %+v", d.Name, env.Status, env.Deliveries)
		}
	}
	bad := hook("split", "https://hooks.example.com/x", false)
	bad.Authorization = "Bearer x\r\nX-Evil: 1"
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	p.Destinations = []destination.Resolved{bad}
	env, _ := Run(context.Background(), f, p)
	if _, ok := stepRun(f); ok || len(env.Deliveries) != 1 || env.Deliveries[0].Error != receipt.DeliveryErrCredential {
		t.Errorf("a header-splitting value: %+v", env.Deliveries)
	}
}

// TestAReceiverThatAnswers500LeavesTheRunSucceeded: the delivery is failed, with
// one word and not the receiver's, and the run's status and exit are the run's.
func TestAReceiverThatAnswers500LeavesTheRunSucceeded(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a", "reports/b.md": "b"}, `{"path":"reports/a.md","marked":true}`, `{"path":"reports/b.md","marked":true}`)
	deliveryStepPlays(t, f, func(job send.Job) send.Report {
		rep := send.Report{Kind: send.KindWebhook, Host: "hooks.example.com:443", Status: send.StatusFailed, Error: send.ReasonReceiverSaid}
		for _, fl := range job.Files {
			rep.Files = append(rep.Files, send.FileResult{Path: fl.Path, Status: send.StatusFailed, Error: send.ReasonReceiverSaid})
		}
		return rep
	})
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("a failed delivery failed the run: %v", err)
	}
	if env.Status != "ok" || env.ExitCode != ExitOK {
		t.Errorf("status %q exit %d", env.Status, env.ExitCode)
	}
	if len(env.Deliveries) != 2 {
		t.Fatalf("deliveries = %+v", env.Deliveries)
	}
	for _, d := range env.Deliveries {
		if d.Status != receipt.DeliveryFailed || d.Error != receipt.DeliveryErrStatus {
			t.Errorf("delivery = %+v", d)
		}
	}
	ledger, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "deliveries.ndjson"))
	if strings.Count(string(ledger), "\n") != 2 || !strings.Contains(string(ledger), `"error":"status"`) {
		t.Errorf("deliveries.ndjson = %q", ledger)
	}
	onDisk, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if !strings.Contains(string(onDisk), `"status": "ok"`) && !strings.Contains(string(onDisk), `"status":"ok"`) {
		t.Errorf("result.json:\n%s", onDisk)
	}
}

// TestTheStepsReportIsReadAsTheWordOfAProgramThatHeldTheCredential: a report with
// a status or reason outside the closed lists, a path the job never held, or none
// at all becomes the word "step", never text a program chose.
func TestTheStepsReportIsReadAsTheWordOfAProgramThatHeldTheCredential(t *testing.T) {
	for name, answer := range map[string]func(send.Job) send.Report{
		"free text": func(job send.Job) send.Report {
			return send.Report{Kind: send.KindWebhook, Status: send.StatusFailed, Error: "receiver said: PLANTED text", Files: []send.FileResult{{Path: job.Files[0].Path, Status: send.StatusFailed, Error: "receiver said: PLANTED text"}}}
		},
		"unknown status": func(job send.Job) send.Report {
			return send.Report{Kind: send.KindWebhook, Status: "great", Files: []send.FileResult{{Path: job.Files[0].Path, Status: "great"}}}
		},
		"no entry for the file": func(job send.Job) send.Report {
			return send.Report{Kind: send.KindWebhook, Status: send.StatusDelivered, Files: []send.FileResult{{Path: "reports/other.md", Status: send.StatusDelivered}}}
		},
	} {
		p, f := fixture(t)
		happyFake(f)
		agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
		deliveryStepPlays(t, f, answer)
		p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
		env, err := Run(context.Background(), f, p)
		if err != nil || len(env.Deliveries) != 1 {
			t.Fatalf("%s: %v %+v", name, err, env.Deliveries)
		}
		if d := env.Deliveries[0]; d.Status != receipt.DeliveryFailed || d.Error != receipt.DeliveryErrStep {
			t.Errorf("%s: delivery = %+v", name, d)
		}
	}
	// No report at all.
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	f.Foreground["-delivery"] = &dockerx.FakeForeground{ExitCode: 1, Stderr: "boom " + plantedSigningSecret}
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	var stderr bytes.Buffer
	p.Stderr = &stderr
	env, err := Run(context.Background(), f, p)
	if err != nil || len(env.Deliveries) != 1 || env.Deliveries[0].Error != receipt.DeliveryErrStep {
		t.Fatalf("%v %+v", err, env.Deliveries)
	}
	if strings.Contains(stderr.String(), "PLANTED") {
		t.Errorf("progress output carries the credential: %s", stderr.String())
	}
}

// TestAStructuredResultIsDeliveredWithNoFile: a run that left no file and has
// only its result sends the result, and its record is the one entry "result".
func TestAStructuredResultIsDeliveredWithNoFile(t *testing.T) {
	p, f := fixture(t)
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, compile.OutputSchemaFile), []byte(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(t.TempDir(), "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	p.Plan, p.PlanDir = plan, planDir
	happyFake(f)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Stdout: `{"type":"agent_choice","message":{"content":"{\"verdict\":\"pass\"}"}}` + "\n"}
	saw := deliveryStepPlays(t, f, delivered)
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(saw.job.Files) != 0 || len(env.Deliveries) != 1 || env.Deliveries[0].Object != "result" || env.Deliveries[0].Status != receipt.DeliveryDelivered {
		t.Errorf("job files %d, deliveries %+v", len(saw.job.Files), env.Deliveries)
	}
}

// TestAFailedRunDeliversNothing: a destination is sent what a run produced, not
// the wreck of one that did not succeed.
func TestAFailedRunDeliversNothing(t *testing.T) {
	p, f := fixture(t)
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: `{"type":"agent_choice","message":{"content":"partial"}}` + "\n", ExitCode: 3},
	}
	deliveryStepPlays(t, f, delivered)
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("the run did not fail")
	}
	if _, ok := stepRun(f); ok {
		t.Error("a failed run delivered")
	}
}

// TestADeliveryLeavesTheRunFolderAsItWas: delivering a finished run's folder
// again to a destination adds lines to receipts/deliveries.ndjson and the
// destination's proxy log, and changes no other byte of the folder.
func TestADeliveryLeavesTheRunFolderAsItWas(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a", "reports/sub/b.md": "bb"}, `{"path":"reports/a.md","marked":true}`, `{"path":"reports/sub/b.md","marked":true}`)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, p.OutDir)
	f2 := &dockerx.Fake{}
	saw := deliveryStepPlays(t, f2, delivered)
	ds, err := Redeliver(context.Background(), f2, RedeliverParams{
		RunDir: p.OutDir, Destinations: []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)},
		Policy: p.Policy, RunnerImage: p.Plan.Proxy.Image, RunRoot: p.RunRoot, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || len(saw.job.Files) != 2 {
		t.Fatalf("deliveries %+v, job %+v", ds, saw.job)
	}
	after := snapshot(t, p.OutDir)
	for name, b := range before {
		if name == "receipts/deliveries.ndjson" {
			continue
		}
		if after[name] != b {
			t.Errorf("%s changed", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok && name != "receipts/deliveries.ndjson" {
			t.Errorf("%s was added", name)
		}
	}
	if got := strings.Count(after["receipts/deliveries.ndjson"], "\n"); got != 2 {
		t.Errorf("the ledger has %d lines: %q", got, after["receipts/deliveries.ndjson"])
	}
	// A folder that is not a run's, and one whose run failed, deliver nothing.
	if _, err := Redeliver(context.Background(), f2, RedeliverParams{RunDir: t.TempDir(), Destinations: []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}, RunRoot: p.RunRoot}); err == nil {
		t.Error("a folder that is no run's was delivered")
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

// TestTheReceiptsClosedListsAreTheStepsWords: the words the step reports are the
// receipt's, so a reason the step gives is one the receipt keeps.
func TestTheReceiptsClosedListsAreTheStepsWords(t *testing.T) {
	for _, w := range []string{send.ReasonTooLarge, send.ReasonChanged, send.ReasonMissing, send.ReasonLink, send.ReasonUnreadable, send.ReasonOutside,
		send.ReasonDestination, send.ReasonCredential, send.ReasonAddress, send.ReasonUnreachable, send.ReasonTLS, send.ReasonTimeout, send.ReasonRedirect,
		send.ReasonReceiverSaid, send.ReasonStep} {
		if !slices.Contains(receipt.DeliveryErrors, w) {
			t.Errorf("the step's word %q is not in the receipt's closed list", w)
		}
	}
	if send.StatusDelivered != receipt.DeliveryDelivered || send.StatusRefused != receipt.DeliveryRefused || send.StatusFailed != receipt.DeliveryFailed {
		t.Error("the step's statuses are not the receipt's")
	}
}

// TestAHostedRunDeliversToNoDestination: whatever a caller puts in the
// parameters, a hosted run's host delivers nowhere.
func TestAHostedRunDeliversToNoDestination(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	deliveryStepPlays(t, f, delivered)
	p.InputReview = &fakeReview{}
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("a hosted run took a destination")
	}
	if _, ok := stepRun(f); ok {
		t.Error("a delivery step ran")
	}
}

// TestAStaleDeliveryFolderIsSweptAndAFreshOneIsNot: what a process that died in
// a step left behind goes with the next delivery; a step in flight keeps its.
func TestAStaleDeliveryFolderIsSweptAndAFreshOneIsNot(t *testing.T) {
	root := t.TempDir()
	stale, fresh, other, notes := filepath.Join(root, "delivery-123456"), filepath.Join(root, "delivery-789"), filepath.Join(root, "runs"), filepath.Join(root, "delivery-notes")
	for _, d := range []string{stale, fresh, other, notes} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{other, notes} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepStaleDeliveryFolders(root)
	for d, want := range map[string]bool{stale: false, fresh: true, other: true, notes: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

// TestAResultOnlyDeliveryIsRecordedAsTheStepReportsIt: the result is delivered
// only when the POST was and the body held it; a POST that failed is a failed
// result whatever the report's result word was, and a result the body left out
// is refused with the reason.
func TestAResultOnlyDeliveryIsRecordedAsTheStepReportsIt(t *testing.T) {
	r := deliveryRun{Result: json.RawMessage(`{"verdict":"pass"}`)}
	dest := hook("n8n", "https://hooks.example.com/x", false)
	for name, c := range map[string]struct {
		rep    send.Report
		status string
		reason string
	}{
		"delivered":                              {send.Report{Status: send.StatusDelivered, Result: send.StatusDelivered}, receipt.DeliveryDelivered, ""},
		"unreachable even if the step said sent": {send.Report{Status: send.StatusFailed, Error: send.ReasonUnreachable, Result: send.StatusDelivered}, receipt.DeliveryFailed, receipt.DeliveryErrUnreachable},
		"address":                                {send.Report{Status: send.StatusFailed, Error: send.ReasonAddress}, receipt.DeliveryFailed, receipt.DeliveryErrAddress},
		"too large":                              {send.Report{Status: send.StatusDelivered, Result: send.ReasonTooLarge}, receipt.DeliveryRefused, receipt.DeliveryErrTooLarge},
		"unreadable":                             {send.Report{Status: send.StatusDelivered, Result: send.ReasonUnreadable}, receipt.DeliveryRefused, receipt.DeliveryErrUnreadable},
		"no word":                                {send.Report{Status: send.StatusDelivered}, receipt.DeliveryFailed, receipt.DeliveryErrStep},
		"free text":                              {send.Report{Status: send.StatusDelivered, Result: "anything the step says"}, receipt.DeliveryFailed, receipt.DeliveryErrStep},
	} {
		c.rep.Schema = send.ReportSchema
		got := recordsFromReport(r, dest, "hooks.example.com:443", c.rep)
		if len(got) != 1 || got[0].Object != "result" || got[0].Status != c.status || got[0].Error != c.reason {
			t.Errorf("%s: %+v, want %s/%s", name, got, c.status, c.reason)
		}
	}
}

// TestSeveralDestinationsShareOneTimeLimit: the destinations of one delivery
// are held to a time together, and one reached after it is spent is a timeout.
func TestSeveralDestinationsShareOneTimeLimit(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	deliveryStepPlays(t, f, delivered)
	r := deliveryRun{RunDir: p.OutDir, RunID: "x", Files: []receipt.Deliverable{{Path: "reports/a.md", SHA256: sha([]byte("a")), Bytes: 1}}, Image: "agentry-runner:test", Root: p.RunRoot, Engine: dockerx.LocalDockerEngine()}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	got := sendToDestinations(ctx, f, r, []destination.Resolved{hook("a", "https://hooks.example.com/a", false), hook("b", "https://hooks.example.com/b", false)})
	if len(got) != 2 || got[0].Error != receipt.DeliveryErrTimeout || got[1].Error != receipt.DeliveryErrTimeout {
		t.Errorf("records: %+v", got)
	}
	if _, ok := stepRun(f); ok {
		t.Error("a step ran after the time was spent")
	}
}

// TestARedeliveryOfAnOversizedListingIsRefused: an envelope that lists more
// files than any run's does is not delivered.
func TestARedeliveryOfAnOversizedListingIsRefused(t *testing.T) {
	dir := t.TempDir()
	env := receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "r", Ref: "a/b@1", OutDir: dir, Deliverables: &receipt.Deliverables{}}
	for i := 0; i <= receipt.MaxDeliverables; i++ {
		env.Deliverables.Files = append(env.Deliverables.Files, receipt.Deliverable{Path: fmt.Sprintf("reports/%d.md", i)})
	}
	b, _ := env.Marshal()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &dockerx.Fake{}
	if _, err := Redeliver(context.Background(), f, RedeliverParams{RunDir: dir, Destinations: []destination.Resolved{hook("n8n", "https://hooks.example.com/x", false)}, RunRoot: t.TempDir()}); err == nil {
		t.Error("an oversized listing was delivered")
	}
}

const (
	plantedAccessKeyID = "AKIAPLANTEDKEYID0123"
	plantedSecretKey   = "PLANTEDsecretACCESSkey0123456789abcdef"
)

func bucket(name string, d destination.Destination) destination.Resolved {
	d.Name, d.Kind = name, destination.KindS3
	return destination.Resolved{Destination: d, Secret: plantedSecretKey, AccessKeyID: plantedAccessKeyID}
}

// bucketStepPlays scripts the step for a bucket: every file delivered.
func bucketDelivered(job send.Job) send.Report {
	rep := send.Report{Kind: send.KindS3, Host: "x", Status: send.StatusDelivered}
	for _, f := range job.Files {
		st := send.FileResult{Path: f.Path, Status: send.StatusDelivered}
		if f.Omitted != "" {
			st.Status, st.Error = send.StatusRefused, f.Omitted
		}
		rep.Files = append(rep.Files, st)
	}
	return rep
}

// TestABucketGetsAStepOfItsOwnAndItsKeyPairReachesItAlone: the step runs behind
// a proxy to the bucket's host alone at public addresses, with the key pair on
// stdin and in no argument, environment, mount, receipt or the run folder; each
// file is staged as it is, and the record names the object written.
func TestABucketGetsAStepOfItsOwnAndItsKeyPairReachesItAlone(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/brief.md": "# Brief\n", "reports/data/t.csv": "a,b\n"},
		`{"path":"reports/brief.md","marked":true}`, `{"path":"reports/data/t.csv","marked":true}`)
	saw := deliveryStepPlays(t, f, bucketDelivered)
	p.Destinations = []destination.Resolved{bucket("archive", destination.Destination{Bucket: "my-reports", Prefix: "agentry/out", Region: "eu-west-1"})}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	proxy, ok := proxyRun(f)
	if !ok {
		t.Fatal("no delivery proxy ran")
	}
	if got := strings.Join(proxy.Argv, " "); !strings.Contains(got, "--allow my-reports.s3.eu-west-1.amazonaws.com:443") || !strings.Contains(got, "--public-only") || strings.Contains(got, "--network-ok") {
		t.Errorf("the proxy's argv: %s", got)
	}
	var cred send.Credential
	if err := json.Unmarshal([]byte(saw.stdin), &cred); err != nil || cred.AccessKeyID != plantedAccessKeyID || cred.SecretAccessKey != plantedSecretKey || cred.Secret != "" || cred.Authorization != "" {
		t.Errorf("the step's stdin: %q (%v)", saw.stdin, err)
	}
	for _, c := range f.Calls {
		r := c.Run
		if strings.Contains(strings.Join(r.Argv, " "), "PLANTED") || strings.Contains(c.Arg, "PLANTED") {
			t.Errorf("%s %s carries a credential in its argv", c.Op, r.Name)
		}
		for k, v := range r.Env {
			if strings.Contains(v, "PLANTED") || strings.Contains(os.Getenv(k), "PLANTED") {
				t.Errorf("%s carries a credential in its environment (%s)", r.Name, k)
			}
		}
		for _, m := range r.Mounts {
			if strings.Contains(m.Source, "PLANTED") {
				t.Errorf("%s mounts a path with a credential", r.Name)
			}
		}
	}
	// What the step was given: no URL, no result, the bucket's fields, the
	// run's own id, and each file staged as it is.
	j := saw.job
	if j.Kind != send.KindS3 || j.URL != "" || j.Bucket != "my-reports" || j.Prefix != "agentry/out" || j.Region != "eu-west-1" || j.Endpoint != "" || j.RunID != env.RunID || j.MaxObject != send.DefaultMaxObject || len(j.Result) != 0 || len(j.Files) != 2 {
		t.Fatalf("job: %+v", j)
	}
	if saw.staged[j.Files[0].Staged] != "# Brief\n" || saw.staged[j.Files[1].Staged] != "a,b\n" {
		t.Errorf("staged: %v", saw.staged)
	}
	if len(env.Deliveries) != 2 {
		t.Fatalf("deliveries = %+v", env.Deliveries)
	}
	for i, want := range []string{"my-reports/agentry/out/" + env.RunID + "/brief.md", "my-reports/agentry/out/" + env.RunID + "/data/t.csv"} {
		d := env.Deliveries[i]
		if d.Destination != "archive" || d.Kind != receipt.DeliveryKindS3 || d.Host != "my-reports.s3.eu-west-1.amazonaws.com:443" || d.Object != want || d.Status != receipt.DeliveryDelivered || d.OnMyNetwork {
			t.Errorf("delivery %d = %+v, want object %s", i, d, want)
		}
	}
	if hits := outputHolds(t, p.OutDir, "PLANTED"); len(hits) != 0 {
		t.Errorf("the credential is in %v", hits)
	}
	if hits := outputHolds(t, p.RunRoot, "PLANTED"); len(hits) != 0 {
		t.Errorf("the credential is in the runner home: %v", hits)
	}
	if left, _ := filepath.Glob(filepath.Join(p.RunRoot, "delivery-*")); len(left) != 0 {
		t.Errorf("the step's folders remain: %v", left)
	}
}

// TestABucketOnMyNetworkWidensTheProxyAndIsRecorded: an endpoint on the person's
// network is reached by a proxy that admits it (plain http too), path-style, and
// the record says so.
func TestABucketOnMyNetworkWidensTheProxyAndIsRecorded(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
	saw := deliveryStepPlays(t, f, bucketDelivered)
	p.Destinations = []destination.Resolved{bucket("minio", destination.Destination{Bucket: "reports", Region: "us-east-1", Endpoint: "http://192.168.1.20:9000", OnMyNetwork: true})}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := proxyRun(f)
	got := strings.Join(proxy.Argv, " ")
	if !strings.Contains(got, "--allow 192.168.1.20:9000") || !strings.Contains(got, "--network-ok") || !strings.Contains(got, "--plain-http 192.168.1.20:9000") || strings.Contains(got, "--public-only") {
		t.Errorf("the proxy's argv: %s", got)
	}
	if saw.job.Endpoint != "http://192.168.1.20:9000" || !saw.job.OnMyNetwork {
		t.Errorf("job: %+v", saw.job)
	}
	if len(env.Deliveries) != 1 || !env.Deliveries[0].OnMyNetwork || env.Deliveries[0].Object != "reports/"+env.RunID+"/a.md" || env.Deliveries[0].Host != "192.168.1.20:9000" {
		t.Errorf("deliveries = %+v", env.Deliveries)
	}
}

// TestABucketThatBreaksTheRulesMakesNoContainer: an endpoint at a private,
// loopback or metadata address without the tick, plain http, a bad bucket or
// prefix, or a missing key makes no network, proxy or step, and is one failed
// record with a closed word.
func TestABucketThatBreaksTheRulesMakesNoContainer(t *testing.T) {
	good := destination.Destination{Bucket: "my-reports", Region: "us-east-1", Endpoint: "https://minio.example.com"}
	mut := func(f func(*destination.Destination)) destination.Resolved {
		d := good
		f(&d)
		return bucket("b", d)
	}
	noKey := bucket("b", good)
	noKey.AccessKeyID = ""
	splitSecret := bucket("b", good)
	splitSecret.Secret = "a\nb"
	for name, want := range map[string]struct {
		d      destination.Resolved
		reason string
	}{
		"private":   {mut(func(d *destination.Destination) { d.Endpoint = "https://10.0.0.5" }), receipt.DeliveryErrDestination},
		"metadata":  {mut(func(d *destination.Destination) { d.Endpoint = "http://169.254.169.254"; d.OnMyNetwork = true }), receipt.DeliveryErrDestination},
		"loopback":  {mut(func(d *destination.Destination) { d.Endpoint = "https://127.0.0.1:9000"; d.OnMyNetwork = true }), receipt.DeliveryErrDestination},
		"plain":     {mut(func(d *destination.Destination) { d.Endpoint = "http://minio.example.com" }), receipt.DeliveryErrDestination},
		"path":      {mut(func(d *destination.Destination) { d.Endpoint = "https://minio.example.com/other" }), receipt.DeliveryErrDestination},
		"bucket":    {mut(func(d *destination.Destination) { d.Bucket = "../x" }), receipt.DeliveryErrDestination},
		"prefix":    {mut(func(d *destination.Destination) { d.Prefix = "../x" }), receipt.DeliveryErrDestination},
		"no key id": {noKey, receipt.DeliveryErrCredential},
		"split":     {splitSecret, receipt.DeliveryErrCredential},
	} {
		p, f := fixture(t)
		happyFake(f)
		agentWrites(p, f, map[string]string{"reports/a.md": "a"}, `{"path":"reports/a.md","marked":true}`)
		deliveryStepPlays(t, f, bucketDelivered)
		p.Destinations = []destination.Resolved{want.d}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stepRun(f); ok {
			t.Errorf("%s: a step ran", name)
		}
		if _, ok := proxyRun(f); ok {
			t.Errorf("%s: a proxy ran", name)
		}
		if env.Status != "ok" || len(env.Deliveries) != 1 || env.Deliveries[0].Status != receipt.DeliveryFailed || env.Deliveries[0].Error != want.reason || env.Deliveries[0].Kind != receipt.DeliveryKindS3 {
			t.Errorf("%s: %s, deliveries %+v", name, env.Status, env.Deliveries)
		}
	}
}

// TestABucketIsSentFilesAloneAndAResultOnlyRunLeavesItNothing: a run with only a
// structured result makes no step for a bucket and no record.
func TestABucketIsSentFilesAloneAndAResultOnlyRunLeavesItNothing(t *testing.T) {
	p, f := fixture(t)
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, compile.OutputSchemaFile), []byte(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(t.TempDir(), "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	p.Plan, p.PlanDir = plan, planDir
	happyFake(f)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Stdout: `{"type":"agent_choice","message":{"content":"{\"verdict\":\"pass\"}"}}` + "\n"}
	deliveryStepPlays(t, f, bucketDelivered)
	p.Destinations = []destination.Resolved{bucket("archive", destination.Destination{Bucket: "my-reports", Region: "us-east-1"})}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stepRun(f); ok || len(env.Deliveries) != 0 {
		t.Errorf("a result-only run: step ran %v, deliveries %+v", ok, env.Deliveries)
	}
}

// TestABucketsReportIsReadAsTheWordOfAProgramThatHeldTheKeys: a partial report
// becomes a record per file, each from the closed lists, with free text,
// an unknown reason and a missing entry all "step".
func TestABucketsReportIsReadAsTheWordOfAProgramThatHeldTheKeys(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/a.md": "a", "reports/b.md": "b", "reports/c.md": "c", "reports/d.md": "d"},
		`{"path":"reports/a.md","marked":true}`, `{"path":"reports/b.md","marked":true}`, `{"path":"reports/c.md","marked":true}`, `{"path":"reports/d.md","marked":true}`)
	deliveryStepPlays(t, f, func(job send.Job) send.Report {
		return send.Report{Kind: send.KindS3, Status: send.StatusFailed, Error: send.ReasonReceiverSaid, Files: []send.FileResult{
			{Path: "reports/a.md", Status: send.StatusDelivered},
			{Path: "reports/b.md", Status: send.StatusFailed, Error: "service said: PLANTED text"},
			{Path: "reports/c.md", Status: send.StatusRefused, Error: send.ReasonTooLarge},
		}}
	})
	p.Destinations = []destination.Resolved{bucket("archive", destination.Destination{Bucket: "my-reports", Region: "us-east-1"})}
	env, err := Run(context.Background(), f, p)
	if err != nil || len(env.Deliveries) != 4 {
		t.Fatalf("%v %+v", err, env.Deliveries)
	}
	type st struct{ status, reason string }
	want := []st{{receipt.DeliveryDelivered, ""}, {receipt.DeliveryFailed, receipt.DeliveryErrStep}, {receipt.DeliveryRefused, receipt.DeliveryErrTooLarge}, {receipt.DeliveryFailed, receipt.DeliveryErrStep}}
	for i, d := range env.Deliveries {
		if d.Status != want[i].status || d.Error != want[i].reason {
			t.Errorf("delivery %d = %+v, want %+v", i, d, want[i])
		}
	}
	if env.Status != "ok" {
		t.Errorf("the run's status is %s", env.Status)
	}
}

// TestAHostileFileNameNamesAnObjectInsideTheRunsFolder: the record's object, like
// the key the step writes, is under <bucket>/<prefix>/<run id>/ whatever the path.
func TestAHostileFileNameNamesAnObjectInsideTheRunsFolder(t *testing.T) {
	r := deliveryRun{RunID: "cafe0000", Files: []receipt.Deliverable{{Path: "reports/a:b*.md"}, {Path: "reports/x/../../y.md"}, {Path: "other/z.md"}}}
	got := objectsFor(r, bucket("a", destination.Destination{Bucket: "bkt-name", Prefix: "p/q", Region: "auto"}))
	if got[0] != "bkt-name/p/q/cafe0000/a_b_.md" || got[1] != "reports/x/../../y.md" || got[2] != "other/z.md" {
		t.Errorf("objects: %v", got)
	}
}
