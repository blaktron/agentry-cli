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
	// agentryHarnessMaxOutput is the output asked of one Anthropic request
	// when the policy names none.
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
	api, baseURL, err := agentryHarnessEndpoint(in.Model)
	if err != nil {
		return Result{}, err
	}
	if policy.KeyKind(in.Harness, in.Model) == policy.KeyKindSubscription {
		return Result{}, &Refusal{Reason: "apiKeyKind subscription: " + policy.SubscriptionTokenSentence}
	}

	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)
	model := proto.Model{
		API:             api,
		Name:            in.Model.Name,
		BaseURL:         baseURL,
		KeyEnv:          agentryHarnessKeyEnv,
		MaxOutputTokens: int(in.Model.MaxOutputTokens),
		ContextWindow:   int(in.Model.ContextWindow),
	}
	// The key: the run's credits session for the registry's own endpoint,
	// none for a server that takes none, else the variable the policy
	// names (or the keychain account beside it).
	var key *spec.EnvVar
	switch {
	case in.Model.Provider == policy.ProviderAgentry:
		key = &spec.EnvVar{Name: agentryHarnessKeyEnv, FromInference: true}
	case policy.TakesNoKey(in.Harness, in.Model):
		model.KeyEnv = ""
	default:
		key = &spec.EnvVar{Name: agentryHarnessKeyEnv, FromHostEnv: policy.KeyEnv(in.Harness, in.Model), FromKeyring: keyringAccount(in)}
		if in.Model.Provider == policy.ProviderAnthropic {
			key.ExpectedKind = anthropicKeyExpectation(in)
		}
	}
	if api == proto.APIAnthropic {
		// The API requires a limit on every request.
		if model.MaxOutputTokens <= 0 {
			model.MaxOutputTokens = agentryHarnessMaxOutput
		}
	} else {
		agentryHarnessModelSettings(&model, in.Model)
	}
	cfg := proto.Config{
		Version:    proto.Version,
		Model:      model,
		System:     instruction,
		Gateway:    proto.Gateway{URL: in.GatewayURL, TokenEnv: spec.GatewayTokenEnv},
		SessionDir: spec.SessionMount,
	}
	file, err := cfg.Encode()
	if err != nil {
		return Result{}, fmt.Errorf("%s adapter: %w", AgentryHarnessAdapterName, err)
	}

	env := []spec.EnvVar{{Name: spec.GatewayTokenEnv, FromRunToken: true}}
	if key != nil {
		env = append(env, *key)
	}
	env = append(env,
		// The proxy is the agent container's only route out; NO_PROXY
		// keeps the gateway's traffic direct on the run network.
		spec.EnvVar{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
		spec.EnvVar{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
		spec.EnvVar{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
	)
	agent := spec.ContainerSpec{
		Image: in.HarnessImage,
		Argv: []string{"harness", "run",
			"--config", spec.PlanMount + "/" + agentryHarnessConfig,
			"--conversation", spec.PHConversation},
		User: agentryHarnessUser,
		Env:  env,
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

// The OpenAI-compatible endpoints of the hosted routers, each the provider's
// own unless the policy names another.
var agentryHarnessBaseURLs = map[string]string{
	policy.ProviderOpenRouter:  "https://openrouter.ai/api/v1",
	policy.ProviderHuggingFace: "https://router.huggingface.co/v1",
	policy.ProviderPoe:         poeBaseURL,
	policy.ProviderVultr:       vultrBaseURL,
}

// agentryHarnessEndpoint is the wire API and address a policy's model is
// reached by, or the refusal of a provider the harness does not reach yet.
func agentryHarnessEndpoint(m policy.Model) (api, baseURL string, err error) {
	base := strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	switch {
	case m.Provider == policy.ProviderAnthropic:
		return proto.APIAnthropic, base, nil
	case m.OwnEndpoint(), m.Provider == policy.ProviderAgentry:
		// A server of the person's own, or the registry's model
		// endpoint: the address is the policy's.
		if base == "" {
			return "", "", &Refusal{Reason: fmt.Sprintf("the %s adapter needs model.baseUrl for the model provider %q", AgentryHarnessAdapterName, m.Provider)}
		}
		return proto.APIChat, base, nil
	}
	if known, ok := agentryHarnessBaseURLs[m.Provider]; ok {
		if base == "" {
			base = known
		}
		return proto.APIChat, base, nil
	}
	return "", "", &Refusal{Reason: fmt.Sprintf("the %s adapter does not reach the model provider %q yet", AgentryHarnessAdapterName, m.Provider)}
}

// agentryHarnessModelSettings fills a Chat Completions model's sampling,
// answer limit and reasoning settings from the model family's profile and
// the policy's own keys, the same ones every container harness is given.
func agentryHarnessModelSettings(out *proto.Model, m policy.Model) {
	sampling, opts := modelSettings(m)
	if sampling.MaxTokens > 0 {
		out.MaxOutputTokens = int(sampling.MaxTokens)
	}
	s := proto.Sampling{
		Temperature: sampling.Temperature, TopP: sampling.TopP, PresencePenalty: sampling.PresencePenalty,
		TopK: opts.TopK, MinP: opts.MinP, RepetitionPenalty: opts.RepetitionPenalty,
	}
	if s != (proto.Sampling{}) {
		out.Sampling = &s
	}
	if opts.ReplayReasoning != nil {
		out.ReplayReasoning = *opts.ReplayReasoning
	}
	if opts.ExtraBody != nil && opts.ExtraBody.ChatTemplateKwargs != nil {
		k := opts.ExtraBody.ChatTemplateKwargs
		kwargs := map[string]bool{}
		if k.EnableThinking != nil {
			kwargs["enable_thinking"] = *k.EnableThinking
		}
		if k.PreserveThinking != nil {
			kwargs["preserve_thinking"] = *k.PreserveThinking
		}
		if len(kwargs) > 0 {
			// Map keys are written in order, so the bytes are the same
			// for the same settings.
			out.ExtraBody, _ = json.Marshal(map[string]any{"chat_template_kwargs": kwargs})
		}
	}
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
	case proto.ClassOverflow:
		// Nothing shortens a conversation yet, so one the server refuses
		// as too long ends the run. The server's own wording is left to
		// the recorded event: the engine reads it as a failure to carry on
		// from, and carrying on would only be refused again.
		return "the model's server refused the conversation as too long for the model; its own words are in the run's recorded events"
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
