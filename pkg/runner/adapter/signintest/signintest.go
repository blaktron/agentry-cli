// Package signintest is a fake harness adapter for tests of model sign-ins:
// a container harness that reads its model credential from the file or
// directory a model sign-in hands it, and speaks a small NDJSON event
// stream the tests script. Nothing but tests imports it.
package signintest

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The adapters Register installs: one that reads a token file, one that
// reads a config directory at ConfigDir.
const (
	TokenAdapter = "signin-test"
	DirAdapter   = "signin-test-dir"
	ConfigDir    = "/home/harness/.config/vendor"
)

// Image is the harness image a test policy names for both adapters.
const Image = "ghcr.io/example/fake-harness:1@sha256:1111111111111111111111111111111111111111111111111111111111111111"

var once sync.Once

// Register installs both adapters, once per process.
func Register() {
	once.Do(func() {
		adapter.Register(fake{name: TokenAdapter, delivery: spec.ModelSignInToken})
		adapter.Register(fake{name: DirAdapter, delivery: spec.ModelSignInConfigDir})
	})
}

// Policy makes p name kind (a model sign-in kind) for both adapters,
// with their image.
func Policy(p *policy.Policy, kind string) {
	p.Model.Provider = policy.SignInProvider(kind)
	p.Model.APIKeyKind = kind
	p.Model.APIKeyEnv = ""
	if allow := policy.SignInProxyAllow(kind); allow != nil {
		p.Model.ProxyAllow = allow
	}
	p.Images.HarnessImages = map[string]policy.HarnessImage{
		TokenAdapter: {Ref: Image},
		DirAdapter:   {Ref: Image},
	}
}

type fake struct {
	name     string
	delivery string
}

func (f fake) Name() string { return f.name }

// ModelSignIns: every kind, so a test picks the delivery by the adapter.
func (f fake) ModelSignIns() []string {
	return []string{policy.KeyKindChatGPTPlan, policy.KeyKindClaudeSignIn, policy.KeyKindCopilotSignIn}
}

// Compile runs "fake-harness --credential <path>": the harness reads the
// credential the sign-in hands it, and nothing else names one.
func (f fake) Compile(in adapter.Input) (adapter.Result, error) {
	path := spec.ModelSignInMount + "/" + spec.ModelSignInTokenFile
	if f.delivery == spec.ModelSignInConfigDir {
		path = ConfigDir
	}
	res := adapter.Result{
		Files: map[string][]byte{"instruction.md": []byte(in.Instruction)},
		Agent: spec.ContainerSpec{
			Image: in.HarnessImage,
			Argv:  []string{"fake-harness", "--credential", path},
			Env: []spec.EnvVar{
				{Name: spec.GatewayTokenEnv, FromRunToken: true},
				{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
			},
			Mounts:    []spec.Mount{{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true}},
			StdinTask: true,
		},
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: "text",
	}
	if kind := policy.KeyKind(in.Harness, in.Model); policy.IsModelSignIn(kind) {
		res.ModelSignIn = &spec.ModelSignInSpec{Kind: kind, Provider: policy.SignInProvider(kind), Delivery: f.delivery, Path: path}
	}
	return res, nil
}

// Event is one line of the fake harness's stream.
type Event struct {
	Type   string    `json:"type"` // tool_call | text | usage | plan_limit | error
	Text   string    `json:"text,omitempty"`
	In     int64     `json:"in,omitempty"`
	Out    int64     `json:"out,omitempty"`
	Cost   float64   `json:"cost,omitempty"`
	Resets time.Time `json:"resets,omitzero"`
}

// Line is ev as the fake harness writes it.
func Line(ev Event) string {
	b, _ := json.Marshal(ev)
	return string(b)
}

func (f fake) Events(line []byte) []adapter.Event {
	var e Event
	if json.Unmarshal(line, &e) != nil {
		return []adapter.Event{{Kind: adapter.EventOther, Raw: line}}
	}
	ev := adapter.Event{Raw: line, Text: e.Text}
	switch e.Type {
	case "tool_call":
		ev.Kind = adapter.EventToolCall
	case "text":
		ev.Kind = adapter.EventAssistantText
	case "usage":
		ev.Kind, ev.InputTokens, ev.OutputTokens, ev.CostUSD = adapter.EventUsage, e.In, e.Out, e.Cost
	case "plan_limit":
		ev.Kind, ev.ResetsAt = adapter.EventPlanLimit, e.Resets
	case "error":
		ev.Kind = adapter.EventError
	default:
		ev.Kind = adapter.EventOther
	}
	return []adapter.Event{ev}
}

func (f fake) Session(context.Context, io.ReadWriteCloser, adapter.SessionOptions) (adapter.SessionDriver, error) {
	return nil, adapter.ErrNotSupported
}
