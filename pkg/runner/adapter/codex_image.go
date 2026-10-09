// The codex-image adapter (plan model-sign-ins.md §4.3, D8, D11, D15, D16;
// agentry-cli#739): OpenAI's Codex CLI in agentry-dockerimages' Codex
// harness image (agentry-dockerimages#53), run in the agent container as
// docker-agent is. It is the default for a ChatGPT plan, and may run on an
// OpenAI API key as a choice.
//
// What it shares with the host codex adapter: the generated config.toml,
// the one-model catalog and the app-server driver, so the §3 invariants of
// codex.go hold the same way (gateway-only tools, secrets by name, no
// ambient context). What the container adds:
//
//   - The image's own user (uid 10001) on a read-only root. CODEX_HOME is
//     the user's home, a tmpfs: codex must write there (its helper links,
//     its state) and refuses a read-only home, so the entrypoint copies the
//     plan's config.toml and models.json into it and starts codex
//     (measured on 0.162.0: a read-only CODEX_HOME fails at start).
//   - A ChatGPT plan's access token in one read-only file
//     (/run/agentry/model-signin/token, mounted by the compiler, I4), which
//     the runner rewrites before it expires; codex reads it with cat. No
//     refresh token enters the container (I3).
//   - The run network's egress proxy, which allows api.openai.com:443
//     alone for a plan (I2).
//
// Both modes are app-server: an autonomous run is its one turn (the
// originator OpenAI's plan usage serves is the app's clientInfo name,
// "Agentry", which exec does not send).
package adapter

import (
	"context"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func init() { Register(&codexImageAdapter{}) }

const (
	// CodexImageAdapterName is the registry key and --adapter value.
	CodexImageAdapterName = "codex-image"
	// codexImageHome is the image user's home, CODEX_HOME (agentry-dockerimages
	// build/agentry-codex/Dockerfile).
	codexImageHome = "/home/codex"
	// codexImageBinary is the codex binary in the image.
	codexImageBinary = "/usr/local/bin/codex"
	// codexImagePlanHome is where the plan's codex files are in the
	// container, read-only.
	codexImagePlanHome = spec.PlanMount + "/" + codexHomeDir
)

// codexImageAdapter implements Adapter for the Codex harness image. It is
// not a HostHarness: nothing of it is installed on the machine.
type codexImageAdapter struct{}

func (c *codexImageAdapter) Name() string { return CodexImageAdapterName }

// ModelSignIns implements ModelSignInAdapter.
func (c *codexImageAdapter) ModelSignIns() []string { return []string{policy.KeyKindChatGPTPlan} }

// codexImageStart is the entrypoint's script: the plan's configuration
// into the writable home, then codex. A run on the restart token route
// reads its token once, here (policy signInTokenRoute restart).
func codexImageStart(restart bool) string {
	script := `cp ` + codexImagePlanHome + `/config.toml ` + codexImagePlanHome + `/models.json "$CODEX_HOME"/ && `
	if restart {
		script += codexTokenEnv + `="$(cat ` + spec.ModelSignInMount + "/" + spec.ModelSignInTokenFile + `)" && export ` + codexTokenEnv + ` && `
	}
	return script + `exec ` + codexImageBinary + ` app-server --strict-config`
}

// Compile renders the instruction, the config and the agent container.
func (c *codexImageAdapter) Compile(in Input) (Result, error) {
	cc, err := codexResolve(CodexImageAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	signIn, err := codexSignIn(CodexImageAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	r := codexRender{
		model: cc.model, instructionPath: spec.PlanMount + "/" + codexInstructionFile,
		catalogPath: codexImagePlanHome + "/models.json", gatewayURL: in.GatewayURL,
	}
	env := []spec.EnvVar{{Name: spec.GatewayTokenEnv, FromRunToken: true}}
	var msi *spec.ModelSignInSpec
	restart := false
	if signIn == "" {
		keyEnv := policy.KeyEnv(in.Harness, in.Model)
		if keyEnv == "" {
			return Result{}, &Refusal{Reason: "the codex-image adapter runs on an OpenAI API key or your ChatGPT plan, and the policy names neither (set model.apiKeyEnv, or sign in with ChatGPT: agentry setup)"}
		}
		r.keyEnv, r.baseURL = keyEnv, codexBaseURL(in)
		env = append(env, spec.EnvVar{Name: keyEnv, FromHostEnv: keyEnv, FromKeyring: keyringAccount(in)})
	} else {
		r.baseURL, r.signInRoute = codexDefaultBaseURL, policy.SignInTokenRoute(in.Harness)
		r.tokenReader, r.tokenPath = "/bin/cat", spec.ModelSignInMount+"/"+spec.ModelSignInTokenFile
		restart = r.signInRoute == policy.TokenRouteRestart
		msi = &spec.ModelSignInSpec{Kind: signIn, Provider: policy.SignInProvider(signIn), Delivery: spec.ModelSignInToken, Path: r.tokenPath}
	}
	env = append(env,
		spec.EnvVar{Name: "CODEX_HOME", Value: codexImageHome},
		// The model goes through the run's egress proxy; MCP traffic stays
		// direct on the run network.
		spec.EnvVar{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
		spec.EnvVar{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
		spec.EnvVar{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
	)
	res := Result{
		Files: map[string][]byte{
			codexInstructionFile: []byte(cc.instruction),
			codexConfigFile:      codexConfig(r),
			codexCatalogFile:     cc.catalog,
		},
		Agent: spec.ContainerSpec{
			Image:        in.HarnessImage,
			Entrypoint:   "/bin/sh",
			Argv:         []string{"-c", codexImageStart(restart)},
			Env:          env,
			Mounts:       []spec.Mount{{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true}},
			ReadOnlyRoot: true,
			Tmpfs:        []string{"/tmp:mode=1777", codexImageHome + ":mode=1777"},
		},
		Model:       "openai/" + cc.model,
		ResultKind:  cc.resultKind,
		ModelSignIn: msi,
	}
	if cc.mode == manifest.ModeInteractive {
		res.Session = codexSessionSpec()
		res.Session.RestartOnRenewal = restart
		if restart {
			// A restart is a new container, whose tmpfs home starts empty:
			// the conversation thread/resume needs is kept in the run's
			// session store instead, a volume given to the image's user,
			// which CODEX_HOME then is.
			res.Session.Mounts = []spec.Mount{{Source: spec.PHSession, Target: spec.SessionMount}}
			res.Session.StoreOwner = "codex"
			for i := range res.Agent.Env {
				if res.Agent.Env[i].Name == "CODEX_HOME" {
					res.Agent.Env[i].Value = spec.SessionMount
				}
			}
		}
	} else {
		res.OneTurn = codexSessionSpec()
	}
	return res, nil
}

// Events implements Adapter. Both modes are driven through the session
// driver, which normalizes the stream itself; a line read here is kept as
// it is.
func (c *codexImageAdapter) Events(line []byte) []Event {
	return []Event{{Kind: EventOther, Raw: rawCopy(line)}}
}

// Session implements Adapter: the app-server driver the host adapter uses.
func (c *codexImageAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	return (&codexAdapter{}).Session(ctx, rw, opts)
}
