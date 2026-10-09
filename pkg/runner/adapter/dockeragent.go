// The docker-agent adapter (RUNNER-PLAN §R10; annex §4.1) — the v1
// default: Apache-2.0 static binary, verified `--exec` one-shot semantics,
// remote-MCP toolsets with header auth, and the widest provider matrix.
// The adapter renders one agent.yaml whose ONLY toolset is the run's MCP
// gateway: docker-agent's built-in shell/filesystem/fetch toolsets are
// never enabled, whatever the manifest says — tools come from the gateway
// or not at all (RQ1/RQ2).
package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func init() { Register(&dockerAgent{}) }

// harnessSafety is the docker-agent safety mode for every plan: the
// gateway is the boundary, so nothing in-harness may prompt (§R4).
const harnessSafety = "autonomous"

// acpProtocolVersion is the Agent Client Protocol version negotiated with
// serve acp (docker-agent 1.128 answers protocolVersion 1).
const acpProtocolVersion = 1

// HarnessConfigVersion is the docker-agent config schema version the
// adapter writes (`version:` in agent.yaml). It belongs to the pinned
// harness image (policy.DefaultHarnessImage, 1.128.0 ships schema 15).
const HarnessConfigVersion = "15"

// dockerAgent implements Adapter for docker/docker-agent.
type dockerAgent struct{}

func (d *dockerAgent) Name() string { return DefaultName }

// agentYAML mirrors the docker-agent config fields we emit (schema is
// theirs and pre-1.0-volatile — annex risk register; the conformance
// suite, not this struct, is what pins compatibility). Structs, not
// map[string]any, so field order and therefore plan bytes stay
// deterministic.
type agentYAML struct {
	Version   string                  `yaml:"version"`
	Providers map[string]providerYAML `yaml:"providers,omitempty"`
	Models    map[string]modelYAML    `yaml:"models"`
	Agents    map[string]agntYAML     `yaml:"agents"`
}

// providerYAML is a named provider of docker-agent's, for an endpoint it
// has no alias for (Poe): its OpenAI-compatible client at base_url, the API
// pinned, and the variable its key rides in.
type providerYAML struct {
	APIType  string `yaml:"api_type"`
	BaseURL  string `yaml:"base_url"`
	TokenKey string `yaml:"token_key"`
}

// poeBaseURL is Poe's OpenAI-compatible API (plan §4.6).
const poeBaseURL = "https://api.poe.com/v1"

// poeProvider is Poe as docker-agent 1.128.0 needs it, which has no alias
// for it: a named provider with Chat Completions set explicitly, since a
// named OpenAI-compatible provider otherwise picks the Responses API for
// every gpt-5, o-series and codex model id (its defaultOpenAIAPIType), and
// Poe's GPT bots would go to another API than every other bot's. The key
// rides in POE_API_KEY as a bearer (token_key).
func poeProvider(baseURL string) providerYAML {
	if baseURL == "" {
		baseURL = poeBaseURL
	}
	return providerYAML{APIType: "openai_chatcompletions", BaseURL: baseURL, TokenKey: harnessKeyVar(policy.ProviderPoe, "")}
}

type modelYAML struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// BaseURL points OpenAI-compatible providers at custom endpoints.
	BaseURL string `yaml:"base_url,omitempty"`
}

// customProvider is the policy's name for an OpenAI-compatible endpoint
// of the operator's own (setup's "custom"); docker-agent has no provider
// of that name — 1.128.0 refuses it as `unknown provider type "custom"` —
// and reaches such an endpoint through its openai client with a base
// URL, which is what the rendered config names (agentry-cli#169).
const customProvider = "custom"

// harnessProvider maps the policy's provider name to docker-agent's.
func harnessProvider(m policy.Model) (string, error) {
	if m.Provider != customProvider {
		return m.Provider, nil
	}
	if m.BaseURL == "" {
		return "", errors.New("docker-agent adapter: provider custom needs model.baseUrl (the endpoint the openai client is pointed at)")
	}
	return "openai", nil
}

// harnessKeyVar is the variable docker-agent's client for a provider
// reads its credential from, measured on 1.128.0 (agentry-cli#169): the
// anthropic client requires ANTHROPIC_API_KEY, the google client
// GOOGLE_API_KEY or GEMINI_API_KEY, the openai client OPENAI_API_KEY —
// and only the openai client honours a config-level token_key, so a
// variable named in the config is not a mechanism the others have. The
// connect providers (agentry-cli#742) read theirs by docker-agent's
// aliases (openrouter OPENROUTER_API_KEY, huggingface HF_TOKEN; its
// aliases.go at v1.128.0), and Poe by the token_key its named provider
// sets (poeProvider). The
// container therefore carries the key under the client's own name, and
// the policy's apiKeyEnv (whatever it is called) is only where the value
// comes FROM on the host. A provider docker-agent knows that this table
// does not (mistral, azure, …) keeps the policy's name, which is then
// the operator's to get right.
func harnessKeyVar(harnessProvider, policyVar string) string {
	switch harnessProvider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "google":
		return "GEMINI_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case policy.ProviderOpenRouter:
		return "OPENROUTER_API_KEY"
	case policy.ProviderHuggingFace:
		return "HF_TOKEN"
	case policy.ProviderPoe:
		return "POE_API_KEY"
	default:
		return policyVar
	}
}

// redactFlagOff is docker-agent's redact_secrets flag, written false: the
// runner wires the builtin itself (redactSecretsHooks).
var redactFlagOff = false

type agntYAML struct {
	Model       string `yaml:"model"`
	Description string `yaml:"description,omitempty"`
	Instruction string `yaml:"instruction"`
	// Safety is docker-agent's declarative safety mode. The ACP server
	// (serve acp) has no --safety flag, so the interactive plan needs it
	// here; it is written for both modes so the config is identical, and
	// the exec path's explicit --safety flag still wins (DR-15).
	Safety   string        `yaml:"safety,omitempty"`
	Toolsets []toolsetYAML `yaml:"toolsets"`
	// RedactSecrets is docker-agent's redact_secrets flag, on unless set.
	// It is written false and its builtin wired by hand in Hooks, so the
	// person's inputs reach the model unchanged (agentry-cli#632).
	RedactSecrets *bool      `yaml:"redact_secrets,omitempty"`
	Hooks         *hooksYAML `yaml:"hooks,omitempty"`
}

// hooksYAML is the part of docker-agent's hooks block the runner writes.
type hooksYAML struct {
	PreToolUse            []hookMatcherYAML `yaml:"pre_tool_use,omitempty"`
	ToolResponseTransform []hookMatcherYAML `yaml:"tool_response_transform,omitempty"`
}

type hookMatcherYAML struct {
	Matcher string     `yaml:"matcher"`
	Hooks   []hookYAML `yaml:"hooks"`
}

type hookYAML struct {
	Type    string `yaml:"type"`
	Command string `yaml:"command"`
}

// InputTools are the runner's tools that answer with the person's own
// inputs, served unchanged (plan quickagents-that-run.md D4, DR-78).
var InputTools = []string{"read_input", "search_inputs"}

// redactSecretsHooks wires docker-agent's redact_secrets builtin, which
// its redact_secrets flag puts under three events: tool arguments
// (pre_tool_use), outgoing chat (before_llm_call) and tool output
// (tool_response_transform), each scrubbing anything secret-shaped
// (agentry-cli#632). The runner keeps two legs and drops one:
//
//   - every call's arguments are scrubbed, as before;
//   - every tool's output is scrubbed but the input tools': a person's
//     input reaches the model unchanged, credentials included, which the
//     flag's blanket scrub undid even after the gateway passed it;
//   - outgoing chat is not scrubbed, since that leg reads the whole
//     history before each model call and would scrub an input again.
//     Every other tool's output is scrubbed before it enters the history.
func redactSecretsHooks() *hooksYAML {
	redact := []hookYAML{{Type: "builtin", Command: "redact_secrets"}}
	return &hooksYAML{
		PreToolUse:            []hookMatcherYAML{{Matcher: "*", Hooks: redact}},
		ToolResponseTransform: []hookMatcherYAML{{Matcher: notExactly(InputTools...), Hooks: redact}},
	}
}

// notExactly is a pattern that matches every string but the given ones,
// for a docker-agent hook matcher, which docker-agent anchors as
// ^(?:pattern)$ and compiles with RE2, which has no negative lookahead.
// It walks a trie of the words: at each point a string may end (where no
// word does), take a byte no word takes next and then anything, or take
// a word's next byte and go on.
func notExactly(words ...string) string {
	type node struct {
		next map[byte]*node
		end  bool
	}
	root := &node{next: map[byte]*node{}}
	for _, w := range words {
		n := root
		for i := 0; i < len(w); i++ {
			c := w[i]
			if n.next[c] == nil {
				n.next[c] = &node{next: map[byte]*node{}}
			}
			n = n.next[c]
		}
		n.end = true
	}
	var walk func(n *node) string
	walk = func(n *node) string {
		keys := make([]byte, 0, len(n.next))
		for c := range n.next {
			keys = append(keys, c)
		}
		slices.Sort(keys)
		var alts []string
		if !n.end {
			alts = append(alts, "")
		}
		if len(keys) == 0 {
			alts = append(alts, "(?s:.+)")
		} else {
			var cls strings.Builder
			for _, c := range keys {
				if strings.IndexByte(`\\]^-`, c) >= 0 {
					cls.WriteByte('\\')
				}
				cls.WriteByte(c)
			}
			alts = append(alts, "[^"+cls.String()+"](?s:.*)")
		}
		for _, c := range keys {
			alts = append(alts, regexp.QuoteMeta(string(c))+"(?:"+walk(n.next[c])+")")
		}
		return strings.Join(alts, "|")
	}
	return walk(root)
}

type toolsetYAML struct {
	Type            string            `yaml:"type"`
	Remote          remoteYAML        `yaml:"remote"`
	AllowPrivateIPs bool              `yaml:"allow_private_ips"`
	Headers         map[string]string `yaml:"-"`
}

type remoteYAML struct {
	URL           string            `yaml:"url"`
	TransportType string            `yaml:"transport_type"`
	Headers       map[string]string `yaml:"headers,omitempty"`
}

// Compile renders agent.yaml and the agent container spec.
func (d *dockerAgent) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, fmt.Errorf("docker-agent adapter: empty instruction")
	}
	if in.HarnessImage == "" {
		return Result{}, fmt.Errorf("docker-agent adapter: no harness image pinned")
	}
	provider, err := harnessProvider(in.Model)
	if err != nil {
		return Result{}, err
	}
	var keyExpect *spec.CredentialKind
	if provider == policy.ProviderAnthropic {
		// A plan-time refusal, so `agentry run --plan` and a run meet it
		// before any container starts (agentry-cli#171).
		if policy.KeyKind(in.Harness, in.Model) == policy.KeyKindSubscription {
			// Named by its policy field, then the shared sentence as it
			// stands (agentry-cli#183).
			return Result{}, &Refusal{Reason: "apiKeyKind subscription: " + policy.SubscriptionTokenSentence}
		}
		keyExpect = anthropicKeyExpectation(in)
	}
	keyEnv := policy.KeyEnv(in.Harness, in.Model)

	// Schema-shaped output is requested through the prompt and validated
	// by the engine before exit (§R2, DR-8 in the deviations register
	// records why the harness-native structured_output block is not
	// emitted in v1). The wording is shared with every other adapter.
	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)

	doc := agentYAML{
		// The config schema version of the pinned harness: docker-agent
		// parses a document with the strict schema of the version it
		// names and upgrades afterwards, so an old number selects an old
		// schema — "2" has no allow_private_ips and the harness refused
		// every plan (agentry-cli#43). Bump together with
		// policy.DefaultHarnessImage (1.128.0 → 15).
		Version: HarnessConfigVersion,
		Models: map[string]modelYAML{
			"main": {
				Provider: provider,
				Model:    in.Model.Name,
				BaseURL:  in.Model.BaseURL,
			},
		},
		Agents: map[string]agntYAML{
			"root": {
				Model:       "main",
				Description: in.Summary,
				Instruction: instruction,
				Safety:      harnessSafety,
				Toolsets: []toolsetYAML{{
					Type: "mcp",
					Remote: remoteYAML{
						URL:           in.GatewayURL,
						TransportType: "streamable",
						// ${env.VAR} is docker-agent's own expansion; the
						// token never appears in plan bytes (§R11 inv. 7).
						Headers: map[string]string{
							"Authorization": "Bearer ${env." + spec.GatewayTokenEnv + "}",
						},
					},
					// The gateway is a private address on the run network;
					// without this docker-agent's SSRF guard refuses it.
					AllowPrivateIPs: true,
				}},
				RedactSecrets: &redactFlagOff,
				Hooks:         redactSecretsHooks(),
			},
		},
	}
	if provider == policy.ProviderPoe {
		// Poe's endpoint is its named provider's, not the model's.
		doc.Providers = map[string]providerYAML{provider: poeProvider(in.Model.BaseURL)}
		m := doc.Models["main"]
		m.BaseURL = ""
		doc.Models["main"] = m
	}
	var buf bytes.Buffer
	buf.WriteString("# generated by agentry run — do not edit (RUNNER-PLAN §R10, docker-agent adapter)\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return Result{}, fmt.Errorf("render agent.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return Result{}, fmt.Errorf("render agent.yaml: %w", err)
	}

	mode := in.Mode
	if mode == "" {
		mode = manifest.ModeAutonomous
	}
	var argv []string
	var session *spec.SessionSpec
	switch mode {
	case manifest.ModeAutonomous:
		// --exec with no unattended policy auto-rejects every tool call
		// (annex §4.1). --safety autonomous is deliberate and safe HERE
		// because in-harness permission patterns are not the boundary:
		// the tool surface is gateway-only and the container is on an
		// internal network — the enforcement lives a layer down (§R4).
		argv = []string{"run", "--exec", "--json", "--safety", harnessSafety,
			spec.PlanMount + "/agent.yaml", "-"}
	case manifest.ModeInteractive:
		// The ACP server on the container's stdio (DR-15; spike on
		// agentry-cli#33). Same config, same toolset, same network; the
		// session store is the one writable mount, added for sessions
		// only. Safety comes from the config (no flag on serve acp).
		argv = []string{"serve", "acp", spec.PlanMount + "/agent.yaml",
			"--session-db", spec.SessionMount + "/session.db"}
		session = &spec.SessionSpec{
			Transport:       "acp-stdio",
			ProtocolVersion: acpProtocolVersion,
			SessionDB:       spec.SessionMount + "/session.db",
			Mounts:          []spec.Mount{{Source: spec.PHSession, Target: spec.SessionMount}},
			// The image runs as its own unprivileged user; the store's
			// volume is given to it before the harness starts.
			StoreOwner: "docker-agent",
			Safety:     harnessSafety,
		}
	default:
		return Result{}, fmt.Errorf("docker-agent adapter: unknown run mode %q", mode)
	}
	agent := spec.ContainerSpec{
		Image: in.HarnessImage,
		Argv:  argv,
		Env: []spec.EnvVar{
			{Name: spec.GatewayTokenEnv, FromRunToken: true},
			// The key, under the name the harness's client reads it by,
			// from the variable the policy names (the harness block's
			// override first, as the receipt's line says: policy.KeyEnv).
			{Name: harnessKeyVar(provider, keyEnv), FromHostEnv: keyEnv, FromKeyring: keyringAccount(in), ExpectedKind: keyExpect},
			// The proxy is the agent container's only route out (§R4);
			// NO_PROXY keeps MCP traffic direct on the run network.
			{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
			{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
			{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
			{Name: "TELEMETRY_ENABLED", Value: "false"},
		},
		Mounts: []spec.Mount{{
			Source:   spec.PHCfg + "/harness",
			Target:   spec.PlanMount,
			ReadOnly: true,
		}},
		StdinTask: session == nil, // the task rides stdin only on the exec path
		// A read-only root (agentry-cli#163): the image's user (uid 100,
		// not root) writes its scratch, its working directory and its home
		// (its session database under ~/.cagent), each a tmpfs that goes
		// with the container. A tmpfs is root's 0755 unless told
		// otherwise, which the harness cannot write, hence the mode. The
		// plan and the session store are mounts of their own.
		ReadOnlyRoot: true,
		Tmpfs:        []string{"/tmp:mode=1777", "/work:mode=1777", "/home/docker-agent:mode=1777"},
	}

	return Result{
		Files:      map[string][]byte{"agent.yaml": buf.Bytes()},
		Agent:      agent,
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: resultKind,
		Session:    session,
	}, nil
}

// Events maps docker-agent --json NDJSON lines to normalized events.
// Best-effort by design: the NDJSON schema is not a stable contract, so
// this recognizes shapes (a "type" discriminator; content/usage fields in
// the places the survey observed them) and passes everything else through
// as EventOther. Receipts always keep the raw line; M-R4's conformance
// suite is where the mapping gets empirically pinned.
func (d *dockerAgent) Events(line []byte) []Event {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		// Not JSON: under --json this should not happen, but a harness
		// printing plain text still deserves to have it treated as
		// output rather than dropped.
		return []Event{{Kind: EventAssistantText, Text: string(trimmed), Raw: rawCopy(trimmed)}}
	}
	raw := rawCopy(trimmed)
	typ, _ := obj["type"].(string)
	var events []Event

	switch {
	case isToolCallStart(typ):
		events = append(events, Event{Kind: EventToolCall, Raw: raw})
	case strings.Contains(typ, "error"):
		text := errorText(obj)
		kind := EventError
		if OutOfCredits(text) {
			kind = EventOutOfCredits
		}
		events = append(events, Event{Kind: kind, Text: text, Raw: raw})
	case strings.Contains(typ, "reasoning") || strings.Contains(typ, "thinking"):
		// The model's reasoning stream (agent_choice_reasoning): receipts
		// keep it, the result must not (agentry-cli#53).
	default:
		if txt := textField(obj); txt != "" {
			events = append(events, Event{Kind: EventAssistantText, Text: txt, Delta: isTextDelta(typ), Raw: raw})
		}
	}
	if in, out, cost, ok := usageFields(obj); ok {
		events = append(events, Event{Kind: EventUsage, InputTokens: in, OutputTokens: out, CostUSD: cost, Raw: raw})
	}
	if len(events) == 0 {
		events = append(events, Event{Kind: EventOther, Raw: raw})
	}
	return events
}

// isToolCallStart is true for the one event that marks a tool call: the
// harness's `tool_call` (or a `tool_use*` variant). A streaming provider
// delivers the call's arguments as many `partial_tool_call` chunks first,
// and every call is followed by a `tool_call_response`; neither is a turn,
// and counting them burned a 60-call budget on four calls (agentry-cli#51).
func isToolCallStart(typ string) bool {
	if !strings.Contains(typ, "tool") {
		return false
	}
	if !strings.Contains(typ, "call") && !strings.Contains(typ, "use") {
		return false
	}
	for _, not := range []string{"partial", "delta", "chunk", "response", "result", "confirm", "output"} {
		if strings.Contains(typ, not) {
			return false
		}
	}
	return true
}

// isTextDelta is true for the shapes that stream one chunk of the assistant
// message at a time — docker-agent 1.128.0 emits agent_choice with a
// content delta per token group; a whole message arrives in one event
// only for the non-streaming shapes (agentry-cli#53).
func isTextDelta(typ string) bool {
	for _, s := range []string{"choice", "delta", "chunk", "partial"} {
		if strings.Contains(typ, s) {
			return true
		}
	}
	return false
}

// errorText is an error event's message: docker-agent's ErrorEvent carries
// it in "error" (pkg/runtime/event.go at v1.128.0), a string that the
// assistant-text shapes never hold; an older shape's text otherwise.
func errorText(obj map[string]any) string {
	if s, ok := obj["error"].(string); ok && s != "" {
		return s
	}
	return textField(obj)
}

// outOfCreditsRE matches a provider's answer that the paying account has
// no credits left: the 402 status as the OpenAI and Anthropic SDKs quote it
// ("402 Payment Required"), a body's 402 code or status, Poe's
// insufficient_credits, and Hugging Face's words for its included credits.
var outOfCreditsRE = regexp.MustCompile(`(?i)\b402 payment required\b|"(?:code|status)"\s*:\s*"?402\b|insufficient[_ ]credits|out of credits|exceeded your monthly included credits`)

// OutOfCredits reports whether a harness's error is a provider saying the
// account that pays for the model has no credits left.
func OutOfCredits(text string) bool { return outOfCreditsRE.MatchString(text) }

// textField digs assistant text out of the shapes docker-agent has been
// observed to emit: a top-level content/text string, or message.content
// as a string or a list of {type: text, text} parts.
func textField(obj map[string]any) string {
	if s, ok := obj["content"].(string); ok && s != "" {
		return s
	}
	if s, ok := obj["text"].(string); ok && s != "" {
		return s
	}
	msg, _ := obj["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if t, _ := pm["type"].(string); t == "text" {
				if s, _ := pm["text"].(string); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// usageFields extracts token/cost accounting from a top-level or nested
// "usage" object. On docker-agent's token_usage event input_tokens and
// output_tokens are that model request's own (input fresh plus cached), so
// the engine sums them; cost is the session's running total, so it keeps
// the high-water mark (agentry-cli#141).
func usageFields(obj map[string]any) (in, out int64, cost float64, ok bool) {
	u := obj
	if nested, isMap := obj["usage"].(map[string]any); isMap {
		u = nested
	}
	in = intField(u, "input_tokens")
	out = intField(u, "output_tokens")
	cost, hasCost := floatField(u, "cost", "cost_usd", "total_cost_usd")
	if in == 0 && out == 0 && !hasCost {
		return 0, 0, 0, false
	}
	return in, out, cost, true
}

func intField(m map[string]any, key string) int64 {
	if f, ok := m[key].(float64); ok {
		return int64(f)
	}
	return 0
}

func floatField(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return f, true
		}
	}
	return 0, false
}

func rawCopy(b []byte) json.RawMessage {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
