package engine

import (
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// StageSignIn is the step of a start that readies the model's sign-in: a
// connected account's token renewed, a subscription's sign-in taken from
// the store. The other steps reuse the receipt's names (receipt.Stage*).
const StageSignIn = "sign-in"

// StageDetailResume is the agent step's detail when the harness is
// continuing the stored conversation.
const StageDetailResume = "resume"

// Stage is a step a session's start has reached, reported through an
// OnStage hook so a client can say what the start is doing while it takes
// minutes (an image build, a server install, a harness's first start).
// A start reports, in order and each when it is reached:
//
//	compile      the release materialized and the plan compiled
//	sign-in      the model's sign-in readied (only when the plan has one)
//	images       the runner, gateway and exec images readied
//	mcp-servers  package servers installed (only when one is not yet)
//	prepare      the session's folders, volumes and secrets prepared
//	infra        the proxy, the gateway and the docker filter started
//	agent        the harness started; then, when it continues the stored
//	             conversation, agent again with Detail "resume"
type Stage struct {
	// Name is the step: one of the receipt's stage names, or StageSignIn.
	Name string `json:"name"`
	// Detail qualifies the step: StageDetailResume on the agent step that
	// continues the stored conversation; empty otherwise.
	Detail string `json:"detail,omitempty"`
	// Subject is what the step acts on, for a person: the harness's name
	// on the agent step ("Claude Code"), the servers being installed on
	// the mcp-servers step; empty when the step has none.
	Subject string `json:"subject,omitempty"`
}

// Words is the stage as a person is shown it, the same in every client:
// "Preparing", "Signing in to your model", "Getting images ready",
// "Installing <servers>", "Starting the gateway", "Starting <harness>",
// "Picking up the conversation".
func (s Stage) Words() string {
	switch s.Name {
	case receipt.StageCompile, receipt.StagePrepare:
		return "Preparing"
	case StageSignIn:
		return "Signing in to your model"
	case receipt.StageImages:
		return "Getting images ready"
	case receipt.StageMCPServers:
		if s.Subject == "" {
			return "Installing your servers"
		}
		return "Installing " + s.Subject
	case receipt.StageInfra:
		return "Starting the gateway"
	case receipt.StageAgent:
		if s.Detail == StageDetailResume {
			return "Picking up the conversation"
		}
		if s.Subject == "" {
			return "Starting the agent"
		}
		return "Starting " + s.Subject
	}
	if s.Name == "" {
		return "Starting"
	}
	// A step this build does not name for a person is shown by its name.
	return strings.ToUpper(s.Name[:1]) + s.Name[1:]
}

// ReportStage calls onStage with the stage, when there is a hook; a nil
// hook does nothing.
func ReportStage(onStage func(Stage), st Stage) {
	if onStage != nil {
		onStage(st)
	}
}

// harnessTitle is the harness an adapter runs, as a person knows it.
func harnessTitle(adapterName string) string {
	switch adapterName {
	case adapter.ClaudeAdapterName, adapter.ClaudeImageAdapterName:
		return "Claude Code"
	case adapter.CodexAdapterName, adapter.CodexImageAdapterName:
		return "Codex"
	case adapter.CopilotImageAdapterName:
		return "Copilot CLI"
	}
	return adapterName
}
