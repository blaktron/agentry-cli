package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/unsafeexec"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestLiveUnsafeExecImage builds a real Unsafe Mode exec image on the pinned
// base and drives its tools server. It pulls the base, builds the runner image
// from this binary's embedded context, and installs from Kali's mirror and
// PyPI, so it runs only when asked: AGENTRY_LIVE_UNSAFE=1 on a host with
// docker.
//
// It forces one install to fail (a package Kali does not carry, held to
// the grammar) and checks that the build goes on, the record names it,
// and the run goes without it. Inside the image, run_command refuses an
// undeclared bash, runs a declared apt command and a pip
// extra, and answers "not present in image" for a declared command the
// image could not place.
func TestLiveUnsafeExecImage(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_UNSAFE") != "1" {
		t.Skip("builds a real Unsafe Mode exec image: set AGENTRY_LIVE_UNSAFE=1")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	pol := policy.Default()
	runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if ok, _ := d.ImageExists(ctx, runnerTag); !ok {
		dir := t.TempDir()
		if err := runnerimage.Materialize(dir); err != nil {
			t.Fatal(err)
		}
		if err := d.Build(ctx, dir, runnerTag, map[string]string{"GO_IMAGE": pol.Images.GoBuilder}); err != nil {
			t.Fatalf("build the runner image: %v", err)
		}
	}
	data, err := unsafeexec.Load(ctx, d, pol.Images.UnsafeImage(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	commands := []string{"strings", "decompyle3", "agentry-not-a-command"}
	pkgs, u := data.Place(commands)
	// The forced failure: a well-formed package name Kali does not carry.
	pkgs = append(pkgs, "agentry-no-such-package")
	img := spec.ExecImageSpec{Tag: spec.UnsafeExecImageTag(runnerTag, u, commands, pkgs), Commands: commands, Packages: pkgs, Unsafe: &u}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", img.Tag).Run() })

	got, err := EnsureExecImage(ctx, d, runnerTag, img, t.TempDir(), os.Stderr)
	if err != nil {
		t.Fatalf("the build failed instead of going on: %v", err)
	}
	t.Logf("records: %q", got)
	if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "binutils=") }) {
		t.Errorf("binutils is not recorded as installed: %q", got)
	}
	if !slices.ContainsFunc(got, func(s string) bool {
		return strings.HasPrefix(s, "pip decompyle3==3.9.3 sha256:")
	}) {
		t.Errorf("the pip extra is not recorded with its hash: %q", got)
	}
	if !slices.ContainsFunc(got, func(s string) bool {
		return strings.HasPrefix(s, spec.ExecNotInstalledPrefix+"agentry-no-such-package (")
	}) {
		t.Errorf("the forced failure is not recorded: %q", got)
	}

	// The tools server, as the gateway starts it: the image's entrypoint
	// with `tools --spec`, and the spec the compile writes for Unsafe Mode.
	work := t.TempDir()
	for _, d := range []string{"ws", "out"} {
		if err := os.MkdirAll(filepath.Join(work, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	ts := toolsrv.Spec{SpecVersion: toolsrv.SpecVersion, WorkspaceDir: "/workspace", OutDir: "/out",
		Exec: toolsrv.Exec{Commands: commands, ImagePath: true}}
	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "spec.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"live","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"bash","args":["-c","echo escaped"]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"strings","args":["--version"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"decompyle3","args":["--version"]}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"agentry-not-a-command"}}}`,
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none",
		"-v", filepath.Join(work, "spec.json")+":/etc/agentry/toolspec.json:ro,z",
		"-v", filepath.Join(work, "ws")+":/workspace:z", "-v", filepath.Join(work, "out")+":/out:z",
		img.Tag, "tools", "--spec", "/etc/agentry/toolspec.json")
	cmd.Stdin = strings.NewReader(strings.Join(calls, "\n") + "\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("tools server: %v\n%s", err, errb.String())
	}
	reply := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		for _, id := range []string{"2", "3", "4", "5"} {
			if strings.Contains(line, `"id":`+id+`,`) || strings.Contains(line, `"id":`+id+`}`) {
				reply[id] = line
			}
		}
	}
	for id, want := range map[string]string{
		"2": "not in the declared exec allowlist",
		"3": "exit code 0",
		"4": "decompyle3, version 3.9.3",
		"5": "not present in image",
	} {
		if !strings.Contains(reply[id], want) {
			t.Errorf("call %s: want %q in %s", id, want, reply[id])
		}
	}
	if strings.Contains(out.String(), "escaped") {
		t.Fatal("an undeclared bash ran")
	}
}
