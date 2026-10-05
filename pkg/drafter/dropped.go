package drafter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// declared is one capability a draft's manifest declares, as keepFirst
// compares two answers: its kind in the notes' words, its value (a
// package name, a host, a command, a secret's name), and each path it is
// declared at (validate's form, "capabilities.mcpServers[0].egress[1]").
type declared struct {
	kind, value string
	paths       []string
}

// The kinds of declared capability a repair must not drop unnamed.
const (
	kindServer  = "the MCP server"
	kindHost    = "the egress host"
	kindCommand = "the command"
	kindSecret  = "the secret"
)

// declaredCapabilities is what a manifest declares that a repair turn must
// not drop without the report naming it (agentry-cli#633): each MCP
// server (by its package, whatever its version), each egress host (the
// QuickAgent's or a server's), each exec command and each secret (the
// QuickAgent's or a server's), in the manifest's order. Nil when the
// manifest does not parse, which validate reports.
func declaredCapabilities(raw json.RawMessage) []declared {
	var m struct {
		Capabilities struct {
			Network struct {
				Egress json.RawMessage `json:"egress"`
			} `json:"network"`
			Exec       json.RawMessage `json:"exec"`
			Secrets    []string        `json:"secrets"`
			MCPServers []struct {
				NPM     string   `json:"npm"`
				PyPI    string   `json:"pypi"`
				Secrets []string `json:"secrets"`
				Egress  []string `json:"egress"`
			} `json:"mcpServers"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []declared
	index := map[string]int{}
	add := func(kind, value, path string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		k := kind + "\x00" + value
		if i, ok := index[k]; ok {
			out[i].paths = append(out[i].paths, path)
			return
		}
		index[k] = len(out)
		out = append(out, declared{kind: kind, value: value, paths: []string{path}})
	}
	caps := m.Capabilities
	// egress "none" or "*" as a scalar is not a list: no host to drop by
	// name ("*" as a scalar is validate's to refuse).
	var hosts []string
	_ = json.Unmarshal(caps.Network.Egress, &hosts)
	for i, h := range hosts {
		add(kindHost, h, fmt.Sprintf("capabilities.network.egress[%d]", i))
	}
	var exec struct {
		Commands []string `json:"commands"`
	}
	_ = json.Unmarshal(caps.Exec, &exec)
	for i, c := range exec.Commands {
		add(kindCommand, c, fmt.Sprintf("capabilities.exec.commands[%d]", i))
	}
	for i, s := range caps.Secrets {
		add(kindSecret, s, fmt.Sprintf("capabilities.secrets[%d]", i))
	}
	for i, srv := range caps.MCPServers {
		p := fmt.Sprintf("capabilities.mcpServers[%d]", i)
		name := srv.NPM
		if name == "" {
			name = srv.PyPI
		}
		add(kindServer, name, p)
		for j, h := range srv.Egress {
			add(kindHost, h, fmt.Sprintf("%s.egress[%d]", p, j))
		}
		for j, s := range srv.Secrets {
			add(kindSecret, s, fmt.Sprintf("%s.secrets[%d]", p, j))
		}
	}
	return out
}

// droppedUnnamed is what the second answer of a repair no longer declares
// of what the first did, and the report that sent the first to its repair
// turn does not name, one line each ("the MCP server osv-mcp"): what the
// task needs, which the turn was told never to remove (repairFraming).
// A capability is named when a line of the report says its value, or a
// path it was declared at, or a path that holds it (a line on
// "capabilities.exec" names every command).
func droppedUnnamed(first, second *Draft, r RepairReport) []string {
	kept := map[string]bool{}
	for _, c := range declaredCapabilities(second.Manifest) {
		kept[c.kind+"\x00"+c.value] = true
	}
	report := append(append([]string{}, r.Rules...), r.Flagged...)
	var out []string
	for _, c := range declaredCapabilities(first.Manifest) {
		if kept[c.kind+"\x00"+c.value] || named(report, c) {
			continue
		}
		out = append(out, c.kind+" "+c.value)
	}
	return out
}

// reportPath is a manifest path as validate and the post-step's own rules
// write one in a report line.
var reportPath = regexp.MustCompile(`capabilities(?:\.[A-Za-z]+(?:\[\d+\])*)*`)

// named says a line of the report names the capability c.
func named(report []string, c declared) bool {
	for _, line := range report {
		if saysValue(line, c.value) {
			return true
		}
		for _, p := range reportPath.FindAllString(line, -1) {
			for _, at := range c.paths {
				if at == p || strings.HasPrefix(at, p+".") || strings.HasPrefix(at, p+"[") {
					return true
				}
			}
		}
	}
	return false
}

// saysValue says line holds value as a word of its own: not inside a
// longer name, host or command ("tar" is not said by "start").
func saysValue(line, value string) bool {
	inName := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.'
	}
	for from := 0; ; {
		i := strings.Index(line[from:], value)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(value)
		// A full stop after it ends a sentence, not a longer host.
		stop := end < len(line) && line[end] == '.' && (end+1 == len(line) || !inName(line[end+1]))
		if (i == 0 || !inName(line[i-1])) && (end == len(line) || !inName(line[end]) || stop) {
			return true
		}
		from = i + 1
	}
}
