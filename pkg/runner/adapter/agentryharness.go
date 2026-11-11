package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The agentry-harness adapter runs the runner's own harness: the `harness`
// role of the runner image the CLI builds on the machine from its own
// source. The adapter and the harness compile one contract (package proto),
// so the configuration written here is the one read there and each event
// read here is one written there.
//
// The harness has no tool of its own. Its configuration names one tool
// source, the run's gateway, and decoding refuses any field beyond the
// contract's, so there is nothing a plan could add.

// AgentryHarnessAdapterName is the registry key and --adapter value.
const AgentryHarnessAdapterName = "agentry-harness"

const (
	// agentryHarnessConfig is the harness's configuration in the plan.
	agentryHarnessConfig = "harness.json"
	// agentryHarnessUser is the user the harness runs as: the runner
	// image's other roles run as root, the harness as nobody.
	agentryHarnessUser = "65534:65534"
	// agentryHarnessKeyEnv is the variable the harness reads the model's
	// key from in its container. The policy's variable is where the value
	// comes from on the host.
	agentryHarnessKeyEnv = "AGENTRY_MODEL_KEY"
	// agentryHarnessMaxOutput is the output asked of one request when the
	// policy names none.
	agentryHarnessMaxOutput = 32000
)

func init() { Register(&agentryHarnessAdapter{}) }

type agentryHarnessAdapter struct{}

func (a *agentryHarnessAdapter) Name() string { return AgentryHarnessAdapterName }

func (a *agentryHarnessAdapter) Compile(in Input) (Result, error) {
	switch in.Mode {
	case manifest.ModeAutonomous:
	case manifest.ModeInteractive:
		return Result{}, &Refusal{Reason: "the " + AgentryHarnessAdapterName + " adapter runs one-shot runs only; an interactive session or a persistent agent needs another adapter"}
	default:
		return Result{}, fmt.Errorf("%s adapter: unknown run mode %q", AgentryHarnessAdapterName, in.Mode)
	}
	if in.Model.Provider != policy.ProviderAnthropic {
		return Result{}, &Refusal{Reason: fmt.Sprintf("the %s adapter reaches Anthropic models only; the policy's model provider is %q", AgentryHarnessAdapterName, in.Model.Provider)}
	}
	if policy.KeyKind(in.Harness, in.Model) == policy.KeyKindSubscription {
		return Result{}, &Refusal{Reason: "apiKeyKind subscription: " + policy.SubscriptionTokenSentence}
	}

	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)
	maxOutput := int(in.Model.MaxOutputTokens)
	if maxOutput <= 0 {
		maxOutput = agentryHarnessMaxOutput
	}
	cfg := proto.Config{
		Version: proto.Version,
		Model: proto.Model{
			API:             proto.APIAnthropic,
			Name:            in.Model.Name,
			BaseURL:         strings.TrimRight(in.Model.BaseURL, "/"),
			KeyEnv:          agentryHarnessKeyEnv,
			MaxOutputTokens: maxOutput,
			ContextWindow:   int(in.Model.ContextWindow),
		},
		System:     instruction,
		Gateway:    proto.Gateway{URL: in.GatewayURL, TokenEnv: spec.GatewayTokenEnv},
		SessionDir: spec.SessionMount,
	}
	file, err := cfg.Encode()
	if err != nil {
		return Result{}, fmt.Errorf("%s adapter: %w", AgentryHarnessAdapterName, err)
	}

	agent := spec.ContainerSpec{
		Image: in.HarnessImage,
		Argv: []string{"harness", "run",
			"--config", spec.PlanMount + "/" + agentryHarnessConfig,
			"--conversation", spec.PHConversation},
		User: agentryHarnessUser,
		Env: []spec.EnvVar{
			{Name: spec.GatewayTokenEnv, FromRunToken: true},
			{
				Name:         agentryHarnessKeyEnv,
				FromHostEnv:  policy.KeyEnv(in.Harness, in.Model),
				FromKeyring:  keyringAccount(in),
				ExpectedKind: anthropicKeyExpectation(in),
			},
			// The proxy is the agent container's only route out; NO_PROXY
			// keeps the gateway's traffic direct on the run network.
			{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
			{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
			{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
		},
		Mounts: []spec.Mount{
			{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true},
			// The conversation's record, the container's one writable
			// mount: a volume of the run's own, removed with it.
			{Source: spec.PHSession, Target: spec.SessionMount},
		},
		StdinTask: true,
		// The harness writes its record and nothing else.
		ReadOnlyRoot: true,
		Tmpfs:        []string{"/tmp:mode=1777"},
	}
	return Result{
		Files:      map[string][]byte{agentryHarnessConfig: file},
		Agent:      agent,
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: resultKind,
		// A harness that ends on a passing model failure is started again
		// on the conversation it recorded.
		Resume: &spec.ResumeSpec{SessionDB: spec.SessionMount, StoreOwner: agentryHarnessUser},
	}, nil
}

// Events maps one line of the harness's output. The line is a proto.Event;
// anything else is kept as other, never an error.
func (a *agentryHarnessAdapter) Events(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	raw := rawCopy([]byte(trimmed))
	var ev proto.Event
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil || ev.Type == "" {
		return []Event{{Kind: EventOther, Raw: raw}}
	}
	switch ev.Type {
	case proto.TypeText:
		return []Event{{Kind: EventAssistantText, Text: ev.Text, Delta: true, Raw: raw}}
	case proto.TypeReasoning:
		return []Event{{Kind: EventReasoning, Raw: raw}}
	case proto.TypeToolCall:
		return []Event{{Kind: EventToolCall, Text: ev.Tool, Tool: ev.Tool, CallID: ev.CallID, Raw: raw}}
	case proto.TypeToolResult:
		return []Event{{Kind: EventToolResult, Text: ev.Text, Tool: ev.Tool, CallID: ev.CallID, Failed: ev.Failed, Redacted: ev.Unchanged, Raw: raw}}
	case proto.TypeUsage:
		if ev.Usage == nil {
			return []Event{{Kind: EventOther, Raw: raw}}
		}
		u := ev.Usage
		return []Event{{
			Kind: EventUsage,
			// The request's whole input, cached or not, as every other
			// container harness reports it.
			InputTokens:   u.Input + u.CacheRead + u.CacheWrite,
			OutputTokens:  u.Output,
			ContextTokens: u.ContextTokens,
			ContextLimit:  u.ContextLimit,
			Raw:           raw,
		}}
	case proto.TypeError:
		text := agentryHarnessErrorText(ev)
		if ev.Class == proto.ClassCredits {
			return []Event{{Kind: EventOutOfCredits, Text: text, Raw: raw}}
		}
		return []Event{{Kind: EventError, Text: text, Raw: raw}}
	case proto.TypeTurnEnd:
		switch ev.Reason {
		case proto.EndAnswerCut:
			return []Event{{Kind: EventAnswerCut, Raw: raw}}
		case proto.EndMaxSteps:
			return []Event{{Kind: EventError, Text: "the harness reached its limit of model requests for one turn", Raw: raw}}
		}
	}
	return []Event{{Kind: EventOther, Raw: raw}}
}

// agentryHarnessErrorText is an error event as one line: the server's own
// words, with its HTTP status when it answered with one. The engine reads
// these words to tell a server at capacity or a passing failure from one
// that ends the run.
func agentryHarnessErrorText(ev proto.Event) string {
	text := strings.TrimSpace(ev.Text)
	if ev.Status != 0 {
		text = fmt.Sprintf("HTTP %d: %s", ev.Status, text)
	}
	switch ev.Class {
	case proto.ClassCapacity:
		return "the model's server is overloaded (" + text + ")"
	case proto.ClassTransport:
		return "the connection to the model was closed unexpectedly (" + text + ")"
	case proto.ClassGateway:
		return "the tool gateway failed (" + text + ")"
	}
	return text
}

func (a *agentryHarnessAdapter) Session(context.Context, io.ReadWriteCloser, SessionOptions) (SessionDriver, error) {
	return nil, ErrNotSupported
}
