// Support-layer tests: secret resolution plumbing, receipts helpers, run
// identity, error classification, and the teardown leak sweep — the parts
// of the engine that own run-scoped values (§R4/§R6) but need no full
// lifecycle to prove.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func TestParseEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	content := strings.Join([]string{
		"# leading comment",
		"",
		"PLAIN=value",
		"  SPACED  =  padded value  ",
		"EMPTY=",
		"NOEQUALS should be ignored",
		"CHAIN=a=b=c",
		"# trailing comment",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN":  "value",
		"SPACED": "padded value",
		"EMPTY":  "",
		"CHAIN":  "a=b=c", // cut at the FIRST '='
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEnvFile =\n%v\nwant\n%v", got, want)
	}
	if _, ok := got["NOEQUALS should be ignored"]; ok {
		t.Error("no-equals line produced an entry")
	}

	if _, err := parseEnvFile(filepath.Join(t.TempDir(), "absent.env")); err == nil {
		t.Fatal("missing env file must error")
	}
}

func TestLookupSecret(t *testing.T) {
	t.Run("env mode", func(t *testing.T) {
		t.Setenv("LOOKUP_TEST_TOKEN", "from-env")
		v, ok := lookupSecret("LOOKUP_TEST_TOKEN", policy.SecretSource{Mode: "env"})
		if !ok || v != "from-env" {
			t.Fatalf("(%q, %v)", v, ok)
		}
		if _, ok := lookupSecret("LOOKUP_TEST_ABSENT", policy.SecretSource{Mode: "env"}); ok {
			t.Fatal("unset variable reported found")
		}
	})
	t.Run("envFile mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.env")
		if err := os.WriteFile(path, []byte("FILE_TOKEN=from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		src := policy.SecretSource{Mode: "envFile", EnvFile: path}
		if v, ok := lookupSecret("FILE_TOKEN", src); !ok || v != "from-file" {
			t.Fatalf("(%q, %v)", v, ok)
		}
		if _, ok := lookupSecret("OTHER", src); ok {
			t.Fatal("absent key reported found")
		}
	})
	t.Run("envFile missing file resolves nothing", func(t *testing.T) {
		src := policy.SecretSource{Mode: "envFile", EnvFile: filepath.Join(t.TempDir(), "gone.env")}
		if _, ok := lookupSecret("ANY", src); ok {
			t.Fatal("missing env file must resolve to not-found, not to a value")
		}
	})
}

func TestStderrTail(t *testing.T) {
	dir := t.TempDir()

	t.Run("long output keeps the last 15 lines", func(t *testing.T) {
		var lines []string
		for i := 1; i <= 20; i++ {
			lines = append(lines, fmt.Sprintf("line-%02d", i))
		}
		path := filepath.Join(dir, "long.log")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := stderrTail(path)
		if !strings.HasPrefix(got, "\nagent stderr tail:\n") {
			t.Fatalf("tail = %q", got)
		}
		if strings.Contains(got, "line-05") || !strings.Contains(got, "line-06") || !strings.Contains(got, "line-20") {
			t.Errorf("tail window wrong:\n%s", got)
		}
	})
	t.Run("short output passes through whole", func(t *testing.T) {
		path := filepath.Join(dir, "short.log")
		if err := os.WriteFile(path, []byte("only line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := stderrTail(path); !strings.Contains(got, "only line") {
			t.Errorf("tail = %q", got)
		}
	})
	t.Run("missing or empty file adds nothing", func(t *testing.T) {
		if got := stderrTail(filepath.Join(dir, "absent.log")); got != "" {
			t.Errorf("missing file tail = %q", got)
		}
		path := filepath.Join(dir, "empty.log")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := stderrTail(path); got != "" {
			t.Errorf("empty file tail = %q", got)
		}
	})
}

func TestNewRunID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 8 {
			t.Fatalf("run id %q is not 8 hex chars", id)
		}
		for _, r := range id {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("run id %q contains non-hex %q", id, r)
			}
		}
		if seen[id] {
			t.Fatalf("run id %q repeated", id)
		}
		seen[id] = true
	}
}

func TestClassifiedError(t *testing.T) {
	base := errors.New("the cause")
	ce := Classify(ExitPolicy, base)
	if ce.Error() != "the cause" {
		t.Errorf("Error() = %q", ce.Error())
	}
	if ce.Unwrap() != base {
		t.Error("Unwrap did not return the cause")
	}
	if !errors.Is(ce, base) {
		t.Error("errors.Is must see through the classification")
	}
	var target *ClassifiedError
	if !errors.As(fmt.Errorf("wrapped: %w", ce), &target) || target.Code != ExitPolicy {
		t.Error("errors.As must recover the class through further wrapping")
	}
}

func TestGatewayDiagnostics(t *testing.T) {
	f := &dockerx.Fake{LogsByName: map[string]string{"agentry-x-gateway": "  bind: cannot listen\n"}}
	got := gatewayDiagnostics(context.Background(), f, "agentry-x-gateway")
	if got != "\ngateway log tail:\nbind: cannot listen" {
		t.Errorf("diagnostics = %q", got)
	}
	if got := gatewayDiagnostics(context.Background(), f, "agentry-x-unknown"); got != "" {
		t.Errorf("empty log diagnostics = %q", got)
	}
}

func TestReadTask(t *testing.T) {
	dir := t.TempDir()
	t.Run("nil reader is the empty task", func(t *testing.T) {
		b, err := readTask(nil, dir)
		if b != nil || err != nil {
			t.Fatalf("(%q, %v)", b, err)
		}
	})
	t.Run("task is drained and receipted", func(t *testing.T) {
		b, err := readTask(strings.NewReader("do the thing"), dir)
		if err != nil || string(b) != "do the thing" {
			t.Fatalf("(%q, %v)", b, err)
		}
		rec, err := os.ReadFile(filepath.Join(dir, "task.txt"))
		if err != nil || string(rec) != "do the thing" {
			t.Fatalf("task receipt = (%q, %v)", rec, err)
		}
	})
	t.Run("read failure is CLI class", func(t *testing.T) {
		_, err := readTask(iotest.ErrReader(errors.New("pipe burst")), dir)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestFailClassification pins fail()'s two duties: bare errors become the
// CLI class, and a failed run still writes its envelope where it can.
func TestFailClassification(t *testing.T) {
	t.Run("bare error becomes exit 1", func(t *testing.T) {
		env := &receipt.Envelope{}
		_, err := fail(env, errors.New("plain failure"))
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI {
			t.Fatalf("err = %v", err)
		}
		if env.Status != "error" || env.ExitCode != ExitCLI || env.Error != "plain failure" {
			t.Fatalf("envelope = %+v", env)
		}
	})
	t.Run("classified error kept, envelope written", func(t *testing.T) {
		dir := t.TempDir()
		env := &receipt.Envelope{OutDir: dir}
		_, err := fail(env, Classify(ExitGateway, errors.New("gw down")))
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "result.json")); statErr != nil {
			t.Errorf("failed run left no result.json: %v", statErr)
		}
	})
	t.Run("unwritable out dir is reported inside the envelope", func(t *testing.T) {
		env := &receipt.Envelope{OutDir: filepath.Join(t.TempDir(), "gone")}
		_, _ = fail(env, Classify(ExitHarness, errors.New("agent died")))
		if !strings.Contains(env.Error, "agent died") || !strings.Contains(env.Error, "result.json could not be written") {
			t.Errorf("env.Error = %q", env.Error)
		}
	})
}

// TestTeardownLeakSweep pins §R11 invariant 6's last line of defense: after
// the ordered teardown, anything still carrying the run label is
// force-removed.
func TestTeardownLeakSweep(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.Leftover = []string{"agentry-cafe0000-zombie"}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	ops := f.Ops()
	joined := strings.Join(ops, " ")
	if !strings.Contains(joined, "PSByLabel:agentry.run=cafe0000") {
		t.Fatalf("teardown never swept by label: %v", ops)
	}
	// The sweep's RemoveForce must come after the PSByLabel that found it.
	sweepAt, removeAt := -1, -1
	for i, op := range ops {
		if op == "PSByLabel:agentry.run=cafe0000" {
			sweepAt = i
		}
		if op == "RemoveForce:agentry-cafe0000-zombie" && removeAt < 0 {
			removeAt = i
		}
	}
	if removeAt < 0 || removeAt < sweepAt {
		t.Fatalf("leftover container not removed by the sweep: %v", ops)
	}
}

func TestStartInfraFailuresClassified(t *testing.T) {
	t.Run("proxy start failure", func(t *testing.T) {
		p, f := fixture(t)
		f.FailDetached = map[string]error{"-proxy": errors.New("no such image")}
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "start llm proxy") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("gateway start failure still tears down the proxy", func(t *testing.T) {
		p, f := fixture(t)
		f.FailDetached = map[string]error{"-gateway": errors.New("no such image")}
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "start mcp gateway") {
			t.Fatalf("err = %v", err)
		}
		joined := strings.Join(f.Ops(), " ")
		for _, want := range []string{"RemoveForce:agentry-cafe0000-proxy", "NetworkRemove:agentry-cafe0000-net"} {
			if !strings.Contains(joined, want) {
				t.Errorf("teardown missing %s after gateway failure: %v", want, f.Ops())
			}
		}
	})
	t.Run("probe docker failure carries gateway diagnostics", func(t *testing.T) {
		p, f := fixture(t)
		f.Foreground = map[string]*dockerx.FakeForeground{"-probe": {Err: errors.New("cannot attach")}}
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "gateway: bad flag\n"}
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.Error(), "cannot attach") || !strings.Contains(err.Error(), "bad flag") {
			t.Errorf("probe failure lost detail: %v", err)
		}
	})
}

func TestPrepareRunDirsErrors(t *testing.T) {
	t.Run("run root blocked by a file", func(t *testing.T) {
		p, f := fixture(t)
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		p.RunRoot = blocker
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "create run dirs") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("workspace source missing", func(t *testing.T) {
		p, f := fixture(t)
		p.WorkspaceSrc = filepath.Join(t.TempDir(), "gone")
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "copy workspace") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("plan dir incomplete", func(t *testing.T) {
		p, f := fixture(t)
		if err := os.RemoveAll(filepath.Join(p.PlanDir, "harness")); err != nil {
			t.Fatal(err)
		}
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "render plan files") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing input path refuses the run", func(t *testing.T) {
		p, f := fixture(t)
		p.InputPaths = []string{filepath.Join(t.TempDir(), "never-created.json")}
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "input file") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The engine decides per plan mount whether the source is a directory this
// run made. Only those are relabeled for an SELinux-enabled daemon; the
// gateway's docker socket must be mounted exactly as it is, because
// relabeling it would change a system-wide file's label for every container
// on the host (agentry-cli#85).
func TestResolveMountsRelabelsOnlyRunDirectories(t *testing.T) {
	rd := &runDirs{cfg: "/run/x/cfg", workspace: "/run/x/ws", session: "/run/x/session"}
	got := resolveMounts([]spec.Mount{
		{Source: spec.PHCfg + "/gateway", Target: "/mcp", ReadOnly: true},
		{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		{Source: spec.PHWorkspace, Target: spec.WorkspaceMount},
		{Source: spec.PHSession, Target: spec.SessionMount},
		{Source: "/etc/machine-id", Target: "/etc/machine-id", ReadOnly: true},
	}, rd.replacer())

	want := map[string]bool{
		"/mcp":                 true,
		"/var/run/docker.sock": false,
		spec.WorkspaceMount:    true,
		spec.SessionMount:      true,
		"/etc/machine-id":      false,
	}
	if len(got) != len(want) {
		t.Fatalf("resolved %d mounts, want %d", len(got), len(want))
	}
	for _, m := range got {
		w, known := want[m.Target]
		if !known {
			t.Errorf("unexpected mount %+v", m)
			continue
		}
		if m.Relabel != w {
			t.Errorf("mount %s relabel = %v, want %v", m.Target, m.Relabel, w)
		}
	}
}

// What a run labels for its tool containers, and how deep. The run's own
// directories are labelled through; the operator's --out directory is not,
// because it is the one directory in the list they chose and it may hold
// files that have nothing to do with this run (agentry-cli#88). The out
// directory itself is still labelled, without recursing, so a tool
// container can traverse into the reports folder it writes.
func TestRelabelTargetsSpareTheOperatorsOutputDirectory(t *testing.T) {
	rd := &runDirs{
		workspace:  "/run/x/workspace",
		inputs:     "/run/x/inputs",
		cfg:        "/run/x/cfg",
		serversDir: "/home/me/.agentry/servers",
		outDir:     "/home/me/somewhere-of-mine",
	}
	got := map[string]bool{}
	for _, tgt := range relabelTargets(rd) {
		got[tgt.dir] = tgt.recursive
	}
	want := map[string]bool{
		"/run/x/workspace":                   true,
		"/run/x/inputs":                      true,
		"/run/x/cfg":                         true,
		"/home/me/.agentry/servers":          true,
		"/home/me/somewhere-of-mine":         false,
		"/home/me/somewhere-of-mine/reports": true,
	}
	if len(got) != len(want) {
		t.Fatalf("labelled %d directories, want %d: %v", len(got), len(want), got)
	}
	for dir, recursive := range want {
		gotRec, ok := got[dir]
		if !ok {
			t.Errorf("%s is never labelled", dir)
			continue
		}
		if gotRec != recursive {
			t.Errorf("%s recursive = %v, want %v", dir, gotRec, recursive)
		}
	}
}
