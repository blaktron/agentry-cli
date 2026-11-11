package harness

import (
	"context"
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

// Run is the harness's one-shot mode: one prompt, read from stdin, on one
// conversation, with events on stdout and anything else on stderr.
//
//	run --config FILE --conversation ID
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
		say(stderr, "usage: harness run --config FILE --conversation ID  (the task on stdin)")
		return ExitUsage
	}
	f, err := os.Open(*configPath)
	if err != nil {
		say(stderr, "harness:", err)
		return ExitUsage
	}
	cfg, err := proto.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		say(stderr, "harness:", err)
		return ExitUsage
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxTask+1))
	if err != nil {
		say(stderr, "harness: reading the task:", err)
		return ExitUsage
	}
	task := strings.TrimSpace(string(raw))
	switch {
	case len(raw) > maxTask:
		say(stderr, "harness: the task is larger than 16 MiB")
		return ExitUsage
	case task == "":
		say(stderr, "harness: no task on stdin")
		return ExitUsage
	}

	client := &http.Client{Transport: transport()}
	h := New(cfg, modelClient(cfg, client, env), &mcp.Client{
		URL:     cfg.Gateway.URL,
		Token:   func() string { return env(cfg.Gateway.TokenEnv) },
		HTTP:    client,
		Name:    "agentry-harness",
		Version: fmt.Sprint(proto.Version),
	}, stdout)
	defer func() { _ = h.Close() }()

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
