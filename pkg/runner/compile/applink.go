package compile

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// appLinkEntry is a persistent agent's app link in the gateway's catalog:
// the runner image's applink role for the agent's tool set, with no network
// and one mount, the supervisor's socket or, where the engine's daemon runs
// in a VM, the run's relay volume (spec.PHAppLinkMount), as the person's
// uid, whose socket it is.
func appLinkEntry(set, runnerImage string) *yaml.Node {
	e := yamlMap{}
	e.set("description", strNode("Agentry's app link: the application that runs this agent (generated per run)"))
	e.set("title", strNode("Agentry"))
	e.set("type", strNode("server"))
	e.set("image", strNode(runnerImage))
	e.set("command", strListNode(AppLinkCommand(set)))
	e.set("user", strNode(spec.PHAppLinkUser))
	e.set("longLived", boolNode(true))
	e.set("volumes", strListNode([]string{spec.PHAppLinkMount}))
	e.set("disableNetwork", boolNode(true))
	return e.node()
}

// AppLinkCommand is the app link's command in its catalog entry, which the
// run's docker filter knows its container by.
func AppLinkCommand(set string) []string { return []string{"applink", "--set", set} }

// appLinkEnforcement is the receipt's line for the app link: what reaches
// the socket, and who answers it.
func appLinkEnforcement(set string) string {
	line := "the app link (" + applink.ServerName + ", the runner image, its own container) has no network and one mount, through which it reaches a socket of the agent's supervisor apart from its owner socket, which takes one tool call a connection and nothing else (persistent.appLink.transport says how); it lists " +
		strings.Join(applink.Tools(set), ", ")
	if set == applink.SetAssistant {
		return line + ": the supervisor answers " + applink.SelfConfigure + " by saving the agent's name and personality and nothing else, and the task tools by keeping the agent's own tasks; the client that runs the agent answers each other call, and a call it does not answer is refused; every call is logged in receipts/applink.log"
	}
	return line + ": the client that runs the agent answers each call, and a call it does not answer is refused; every call is logged in receipts/applink.log"
}
