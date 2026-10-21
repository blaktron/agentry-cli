package toolsrv

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestABackgroundProgramOutlivesItsCall: a program started in the
// background is still running after the call, its output is read with
// background_output, at most maxBackground run at once, and stop_background
// ends it and what it started.
func TestABackgroundProgramOutlivesItsCall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a program runs confined on Linux alone")
	}
	spec := anySpec(t)
	s := newTestServer(t, spec)
	if s.confine.refused != "" {
		t.Skipf("this machine cannot confine a program: %s", s.confine.refused)
	}
	defer s.stopAllBackground()
	was := backgroundSettle
	backgroundSettle = 300 * time.Millisecond
	defer func() { backgroundSettle = was }()
	for _, name := range []string{ToolBackgroundOutput, ToolStopBackground} {
		if !slices.Contains(spec.GrantedTools(), name) || s.tools[name].schema == nil {
			t.Fatalf("%s is not served under exec.any: %v", name, spec.GrantedTools())
		}
	}

	got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "echo serving; sleep 1000 & wait"}, "background": true}))
	if isErr || !strings.HasPrefix(got, "started in the background as bg1") || !strings.Contains(got, "serving") {
		t.Fatalf("start = %q (isError %v)", got, isErr)
	}
	got, isErr = s.toolBackgroundOutput(rawArgs(t, map[string]any{"id": "bg1"}))
	if isErr || !strings.HasPrefix(got, "bg1: running since") || !strings.Contains(got, "serving") {
		t.Fatalf("output = %q (isError %v)", got, isErr)
	}

	t.Run("one that ends at once is answered as in the foreground", func(t *testing.T) {
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "echo done; exit 3"}, "background": true}))
		if isErr || !strings.Contains(got, "ended, exit code 3") || !strings.Contains(got, "nothing runs in the background") || !strings.Contains(got, "done") {
			t.Fatalf("a program that ends: %q (isError %v)", got, isErr)
		}
	})
	t.Run("a time limit is refused beside background", func(t *testing.T) {
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sh", "background": true, "timeoutSeconds": 5}))
		if !isErr || !strings.Contains(got, "no time limit") {
			t.Fatalf("background with a time limit: %q", got)
		}
	})
	t.Run("at most maxBackground run at once", func(t *testing.T) {
		for i := 0; i < maxBackground-1; i++ {
			if got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sleep", "args": []string{"1000"}, "background": true})); isErr {
				t.Fatalf("start %d: %q", i, got)
			}
		}
		got, isErr := s.toolRunAny(rawArgs(t, map[string]any{"command": "sleep", "args": []string{"1000"}, "background": true}))
		if !isErr || !strings.Contains(got, "the most at once") {
			t.Fatalf("one too many: %q", got)
		}
	})

	got, isErr = s.toolStopBackground(rawArgs(t, map[string]any{"id": "bg1"}))
	if isErr || !strings.Contains(got, "bg1: ended") {
		t.Fatalf("stop = %q (isError %v)", got, isErr)
	}
	list, _ := s.toolBackgroundOutput(rawArgs(t, map[string]any{}))
	if lines := strings.Split(list, "\n"); len(lines) != maxBackground+1 || !strings.HasPrefix(lines[0], "bg1: ended") {
		t.Fatalf("the list = %q", list)
	}
	if got, isErr := s.toolStopBackground(rawArgs(t, map[string]any{"id": "bg99"})); !isErr || !strings.Contains(got, "no program") {
		t.Errorf("stop of an unknown program: %q", got)
	}
}

func TestPortsAreNamedToTheAgentAndHeldToTheirShape(t *testing.T) {
	spec := anySpec(t)
	if d := newTestServer(t, spec).runAnyDescription(); !strings.Contains(d, "No port of yours is published") {
		t.Errorf("without ports: %q", d)
	}
	spec.Exec.Ports = []Port{{Name: "web", Port: 3000}}
	if d := newTestServer(t, spec).runAnyDescription(); !strings.Contains(d, "web on port 3000 (http://127.0.0.1:3000 there)") {
		t.Errorf("with a port: %q", d)
	}
	for name, ports := range map[string][]Port{
		"privileged":     {{Name: "web", Port: 80}},
		"too high":       {{Name: "web", Port: 70000}},
		"unnamed":        {{Port: 3000}},
		"a name twice":   {{Name: "web", Port: 3000}, {Name: "web", Port: 3001}},
		"a number twice": {{Name: "web", Port: 3000}, {Name: "api", Port: 3000}},
	} {
		spec := anySpec(t)
		spec.Exec.Ports = ports
		if err := validateSpec(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	spec = baseSpec(t)
	spec.Exec = Exec{Commands: []string{"sh"}, Ports: []Port{{Name: "web", Port: 3000}}}
	if err := validateSpec(spec); err == nil {
		t.Error("ports beside declared commands: accepted")
	}
}
