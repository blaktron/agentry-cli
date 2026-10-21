package engine

import (
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// StageSignIn is the step of a start that readies the model's sign-in: a
// subscription's sign-in (Claude, ChatGPT, Copilot) taken from the store
// and renewed when it has to be. The other steps reuse the receipt's names
// (receipt.Stage*).
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
//	agent        the harness started; then, when it is given the stored
//	             conversation to continue, agent again with Detail
//	             "resume" while it loads it (one that cannot opens a new
//	             conversation, and the envelope says why)
type Stage struct {
	// Name is the step: one of the receipt's stage names, or StageSignIn.
	Name string `json:"name"`
	// Detail qualifies the step: StageDetailResume on the agent step that
	// continues the stored conversation; empty otherwise.
	Detail string `json:"detail,omitempty"`
	// Subject is what the step acts on, for a person: the harness's name
	// on the agent step ("Claude Code"; empty for the default harness,
	// which a person never chooses), the servers being installed on the
	// mcp-servers step; empty when the step has none.
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
// hook does nothing. A hook that panics is ignored: a start tears down what
// it made only on its own failure paths, and a client's report must never
// skip them and leave the gateway or the harness running.
func ReportStage(onStage func(Stage), st Stage) {
	if onStage == nil {
		return
	}
	defer func() { _ = recover() }()
	onStage(st)
}
