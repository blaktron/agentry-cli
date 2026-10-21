package localrun

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/probe"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestASegmentStartReportsEachStageOnceInOrder: a persistent agent's start
// reports each of its steps once, in order, from the plan's compile to the
// harness; a server the store does not hold is named while it installs, and
// one it holds is not. A start with no hook reports to no one and starts the
// same.
func TestASegmentStartReportsEachStageOnceInOrder(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-9876")
	pol := wordServerRegistry(t)
	home, store := t.TempDir(), t.TempDir()
	builds := 0
	fake := &dockerx.Fake{
		ExistingImages: map[string]bool{
			pol.Images.PythonRuntime: true, pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true,
			pol.RunnerImageTag(runnerimage.ContentVersion()): true,
		},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-word-server": {Before: func(r dockerx.RunSpec) {
				builds++
				if err := os.WriteFile(filepath.Join(r.Mounts[0].Source, "console-scripts"), []byte("word_server\n"), 0o600); err != nil {
					t.Error(err)
				}
			}},
			"-probe": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
				if slices.Contains(r.Argv, "--list-tools") {
					b, _ := json.Marshal(probe.Listing{Tools: []string{}})
					_, _ = stdout.Write(append(b, '\n'))
				}
				return 0, nil
			}},
		},
	}
	segment := func(n int, onStage func(engine.Stage)) Segment {
		return Segment{Builtin: Assistant(), Home: home, Policy: pol, PolicySHA: "s", Build: "test",
			OutDir: t.TempDir(), RunID: "5e9e000" + string(rune('0'+n)), OnStage: onStage,
			Persistent: engine.PersistentSegment{Agent: "assistant", Segment: n,
				SessionVolume: "agentry-persistent-assistant-session", DataVolume: "agentry-persistent-assistant-data"},
			Grants: Grants{
				Servers:    []manifest.MCPServer{{PyPI: "some-word-server", Version: "1.1.11", Tools: []string{"create_document"}}},
				ServersDir: store,
			}}
	}
	// The fake harness exits at once, so each start ends at the harness's
	// handshake, after every step has been reached.
	start := func(seg Segment) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sess, err := seg.Start(ctx, fake)
		if err == nil {
			_, _ = sess.End(context.Background(), false)
			t.Fatal("the start with a harness that exits at once did not fail")
		}
		if !strings.Contains(err.Error(), "interactive session") {
			t.Fatalf("the start failed before the harness: %v", err)
		}
	}

	var got []string
	record := func(st engine.Stage) { got = append(got, st.Name+"/"+st.Subject+": "+st.Words()) }
	start(segment(1, record))
	want := []string{
		"compile/: Preparing",
		"images/: Getting images ready",
		"mcp-servers/some-word-server: Installing some-word-server",
		"prepare/: Preparing",
		"infra/: Starting the gateway",
		"agent/docker-agent: Starting docker-agent",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if builds != 1 {
		t.Fatalf("the server was built %d times, want once", builds)
	}

	// The next start finds the server in the store and does not say it is
	// installing it.
	got = nil
	start(segment(2, record))
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "mcp-servers/") }) {
		t.Errorf("a start with the server stored reported installing it: %v", got)
	}

	// No hook: the same start, reported to no one.
	start(segment(3, nil))
	if builds != 1 {
		t.Errorf("the server was built %d times, want once", builds)
	}
}
