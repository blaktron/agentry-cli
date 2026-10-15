package compile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/unsafeexec"
)

const testUnsafeBase = "ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.2@sha256:87fa4ae406a9621cef3a4cffcdccf2a512da03141ca21727ce663ca715a191da"

func testUnsafeData(t *testing.T) *unsafeexec.Data {
	t.Helper()
	d, err := unsafeexec.Parse(testUnsafeBase,
		[]byte("jq\tjq\ngit\tgit\nfile\tfile\nstrings\tbinutils\n"),
		[]byte("pip\tdecompyle3\tdecompyle3==3.9.3\nlink\tanalyzeHeadless\tghidra\tusr/share/ghidra/support/analyzeHeadless\n"),
		[]byte("Crack\tcrack,crack-md5\n"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// compileBothWays compiles one manifest with and without Unsafe Mode and
// returns each output file flattened to path → value.
func compileBothWays(t *testing.T, yamlText string, pol func(*policy.Policy)) (safe, unsafe map[string]string, plan *spec.Plan) {
	t.Helper()
	m := parseManifest(t, yamlText)
	ws := workspaceFor(t, m, nil)
	run := func(data *unsafeexec.Data) (map[string]string, *spec.Plan) {
		p := testPolicy()
		if pol != nil {
			pol(p)
		}
		dir := t.TempDir()
		plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
			Policy: p, PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test", Unsafe: data}, dir)
		if err != nil {
			t.Fatalf("compile (unsafe %v): %v", data != nil, err)
		}
		return flattenDir(t, dir), plan
	}
	safe, _ = run(nil)
	unsafe, plan = run(testUnsafeData(t))
	return safe, unsafe, plan
}

// flattenDir reads every JSON and YAML file under dir into path → value.
func flattenDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var v any
		switch {
		case strings.HasSuffix(p, ".json"):
			err = json.Unmarshal(b, &v)
		case strings.HasSuffix(p, ".yaml"):
			err = yaml.Unmarshal(b, &v)
		default:
			out[rel] = string(b)
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		flatten(out, filepath.ToSlash(rel), v)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func flatten(out map[string]string, prefix string, v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			flatten(out, prefix+"."+k, e)
		}
	case []any:
		for i, e := range x {
			flatten(out, fmt.Sprintf("%s[%d]", prefix, i), e)
		}
	default:
		out[prefix] = fmt.Sprint(x)
	}
}

// A plan compiled in Unsafe Mode differs from the one the default posture
// compiles for the same manifest (its commands catalogued) only in the exec
// image spec, the lines naming Unsafe Mode, and what follows from them: the
// tools image the gateway runs, the tool spec's image PATH, and the file
// hashes. The gateway's --block-network, the containers' posture, the
// agent's /plan-only mount, egress, the declared servers, the budgets and
// every other enforcement line are byte for byte the same.
func TestUnsafeModeChangesOnlyTheExecImage(t *testing.T) {
	safe, unsafe, plan := compileBothWays(t, fullCapsYAML, nil)
	if plan.ExecImage == nil || plan.ExecImage.Unsafe == nil {
		t.Fatalf("the unsafe compile has no Unsafe Mode exec image: %+v", plan.ExecImage)
	}
	allowed := []string{
		"plan.json.execImage.",                              // the exec image spec itself
		"plan.json.enforcement.exec",                        // the line naming Unsafe Mode
		"plan.json.planHash",                                // hashes over what changed
		"plan.json.files.",                                  //
		"gateway/catalog.yaml.registry.agentry-tools.image", // the tools image is the exec image
		"gateway/toolspec.json.exec.imagePath",              // a declared command's PATH
	}
	var diffs []string
	for _, k := range union(safe, unsafe) {
		if safe[k] == unsafe[k] {
			continue
		}
		ok := false
		for _, a := range allowed {
			if k == a || strings.HasPrefix(k, a) {
				ok = true
			}
		}
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: %q → %q", k, safe[k], unsafe[k]))
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("Unsafe Mode changed what it must not:\n%s", strings.Join(diffs, "\n"))
	}
	for _, must := range []string{"plan.json.enforcement.exec", "gateway/toolspec.json.exec.imagePath"} {
		if safe[must] == unsafe[must] {
			t.Errorf("%s did not change", must)
		}
	}
	for k, v := range unsafe {
		if strings.HasPrefix(k, "gateway/catalog.yaml") && strings.HasSuffix(k, ".image") && v == plan.ExecImage.Tag {
			return
		}
	}
	t.Fatalf("no gateway catalog entry runs the Unsafe Mode exec image %s", plan.ExecImage.Tag)
}

func union(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Without the option, an uncatalogued command is refused (exit 11, naming
// exec.packages); with it the same manifest compiles, and what the image
// cannot place is named in the exec line rather than refused. The catalogue
// is not consulted: an empty one changes nothing.
func TestUnsafeModeNeverRefusesAndTheDefaultStillDoes(t *testing.T) {
	m := parseManifest(t, "schemaVersion: \"2\"\nname: re\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"r\"\n"+
		"capabilities:\n  exec: { commands: [strings, analyzeHeadless, decompyle3, Crack, nothere] }\npublish:\n  include: [agent.yaml, AGENT.md]\n")
	ws := workspaceFor(t, m, nil)
	pol := testPolicy()
	pol.Exec.Packages = nil
	compileWith := func(data *unsafeexec.Data) (*spec.Plan, error) {
		return Compile(Inputs{Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
			Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test", Unsafe: data}, t.TempDir())
	}
	var refusal *PolicyRefusal
	if _, err := compileWith(nil); !errors.As(err, &refusal) || !strings.Contains(err.Error(), "exec.packages") {
		t.Fatalf("the default compile did not refuse the uncatalogued commands: %v", err)
	}
	plan, err := compileWith(testUnsafeData(t))
	if err != nil {
		t.Fatalf("Unsafe Mode refused: %v", err)
	}
	img := plan.ExecImage
	if got := strings.Join(img.Packages, ","); got != "binutils,ghidra" {
		t.Fatalf("packages = %q", got)
	}
	enf := plan.Enforcement["exec"]
	for _, want := range []string{
		"UNSAFE MODE",
		testUnsafeBase,
		"strings ← apt",
		"analyzeHeadless ← link ghidra:/usr/share/ghidra/support/analyzeHeadless",
		"decompyle3 ← pip decompyle3==3.9.3",
		"this machine's exec catalogue was not consulted",
		"not minimized",
		"image's own PATH",
		"not available in Unsafe Mode's image: Crack (several packages ship it: crack, crack-md5); nothere",
	} {
		if !strings.Contains(enf, want) {
			t.Errorf("the exec line lacks %q:\n%s", want, enf)
		}
	}
	if !strings.HasPrefix(img.Tag, "agentry-runner-exec:test-u") {
		t.Fatalf("tag = %q", img.Tag)
	}
}
