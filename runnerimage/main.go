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
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/bridge"
	"github.com/blaktron/agentry-cli/runnerimage/confine"
	"github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/minimize"
	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
	"github.com/blaktron/agentry-cli/runnerimage/probe"
	"github.com/blaktron/agentry-cli/runnerimage/source"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

const usageText = `usage: agentry-runner <mode> [flags]

  tools --spec <path>
        serve the §R3 MCP tools on stdio, configured by the compiled spec file
  proxy --allow <host[:port],...> [--addr :3128] [--forward LISTEN=TARGET] [--public-only]
        run the CONNECT egress allowlist proxy (llm-proxy); --forward adds
        a plain TCP pipe (the host's door to the gateway, which never
        leaves the run network)
  bridge --url <https address> [--token-file <path> [--oauth]]
        serve one remote MCP server on stdio, forwarding each message to it
        over streamable HTTP (or HTTP+SSE); the bearer, when there is one,
        is read from --token-file at every request
  source --kind repository --url <https address> [--ref <ref>] --out <dir> --work <dir> --proxy <url>
        [--max-files N] [--max-bytes N] [--token-stdin]
        (the files container's fetch step) fetch an input source into --out as
        plain files, the report into --work; the access token, when there is
        one, is read from stdin, never from an argument or the environment
  confine --write <dir> [--write <dir>...] -- <program> [<arg>...]
        (the tools server's) run a declared command, able to write only
        beneath the named directories
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
	case "bridge":
		err = runBridge(os.Args[2:])
	case "source":
		err = runSource(os.Args[2:])
	case confine.Role:
		// agentry-cli#596: run_command's trampoline. It never returns,
		// and the command never runs unconfined.
		confine.Trampoline(os.Args[2:])
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

// maxTokenBytes bounds the access token read from stdin.
const maxTokenBytes = 16 << 10

// runSource is an input source's fetch step (plan input-sources.md §5,
// D6; agentry-cli#833): in the files container, on a network whose only
// route is --proxy, with the token on stdin. The report is written to
// --work whatever happens; the exit is non-zero when the fetch could not
// be made, and zero with a refusal in the report when a cap refused it.
func runSource(args []string) error {
	fs := flag.NewFlagSet("source", flag.ContinueOnError)
	kind := fs.String("kind", "", "the source kind (repository)")
	u := fs.String("url", "", "the source's https address, with no credentials")
	ref := fs.String("ref", "", "the branch, tag or full commit name (default: the default branch)")
	out := fs.String("out", "", "the empty directory the files are written to")
	work := fs.String("work", "", "the directory the pack and source.json are written to")
	proxy := fs.String("proxy", "", "the CONNECT proxy every request goes through")
	maxFiles := fs.Int("max-files", source.DefaultMaxFiles, "refuse a source of more files")
	maxBytes := fs.Int64("max-bytes", source.DefaultMaxBytes, "refuse a source of more bytes")
	tokenStdin := fs.Bool("token-stdin", false, "read the access token from stdin")
	timeout := fs.Duration("timeout", 20*time.Minute, "end the fetch after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kind == "" || *u == "" || *out == "" || *work == "" || *proxy == "" {
		return errors.New("source: --kind, --url, --out, --work and --proxy are required")
	}
	pu, err := url.Parse(*proxy)
	if err != nil || pu.Scheme != "http" || pu.Host == "" {
		return errors.New("source: --proxy must be http://host:port")
	}
	var token string
	if *tokenStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, maxTokenBytes+1))
		if err != nil {
			return fmt.Errorf("source: read the token: %w", err)
		}
		if len(b) > maxTokenBytes {
			return errors.New("source: the token is longer than any access token")
		}
		token = strings.TrimSpace(string(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	_, err = source.Fetch(ctx, source.Options{
		Kind: *kind, URL: *u, Ref: *ref, Token: token, Proxy: pu,
		Out: *out, Work: *work, MaxFiles: *maxFiles, MaxBytes: *maxBytes,
	})
	if err != nil {
		// The words are in the report; the exit says the fetch failed.
		return errors.New("source: the fetch failed (see source.json)")
	}
	return nil
}

// runDecode turns the documents under --in into text under --out, with the
// report beside them (plan knowledge.md §4.2), after checking each with
// ClamAV against the signatures under --clamav (plan file-handling.md §7).
// The caller runs it in the decode image: no network, no capabilities, --in
// and --clamav read-only.
func runDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	in := fs.String("in", "", "the directory of documents to decode (read-only)")
	out := fs.String("out", "", "the directory the texts and decode.json are written to")
	unpack := fs.String("unpack", "", "the directory archives are unpacked in (default the scratch directory)")
	clamav := fs.String("clamav", "", "the directory of ClamAV signatures every file is checked against before it is decoded (read-only; none, nothing is checked)")
	var sources []string
	fs.Func("source", "a top-level folder that is an input source: its own file cap, and only its executable and binary risky files checked (repeatable)", func(v string) error {
		if v == "" || strings.ContainsAny(v, `/\`) || v == "." || v == ".." {
			return fmt.Errorf("--source %q is not a folder name", v)
		}
		sources = append(sources, v)
		return nil
	})
	sourceMax := fs.Int("source-max-files", decode.DefaultSourceMaxFiles, "each input source's file cap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("decode: --in and --out are required")
	}
	rep, err := decode.Decode(context.Background(), *in, *out, decode.Tools{}, decode.Limits{UnpackDir: *unpack, ClamDB: *clamav, Sources: sources, SourceMaxFiles: *sourceMax})
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
	if err := toolsrv.PrepareMounts(&spec); err != nil {
		return fmt.Errorf("agentry-runner tools: %w", err)
	}
	return toolsrv.Serve(os.Stdin, os.Stdout, os.Stderr, &spec)
}

// runBridge serves one remote MCP server on stdio (plan
// mcp-oauth-connections.md §5, agentry-cli#663) until the gateway closes
// stdin.
func runBridge(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	addr := fs.String("url", "", "the remote MCP server's https address")
	tokenFile := fs.String("token-file", "", "the file holding the bearer value, read at every request (unset: auth none)")
	oauth := fs.Bool("oauth", false, "the token file holds a connection's OAuth access token (auth: oauth)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "agentry-runner bridge: --url is required")
		os.Exit(2)
	}
	b, err := bridge.New(bridge.Config{URL: *addr, TokenFile: *tokenFile, OAuth: *oauth, Log: os.Stderr}, os.Stdout)
	if err != nil {
		return err
	}
	return b.Serve(context.Background(), os.Stdin)
}

// runProxy starts the llm-proxy. The allowlist is required: an empty one
// would deny all model traffic, and that is a broken launcher, not a
// policy anyone wrote.
func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	allow := fs.String("allow", "", "comma-separated host[:port] CONNECT allowlist")
	plain := fs.String("plain-http", "", "comma-separated host[:port] plain-http model endpoints (DR-45); each must also be on --allow")
	addr := fs.String("addr", ":3128", "listen address")
	publicOnly := fs.Bool("public-only", false, "dial a permitted host only at a public address, refusing one that resolves to any other (an input source's fetch step)")
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
	p := &netproxy.Proxy{Allow: hosts, PlainHTTP: plainHosts, PublicOnly: *publicOnly, Logw: os.Stderr}
	return p.ListenAndServe(*addr)
}

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
		lastErr = probe.Healthy(client, *rawURL, token)
		if lastErr == nil {
			if !*listTools {
				return nil
			}
			// The listing has its own minute: the health deadline is spent.
			names, err := probe.ListTools(client, *rawURL, token, time.Now().Add(time.Minute))
			if err != nil {
				return fmt.Errorf("list the gateway's tools: %w", err)
			}
			return json.NewEncoder(os.Stdout).Encode(probe.Listing{Tools: names})
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gateway not ready after %ds: %w", *timeout, lastErr)
}
