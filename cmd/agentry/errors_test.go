// A registry that cannot be reached and a container engine that does not
// answer each reach the person as one line that says what to do, with the
// client's own error only under --verbose; doctor says the same per row,
// with a remedy, and reports its result once.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestRegistryCommandsOfflineSayCannotReach: search, info, install and
// run against a registry nothing listens on each fail with the one line,
// and the dial error is kept beneath it for --verbose.
func TestRegistryCommandsOfflineSayCannotReach(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	const dead = "http://127.0.0.1:1"
	for name, args := range map[string][]string{
		"search":  {"--registry", dead, "search", "audit"},
		"info":    {"--registry", dead, "info", "acme/demo"},
		"install": {"--registry", dead, "install", "acme/demo@1.0.0", "--yes"},
		"run":     {"--registry", dead, "run", "acme/demo", "--yes", "--policy", policyPath},
	} {
		_, _, err := runCLI(t, strings.NewReader(""), args...)
		if err == nil {
			t.Fatalf("%s against a dead registry exited 0", name)
		}
		line, detail := describeError(err)
		if want := "cannot reach " + dead + " (connection refused): check the network connection, or pass --registry to use another registry"; line != want {
			t.Errorf("%s: line %q, want %q", name, line, want)
		}
		if !strings.Contains(detail, "dial tcp") {
			t.Errorf("%s: the dial error is not kept as the detail: %q", name, detail)
		}
		var out bytes.Buffer
		printCommandError(&out, err)
		if strings.Contains(out.String(), "dial tcp") || !strings.HasPrefix(out.String(), "agentry: cannot reach ") {
			t.Errorf("%s: without --verbose: %q", name, out.String())
		}
		out.Reset()
		flags.verbose = true
		printCommandError(&out, err)
		flags.verbose = false
		if !strings.Contains(out.String(), "\n  detail: ") || !strings.Contains(out.String(), "dial tcp") {
			t.Errorf("%s: with --verbose: %q", name, out.String())
		}
	}
}

// TestTransportCauseInWords: the causes a person reads.
func TestTransportCauseInWords(t *testing.T) {
	if got := transportCause(errors.New("dial tcp 127.0.0.1:1: connect: connection refused")); got != "connection refused" {
		t.Errorf("refused = %q", got)
	}
	if got := transportCause(errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")); !strings.HasPrefix(got, "its TLS certificate was not accepted") {
		t.Errorf("tls = %q", got)
	}
}

// engineDown is a Docker Engine whose socket nothing answers on, as the
// client reports it.
func engineDown() *dockerx.Unreachable {
	return &dockerx.Unreachable{CLI: "docker", Endpoint: "unix:///var/run/docker.sock", Engine: "Docker Engine",
		Remedy: "start it with `sudo systemctl start docker`", Err: errors.New("docker version: exit status 1: Cannot connect to the Docker daemon")}
}

// TestDoctorWithTheEngineDownNamesTheRemedy: the docker row is the
// sentence and its fix, the raw client error appears only under --verbose,
// the result is reported once, and doctor --json carries the remedy.
func TestDoctorWithTheEngineDownNamesTheRemedy(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	fake.AvailableErr = engineDown()
	fr := newFakeRegistry(t)
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	var reported *reportedError
	if !errors.As(err, &reported) {
		t.Fatalf("the text report is said once, so the error is marked reported: %v", err)
	}
	wantClassified(t, err, engine.ExitCLI)
	for _, want := range []string{
		"  FAIL  docker             Docker Engine is not running (/var/run/docker.sock)\n        fix                start it with `sudo systemctl start docker`\n",
		"1 check(s) failed\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Cannot connect to the Docker daemon") {
		t.Errorf("the client's error reached the report without --verbose:\n%s", out)
	}
	out, _, _ = runCLI(t, nil, "--verbose", "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "Cannot connect to the Docker daemon") {
		t.Errorf("--verbose left the client's error out:\n%s", out)
	}
	stdout, _, err := runCLI(t, nil, "--json", "doctor", "--registry", fr.url(), "--policy", policyPath)
	if errors.As(err, &reported) {
		t.Fatalf("under --json the failure line is main's to print: %v", err)
	}
	var rep doctorReport
	if jerr := json.Unmarshal([]byte(stdout), &rep); jerr != nil {
		t.Fatalf("doctor --json: %v\n%s", jerr, stdout)
	}
	rows := map[string]doctorCheck{}
	for _, c := range rep.Checks {
		rows[c.Name] = c
	}
	if d := rows["docker"]; d.Status != "fail" || d.Remedy != "start it with `sudo systemctl start docker`" {
		t.Errorf("docker row = %+v", d)
	}
	for _, name := range []string{"signin", "version", "path"} {
		if _, ok := rows[name]; !ok {
			t.Errorf("no %s row in:\n%s", name, stdout)
		}
	}
	if s := rows["signin"]; s.Status != "warn" || s.Remedy != "run `agentry login`" {
		t.Errorf("signin row = %+v", s)
	}
}

// TestDoctorRegistryOfflineAndSignedIn: an unreachable registry is one
// failed row with the network remedy; a stored sign-in and a registry
// minimum this build satisfies are ok rows; a minimum ahead of a stamped
// build is a warning with the upgrade remedy.
func TestDoctorRegistryOfflineAndSignedIn(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	out, _, _ := runCLI(t, nil, "doctor", "--registry", "http://127.0.0.1:1", "--policy", policyPath)
	if !strings.Contains(out, "  FAIL  registry           cannot reach http://127.0.0.1:1 (connection refused)\n        fix                check the network connection, or pass --registry to use another registry\n") {
		t.Errorf("offline registry:\n%s", out)
	}
	if strings.Contains(out, "dial tcp") {
		t.Errorf("the dial error reached the report:\n%s", out)
	}
	fr := newFakeRegistry(t)
	fr.metaMinCLI = "0.5.0"
	if _, _, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login", "--paste"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, nil, "--json", "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout)
	}
	var rep doctorReport
	if jerr := json.Unmarshal([]byte(stdout), &rep); jerr != nil {
		t.Fatal(jerr)
	}
	rows := map[string]doctorCheck{}
	for _, c := range rep.Checks {
		rows[c.Name] = c
	}
	if s := rows["signin"]; s.Status != "ok" || !strings.Contains(s.Detail, "signed in to "+fr.url()) {
		t.Errorf("signin row = %+v", s)
	}
	if v := rows["version"]; v.Status != "ok" || !strings.Contains(v.Detail, "development build") {
		t.Errorf("version row = %+v", v)
	}
	if r := rows["registry"]; r.Status != "ok" || r.Detail != fr.url()+" reachable" {
		t.Errorf("registry row = %+v", r)
	}
}

// TestRunChecksTheEngineBeforeItInstalls: with no engine answering, a run
// of a local folder stops with the gateway class before anything is laid
// down under the runner home.
func TestRunChecksTheEngineBeforeItInstalls(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fake := &dockerx.Fake{AvailableErr: engineDown()}
	newDockerClient = func() dockerx.Client { return fake }
	_, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes", "--policy", policyPath)
	wantClassified(t, err, engine.ExitGateway)
	if !strings.Contains(stderr, "cause: Docker Engine is not running (/var/run/docker.sock): start it with `sudo systemctl start docker`") {
		t.Errorf("the failure report:\n%s", stderr)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("the runner home was written by a run that could not start: %v", entries)
	}
	// A dry run needs no engine and still installs.
	if _, _, err := runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--yes", "--dry-run", "--policy", policyPath); err != nil {
		t.Errorf("dry run with the engine down: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(home, "agents")); serr != nil {
		t.Errorf("a dry run laid nothing down: %v", serr)
	}
}

// TestSetupWithTheEngineDownSaysTheRemedy: setup's image half fails with
// the sentence, under the gateway class.
func TestSetupWithTheEngineDownSaysTheRemedy(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	newDockerClient = func() dockerx.Client { return &dockerx.Fake{AvailableErr: engineDown()} }
	_, _, err := runCLI(t, strings.NewReader(""), "setup", "--yes", "--provider", "openai", "--policy", policyPath)
	wantClassified(t, err, engine.ExitGateway)
	if line, detail := describeError(err); line != "Docker Engine is not running (/var/run/docker.sock): start it with `sudo systemctl start docker`" || !strings.Contains(detail, "Cannot connect") {
		t.Errorf("line %q detail %q", line, detail)
	}
	if _, _, lerr := policy.Load(policyPath); lerr != nil {
		t.Errorf("the policy half was not written: %v", lerr)
	}
}
