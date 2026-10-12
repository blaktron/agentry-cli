package compile

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// The host link (plan runtime-hosts.md §4, D3; agentry-cli#861). A release
// that declares target hosts gets one more server in the gateway's catalog,
// the host link, on its own image: the runner's hostlink role behind one
// relay a target host, holding each host's sign-in in files bound into it
// alone. Its run-time values are placeholders (the relay targets, the user
// it runs as), so the plan is the release's and a run without a target
// host compiles exactly as before.

// hostSpecFile is the plan's gateway/hostspec.json.
const hostSpecFile = "gateway/hostspec.json"

// hostLinkFiles writes hostspec.json: the hosts and the harness's result
// limit, which the host link pages within as the tools server does.
func hostLinkFiles(files map[string][]byte, hosts []string, budget adapter.Budget) error {
	b, err := json.MarshalIndent(hostlink.Spec{
		SpecVersion: hostlink.SpecVersion, Hosts: hosts,
		MaxResultBytes: budget.Bytes, MaxResultLines: budget.Lines,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hostspec: %w", err)
	}
	files[hostSpecFile] = append(b, '\n')
	return nil
}

// hostFolder is a run folder of the host link's, in plan placeholders.
func hostFolder(name string) string { return spec.PHCfg + "/" + name }

// hostLinkEntry is the host link's catalog entry. It reaches each target
// host through one L4 relay to the address and port the person's machine
// resolved (allowHosts, a placeholder a host), and no other network; the
// gateway links each relay under that address, which the host link asks
// docker's DNS for (hostlink/relay.go). It runs as the person's uid, with a
// passwd line for it (ssh refuses a uid it cannot name, DR-91), so the
// sign-in files stay theirs, 0600 in a 0700 folder.
func hostLinkEntry(hosts []string, image string) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode("Agentry's host link: the run's target hosts, over SSH the runner makes (generated per run)"))
	e.set("title", strNode("Target hosts"))
	e.set("type", strNode("server"))
	e.set("image", strNode(image))
	e.set("command", strListNode([]string{"hostlink"}))
	e.set("user", strNode(spec.PHHostLinkUser))
	e.set("longLived", boolNode(true))
	e.set("volumes", strListNode([]string{
		spec.PHCfg + "/" + hostSpecFile + ":" + hostlink.SpecMount + ":ro",
		hostFolder(spec.HostsDir) + ":" + hostlink.HostsMount + ":ro",
		hostFolder(spec.HostsDir) + "/" + hostlink.PasswdFile + ":/etc/passwd:ro",
		hostFolder(spec.HostLogDir) + ":" + hostlink.LogMount + ":rw",
		hostFolder(spec.HostFilesDir) + ":" + hostlink.FilesMount + ":rw",
	}))
	allow := make([]string, 0, len(hosts))
	for _, id := range hosts {
		allow = append(allow, spec.PHTargetHost(id)+"/tcp")
	}
	e.set("allowHosts", strListNode(allow))
	return e.node()
}

// hostLinkWritable are the host link's writable folders, each by its exact
// path, as the gateway's bind check takes them.
func hostLinkWritable() []string {
	return []string{hostFolder(spec.HostLogDir), hostFolder(spec.HostFilesDir)}
}

// hostFilesVolume is the shared folder in the tools server, inside the
// agent's scratch folder, where the scratch tools read what host_get
// copied and write what host_put will.
func hostFilesVolume() string {
	return hostFolder(spec.HostFilesDir) + ":" + hostlink.FilesPath + ":rw"
}

// targetHostsEnforcement is the plan's targetHosts line: what the run does
// with a declared target host, before the run knows which.
func targetHostsEnforcement(hosts []string) string {
	return fmt.Sprintf("the target hosts %s, named by the person at the run's start, are reached only by the host link (%s, its own container) through one relay a host to the address and port the person's machine resolved; "+
		"its sign-in is in files bound into the host link alone, and the agent works the host through %s, every call logged; what the host prints reaches the agent unchanged, past the gateway's secret block and the harness's scrub, but for the run's own sign-in (D9, D19); a hosted run refuses a target host",
		strings.Join(hosts, ", "), hostlink.ServerName, strings.Join(hostlink.Tools, ", "))
}

// unchangedTools are the tools whose results a harness's own scrub leaves
// as they are, beyond the input tools: the host link's, where there is one.
func unchangedTools(caps spec.CapSummary) []string {
	if len(caps.TargetHosts) == 0 {
		return nil
	}
	return hostlink.Tools
}
