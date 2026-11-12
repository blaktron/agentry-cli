package adapter

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/modelfacts"
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
	// when neither the policy nor the catalogue names a limit.
	agentryHarnessMaxOutput = 32000
)

// What the harness does on a long run, as its configuration is given it.
const (
	// agentryHarnessRepeats is how many times in a row the same call is
	// made before the model is told, and agentryHarnessCycleRepeats how
	// many times the same sequence of two to agentryHarnessMaxPeriod calls.
	agentryHarnessRepeats      = 5
	agentryHarnessCycleRepeats = 3
	agentryHarnessMaxPeriod    = 4
	// agentryHarnessLengthRecoveries is how many replies cut off at the
	// output limit one turn goes on from.
	agentryHarnessLengthRecoveries = 2
	// A failed model request is sent again agentryHarnessRetries times,
	// after 2, 4 and 8 seconds, or after the wait the server asks for when
	// that is no longer than agentryHarnessRetryMax seconds.
	agentryHarnessRetries   = 3
	agentryHarnessRetryBase = 2
	agentryHarnessRetryMax  = 60
	// A reply that sends nothing for this long is taken as a broken
	// connection: five minutes on a hosted model, and half an hour on a
	// server of the person's own, which may read a long conversation for
	// minutes before its first token.
	agentryHarnessIdleHosted = 5 * 60
	agentryHarnessIdleOwn    = 30 * 60
)

// agentryHarnessLongRun fills the parts of the configuration that carry a
// long run: the result policy, the scrub, the answer to a repeated call,
// the recovery from a cut-off reply, the clearing of old results, the
// summary and the retries.
func agentryHarnessLongRun(cfg *proto.Config, in Input) {
	// One figure with the tools server: its pages fit the same budget, so
	// only a result that server did not page is ever cut.
	cfg.Results = &proto.Results{MaxBytes: int(PageBudget(AgentryHarnessAdapterName, in.Model).Bytes)}
	// The person's own inputs reach the model unchanged.
	cfg.Scrub = &proto.Scrub{Unchanged: append(slices.Clone(InputTools), in.UnchangedTools...)}
	cfg.Loop = &proto.Loop{Repeats: agentryHarnessRepeats, CycleRepeats: agentryHarnessCycleRepeats, MaxPeriod: agentryHarnessMaxPeriod}
	cfg.LengthRecoveries = agentryHarnessLengthRecoveries
	cfg.ColdClear = &proto.ColdClear{
		AfterSeconds: int(ColdAfter(AgentryHarnessAdapterName, in.Model.Provider).Seconds()),
		KeepTurns:    coldKeepTurns,
		KeepBrowser:  coldKeepBrowser,
		BrowserTools: in.BrowserTools,
		MinBytes:     coldMinBytes,
	}
	threshold, idle := hostedCompaction, agentryHarnessIdleHosted
	if in.Model.OwnEndpoint() {
		threshold, idle = ownEndpointCompaction, agentryHarnessIdleOwn
	}
	notes := spec.ChecklistForHarness
	if in.Persistent {
		notes = spec.AgentChecklistForHarness
	}
	cfg.Compaction = &proto.Compaction{Threshold: threshold, Checkpoint: checkpointText, Notes: notes}
	cfg.Retry = &proto.Retry{Attempts: agentryHarnessRetries, BaseSeconds: agentryHarnessRetryBase, MaxSeconds: agentryHarnessRetryMax, IdleSeconds: idle}
}

func init() { Register(&agentryHarnessAdapter{}) }

type agentryHarnessAdapter struct{}

func (a *agentryHarnessAdapter) Name() string { return AgentryHarnessAdapterName }

func (a *agentryHarnessAdapter) Compile(in Input) (Result, error) {
	switch in.Mode {
	case manifest.ModeAutonomous:
	case manifest.ModeInteractive:
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
	// What the catalogue knows about a hosted model fills what the policy
	// left out: the window, the output limit, the prices. A server of the
	// person's own and the registry's endpoint are not in it under the
	// policy's name, whatever the model is called.
	if !in.Model.OwnEndpoint() && in.Model.Provider != policy.ProviderAgentry {
		if facts, ok := modelfacts.Lookup(in.Model.Provider, in.Model.Name); ok {
			if model.ContextWindow <= 0 {
				model.ContextWindow = int(facts.ContextWindow)
			}
			if model.MaxOutputTokens <= 0 && api == proto.APIAnthropic {
				model.MaxOutputTokens = int(facts.MaxOutput)
			}
			if facts.Priced {
				model.Prices = &proto.Prices{Input: facts.Input, CacheRead: facts.CacheRead, CacheWrite: facts.CacheWrite, Output: facts.Output}
			}
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
	agentryHarnessLongRun(&cfg, in)
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
	// The conversation's record is the container's one writable mount: a
	// volume of the run's own, removed with it.
	store := spec.Mount{Source: spec.PHSession, Target: spec.SessionMount}
	agent := spec.ContainerSpec{
		Image:  in.HarnessImage,
		User:   agentryHarnessUser,
		Env:    env,
		Mounts: []spec.Mount{{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true}},
		// The harness writes its record and nothing else.
		ReadOnlyRoot: true,
		Tmpfs:        []string{"/tmp:mode=1777"},
	}
	res := Result{
		Files:      map[string][]byte{agentryHarnessConfig: file},
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: resultKind,
	}
	config := spec.PlanMount + "/" + agentryHarnessConfig
	if in.Mode == manifest.ModeInteractive {
		// The same harness, the same configuration and the same events:
		// a session names its conversation and sends its turns as commands
		// on the container's stdin. A persistent agent's segment is such a
		// session on a store that outlives it.
		agent.Argv = []string{"harness", "serve", "--config", config}
		res.Session = &spec.SessionSpec{
			Transport:       agentryHarnessTransport,
			ProtocolVersion: proto.Version,
			SessionDB:       spec.SessionMount,
			Mounts:          []spec.Mount{store},
			StoreOwner:      agentryHarnessUser,
		}
	} else {
		agent.Argv = []string{"harness", "run", "--config", config, "--conversation", spec.PHConversation}
		agent.Mounts = append(agent.Mounts, store)
		agent.StdinTask = true
		// A harness that ends on a passing model failure is started again
		// on the conversation it recorded.
		res.Resume = &spec.ResumeSpec{SessionDB: spec.SessionMount, StoreOwner: agentryHarnessUser}
	}
	res.Agent = agent
	return res, nil
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
	case m.Provider == "google":
		return proto.APIGemini, base, nil
	case m.Provider == policy.ProviderOpenAI && base == "":
		// OpenAI's own API. The same provider with an address is a server
		// of the person's own, below.
		return proto.APIResponses, "", nil
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
		if u.Compacted {
			// No request of its own: the conversation's size after a
			// summary replaced it.
			return []Event{{Kind: EventUsage, ContextTokens: u.ContextTokens, ContextLimit: u.ContextLimit, ContextCompacted: true, CostUSD: u.CostUSD, Raw: raw}}
		}
		return []Event{{
			Kind: EventUsage,
			// The request's whole input, cached or not, as every other
			// container harness reports it.
			InputTokens:   u.Input + u.CacheRead + u.CacheWrite,
			OutputTokens:  u.Output,
			ContextTokens: u.ContextTokens,
			ContextLimit:  u.ContextLimit,
			// The conversation's running cost, which the engine keeps the
			// highest of and holds a cost budget to.
			CostUSD: u.CostUSD,
			Raw:     raw,
		}}
	case proto.TypeError:
		text := agentryHarnessErrorText(ev)
		if ev.Class == proto.ClassCredits {
			return []Event{{Kind: EventOutOfCredits, Text: text, Raw: raw}}
		}
		return []Event{{Kind: EventError, Text: text, Raw: raw}}
	case proto.TypeWarning:
		switch ev.Code {
		case proto.WarnLoopNudge:
			return []Event{{Kind: EventLoopNudge, Text: ev.Text, Raw: raw}}
		case proto.WarnLengthRecovery:
			return []Event{{Kind: EventLengthRecovery, Text: ev.Reason, Raw: raw}}
		case proto.WarnColdClear:
			return []Event{{Kind: EventResultsCleared, Text: ev.Reason, Raw: raw}}
		case proto.WarnCompaction:
			return []Event{{Kind: EventCompaction, Text: ev.Reason, Raw: raw}}
		case proto.WarnRetry:
			return []Event{{Kind: EventModelRetry, Text: ev.Text, Raw: raw}}
		}
	case proto.TypeSteered:
		return []Event{{Kind: EventSteered, Raw: raw}}
	case proto.TypeTurnEnding:
		return []Event{{Kind: EventTurnEnding, Raw: raw}}
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
		// The harness has already tried a summary of the conversation, so
		// this refusal ends the run. The server's own wording is left to
		// the recorded event: the engine reads it as a failure to carry on
		// from, and carrying on would only be refused again.
		return "the model's server refused the conversation as too long for the model, and the harness could not shorten it; the server's own words are in the run's recorded events"
	case proto.ClassCapacity:
		return "the model's server is overloaded (" + text + ")"
	case proto.ClassTransport:
		return "the connection to the model was closed unexpectedly (" + text + ")"
	case proto.ClassGateway:
		return "the tool gateway failed (" + text + ")"
	}
	return text
}

// PromptCommand frames a turn's first message as the harness reads it.
func (a *agentryHarnessAdapter) PromptCommand(text []byte) []byte {
	return agentryHarnessCommand(proto.Command{Type: proto.CmdPrompt, Text: string(text)})
}

// EndingAnswer frames the runner's answer to a turn that is about to end.
func (a *agentryHarnessAdapter) EndingAnswer(message string) []byte {
	if message == "" {
		return agentryHarnessCommand(proto.Command{Type: proto.CmdAccept})
	}
	return agentryHarnessCommand(proto.Command{Type: proto.CmdContinue, Text: message})
}

func agentryHarnessCommand(c proto.Command) []byte {
	line, _ := json.Marshal(c) // a struct of strings always encodes
	return append(line, '\n')
}
