// YAML node construction for the generated gateway files. Built on
// yaml.Node rather than Go maps for one reason: plan bytes must be
// deterministic, and explicit node trees make key order a decision, not an
// iteration-order accident.
package compile

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// yamlMap accumulates a mapping node in insertion order.
type yamlMap struct {
	content []*yaml.Node
}

// set appends key: value. Duplicate keys are a programmer error the YAML
// encoder would happily serialize, so callers keep keys unique.
func (m *yamlMap) set(key string, value *yaml.Node) {
	m.content = append(m.content, strNode(key), value)
}

// node returns the mapping node.
func (m *yamlMap) node() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: m.content}
}

// strNode is a string scalar (explicitly tagged so "" and "true" stay
// strings).
func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// boolNode is a boolean scalar.
func boolNode(b bool) *yaml.Node {
	v := "false"
	if b {
		v = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
}

// strListNode is a sequence of string scalars.
func strListNode(items []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, it := range items {
		seq.Content = append(seq.Content, strNode(it))
	}
	return seq
}

// seqNode is a sequence of arbitrary nodes.
func seqNode(items []*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
}

// marshalYAML renders a node with a leading comment header.
func marshalYAML(header string, node *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("render yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render yaml: %w", err)
	}
	return buf.Bytes(), nil
}
