//go:build linux

package toolsrv

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The planted value: long and unique so no other text can contain it.
const plantedSecret = "agentry-246-planted-9f1c2e7b5a"

// TestExecChildCannotReadServerEnvironment is the unit-seam check: a tools
// server started with a granted secret in its initial environment, and exec
// granting sh and cat, must not let `cat /proc/$PPID/environ` hand that
// secret back. The server runs in a re-executed copy of this test binary so
// the secret is in its STARTING environment block — the one
// /proc/<pid>/environ shows — exactly as the gateway starts the container.
func TestExecChildCannotReadServerEnvironment(t *testing.T) {
	if os.Getenv("AGENTRY_TOOLSRV_HELPER") == "1" {
		serveHelper()
		return
	}
	if hasCapSysPtrace(t) {
		// A caller holding CAP_SYS_PTRACE (a privileged CI container) may
		// read a non-dumpable process's environ; the tool containers the
		// gateway starts hold no such capability. Say so rather than pass.
		t.Skip("this process holds CAP_SYS_PTRACE, which reads non-dumpable processes; the tool containers do not hold it")
	}
	ws := t.TempDir()
	spec := &Spec{SpecVersion: SpecVersion, WorkspaceDir: ws, OutDir: t.TempDir(),
		Exec: Exec{Commands: []string{"sh", "cat"}}}
	specPath := filepath.Join(t.TempDir(), "toolspec.json")
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	reqs := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		// The parent of the shim is the tools server.
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"sh","args":["-c","cat /proc/$PPID/environ; echo; echo ppid=$PPID"]}}}`,
		// And the server's own view: it can still read itself (non-dumpable
		// restricts others, not the process) — proves the secret is there.
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"sh","args":["-c","cat /proc/self/environ >/dev/null && echo self-ok"]}}}`,
	}, "\n") + "\n"

	cmd := exec.Command(os.Args[0], "-test.run=^TestExecChildCannotReadServerEnvironment$")
	cmd.Env = append(os.Environ(), "AGENTRY_TOOLSRV_HELPER=1", "AGENTRY_TOOLSRV_SPEC="+specPath, "PLANTED_SECRET="+plantedSecret)
	cmd.Stdin = strings.NewReader(reqs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper tools server: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, plantedSecret) {
		t.Fatalf("an exec child read the tools server's environment:\n%s", out)
	}
	line2 := lineWithID(t, out, 2)
	if !strings.Contains(line2, "Permission denied") {
		t.Fatalf("reading /proc/$PPID/environ was not refused: %s", line2)
	}
	if !strings.Contains(line2, "ppid=") {
		t.Fatalf("probe did not run: %s", line2)
	}
	if line3 := lineWithID(t, out, 3); !strings.Contains(line3, "self-ok") {
		t.Fatalf("a child must still read its own environ: %s", line3)
	}
}

// serveHelper runs Serve on this process's stdio: the process started with
// PLANTED_SECRET in its environment block.
func serveHelper() {
	b, err := os.ReadFile(os.Getenv("AGENTRY_TOOLSRV_SPEC"))
	if err != nil {
		os.Exit(2)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		os.Exit(2)
	}
	if err := Serve(os.Stdin, os.Stdout, os.Stderr, &spec); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func lineWithID(t *testing.T, out string, id int) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, `"id":`+strconv.Itoa(id)+`,`) || strings.Contains(l, `"id":`+strconv.Itoa(id)+`}`) {
			return l
		}
	}
	t.Fatalf("no response with id %d in:\n%s", id, out)
	return ""
}

// hasCapSysPtrace reads CapEff from /proc/self/status (bit 19).
func hasCapSysPtrace(t *testing.T) bool {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return caps&(1<<19) != 0
		}
	}
	return false
}

func TestHideEnvironmentMarksTheProcessNonDumpable(t *testing.T) {
	// In a child, so the test binary itself stays dumpable.
	if os.Getenv("AGENTRY_TOOLSRV_DUMPABLE_HELPER") == "1" {
		const prGetDumpable = 3
		if before, _, _ := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0); before != 1 {
			os.Exit(4) // the premise: a fresh process is dumpable
		}
		if err := hideEnvironment(); err != nil {
			os.Exit(2)
		}
		if after, _, _ := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0); after != 0 {
			os.Exit(3)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHideEnvironmentMarksTheProcessNonDumpable$")
	cmd.Env = append(os.Environ(), "AGENTRY_TOOLSRV_DUMPABLE_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hideEnvironment did not leave the process non-dumpable: %v\n%s", err, out)
	}
}
