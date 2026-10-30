package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

const gatewayLog = `- Reading configuration...
  > mcp-pypi-time: (1 tools) (0 prompts)
  > mcp-pypi-other: (3 tools)
  - Running mcp-pypi-time
`

func TestGatewayToolCountReadsTheGatewaysStartupLog(t *testing.T) {
	for name, want := range map[string]int{"mcp-pypi-time": 1, "mcp-pypi-other": 3, "mcp-pypi-none": 0, "mcp-pypi": 0} {
		if got := gatewayToolCount(gatewayLog, name); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}
}

func filterParams(stderr io.Writer, servers ...spec.MCPServerSpec) Params {
	return Params{Plan: &spec.Plan{MCPServers: servers}, Stderr: stderr}
}

func timeServer(tools ...string) spec.MCPServerSpec {
	return spec.MCPServerSpec{Name: "mcp-pypi-time", Source: "pypi", Package: "mcp-server-time", Version: "1.0.0", Tools: tools}
}

func listerOf(names []string, err error, calls *int) toolLister {
	return func(context.Context, dockerx.Client, Params, *runDirs, string, spec.MCPServerSpec) ([]string, error) {
		*calls++
		return names, err
	}
}

// A filter entry that differs from the server's tool name only in case stops
// the run, naming the entry, the server and the exact name to write.
func TestACaseOnlyToolFilterEntryStopsTheRun(t *testing.T) {
	var stderr bytes.Buffer
	calls := 0
	err := checkToolFilters(t.Context(), nil, filterParams(&stderr, timeServer("Get_Current_Time")), &runDirs{}, "r1", "  > mcp-pypi-other: (3 tools)\n", listerOf([]string{"get_current_time", "convert_time"}, nil, &calls))
	if err == nil {
		t.Fatal("a case-only entry did not stop the run")
	}
	for _, want := range []string{"mcp-server-time@1.0.0", `"Get_Current_Time"`, `"get_current_time"`, "only when case is ignored", "write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	if reason := receipt.ReasonOf(err); reason == nil || reason.Code != receipt.ReasonMCPStartFailed || len(reason.Names) != 1 || reason.Names[0] != "pypi:mcp-server-time" {
		t.Errorf("reason = %+v", reason)
	}
	if calls != 1 {
		t.Errorf("the server was listed %d times", calls)
	}
}

// The server is asked only when the gateway enabled fewer tools than the
// filter names: a filter that matched is not looked into.
func TestAFilterTheGatewayServedInFullIsNotLookedInto(t *testing.T) {
	calls := 0
	var stderr bytes.Buffer
	err := checkToolFilters(t.Context(), nil, filterParams(&stderr, timeServer("convert_time"), spec.MCPServerSpec{Name: "mcp-pypi-other", Source: "pypi", Package: "other", Tools: []string{"a", "b", "c"}}),
		&runDirs{}, "r1", gatewayLog, listerOf([]string{"x"}, nil, &calls))
	if err != nil || calls != 0 || stderr.Len() != 0 {
		t.Fatalf("err %v, lister calls %d, stderr %q", err, calls, stderr.String())
	}
	// A server with no filter is never listed either.
	if err := checkToolFilters(t.Context(), nil, filterParams(&stderr, timeServer()), &runDirs{}, "r1", "", listerOf(nil, nil, &calls)); err != nil || calls != 0 {
		t.Fatalf("no filter: %v, %d", err, calls)
	}
}

// An entry that matches no tool even ignoring case is told to the person and
// the run goes on, as it did before; so is a count the server's list cannot
// explain, and a server whose list could not be read.
func TestAnEntryMatchingNoToolIsReportedAndTheRunGoesOn(t *testing.T) {
	for name, c := range map[string]struct {
		listed  []string
		err     error
		wantErr string
		want    string
	}{
		"absent":      {listed: []string{"get_current_time"}, want: `names "nope", which the server does not list`},
		"unreadable":  {err: errors.New("exited"), want: "could not be read to say which"},
		"all present": {listed: []string{"nope", "get_current_time"}, want: "the gateway enabled 0 of the 2 tools its filter names"},
	} {
		var stderr bytes.Buffer
		calls := 0
		filter := []string{"nope", "get_current_time"}
		if name == "absent" {
			filter = []string{"nope"}
		}
		srv := timeServer(filter...)
		srv.Name = "mcp-pypi-time"
		err := checkToolFilters(t.Context(), nil, filterParams(&stderr, srv), &runDirs{}, "r1", "", listerOf(c.listed, c.err, &calls))
		if err != nil {
			t.Errorf("%s: stopped the run: %v", name, err)
		}
		if !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", name, stderr.String(), c.want)
		}
	}
}

// A remote server's tools are not listed by the runner: its count shortfall is
// reported without asking anything.
func TestARemoteServersShortfallIsReportedWithoutListingIt(t *testing.T) {
	var stderr bytes.Buffer
	calls := 0
	srv := spec.MCPServerSpec{Name: "mcp-url-x", Source: spec.SourceURL, Package: "https://x.example/mcp", Tools: []string{"Search"}}
	if err := checkToolFilters(t.Context(), nil, filterParams(&stderr, srv), &runDirs{}, "r1", "", listerOf(nil, nil, &calls)); err != nil || calls != 0 {
		t.Fatalf("err %v, calls %d", err, calls)
	}
	if !strings.Contains(stderr.String(), "a remote server's tool list is not read") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestReadToolListingIgnoresEverythingButTheAnswerToToolsList(t *testing.T) {
	out := strings.Join([]string{
		`starting up`,
		`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"x"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"b"},{"name":""},{"name":"a"}]}}`,
	}, "\n")
	got, err := readToolListing(strings.NewReader(out))
	if err != nil || strings.Join(got, ",") != "b,a" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", `{"jsonrpc":"2.0","id":2,"error":{"message":"no"}}`, `{"jsonrpc":"2.0","id":1,"result":{}}`} {
		if _, err := readToolListing(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// The listing container is the server alone: its image, command and arguments,
// its store read-only, no network, placeholders for its secrets, speaking MCP
// on stdio.
func TestListServerToolsStartsTheServerAloneAndReadsItsListing(t *testing.T) {
	fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"toollist": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			sc := bufio.NewScanner(stdin)
			for sc.Scan() {
				if strings.Contains(sc.Text(), `"tools/list"`) {
					_, _ = io.WriteString(stdout, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"get_current_time"},{"name":"convert_time"}]}}`+"\n")
					return 0, nil
				}
			}
			return 1, nil
		}},
	}}
	srv := spec.MCPServerSpec{
		Name: "mcp-pypi-time", Source: "pypi", Package: "mcp-server-time", Image: "python:test", StoreSubdir: "pypi-mcp-server-time",
		Command: []string{"/opt/agentry-mcp/pypi-mcp-server-time/venv/bin/mcp-server-time"}, Args: []string{"--local-timezone=UTC"},
		Env: map[string]string{"A": "b"}, Secrets: []string{"TOKEN"}, Tools: []string{"Get_Current_Time"},
	}
	got, err := listServerTools(t.Context(), fake, filterParams(io.Discard, srv), &runDirs{serversDir: "/store"}, "r1", srv)
	if err != nil || strings.Join(got, ",") != "get_current_time,convert_time" {
		t.Fatalf("%v %v", got, err)
	}
	run, ok := fake.FindRun("toollist")
	if !ok {
		t.Fatal("no listing container ran")
	}
	if run.Network != "none" || !run.AutoRemove || !run.Interactive || run.Image != "python:test" {
		t.Errorf("run = %+v", run)
	}
	if want := []string{srv.Command[0], "--local-timezone=UTC"}; strings.Join(run.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v", run.Argv)
	}
	if len(run.Mounts) != 1 || !run.Mounts[0].ReadOnly || run.Mounts[0].Source != "/store/pypi-mcp-server-time" || run.Mounts[0].Target != "/opt/agentry-mcp/pypi-mcp-server-time" {
		t.Errorf("mounts = %+v", run.Mounts)
	}
	if !slices.Equal(run.CapAdd, sockfilter.ServerReadCapabilities()) || !slices.Equal(run.CapDrop, []string{"ALL"}) || !run.NoNewPrivileges {
		t.Errorf("posture: add %v, drop %v, no-new-privileges %v", run.CapAdd, run.CapDrop, run.NoNewPrivileges)
	}
	if run.Env["A"] != "b" || run.Env["TOKEN"] == "" || !strings.Contains(run.Env["TOKEN"], "placeholder") {
		t.Errorf("env = %v", run.Env)
	}
	if ops := strings.Join(fake.Ops(), " "); !strings.Contains(ops, "RemoveForce") {
		t.Errorf("the container is not removed: %s", ops)
	}
	// A server with no store to read is not started.
	if _, err := listServerTools(t.Context(), fake, filterParams(io.Discard, srv), &runDirs{}, "r1", srv); err == nil {
		t.Error("listed a server with no store")
	}
}

// A container that never starts, or exits at once, is an error that says why,
// not a wait for a listing that cannot come.
func TestListServerToolsGivesUpWhenTheContainerDoesNotStart(t *testing.T) {
	srv := spec.MCPServerSpec{Name: "mcp-pypi-x", Source: "pypi", Package: "x", Image: "python:test", StoreSubdir: "pypi-x", Command: []string{"/opt/agentry-mcp/pypi-x/venv/bin/x"}}
	// None of them reads its stdin, as a container that did not start does not.
	for name, h := range map[string]func(io.Writer, io.Writer) (int, error){
		"docker failed": func(_, stderr io.Writer) (int, error) {
			_, _ = io.WriteString(stderr, "exec: permission denied")
			return 126, errors.New("docker run failed (exit 126)")
		},
		"exited at once": func(_, stderr io.Writer) (int, error) {
			_, _ = io.WriteString(stderr, "boom")
			return 1, nil
		},
		"printed no listing": func(stdout, _ io.Writer) (int, error) {
			_, _ = io.WriteString(stdout, "starting\n")
			return 0, nil
		},
	} {
		fg := &dockerx.FakeForeground{Handler: func(_ context.Context, _ dockerx.RunSpec, _ io.Reader, stdout, stderr io.Writer) (int, error) {
			return h(stdout, stderr)
		}}
		fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"toollist": fg}}
		_, err := listServerTools(t.Context(), fake, filterParams(io.Discard, srv), &runDirs{serversDir: "/store"}, "r1", srv)
		if err == nil || !strings.Contains(err.Error(), "did not answer") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
