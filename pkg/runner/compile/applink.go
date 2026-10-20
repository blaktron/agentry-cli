package compile

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// appLinkEntry is a persistent agent's app link in the gateway's catalog:
// the runner image's applink role for the agent's tool set, with no network
// and one bind, the supervisor's socket, as the person's uid, whose socket
// it is.
func appLinkEntry(set, runnerImage string) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode("Agentry's app link: the application that runs this agent (generated per run)"))
	e.set("title", strNode("Agentry"))
	e.set("type", strNode("server"))
	e.set("image", strNode(runnerImage))
	e.set("command", strListNode([]string{"applink", "--set", set}))
	e.set("user", strNode(spec.PHAppLinkUser))
	e.set("longLived", boolNode(true))
	e.set("volumes", strListNode([]string{spec.PHAppLink + ":" + applink.SocketMount + ":rw"}))
	e.set("disableNetwork", boolNode(true))
	return e.node()
}

// appLinkEnforcement is the receipt's line for the app link.
func appLinkEnforcement(set string) string {
	return "the app link (" + applink.ServerName + ", the runner image, its own container) has no network and one bind, the supervisor's socket, by its exact path; it lists " +
		strings.Join(applink.Tools(set), ", ") + " and forwards each call to the application, which answers it with its own operations and its own consent, every call logged in receipts/applink.log; no tool changes what the agent is given"
}
