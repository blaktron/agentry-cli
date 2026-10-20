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

// appLinkEnforcement is the receipt's line for the app link: what reaches
// the socket, and who answers it.
func appLinkEnforcement(set string) string {
	line := "the app link (" + applink.ServerName + ", the runner image, its own container) has no network and one bind, a socket of the agent's supervisor apart from its owner socket, by its exact path, which takes one tool call a connection and nothing else; it lists " +
		strings.Join(applink.Tools(set), ", ")
	if set == applink.SetAssistant {
		return line + ": the supervisor answers " + applink.SelfConfigure + " by saving the agent's name and personality and nothing else; every call is logged in receipts/applink.log"
	}
	return line + ": the client that runs the agent answers each call, and a call it does not answer is refused; every call is logged in receipts/applink.log"
}
