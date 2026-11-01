package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

func TestParseSSHTarget(t *testing.T) {
	a, err := parseSSHTarget("ssh://admin@server.example:2222")
	if err != nil || a.User != "admin" || a.Host != "server.example" || a.Port != 2222 {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := parseSSHTarget("ssh://192.0.2.7"); err != nil || a.Port != 22 || a.Host != "192.0.2.7" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range []string{"server.example", "ssh://admin:secret@server.example", "ssh://server.example/path", "http://server.example", "ssh://"} {
		if _, err := parseSSHTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the refusal repeats the password: %v", bad, err)
		}
	}
}

func TestAdminFlagsNameFilesAndVariablesNeverValues(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RH_TEST_SUDO", "sudo-from-env")
	t.Setenv("SSH_AUTH_SOCK", "")
	f := adminFlags{passwordFile: pw, sudoEnv: "RH_TEST_SUDO"}
	a, err := f.resolve(nil, "admin@server")
	if err != nil || a.Password != "from-a-file" || a.Sudo != "sudo-from-env" || a.Agent {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := (&adminFlags{passwordEnv: "RH_TEST_UNSET_VAR"}).resolve(nil, "x"); err == nil {
		t.Fatal("an unset variable was taken as an empty password")
	}
	if _, err := (&adminFlags{passwordEnv: "A", passwordFile: pw}).resolve(nil, "x"); err == nil {
		t.Fatal("two ways to give the password were accepted")
	}
	// No sign-in named, no agent, no one to ask: refused, not guessed.
	if _, err := (&adminFlags{}).resolve(nil, "x"); err == nil {
		t.Fatal("setup would go on with no sign-in")
	}
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
	if a, err := (&adminFlags{}).resolve(nil, "x"); err != nil || !a.Agent {
		t.Fatalf("the SSH agent was not the default: %+v %v", a, err)
	}
}

func TestRunhostRuntimeChecksHoldTheRegistrationsToSetups(t *testing.T) {
	good := map[string]dockerx.Runtime{
		runhost.RunscRuntime:    {Path: runhost.RunscPath},
		runhost.RunscUDSRuntime: {Path: runhost.RunscPath, Args: []string{dockerx.HostUDSOpen}},
	}
	failed := func(table map[string]dockerx.Runtime) []string {
		var out []string
		for _, c := range runhostRuntimeChecks(table) {
			if c.Status == "fail" {
				out = append(out, c.Name)
			}
		}
		return out
	}
	if f := failed(good); len(f) != 0 {
		t.Fatalf("setup's own registrations failed: %v", f)
	}
	cases := map[string]map[string]dockerx.Runtime{
		"no runsc":               {runhost.RunscUDSRuntime: good[runhost.RunscUDSRuntime]},
		"no runsc-uds":           {runhost.RunscRuntime: good[runhost.RunscRuntime]},
		"runsc elsewhere":        {runhost.RunscRuntime: {Path: "/usr/bin/runsc"}, runhost.RunscUDSRuntime: good[runhost.RunscUDSRuntime]},
		"runsc opening host uds": {runhost.RunscRuntime: {Path: runhost.RunscPath, Args: []string{dockerx.HostUDSOpen}}, runhost.RunscUDSRuntime: good[runhost.RunscUDSRuntime]},
		"runsc-uds without it":   {runhost.RunscRuntime: good[runhost.RunscRuntime], runhost.RunscUDSRuntime: {Path: runhost.RunscPath}},
		"runsc-uds with more":    {runhost.RunscRuntime: good[runhost.RunscRuntime], runhost.RunscUDSRuntime: {Path: runhost.RunscPath, Args: []string{dockerx.HostUDSOpen, "--network=host"}}},
	}
	for name, table := range cases {
		if len(failed(table)) == 0 {
			t.Errorf("%s: doctor passed", name)
		}
	}
}
