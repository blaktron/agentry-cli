package claudesignin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessprobe"
	"github.com/blaktron/agentry-cli/pkg/runner/modelcheck"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// CheckTimeout bounds the save check's one message, once the image is here.
const CheckTimeout = 2 * time.Minute

// CheckModel sends the Claude Code the sign-in is kept for the save check's
// one message on model (modelcheck.ClaudeArgs), in the sign-in's own
// directory: the image's on its volume, else the installed one's. The
// image's container is given no variable of the person's; the installed
// Claude Code none of their ANTHROPIC_* or CLAUDE_CODE_* ones. No sign-in
// kept is an answer, NotSignedIn, not an error.
func (s SignIn) CheckModel(ctx context.Context, model string) (modelcheck.ClaudeAnswer, error) {
	r, err := modelsignin.Open(s.Home, nil).Get(modelsignin.Claude)
	if errors.Is(err, modelsignin.ErrNone) {
		return modelcheck.ClaudeAnswer{NotSignedIn: true}, nil
	}
	if err != nil {
		return modelcheck.ClaudeAnswer{}, err
	}
	pol, path, err := s.policyAt()
	if err != nil {
		return modelcheck.ClaudeAnswer{}, err
	}
	var out bytes.Buffer
	switch {
	case r.ConfigDir == Volume:
		record := ""
		if path != "" {
			record = policy.NewestPathFor(path)
		}
		d := s.docker()
		h, err := gateway.EnsureNewestHarnessImage(ctx, d, pol, record, adapter.ClaudeImageAdapterName, io.Discard, nil)
		if err != nil {
			return modelcheck.ClaudeAnswer{}, err
		}
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		defer cancel()
		if _, err := runContainer(cctx, d, containerRun(pol, h.Ref, "check", modelcheck.ClaudeArgs(model)...), nil, &out, io.Discard); err != nil {
			return modelcheck.ClaudeAnswer{}, fmt.Errorf("Claude Code in its image: %w", err)
		}
	case r.HostConfigDir != "":
		exe, _, err := harnessprobe.Find(pol, adapter.ClaudeAdapterName)
		if err != nil {
			return modelcheck.ClaudeAnswer{}, fmt.Errorf("the installed Claude Code: %w", err)
		}
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		defer cancel()
		cmd := hostCommand(cctx, exe, r.HostConfigDir, modelcheck.ClaudeArgs(model)...)
		cmd.Env = checkEnv(r.HostConfigDir)
		cmd.Stdout, cmd.Stderr = &out, io.Discard
		// Claude Code exits 1 on an error with its JSON result printed: the
		// result is the answer, and only no result is an error.
		if err := cmd.Run(); err != nil && out.Len() == 0 {
			return modelcheck.ClaudeAnswer{}, fmt.Errorf("the installed Claude Code: %w", err)
		}
	default:
		return modelcheck.ClaudeAnswer{NotSignedIn: true}, nil
	}
	return modelcheck.ParseClaudeResult(out.Bytes())
}

// checkEnv is the installed Claude Code's environment for the check: the
// person's, without any of their ANTHROPIC_* or CLAUDE_CODE_* variables,
// which would point it at another endpoint or another credential, and with
// its config directory Agentry's own.
func checkEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_") || name == "CLAUDE_CONFIG_DIR" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+dir, "DISABLE_AUTOUPDATER=1")
}
