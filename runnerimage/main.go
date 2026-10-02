//go:build ignore

// The runner-image entrypoint (RUNNER-PLAN §R7): one stdlib-only binary
// serving every in-container role — `tools` (the §R3 MCP stdio server the
// gateway launches per run), `proxy` (the §R4 llm-proxy CONNECT
// allowlist), and `probe` (the engine's gateway health check in the §R6
// execute order). One binary keeps the locally built image to a single
// `go build` and the Dockerfile short enough to audit at a glance.
//
// The `//go:build ignore` constraint above exists only in the REPO copy
// of this file, where it keeps package main from colliding with package
// runnerimage (embed.go) in the same directory. Materialize strips it, so
// in the materialized build context this file compiles as the ordinary
// ./runnerimage main package the Dockerfile builds — embed_test.go pins
// both halves of that arrangement.
package main

import (
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

	"github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/minimize"
	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
	"github.com/blaktron/agentry-cli/runnerimage/probe"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

const usageText = `usage: agentry-runner <mode> [flags]

  tools --spec <path>
        serve the §R3 MCP tools on stdio, configured by the compiled spec file
  proxy --allow <host[:port],...> [--addr :3128] [--forward LISTEN=TARGET]
        run the CONNECT egress allowlist proxy (llm-proxy); --forward adds
        a plain TCP pipe (the host's door to the gateway, which never
        leaves the run network)
  minimize <command> [<command>...]
        (docker build only) delete every executable except the named commands
  probe --url <url> [--token-env MCP_GATEWAY_TOKEN] [--timeout 60] [--list-tools]
        wait until the MCP gateway answers an authed initialize with a 2xx
`

func usageExit() {
	fmt.Fprint(os.Stderr, usageText)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usageExit()
	}
	var err error
	switch os.Args[1] {
	case "tools":
		err = runTools(os.Args[2:])
	case "proxy":
		err = runProxy(os.Args[2:])
	case "probe":
		err = runProbe(os.Args[2:])
	case "decode":
		err = runDecode(os.Args[2:])
	case "minimize":
		// agentry-cli#9: `docker build` runs this once to strip every
		// executable but the declared commands from the image.
		if len(os.Args) < 3 {
			err = errors.New("minimize: at least one command name is required")
		} else {
			err = minimize.Minimize("/", os.Args[2:], os.Stderr)
		}
	default:
		fmt.Fprintf(os.Stderr, "agentry-runner: unknown mode %q\n", os.Args[1])
		usageExit()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentry-runner: %v\n", err)
		os.Exit(1)
	}
}

// runDecode turns the documents under --in into text under --out, with the
// report beside them (plan knowledge.md §4.2). The caller runs it in the
// decode image: no network, no capabilities, --in read-only.
func runDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	in := fs.String("in", "", "the directory of documents to decode (read-only)")
	out := fs.String("out", "", "the directory the texts and decode.json are written to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("decode: --in and --out are required")
	}
	rep, err := decode.Decode(context.Background(), *in, *out, decode.Tools{}, decode.Limits{})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "decode: %d files\n", len(rep.Files))
	return nil
}

// runTools reads the compiled tool spec and serves MCP on stdio until the
// gateway closes stdin.
func runTools(args []string) error {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	specPath := fs.String("spec", "", "path to the compiled toolspec.json")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *specPath == "" {
		fmt.Fprintln(os.Stderr, "agentry-runner tools: --spec is required")
		os.Exit(2)
	}
	f, err := os.Open(*specPath)
	if err != nil {
		return fmt.Errorf("open tool spec: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	// The spec file is the compiler↔server contract: a field this binary
	// does not know means a newer compiler talking to a stale image, and
	// that must fail loudly here, not subtly in a tool call (spec.go's
	// SpecVersion reasoning, applied to individual fields).
	dec.DisallowUnknownFields()
	var spec toolsrv.Spec
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("parse tool spec %s: %w", *specPath, err)
	}
	return toolsrv.Serve(os.Stdin, os.Stdout, os.Stderr, &spec)
}

// runProxy starts the llm-proxy. The allowlist is required: an empty one
// would deny all model traffic, and that is a broken launcher, not a
// policy anyone wrote.
func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	allow := fs.String("allow", "", "comma-separated host[:port] CONNECT allowlist")
	plain := fs.String("plain-http", "", "comma-separated host[:port] plain-http model endpoints (DR-45); each must also be on --allow")
	addr := fs.String("addr", ":3128", "listen address")
	type forward struct{ listen, target string }
	var forwards []forward
	fs.Func("forward", "LISTEN=TARGET plain TCP pipe (repeatable)", func(v string) error {
		listen, target, err := netproxy.ParseForward(v)
		if err != nil {
			return err
		}
		forwards = append(forwards, forward{listen, target})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	for _, f := range forwards {
		l, err := net.Listen("tcp", f.listen)
		if err != nil {
			return fmt.Errorf("forward %s: %w", f.listen, err)
		}
		fmt.Fprintf(os.Stderr, "forward %s -> %s\n", f.listen, f.target)
		go func() {
			if err := netproxy.Forward(context.Background(), l, f.target, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "forward %s: %v\n", f.listen, err)
			}
		}()
	}
	var hosts []string
	for _, h := range strings.Split(*allow, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "agentry-runner proxy: --allow must list at least one host[:port]")
		os.Exit(2)
	}
	var plainHosts []string
	for _, h := range strings.Split(*plain, ",") {
		if h = strings.TrimSpace(h); h != "" {
			plainHosts = append(plainHosts, h)
		}
	}
	p := &netproxy.Proxy{Allow: hosts, PlainHTTP: plainHosts, Logw: os.Stderr}
	return p.ListenAndServe(*addr)
}

// probeInitBody is the probe's MCP initialize. A 2xx answer is proof the
// gateway is up and accepted our Bearer token; the body may be SSE-framed,
// so it is deliberately not parsed (§R6 health probe).
const probeInitBody = probe.InitBody

// runProbe retries the initialize POST every 2s until the deadline. It
// exists because "the gateway container started" and "the gateway is
// accepting authed MCP traffic" are different moments, and the agent
// container must only start after the second one (§R6 execute order).
func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	rawURL := fs.String("url", "", "gateway MCP endpoint URL")
	tokenEnv := fs.String("token-env", "MCP_GATEWAY_TOKEN", "env var holding the gateway Bearer token (empty sends no Authorization)")
	timeout := fs.Int("timeout", 60, "seconds to keep retrying")
	listTools := fs.Bool("list-tools", false, `once the gateway answers, print the tools it serves as one JSON line, {"tools":[…]} (a session's permit list, agentry-cli#578)`)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *rawURL == "" {
		fmt.Fprintln(os.Stderr, "agentry-runner probe: --url is required")
		os.Exit(2)
	}
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	var lastErr error
	for {
		lastErr = probeOnce(client, *rawURL, token)
		if lastErr == nil {
			if !*listTools {
				return nil
			}
			names, err := probe.ListTools(client, *rawURL, token)
			if err != nil {
				return fmt.Errorf("list the gateway's tools: %w", err)
			}
			return json.NewEncoder(os.Stdout).Encode(map[string][]string{"tools": names})
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gateway not ready after %ds: %w", *timeout, lastErr)
}

func probeOnce(client *http.Client, url, token string) error {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(probeInitBody))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused across retries; the
	// content is uninteresting by design.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("gateway answered %s", resp.Status)
	}
	return nil
}
