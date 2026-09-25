package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// makePack builds a valid pack through the format's own apply step, the
// way every pack a person attaches was made.
func makePack(t *testing.T, name string) *knowledge.Pack {
	t.Helper()
	out := t.TempDir()
	files := map[string]string{
		"wiki/topics/buckets.md": "---\ntitle: Buckets\ntype: topic\nsummary: Naming\nsources: [doc:guide.txt]\nupdated: 2026-09-25\n---\n\nA bucket name is globally unique [src: doc:guide.txt].\n",
		"sources/guide.txt":      "the guide\n",
	}
	changes, _ := json.Marshal(knowledge.Changes{Schema: knowledge.ChangesSchema, Write: []string{"sources/guide.txt", "wiki/topics/buckets.md"}})
	files[knowledge.ChangesFile] = string(changes)
	for rel, body := range files {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), name)
	if _, err := knowledge.Apply(dir, out, knowledge.ApplyOptions{Name: name, Now: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	p, err := knowledge.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKnowledgeIsNamedInTheReceipt(t *testing.T) {
	pack := makePack(t, "aws-docs")
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	p.Knowledge = []KnowledgeMount{Attach(pack)}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := pack.Manifest
	want := fmt.Sprintf("aws-docs mounted read-only at /knowledge/aws-docs (%s, 1 page, %d bytes, scan: not scanned)"+
		"; tools knowledge_list, knowledge_search and knowledge_read, on the tools container only", m.SHA256, m.Bytes)
	if got := env.CapabilitiesEnforced["knowledge"]; got != want {
		t.Fatalf("receipt knowledge line =\n%q\nwant\n%q", got, want)
	}

	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")
	// The pack was copied into the run, and it is that copy the tools
	// container mounts, read-only.
	if _, err := knowledge.Load(filepath.Join(runDir, "knowledge", "aws-docs")); err != nil {
		t.Fatalf("the run's copy of the pack: %v", err)
	}
	catalog, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), runDir+"/knowledge:/knowledge:ro") {
		t.Fatalf("rendered catalog lacks the ro knowledge mount:\n%s", catalog)
	}
	// The grant is the run's: the rendered toolspec names the pack and the
	// allowlist routes exactly what the server will register.
	var ts toolsrv.Spec
	raw, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.KnowledgeDir != "/knowledge" || len(ts.Knowledge) != 1 || ts.Knowledge[0] != (toolsrv.KnowledgePack{Name: "aws-docs", Digest: m.SHA256}) {
		t.Fatalf("rendered toolspec knowledge = %q %+v", ts.KnowledgeDir, ts.Knowledge)
	}
	tools, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wantTools := "agentry-tools:\n  - fs_read\n  - fs_list\n  - fs_write\n  - fetch\n  - run_command\n  - list_inputs\n  - read_input\n  - knowledge_list\n  - knowledge_search\n  - knowledge_read\n  - write_report\n"
	if !strings.HasSuffix(string(tools), wantTools) {
		t.Fatalf("rendered tools.yaml =\n%s\nwant it to end\n%s", tools, wantTools)
	}
	// §R11 invariant 2 holds with a pack attached: the agent container
	// mounts /plan and nothing else.
	agent, ok := f.FindRun("-agent")
	if !ok {
		t.Fatal("agent run not recorded")
	}
	if len(agent.Mounts) != 1 || agent.Mounts[0].Target != "/plan" || !agent.Mounts[0].ReadOnly {
		t.Fatalf("agent mounts = %+v", agent.Mounts)
	}

	t.Run("no pack, no line and no tools", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		p.KeepRunDir = true
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatal(err)
		}
		if line, ok := env.CapabilitiesEnforced["knowledge"]; ok {
			t.Fatalf("a run with no pack names knowledge: %q", line)
		}
		tools, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", "gateway", "tools.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(tools), "knowledge_") {
			t.Fatalf("a run with no pack routes the knowledge tools:\n%s", tools)
		}
	})
}

// TestAttachedKnowledgeWidensNothing: attaching a pack is the person's
// grant, as handing over an input is. The plan the run executes and its
// hash are the same with and without it, the installed plan files are not
// touched, and no enforcement line but the knowledge one differs.
func TestAttachedKnowledgeWidensNothing(t *testing.T) {
	run := func(t *testing.T, attach bool) (map[string]string, string, map[string]string) {
		t.Helper()
		p, f := fixture(t)
		happyFake(f)
		if attach {
			p.Knowledge = []KnowledgeMount{Attach(makePack(t, "aws-docs")), Attach(makePack(t, "voice"))}
		}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		files := map[string]string{}
		for _, rel := range []string{"gateway/toolspec.json", "gateway/tools.yaml", "gateway/catalog.yaml"} {
			b, err := os.ReadFile(filepath.Join(p.PlanDir, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			if sha256Hex(b) != p.Plan.Files[rel] {
				t.Fatalf("installed %s changed under the run", rel)
			}
			files[rel] = p.Plan.Files[rel]
		}
		return maps.Clone(env.CapabilitiesEnforced), env.Receipts.RunPlanSHA256, files
	}
	without, planWithout, filesWithout := run(t, false)
	with, planWith, filesWith := run(t, true)
	if planWith != planWithout {
		t.Fatalf("attaching knowledge moved the run plan's hash: %s → %s", planWithout, planWith)
	}
	if !maps.Equal(filesWith, filesWithout) {
		t.Fatalf("attaching knowledge moved the compiled files: %v → %v", filesWithout, filesWith)
	}
	if _, ok := with["knowledge"]; !ok {
		t.Fatal("the attached run's receipt does not name its knowledge")
	}
	delete(with, "knowledge")
	if !maps.Equal(with, without) {
		for k := range with {
			if with[k] != without[k] {
				t.Errorf("enforcement line %q differs:\n  without: %q\n  with:    %q", k, without[k], with[k])
			}
		}
		t.Fatal("attaching knowledge changed an enforcement line besides its own")
	}
}

func TestKnowledgeChangedAfterVerificationIsRefused(t *testing.T) {
	pack := makePack(t, "aws-docs")
	mount := Attach(pack)
	// The pack changes between the caller's check and the run's copy.
	if err := os.WriteFile(filepath.Join(pack.Dir, "sources", "guide.txt"), []byte("not the guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, f := fixture(t)
	happyFake(f)
	p.Knowledge = []KnowledgeMount{mount}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitVerify || !strings.Contains(err.Error(), "aws-docs") {
		t.Fatalf("want exit %d naming the pack, got %v", ExitVerify, err)
	}
	if _, ran := f.FindRun("-agent"); ran {
		t.Fatal("the agent ran on a pack that changed after it was verified")
	}
}

func TestKnowledgeTwoPacksOneNameRefused(t *testing.T) {
	a, b := makePack(t, "aws-docs"), makePack(t, "aws-docs")
	p, f := fixture(t)
	p.Knowledge = []KnowledgeMount{Attach(a), Attach(b)}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("want exit %d, got %v", ExitCLI, err)
	}
}
