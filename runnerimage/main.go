//go:build ignore

// The runner-image entrypoint: one stdlib-only binary
// serving every in-container role: `tools` (the MCP stdio server the
// gateway launches per run), `proxy` (the llm-proxy CONNECT
// allowlist), and `probe` (the engine's gateway health check in the
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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/applink"
	"github.com/blaktron/agentry-cli/runnerimage/bridge"
	"github.com/blaktron/agentry-cli/runnerimage/confine"
	"github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/minimize"
	"github.com/blaktron/agentry-cli/runnerimage/netproxy"
	"github.com/blaktron/agentry-cli/runnerimage/probe"
	"github.com/blaktron/agentry-cli/runnerimage/send"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
	"github.com/blaktron/agentry-cli/runnerimage/source"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

const usageText = `usage: agentry-runner <mode> [flags]

  tools --spec <path>
        serve the MCP tools on stdio, configured by the compiled spec file
  proxy --allow <host[:port],...> [--addr :3128] [--forward LISTEN=TARGET] [--public-only]
        run the CONNECT egress allowlist proxy (llm-proxy); --forward adds
        a plain TCP pipe (the host's door to the gateway, which never
        leaves the run network)
  bridge --url <https address> [--token-file <path> [--oauth]]
        serve one remote MCP server on stdio, forwarding each message to it
        over streamable HTTP (or HTTP+SSE); the bearer, when there is one,
        is read from --token-file at every request
  applink --set main|assistant
        (a persistent agent's app link) serve the set's tools on stdio,
        forwarding each call to the supervisor's socket at
        /run/agentry/app.sock; it has no network
  applink-relay [--owner uid:gid]
        (where the engine's daemon runs in a VM) serve the app link's socket
        at /run/agentry/app.sock and carry each tool call over stdio to the
        CLI, which answers it from the supervisor; it has no network
  hostlink
        (the host link's container) serve the target-host tools on stdio,
        from /run/agentry/hostspec.json and the run's /run/agentry/hosts;
        every SSH session is the host link's own, through one relay a host
  source --kind repository|url --url <https address> [--ref <ref>] --out <dir> --work <dir> --proxy <url>
        [--max-files N] [--max-bytes N] [--token-stdin]
        (the files container's fetch step) fetch an input source into --out as
        plain files, the report into --work; the access token, when there is
        one, is read from stdin, never from an argument or the environment
  deliver --job <dir> --staged <dir> --work <dir> --proxy <url>
        (the delivery step) send a run's deliverables to a destination from the
        job file in --job, the staged files read-only in --staged, the report
        into --work; the credential is read from stdin as JSON, never from an
        argument or the environment
  confine --write <dir> [--write <dir>...] -- <program> [<arg>...]
        (the tools server's) run a declared command, able to write only
        beneath the named directories
  minimize <command> [<command>...]
        (docker build only) delete every executable except the named commands
  docker-filter --config <file> --listen <socket> --log <file>
        (Docker Desktop's per-run filter container) serve the
        run's docker filter on --listen in front of /var/run/docker.sock
  probe --url <url> [--token-env MCP_GATEWAY_TOKEN] [--timeout 60] [--list-tools]
        wait until the MCP gateway answers an authed initialize with a 2xx
`

func usageExit() {
	fmt.Fprint(os.Stderr, usageText)
	os.Exit(2)
}

func main() {
	// As OpenSSH's SSH_ASKPASS in the host link's image (a link named
	// agentry-askpass): print the one password or
	// passphrase file the host link named, and nothing else.
	if filepath.Base(os.Args[0]) == filepath.Base(hostlink.AskpassProgram) {
		os.Exit(hostlink.Askpass(os.Stdout, os.Stderr))
	}
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
	case "docker-filter":
		err = runDockerFilter(os.Args[2:])
	case "decode":
		err = runDecode(os.Args[2:])
	case "bridge":
		err = runBridge(os.Args[2:])
	case "applink":
		err = runAppLink(os.Args[2:])
	case "applink-relay":
		err = runAppLinkRelay(os.Args[2:])
	case "hostlink":
		if len(os.Args) != 2 {
			usageExit()
		}
		err = hostlink.Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr, hostlink.DefaultConfig())
	case "source":
		err = runSource(os.Args[2:])
	case "deliver":
		err = runDeliver(os.Args[2:])
	case confine.Role:
		// run_command's trampoline. It never returns,
		// and the command never runs unconfined.
		confine.Trampoline(os.Args[2:])
	case "minimize":
		// `docker build` runs this once to strip every
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

// runDockerFilter is the filter container's role where the daemon runs in
// a VM (Docker Desktop): the run's docker filter, the
// same package the CLI serves in process on Linux, read from the file the
// engine wrote, in front of the daemon's socket, on a socket in the volume
// the gateway mounts. It prints sockfilter.SidecarReady once it listens and
// serves until it is stopped.
func runDockerFilter(args []string) error {
	fs := flag.NewFlagSet("docker-filter", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "the filter's configuration (sockfilter.MarshalConfig)")
	listen := fs.String("listen", "", "the socket to serve the filter on")
	logPath := fs.String("log", "", "the decision log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *listen == "" || *logPath == "" || fs.NArg() != 0 {
		return errors.New("docker-filter: --config, --listen and --log are required, and nothing else")
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := sockfilter.UnmarshalConfig(raw)
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("docker-filter: the decision log: %w", err)
	}
	defer func() { _ = logf.Close() }()
	cfg.Listen, cfg.Upstream, cfg.Log = *listen, "/var/run/docker.sock", logf
	f, err := sockfilter.Start(cfg)
	if err != nil {
		return err
	}
	fmt.Println(sockfilter.SidecarReady)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	return f.Close()
}

// maxTokenBytes bounds the access token read from stdin.
const maxTokenBytes = 16 << 10

// runSource is an input source's fetch step: in the files container, on a
// network whose only route is --proxy, with the token on stdin. The report is
// written to
// --work whatever happens; the exit is non-zero when the fetch could not
// be made, and zero with a refusal in the report when a cap refused it.
func runSource(args []string) error {
	fs := flag.NewFlagSet("source", flag.ContinueOnError)
	kind := fs.String("kind", "", "the source kind (repository, url)")
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

// runDeliver is the delivery step: in the runner container, on a network whose
// only route is --proxy, with the credential on stdin. The report is written to
// --work whatever happens; the exit is non-zero when the delivery failed or
// could not be reported.
func runDeliver(args []string) error {
	fs := flag.NewFlagSet("deliver", flag.ContinueOnError)
	jobDir := fs.String("job", "", "the directory holding job.json")
	staged := fs.String("staged", "", "the directory of staged deliverables (read-only)")
	work := fs.String("work", "", "the directory delivery.json is written to")
	proxy := fs.String("proxy", "", "the CONNECT proxy the request goes through")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobDir == "" || *staged == "" || *work == "" || *proxy == "" || fs.NArg() != 0 {
		return errors.New("deliver: --job, --staged, --work and --proxy are required, and nothing else")
	}
	pu, err := url.Parse(*proxy)
	if err != nil || pu.Scheme != "http" || pu.Host == "" {
		return errors.New("deliver: --proxy must be http://host:port")
	}
	cred, err := send.ReadCredential(os.Stdin)
	if err != nil {
		return fmt.Errorf("deliver: %w", err)
	}
	job, err := send.ReadJob(*jobDir)
	if err != nil {
		return fmt.Errorf("deliver: %w", err)
	}
	rep, err := send.Run(context.Background(), send.Options{Job: job, Staging: *staged, Work: *work, Cred: cred, Proxy: pu})
	if err != nil {
		return errors.New("deliver: the report could not be written")
	}
	if rep.Status != send.StatusDelivered {
		return fmt.Errorf("deliver: the delivery failed (%s)", rep.Error)
	}
	return nil
}

// runDecode turns the documents under --in into text under --out, with the
// report beside them, after checking each with
// ClamAV against the signatures under --clamav.
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
	execOnly := fs.Bool("executables-only", false, "check only the executables among the files (an archive is still checked whole)")
	keepMembers := fs.Bool("keep-members", false, "copy each archive member read as text or decoded, as it was, under originals/ in the output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("decode: --in and --out are required")
	}
	rep, err := decode.Decode(context.Background(), *in, *out, decode.Tools{}, decode.Limits{UnpackDir: *unpack, ClamDB: *clamav, Sources: sources, SourceMaxFiles: *sourceMax, ExecutablesOnly: *execOnly, KeepMembers: *keepMembers, Record: os.Stdout})
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

// runAppLink serves a persistent agent's app link on stdio: the tool set
// --set names, each call forwarded over the supervisor's socket.
func runAppLink(args []string) error {
	fs := flag.NewFlagSet("applink", flag.ContinueOnError)
	set := fs.String("set", "", "the agent's tool set: "+strings.Join(applink.Sets(), " or "))
	if err := fs.Parse(args); err != nil || *set == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "agentry-runner applink: --set is required, and nothing else")
		os.Exit(2)
	}
	return applink.Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr, *set, applink.SocketMount)
}

// runAppLinkRelay carries the app link's calls over stdio where the
// engine's daemon runs in a VM: the socket at --listen, each call a line out
// and its answer a line in.
func runAppLinkRelay(args []string) error {
	fs := flag.NewFlagSet("applink-relay", flag.ContinueOnError)
	owner := fs.String("owner", "", "the uid:gid the socket is given (the app link's user); unset leaves it the relay's")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "agentry-runner applink-relay: takes --owner and nothing else")
		os.Exit(2)
	}
	return applink.Relay(context.Background(), applink.RelayConfig{
		Listen: applink.SocketMount, Owner: *owner, In: os.Stdin, Out: os.Stdout, Log: os.Stderr,
	})
}

// runBridge serves one remote MCP server on stdio until the gateway closes
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
	networkOK := fs.Bool("network-ok", false, "dial a permitted host at a public address or one in the person's own network (private, shared, loopback; never link-local), refusing one that resolves to any other (a delivery to a destination on the person's network)")
	plain := fs.String("plain-http", "", "comma-separated host[:port] plain-http model endpoints; each must also be on --allow")
	addr := fs.String("addr", ":3128", "listen address")
	publicOnly := fs.Bool("public-only", false, "dial a permitted host only at a public address, refusing one that resolves to any other (an input source's fetch step)")
	// The browse proxy's scope: any of these makes the
	// proxy one, and --allow, --plain-http and --public-only are refused
	// beside them.
	sites := fs.String("browse-sites", "", "a browse proxy: comma-separated host[:port] sites, each with its subdomains")
	browseHosts := fs.String("browse-hosts", "", "a browse proxy: comma-separated hosts, exact or *. suffix")
	browsePublic := fs.Bool("browse-public", false, "a browse proxy: any public site")
	browsePrivate := fs.String("browse-private", "", "a browse proxy: comma-separated sites the person typed that may resolve to a LAN's or a tailnet's address")
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
	if scope := (netproxy.Scope{Sites: splitList(*sites), Hosts: splitList(*browseHosts), Public: *browsePublic, Private: splitList(*browsePrivate)}); len(scope.Sites)+len(scope.Hosts) > 0 || scope.Public {
		if *allow != "" || *plain != "" || *publicOnly || *networkOK {
			fmt.Fprintln(os.Stderr, "agentry-runner proxy: a browse proxy takes no --allow, --plain-http, --public-only or --network-ok")
			os.Exit(2)
		}
		if len(scope.Private) > 0 && len(scope.Sites) == 0 {
			fmt.Fprintln(os.Stderr, "agentry-runner proxy: --browse-private names sites, so it needs --browse-sites")
			os.Exit(2)
		}
		p := &netproxy.Proxy{Scope: &scope, Logw: os.Stderr}
		return p.ListenAndServe(*addr)
	}
	hosts := splitList(*allow)
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
	if *networkOK && *publicOnly {
		fmt.Fprintln(os.Stderr, "agentry-runner proxy: --network-ok and --public-only exclude each other")
		os.Exit(2)
	}
	p := &netproxy.Proxy{Allow: hosts, PlainHTTP: plainHosts, PublicOnly: *publicOnly, NetworkOK: *networkOK, Logw: os.Stderr}
	return p.ListenAndServe(*addr)
}

// splitList is a comma-separated flag's entries, trimmed, empty ones left
// out.
func splitList(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// runProbe retries the initialize POST every 2s until the deadline. It
// exists because "the gateway container started" and "the gateway is
// accepting authed MCP traffic" are different moments, and the agent
// container must only start after the second one.
func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	rawURL := fs.String("url", "", "gateway MCP endpoint URL")
	tokenEnv := fs.String("token-env", "MCP_GATEWAY_TOKEN", "env var holding the gateway Bearer token (empty sends no Authorization)")
	timeout := fs.Int("timeout", 60, "seconds to keep retrying")
	listTools := fs.Bool("list-tools", false, `once the gateway answers, print the tools it serves as one JSON line, {"tools":[…]} (a session's permit list)`)
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
