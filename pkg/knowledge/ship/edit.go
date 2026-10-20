package ship

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// Edit is a change to a package's agent.yaml and what it did, one plain
// sentence each.
type Edit struct {
	YAML []byte
	Did  []string
}

// AddShip puts the pack's name in agent.yaml's knowledge.ships and the
// pattern that puts knowledge/ in the archive in publish.include, when
// they are not there. The rest of the file is kept as written.
func AddShip(agentYAML []byte, name string) (Edit, error) {
	root, doc, err := parseRoot(agentYAML)
	if err != nil {
		return Edit{}, err
	}
	if v := scalar(root, "schemaVersion"); v != "" && v[0] != '2' {
		return Edit{}, fmt.Errorf("agent.yaml is schemaVersion %s: knowledge.ships is a schemaVersion 2 field", v)
	}
	var did []string
	k := child(root, "knowledge")
	if k == nil {
		k = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, str("knowledge"), k)
	}
	if k.Kind != yaml.MappingNode {
		return Edit{}, errors.New("agent.yaml: knowledge is not a mapping")
	}
	ships := child(k, "ships")
	if ships == nil {
		ships = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		k.Content = append(k.Content, str("ships"), ships)
	}
	if ships.Kind != yaml.SequenceNode {
		return Edit{}, errors.New("agent.yaml: knowledge.ships is not a list")
	}
	if !slices.ContainsFunc(ships.Content, func(n *yaml.Node) bool { return shipName(n) == name }) {
		ships.Content = append(ships.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{str("name"), str(name)}})
		did = append(did, "added "+name+" to knowledge.ships")
	}
	inc, err := includeList(root)
	if err != nil {
		return Edit{}, err
	}
	if !Covers(listStrings(inc), name) {
		inc.Content = append(inc.Content, str(IncludeAll))
		did = append(did, "added "+IncludeAll+" to publish.include")
	}
	return encode(&doc, did)
}

// RemoveShip takes the pack's name out of knowledge.ships. A block left
// with nothing to say goes with it, and so does the knowledge/** pattern
// once no pack is shipped.
func RemoveShip(agentYAML []byte, name string) (Edit, error) {
	root, doc, err := parseRoot(agentYAML)
	if err != nil {
		return Edit{}, err
	}
	var did []string
	if k := child(root, "knowledge"); k != nil && k.Kind == yaml.MappingNode {
		if ships := child(k, "ships"); ships != nil && ships.Kind == yaml.SequenceNode {
			n := len(ships.Content)
			ships.Content = slices.DeleteFunc(ships.Content, func(n *yaml.Node) bool { return shipName(n) == name })
			if len(ships.Content) < n {
				did = append(did, "removed "+name+" from knowledge.ships")
			}
			if len(ships.Content) == 0 {
				drop(k, "ships")
			}
		}
		if len(k.Content) == 0 {
			drop(root, "knowledge")
			did = append(did, "removed the empty knowledge block")
		}
	}
	if len(shipsOf(root)) == 0 {
		if inc, err := includeList(root); err == nil {
			before := len(inc.Content)
			inc.Content = slices.DeleteFunc(inc.Content, func(n *yaml.Node) bool { return n.Value == IncludeAll })
			if len(inc.Content) < before {
				did = append(did, "removed "+IncludeAll+" from publish.include, since no pack is shipped")
			}
		}
	}
	return encode(&doc, did)
}

// Ships are the names agent.yaml's knowledge.ships lists.
func Ships(agentYAML []byte) ([]string, error) {
	root, _, err := parseRoot(agentYAML)
	if err != nil {
		return nil, err
	}
	return shipsOf(root), nil
}

func shipsOf(root *yaml.Node) []string {
	var out []string
	if k := child(root, "knowledge"); k != nil && k.Kind == yaml.MappingNode {
		if ships := child(k, "ships"); ships != nil && ships.Kind == yaml.SequenceNode {
			for _, n := range ships.Content {
				if s := shipName(n); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func parseRoot(b []byte) (*yaml.Node, yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, doc, fmt.Errorf("agent.yaml: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, doc, errors.New("agent.yaml is not a mapping")
	}
	return doc.Content[0], doc, nil
}

func includeList(root *yaml.Node) (*yaml.Node, error) {
	p := child(root, "publish")
	if p == nil || p.Kind != yaml.MappingNode {
		return nil, errors.New("agent.yaml has no publish block: name what the release holds in publish.include first")
	}
	inc := child(p, "include")
	if inc == nil || inc.Kind != yaml.SequenceNode {
		return nil, errors.New("agent.yaml: publish.include is not a list")
	}
	return inc, nil
}

func listStrings(l *yaml.Node) []string {
	out := make([]string, 0, len(l.Content))
	for _, n := range l.Content {
		out = append(out, n.Value)
	}
	return out
}

func encode(doc *yaml.Node, did []string) (Edit, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return Edit{}, err
	}
	if err := enc.Close(); err != nil {
		return Edit{}, err
	}
	return Edit{YAML: b.Bytes(), Did: did}, nil
}

func child(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func drop(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

func scalar(m *yaml.Node, key string) string {
	if v := child(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func shipName(n *yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		return scalar(n, "name")
	}
	return ""
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
