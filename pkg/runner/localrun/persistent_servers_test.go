package localrun

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestSegmentLaunchesTheConsoleScriptItsFirstInstallFound: a persistent
// agent's first start with a PyPI server whose only console script is not
// named after its project compiles its plan before the server is installed;
// the plan the segment starts launches the script the install found, and the
// next start resolves the server from the store and reuses that plan.
func TestSegmentLaunchesTheConsoleScriptItsFirstInstallFound(t *testing.T) {
	home, store := t.TempDir(), t.TempDir()
	pol := wordServerRegistry(t)
	seg := Segment{Builtin: Assistant(), Home: home, Policy: pol, PolicySHA: "s", Build: "test", Grants: Grants{
		Servers:    []manifest.MCPServer{{PyPI: "some-word-server", Version: "1.1.11", Tools: []string{"create_document"}}},
		ServersDir: store,
	}}
	builds := 0
	fake := &dockerx.Fake{
		// The runtime image, and the runner image the install proxy runs
		// from, which the segment's start has built before any install.
		ExistingImages: map[string]bool{pol.Images.PythonRuntime: true, pol.RunnerImageTag(runnerimage.ContentVersion()): true},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-word-server": wordServerBuild(t, &builds),
		},
	}
	want := spec.MCPStorePrefix + "/pypi-some-word-server/venv/bin/word_server"

	var log strings.Builder
	sp, err := seg.Plan(context.Background(), &log)
	if err != nil {
		t.Fatal(err)
	}
	planned := sp.Plan
	if err := sp.installServers(context.Background(), fake, store); err != nil {
		t.Fatalf("install: %v\n%s", err, log.String())
	}
	// The install found a command the guess did not name, so the plan is
	// compiled again from the servers as the store now holds them, and the
	// log names the command.
	if sp.Plan == planned || !strings.Contains(log.String(), "the install found the command an MCP server launches (some-word-server@1.1.11 runs "+want+") — compiling the plan again") {
		t.Fatalf("the plan was not compiled again from the install (%v):\n%s", sp.Plan != planned, log.String())
	}
	if builds != 1 {
		t.Fatalf("the server was built %d times, want once", builds)
	}
	segmentLaunches(t, sp, want)
	compiled, err := os.Stat(filepath.Join(sp.Dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}

	log.Reset()
	again, err := seg.Plan(context.Background(), &log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "resolving") {
		t.Fatalf("the next start resolved the server again:\n%s", log.String())
	}
	// A compile replaces the plan's folder whole, so a reused plan is the
	// same file.
	if reused, err := os.Stat(filepath.Join(again.Dir, "plan.json")); err != nil || !os.SameFile(compiled, reused) {
		t.Fatalf("the next start compiled its plan again (%v):\n%s", err, log.String())
	}
	segmentLaunches(t, again, want)
	reused, reusedDir := again.Plan, again.Dir
	log.Reset()
	if err := again.installServers(context.Background(), fake, store); err != nil || builds != 1 {
		t.Fatalf("the next start installed again (%d builds): %v", builds, err)
	}
	// An install that changes nothing leaves the plan and its folder as they
	// were, and says nothing of compiling.
	if again.Plan != reused || again.Dir != reusedDir || strings.Contains(log.String(), "compiling the plan again") {
		t.Fatalf("an install that found nothing new moved the plan (%v):\n%s", again.Plan != reused, log.String())
	}
	segmentLaunches(t, again, want)

	// A server is installed outside a segment's start the same way, and a
	// failed install is the gateway's exit, a pin the registry does not hold
	// the verify exit.
	pin := manifest.MCPServer{PyPI: "some-word-server", Version: "1.1.11", Tools: []string{"create_document"}}
	apart := t.TempDir()
	if err := InstallServer(context.Background(), fake, pol, apart, pin, nil); err != nil || builds != 2 {
		t.Fatalf("install apart (%d builds): %v", builds, err)
	}
	var ce *engine.ClassifiedError
	broken := &dockerx.Fake{
		ExistingImages: fake.ExistingImages,
		Foreground:     map[string]*dockerx.FakeForeground{"agentry-mcpinstall-pypi-some-word-server": {ExitCode: 1}},
	}
	if err := InstallServer(context.Background(), broken, pol, t.TempDir(), pin, io.Discard); !errors.As(err, &ce) || ce.Code != engine.ExitGateway {
		t.Errorf("a failed install apart: %v", err)
	}
	missing := pin
	missing.Version = "9.9.9"
	if err := InstallServer(context.Background(), fake, pol, t.TempDir(), missing, io.Discard); !errors.As(err, &ce) || ce.Code != engine.ExitVerify {
		t.Errorf("a pin the registry does not hold, installed apart: %v", err)
	}
	failing := seg
	failing.Grants.ServersDir = t.TempDir()
	failed, err := failing.Plan(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.installServers(context.Background(), broken, failing.Grants.ServersDir); !errors.As(err, &ce) || ce.Code != engine.ExitGateway {
		t.Errorf("a failed install in a start: %v", err)
	}

	// A package server needs the agent's store, and a pin the registry does
	// not hold is refused as unverifiable.
	noStore := seg
	noStore.Grants.ServersDir = ""
	if _, err := noStore.Plan(context.Background(), io.Discard); !errors.As(err, &ce) || ce.Code != engine.ExitCLI || !strings.Contains(err.Error(), "the agent has a package server on, and no store to install it into") {
		t.Errorf("a package server with no store: %v", err)
	}
	unpinned := seg
	unpinned.Grants = Grants{ServersDir: t.TempDir(), Servers: []manifest.MCPServer{missing}}
	if _, err := unpinned.Plan(context.Background(), io.Discard); !errors.As(err, &ce) || ce.Code != engine.ExitVerify {
		t.Errorf("a pin the registry does not hold: %v", err)
	}

	// A command that changed is named with its server and version; a server
	// that was not in the plan is named too.
	one := spec.MCPServerSpec{Name: "one", Package: "pkg-one", Version: "1.0.0", Command: []string{"one-run"}}
	two := spec.MCPServerSpec{Name: "two", Package: "pkg-two", Version: "2.0.0", Command: []string{"two-run", "--serve"}}
	moved := two
	moved.Command = []string{"two-run", "--stdio"}
	for _, tc := range []struct {
		name                string
		compiled, installed []spec.MCPServerSpec
		want                string
	}{
		{"nothing changed", []spec.MCPServerSpec{one, two}, []spec.MCPServerSpec{one, two}, ""},
		{"one command changed", []spec.MCPServerSpec{one, two}, []spec.MCPServerSpec{one, moved}, "pkg-two@2.0.0 runs two-run --stdio"},
		{"servers the plan lacked", nil, []spec.MCPServerSpec{one, two}, "pkg-one@1.0.0 runs one-run, pkg-two@2.0.0 runs two-run --serve"},
	} {
		if got := launchChanges(tc.compiled, tc.installed); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// wordServerRegistry serves PyPI's metadata of some-word-server 1.1.11 for
// the test and returns the default policy pointed at it.
func wordServerRegistry(t *testing.T) *policy.Policy {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pypi/some-word-server/1.1.11/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"info":{},"urls":[
		  {"packagetype":"bdist_wheel","filename":"some_word_server-1.1.11-py3-none-any.whl","digests":{"sha256":"cafe"}}]}`))
	})
	registry := httptest.NewTLSServer(mux)
	t.Cleanup(registry.Close)
	before := http.DefaultTransport
	http.DefaultTransport = registry.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = before })
	pol := policy.Default()
	pol.MCPRegistries.PyPI = registry.URL
	return pol
}

// wordServerBuild plays some-word-server's install, counting the builds:
// the distribution's one console script is word_server.
func wordServerBuild(t *testing.T, builds *int) *dockerx.FakeForeground {
	return &dockerx.FakeForeground{Print: func(dockerx.RunSpec) string {
		*builds++
		return consoleScriptsArchive(t)
	}}
}

// consoleScriptsArchive is a PyPI builder's stdout for a server whose one
// console script is word_server: the tree it installed, as one tar archive.
func consoleScriptsArchive(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := "word_server\n"
	if err := tw.WriteHeader(&tar.Header{Name: "console-scripts", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// segmentLaunches holds a segment's plan, and the gateway catalog compiled
// with it, to launching the one added server with want.
func segmentLaunches(t *testing.T, sp *SegmentPlan, want string) {
	t.Helper()
	var cmd []string
	for _, s := range sp.Plan.MCPServers {
		if s.Package == "some-word-server" {
			cmd = s.Command
		}
	}
	if len(cmd) != 1 || cmd[0] != want {
		t.Fatalf("the plan launches %q, want %s", cmd, want)
	}
	catalog, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), want) || strings.Contains(string(catalog), "/venv/bin/some-word-server") {
		t.Fatalf("the gateway catalog does not launch %s:\n%s", want, catalog)
	}
}
