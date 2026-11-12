package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
	"github.com/blaktron/agentry-cli/runnerimage/harness/mcp"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// Exit codes of Run.
const (
	// ExitAnswered: the turn ended with the model's answer.
	ExitAnswered = 0
	// ExitFailed: a failure ended the turn; an error event says which.
	ExitFailed = 1
	// ExitUsage: the command line, the configuration or the task could
	// not be used; nothing was sent to the model.
	ExitUsage = 2
	// ExitIncomplete: the turn ended with no whole answer (the answer
	// stopped at the output limit, or the step limit was reached).
	ExitIncomplete = 3
)

// maxTask is the most task text Run reads.
const maxTask = 16 << 20

// Env reads the process's environment. It is a parameter so a test can
// supply one.
type Env func(name string) string

// Run is the harness's one-shot mode: one prompt on one conversation, with
// events on stdout and anything else on stderr.
//
//	run --config FILE --conversation ID
//
// Stdin carries the runner's commands, one JSON object a line: first the
// prompt, then an accept or a continue for each turn_ending event the
// harness writes. A stdin that ends is an accept: with no one to ask, a turn
// ends on the model's answer.
//
// A conversation that already exists is carried on: the prompt is added to
// what was recorded.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("harness run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "the harness configuration file")
	conversation := fs.String("conversation", "", "the conversation's id")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" || *conversation == "" || fs.NArg() != 0 {
		say(stderr, "usage: harness run --config FILE --conversation ID  (the runner's commands on stdin)")
		return ExitUsage
	}
	h, code := build(*configPath, stdout, stderr, env)
	if h == nil {
		return code
	}
	defer func() { _ = h.Close() }()
	cmds := readCommands(stdin)
	first, err := cmds.next(ctx)
	task := strings.TrimSpace(first.Text)
	switch {
	case errors.Is(err, io.EOF):
		say(stderr, "harness: no prompt on stdin")
		return ExitUsage
	case err != nil:
		say(stderr, "harness: reading the prompt:", err)
		return ExitUsage
	case first.Type != proto.CmdPrompt:
		say(stderr, fmt.Sprintf("harness: the first command on stdin is %q, not a prompt", first.Type))
		return ExitUsage
	case task == "":
		say(stderr, "harness: the prompt is empty")
		return ExitUsage
	}
	h.Gate = func() func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			cmd, err := cmds.next(ctx)
			switch {
			case errors.Is(err, io.EOF):
				return "", nil
			case err != nil:
				return "", err
			case cmd.Type == proto.CmdContinue && strings.TrimSpace(cmd.Text) != "":
				return cmd.Text, nil
			case cmd.Type == proto.CmdAccept || cmd.Type == proto.CmdContinue:
				return "", nil
			}
			return "", fmt.Errorf("the runner answered a turn's ending with the command %q", cmd.Type)
		}
	}

	if err := h.Start(ctx, *conversation); err != nil {
		say(stderr, "harness:", err)
		return ExitFailed
	}
	reason, err := h.Prompt(ctx, task)
	if err != nil {
		say(stderr, "harness:", err)
		return ExitFailed
	}
	if reason != proto.EndAnswered {
		return ExitIncomplete
	}
	return ExitAnswered
}

// build reads the configuration at path and makes the harness it describes,
// writing its events to stdout. A nil harness comes with the exit code that
// says why there is none, and stderr has been told.
func build(path string, stdout, stderr io.Writer, env Env) (*Harness, int) {
	f, err := os.Open(path) //nolint:gosec // the configuration the runner mounted
	if err != nil {
		say(stderr, "harness:", err)
		return nil, ExitUsage
	}
	cfg, err := proto.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		say(stderr, "harness:", err)
		return nil, ExitUsage
	}
	plain := transport()
	var toModel http.RoundTripper = plain
	if cfg.Retry != nil && cfg.Retry.IdleSeconds > 0 {
		toModel = &idleTransport{base: plain, idle: time.Duration(cfg.Retry.IdleSeconds) * time.Second}
	}
	return New(cfg, modelClient(cfg, &http.Client{Transport: toModel}, env), &mcp.Client{
		URL:   cfg.Gateway.URL,
		Token: func() string { return env(cfg.Gateway.TokenEnv) },
		// A tool may take as long as it takes: the idle limit is the
		// model's alone.
		HTTP:    &http.Client{Transport: plain},
		Name:    "agentry-harness",
		Version: fmt.Sprint(proto.Version),
	}, stdout), 0
}

// commands reads the runner's commands from stdin on a goroutine of its own,
// so a wait for one can be given up.
type commands struct {
	lines chan commandOrError
}

type commandOrError struct {
	cmd proto.Command
	err error
}

func readCommands(stdin io.Reader) *commands {
	c := &commands{lines: make(chan commandOrError)}
	go func() {
		defer close(c.lines)
		r := bufio.NewReaderSize(stdin, 64<<10)
		for {
			line, err := readLine(r, maxTask+4096)
			if len(strings.TrimSpace(string(line))) > 0 {
				var cmd proto.Command
				dec := json.NewDecoder(strings.NewReader(string(line)))
				dec.DisallowUnknownFields()
				if derr := dec.Decode(&cmd); derr != nil {
					c.lines <- commandOrError{err: fmt.Errorf("a line on stdin is not a command: %w", derr)}
					return
				}
				if dec.More() {
					c.lines <- commandOrError{err: errors.New("a line on stdin holds more than one command")}
					return
				}
				c.lines <- commandOrError{cmd: cmd}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					c.lines <- commandOrError{err: err}
				}
				return
			}
		}
	}()
	return c
}

// next is the next command, io.EOF once stdin has ended.
func (c *commands) next(ctx context.Context) (proto.Command, error) {
	select {
	case <-ctx.Done():
		return proto.Command{}, ctx.Err()
	case l, ok := <-c.lines:
		if !ok {
			return proto.Command{}, io.EOF
		}
		return l.cmd, l.err
	}
}

// readLine reads one line of at most limit bytes, without its newline.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, fmt.Errorf("a command on stdin is larger than %d bytes", limit)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// say writes one line for a person to read.
func say(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}

// modelClient is the client for the configuration's wire API.
// DecodeConfig has already refused an API this harness does not speak.
func modelClient(cfg proto.Config, client *http.Client, env Env) llm.Client {
	key := func() string {
		if cfg.Model.KeyEnv == "" {
			return ""
		}
		return env(cfg.Model.KeyEnv)
	}
	if cfg.Model.API == proto.APIChat {
		c := &llm.Chat{
			BaseURL:         cfg.Model.BaseURL,
			Model:           cfg.Model.Name,
			Key:             key,
			HTTP:            client,
			ExtraBody:       cfg.Model.ExtraBody,
			ReplayReasoning: cfg.Model.ReplayReasoning,
		}
		if s := cfg.Model.Sampling; s != nil {
			c.Temperature, c.TopP, c.TopK, c.MinP = s.Temperature, s.TopP, s.TopK, s.MinP
			c.PresencePenalty, c.RepetitionPenalty = s.PresencePenalty, s.RepetitionPenalty
		}
		return c
	}
	if cfg.Model.API == proto.APIResponses {
		return &llm.Responses{BaseURL: cfg.Model.BaseURL, Model: cfg.Model.Name, Key: key, HTTP: client}
	}
	if cfg.Model.API == proto.APIGemini {
		return &llm.Gemini{BaseURL: cfg.Model.BaseURL, Model: cfg.Model.Name, Key: key, HTTP: client}
	}
	return &llm.Anthropic{
		BaseURL: cfg.Model.BaseURL,
		Model:   cfg.Model.Name,
		Key:     key,
		HTTP:    client,
	}
}

// transport reaches the model and the gateway. It follows the environment's
// proxy settings, so a run whose only route out is a proxy uses it, and it
// sets no limit on how long a reply may take: a reply streams for as long
// as the model writes.
func transport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          8,
		ForceAttemptHTTP2:     true,
	}
}
