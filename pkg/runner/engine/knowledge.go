// Knowledge attached to a run (plan knowledge.md §4.5, agentry-cli#317,
// DR-36). The compiled plan always mounts one read-only /knowledge volume
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
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"gopkg.in/yaml.v3"
)

// KnowledgeMount is one knowledge pack attached to a run: where its
// plaintext tree is on the host, and what the caller verified it to be.
type KnowledgeMount struct {
	// Dir is the pack's directory. The engine copies it; it is never
	// mounted itself.
	Dir string
	// Name is the pack's name, and its mount /knowledge/<Name>.
	Name string
	// Digest is the tree digest the caller verified ("sha256:<hex>").
	Digest string
	Pages  int
	Bytes  int64
	// Scan is the pack's scan record as a person reads it.
	Scan string
}

// Attach is the mount for a pack knowledge.Load verified.
func Attach(p *knowledge.Pack) KnowledgeMount {
	m := p.Manifest
	return KnowledgeMount{Dir: p.Dir, Name: m.Name, Digest: m.SHA256, Pages: m.Pages, Bytes: m.Bytes, Scan: m.Scan.Describe()}
}

// stageKnowledge copies each pack into dest/<name> and verifies the copy:
// what the run reads is what the caller verified, whatever happened to the
// original in between.
func stageKnowledge(mounts []KnowledgeMount, dest string) error {
	seen := map[string]bool{}
	for _, m := range mounts {
		if err := knowledge.ValidName(m.Name); err != nil {
			return Classify(ExitCLI, err)
		}
		if seen[m.Name] {
			return Classify(ExitCLI, fmt.Errorf("the knowledge pack %s is attached twice", m.Name))
		}
		seen[m.Name] = true
		staged := filepath.Join(dest, m.Name)
		if err := copyTree(m.Dir, staged); err != nil {
			return Classify(ExitCLI, fmt.Errorf("stage knowledge pack %s: %w", m.Name, err))
		}
		got, err := knowledge.Load(staged)
		if err != nil {
			return Classify(ExitVerify, fmt.Errorf("knowledge pack %s changed after it was verified: %w", m.Name, err))
		}
		if got.Manifest.Name != m.Name || got.Manifest.SHA256 != m.Digest {
			return Classify(ExitVerify, fmt.Errorf("knowledge pack %s changed after it was verified (verified %s, staged %s %s)",
				m.Name, m.Digest, got.Manifest.Name, got.Manifest.SHA256))
		}
	}
	return nil
}

// grantKnowledge writes the attached packs into the run's rendered tools
// spec and rebuilds the tools server's line of the gateway allowlist from
// the spec's own GrantedTools, so the two agree as the compiler's always
// do. With no pack attached it changes nothing.
func grantKnowledge(gatewayDir string, mounts []KnowledgeMount) error {
	if len(mounts) == 0 {
		return nil
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
	packs := make([]toolsrv.KnowledgePack, 0, len(mounts))
	for _, m := range mounts {
		packs = append(packs, toolsrv.KnowledgePack{Name: m.Name, Digest: m.Digest})
	}
	doc["knowledgeDir"] = spec.KnowledgeMount
	doc["knowledge"] = packs
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
// each pack's mount, digest, size and scan record, and the tools that
// reach it (plan knowledge.md §4.5). A run with none carries no line.
func withKnowledge(enforced map[string]string, mounts []KnowledgeMount) map[string]string {
	if len(mounts) == 0 {
		return enforced
	}
	out := make(map[string]string, len(enforced)+1)
	for k, v := range enforced {
		out[k] = v
	}
	parts := make([]string, 0, len(mounts))
	for _, m := range mounts {
		pages := fmt.Sprintf("%d pages", m.Pages)
		if m.Pages == 1 {
			pages = "1 page"
		}
		parts = append(parts, fmt.Sprintf("%s mounted read-only at %s/%s (%s, %s, %d bytes, scan: %s)",
			m.Name, spec.KnowledgeMount, m.Name, m.Digest, pages, m.Bytes, m.Scan))
	}
	out["knowledge"] = strings.Join(parts, "; ") + "; tools " + toolsrv.ToolKnowledgeList + ", " +
		toolsrv.ToolKnowledgeSearch + " and " + toolsrv.ToolKnowledgeRead + ", on the tools container only"
	return out
}
