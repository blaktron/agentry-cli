package toolsrv

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// anySpec is a Coding agent's tools spec: any program, writes beneath its
// workspace, HOME there.
func anySpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Exec = Exec{Any: true, ImagePath: true, Home: filepath.Join(spec.WorkspaceDir, "data", ".home")}
	return spec
}

func TestRunAnyRunsAnyProgramInAFolderBeneathTheWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a program runs confined on Linux alone")
	}
	spec := anySpec(t)
	s := newTestServer(t, spec)
	if !slices.Contains(spec.GrantedTools(), ToolRunCommand) || s.tools[ToolRunCommand].schema["properties"].(map[string]any)["dir"] == nil {
		t.Fatalf("run_command under exec.any: granted %v, schema %v", spec.GrantedTools(), s.tools[ToolRunCommand].schema)
	}
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	if fi, err := os.Stat(spec.Exec.Home); err != nil || !fi.IsDir() {
		t.Fatalf("HOME %s was not made: %v", spec.Exec.Home, err)
	}
	proj := filepath.Join(spec.WorkspaceDir, "data", "projects", "app")
	mustWrite(t, filepath.Join(proj, "build.sh"), []byte("#!/bin/sh\necho built in \"$(pwd)\" > out.txt\necho ok\n"))
	if err := os.Chmod(filepath.Join(proj, "build.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("a program on the PATH, undeclared", func(t *testing.T) {
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "echo hi"}, "dir": "data/projects/app"}))
		if isErr || got != "exit code 0\nhi\n" {
			t.Fatalf("sh = %q (isError %v)", got, isErr)
		}
	})
	t.Run("a path relative to dir, writing in the workspace", func(t *testing.T) {
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "./build.sh", "dir": "data/projects/app"}))
		if isErr || got != "exit code 0\nok\n" {
			t.Fatalf("./build.sh = %q (isError %v)", got, isErr)
		}
		b, err := os.ReadFile(filepath.Join(proj, "out.txt"))
		if err != nil || !strings.Contains(string(b), proj) {
			t.Fatalf("the program's file: %q, %v", b, err)
		}
	})
	t.Run("HOME is the workspace's and the server's environment stays its own", func(t *testing.T) {
		t.Setenv("AGENTRY_TEST_SERVER_ONLY", "secret-value")
		t.Setenv("GOTOOLCHAIN", "local")
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "env"}}))
		if isErr || !strings.Contains(got, "HOME="+spec.Exec.Home+"\n") || !strings.Contains(got, "GOTOOLCHAIN=local\n") ||
			strings.Contains(got, "AGENTRY_TEST_SERVER_ONLY") {
			t.Fatalf("env = %q (isError %v)", got, isErr)
		}
	})
	t.Run("a dir outside the workspace is refused", func(t *testing.T) {
		for _, dir := range []string{"../", "/etc", "data/../../x", "data/missing"} {
			got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "true"}, "dir": dir}))
			if !isErr || !strings.Contains(got, "dir") {
				t.Errorf("dir %q = %q (isError %v), want a refusal", dir, got, isErr)
			}
		}
	})
	t.Run("what is not a program is refused", func(t *testing.T) {
		for _, c := range []string{"agentry-no-such-program", "./out.txt", "./", `bin\sh`} {
			got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": c, "dir": "data/projects/app"}))
			if !isErr {
				t.Errorf("command %q = %q, want a refusal", c, got)
			}
		}
	})
	t.Run("a write outside its folders is refused", func(t *testing.T) {
		// The test's folders are all under /tmp, which a program may write:
		// this server's /tmp is one of its own, so the output folder is
		// outside it.
		abi, err := confine.ABI()
		if err != nil {
			t.Skipf("no Landlock: %v", err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		was := s.confine
		s.confine = chooseConfinement(spec, "linux", abi, nil, os.Geteuid(), exe, t.TempDir())
		defer func() { s.confine = was }()
		outside := filepath.Join(spec.OutDir, "escaped")
		got, _ := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "echo x > " + outside}}))
		if _, err := os.Stat(outside); err == nil || strings.HasPrefix(got, "exit code 0") {
			t.Fatalf("a write to the output folder: %q", got)
		}
	})
}

func TestRunAnySaysWhereItReaches(t *testing.T) {
	spec := anySpec(t)
	if d := newTestServer(t, spec).runAnyDescription(); !strings.Contains(d, "It has no network.") || !strings.Contains(d, spec.Exec.Home) {
		t.Errorf("without egress: %q", d)
	}
	spec = anySpec(t)
	spec.Fetch = Fetch{Enabled: true, AllowAll: true}
	if d := newTestServer(t, spec).runAnyDescription(); !strings.Contains(d, "It reaches the internet") {
		t.Errorf("with open egress: %q", d)
	}
}

func TestRunAnyIsHeldToItsShape(t *testing.T) {
	for name, mod := range map[string]func(*Spec){
		"beside declared commands":   func(s *Spec) { s.Exec.Commands = []string{"sh"} },
		"without the image's PATH":   func(s *Spec) { s.Exec.ImagePath = false },
		"HOME outside the workspace": func(s *Spec) { s.Exec.Home = "/tmp/home" },
		"HOME not clean":             func(s *Spec) { s.Exec.Home = s.WorkspaceDir + "/data/../../home" },
		"beside the web tools":       func(s *Spec) { s.Web = Web{Enabled: true} },
	} {
		spec := anySpec(t)
		mod(spec)
		if err := validateSpec(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateSpec(anySpec(t)); err != nil {
		t.Fatalf("the Coding agent's spec: %v", err)
	}
}
