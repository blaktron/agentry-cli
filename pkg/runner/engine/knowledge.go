// Knowledge attached to a run (plan knowledge.md §4.5, agentry-cli#317,
// DR-38). The compiled plan always mounts one read-only /knowledge volume
// on the tools container, as it mounts /inputs, and grants no knowledge
// tool: which packs a run reads is the person's choice at run time, like
// its input files, so it must not move the plan or its hash. The engine
// copies each attached pack into the run, verifies the copy against the
// digest its caller checked, and only then grants the three tools — in the
// run's own copies of the tools spec and the gateway's allowlist, as it
// stamps the owner — and names what it mounted in the receipt.
package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"gopkg.in/yaml.v3"
)

// AttachedPack is one knowledge pack attached to a run: where its plaintext
// tree is on the host, and what the caller verified it to be.
type AttachedPack struct {
	// Dir is the pack's directory. The engine copies it; it is never
	// mounted itself.
	Dir string
	// Name is the pack's name, and its mount /knowledge/<Name>.
	Name string
	// Digest is the tree digest the caller verified ("sha256:<hex>").
	Digest string
	Pages  int
	Bytes  int64
	Scan   knowledge.Scan
	// Shipped is set for a pack the release ships in its own archive (plan
	// knowledge.md §4.6): its trust is the release's, the attestation over
	// the archive and the scan of the version, not its own manifest's scan
	// record. Nil for a pack the person attaches.
	Shipped *ShippedFrom
}

// ShippedFrom is where a shipped pack comes from: its directory in the
// release's workspace, which the engine stages from the run's own copy of
// the workspace once CheckWorkspace has held that copy to the install
// record, and how the receipt names the release.
type ShippedFrom struct {
	// InWorkspace is the pack's slash path in the workspace,
	// "knowledge/<name>".
	InWorkspace string
	// Release names the release's bytes: "archive sha256:…" for an
	// installed release, "local package tree sha256:…" for --local.
	Release string
}

// AttachPack is the attachment of a pack knowledge.Load verified.
func AttachPack(p *knowledge.Pack) AttachedPack {
	m := p.Manifest
	return AttachedPack{Dir: p.Dir, Name: m.Name, Digest: m.SHA256, Pages: m.Pages, Bytes: m.Bytes, Scan: m.Scan}
}

// stageKnowledge copies each pack into dest/<name> — regular files only,
// within the pack cap — and verifies the copy: what the run reads is what
// the caller verified, whatever happened to the original in between. A
// shipped pack is copied from the run's own copy of the workspace, which
// CheckWorkspace verified first, so it is held both to the digest its
// caller pinned and to the install record.
func stageKnowledge(packs []AttachedPack, dest, workspace string) error {
	seen := map[string]bool{}
	for _, a := range packs {
		if err := knowledge.ValidName(a.Name); err != nil {
			return Classify(ExitCLI, err)
		}
		if seen[a.Name] {
			return Classify(ExitCLI, fmt.Errorf("the knowledge pack %s is attached twice", a.Name))
		}
		seen[a.Name] = true
		staged := filepath.Join(dest, a.Name)
		src := a.Dir
		if a.Shipped != nil {
			src = filepath.Join(workspace, filepath.FromSlash(a.Shipped.InWorkspace))
		}
		if err := knowledge.CopyPack(src, staged); err != nil {
			return Classify(ExitVerify, fmt.Errorf("knowledge pack %s changed after it was verified: %w", a.Name, err))
		}
		got, err := knowledge.Load(staged)
		if err != nil {
			return Classify(ExitVerify, fmt.Errorf("knowledge pack %s changed after it was verified: %w", a.Name, err))
		}
		if got.Manifest.Name != a.Name || got.Manifest.SHA256 != a.Digest {
			return Classify(ExitVerify, fmt.Errorf("knowledge pack %s changed after it was verified (verified %s, staged %s %s)",
				a.Name, a.Digest, got.Manifest.Name, got.Manifest.SHA256))
		}
	}
	return nil
}

// grantKnowledge writes the attached packs into the run's rendered tools
// spec and rebuilds the tools server's line of the gateway allowlist from
// the spec's own GrantedTools, so the two agree as the compiler's always
// do. It first holds the rendered catalog to the mount the tools read: a
// plan compiled without it would be granted tools that find nothing, and a
// receipt naming a mount the run never had. With no pack attached it
// changes nothing.
func grantKnowledge(gatewayDir, knowledgeDir string, packs []AttachedPack) error {
	if len(packs) == 0 {
		return nil
	}
	catalog, err := os.ReadFile(filepath.Join(gatewayDir, "catalog.yaml")) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	if !bytes.Contains(catalog, []byte(knowledgeDir+":"+spec.KnowledgeMount+":ro")) {
		return fmt.Errorf("this run's plan has no read-only %s mount for the tools container; remove the plan directory to recompile it with this CLI", spec.KnowledgeMount)
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	listed := make([]toolsrv.KnowledgePack, 0, len(packs))
	for _, a := range packs {
		listed = append(listed, toolsrv.KnowledgePack{Name: a.Name, Digest: a.Digest})
	}
	doc["knowledgeDir"] = spec.KnowledgeMount
	doc["knowledge"] = listed
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if err := os.WriteFile(specPath, append(out, '\n'), 0o644); err != nil { //nolint:gosec // read by the tools container
		return err
	}
	return setAllowlist(filepath.Join(gatewayDir, "tools.yaml"), ts.GrantedTools())
}

// setAllowlist replaces the tools server's entry in the gateway's tools.yaml
// and leaves every other server's entry, and the header, as compiled.
func setAllowlist(path string, tools []string) error {
	b, err := os.ReadFile(path) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a mapping", path)
	}
	m := doc.Content[0]
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, t := range tools {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: t})
	}
	found := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == spec.ToolServerName {
			m.Content[i+1] = seq
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s has no %s entry", path, spec.ToolServerName)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644) //nolint:gosec // read by the gateway
}

// withKnowledge adds the receipt's knowledge line when packs are attached:
// each pack's mount, digest, size and scan record, and what reaches it —
// the three tools, and, when the contract declares exec, the declared
// commands, which run in the same container (plan knowledge.md §4.5). A run
// with none carries no line.
func withKnowledge(enforced map[string]string, packs []AttachedPack, execCommands []string) map[string]string {
	if len(packs) == 0 {
		return enforced
	}
	out := make(map[string]string, len(enforced)+1)
	for k, v := range enforced {
		out[k] = v
	}
	parts := make([]string, 0, len(packs))
	for _, a := range packs {
		if a.Shipped != nil {
			// The pack is part of the release: the attestation line says
			// what covers the archive and whether it was scanned.
			parts = append(parts, fmt.Sprintf("%s shipped with the release (%s) mounted read-only at %s/%s (%s, %s, %d bytes)",
				a.Name, a.Shipped.Release, spec.KnowledgeMount, a.Name, a.Digest, knowledge.CountPages(a.Pages), a.Bytes))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s mounted read-only at %s/%s (%s, %s, %d bytes, scan: %s)",
			a.Name, spec.KnowledgeMount, a.Name, a.Digest, knowledge.CountPages(a.Pages), a.Bytes, a.Scan.Describe()))
	}
	line := strings.Join(parts, "; ") + "; tools " + toolsrv.ToolKnowledgeList + ", " +
		toolsrv.ToolKnowledgeSearch + " and " + toolsrv.ToolKnowledgeRead + ", on the tools container only"
	if len(execCommands) > 0 {
		line += "; the declared exec commands run in that container and can read it too, as they can " + spec.InputsMount
	}
	out["knowledge"] = line
	return out
}

// withDecoded adds the receipt's decode line when the caller decoded the
// inputs before the run (pkg/runner/decode): the mechanism, the versions
// and what happened to the files. A run whose inputs were not decoded has
// no such line.
func withDecoded(enforced map[string]string, d *decode.Result) map[string]string {
	if d == nil {
		return enforced
	}
	out := make(map[string]string, len(enforced)+1)
	for k, v := range enforced {
		out[k] = v
	}
	out["decode"] = d.Line()
	return out
}

// grantedTools is the tools server's grant for this run: the GrantedTools
// of the run's own rendered tools spec, after every run-time grant
// (knowledge, the voice, web, review), the same list setAllowlist gives
// the gateway. A session's driver permits exactly these, never the
// compiled plan's Tools, which no run-time attachment moves
// (agentry-cli#576).
func grantedTools(gatewayDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(gatewayDir, "toolspec.json")) //nolint:gosec // our own rendered config
	if err != nil {
		return nil, err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return nil, fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	return ts.GrantedTools(), nil
}
