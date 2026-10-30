// A declared server's manifest tool filter becomes the gateway's allowlist
// (tools.yaml), which the gateway matches against the tool names the server
// lists exactly, case included. A filter entry that differs from the real name
// only in case therefore matches nothing, and the tool is dropped without a
// word. The runner learns a server's real names only from the server, so the
// check runs when the gateway has started: it reads how many tools the gateway
// enabled for each filtered server, and for one that enabled fewer than its
// filter names, it asks the server itself for its tool list (a throwaway
// container with no network) to say which entry differs only in case, and what
// to write. A case-only entry stops the run before the agent starts; an entry
// that matches no tool at all is reported and the run goes on, as it always
// has.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// toolCountRE is the count the gateway logs for a server after listing its
// tools: "  > <name>: (3 tools) (1 prompts)" (docker/mcp-gateway v0.44.3,
// pkg/gateway/capabilitites.go). The count is of the tools its allowlist let
// through; a server with none left has no such line.
var toolCountRE = regexp.MustCompile(`\((\d+) tools\)`)

// gatewayToolCount is the number of tools the gateway's startup log says it
// enabled for the server called name, 0 when it logs none.
func gatewayToolCount(logs, name string) int {
	prefix := "> " + name + ":"
	for _, line := range strings.Split(logs, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		if m := toolCountRE.FindStringSubmatch(t); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// toolListTimeout bounds the throwaway container that lists a server's tools.
const toolListTimeout = 45 * time.Second

// maxListedTools bounds the names read from a server's listing.
const maxListedTools = 4096

// toolLister lists a package server's tools; a test replaces it.
type toolLister func(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, runID string, srv spec.MCPServerSpec) ([]string, error)

// checkToolFilters is the check described above, over the plan's declared
// servers, against the gateway's startup log. It returns the error that stops
// the run, and writes any other finding to the run's stderr.
func checkToolFilters(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, runID, logs string, list toolLister) error {
	for _, srv := range p.Plan.MCPServers {
		if srv.FromImage() || len(srv.Tools) == 0 {
			continue
		}
		wanted := slices.Compact(slices.Sorted(slices.Values(srv.Tools)))
		enabled := gatewayToolCount(logs, srv.Name)
		if enabled >= len(wanted) {
			continue
		}
		var diff spec.ToolFilterDiff
		var listErr error
		if !srv.Remote() {
			var listed []string
			if listed, listErr = list(ctx, d, p, rd, runID, srv); listErr == nil {
				diff = spec.DiffToolFilter(wanted, listed)
			}
		}
		if msg := diff.CaseMessage(); msg != "" {
			return receipt.WithReason(fmt.Errorf("declared MCP server %s: its tool filter has an entry the gateway would not serve: %s",
				srv.Ref(), msg), receipt.ReasonMCPStartFailed, "", srv.SourceID())
		}
		if p.Stderr == nil {
			continue
		}
		switch {
		case len(diff.Absent) > 0:
			_, _ = fmt.Fprintf(p.Stderr, "warning: declared MCP server %s: its tool filter names %s, which the server does not list, so the agent runs without %s\n",
				srv.Ref(), quoteNames(diff.Absent), pluralTool(len(diff.Absent)))
		default:
			why := "the server's tool list could not be read to say which"
			if srv.Remote() {
				why = "a remote server's tool list is not read to say which"
			} else if listErr == nil {
				why = "every entry names a tool the server lists, so a duplicate or a prompt-only listing explains the count"
			}
			_, _ = fmt.Fprintf(p.Stderr, "warning: declared MCP server %s: the gateway enabled %d of the %d tools its filter names (it compares names exactly, case included; %s)\n",
				srv.Ref(), enabled, len(wanted), why)
		}
	}
	return nil
}

func pluralTool(n int) string {
	if n == 1 {
		return "that tool"
	}
	return "those tools"
}

func quoteNames(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		if len(n) > 128 {
			n = n[:128] + "..."
		}
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}

// listServerTools starts the declared package server alone, in a throwaway
// container with the server's own image, command and store (read-only), no
// network, and the run's container posture, speaks MCP to it over stdio and
// returns the names it lists. Each secret the server declares is given a
// placeholder, enough for a server that checks one is set at start and never a
// real value. A server that cannot start that way gives an error, which the
// caller treats as "not known".
func listServerTools(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, runID string, srv spec.MCPServerSpec) ([]string, error) {
	if srv.StoreSubdir == "" || len(srv.Command) == 0 || srv.Image == "" || rd.serversDir == "" {
		return nil, errors.New("the server is not a package server of this run's store")
	}
	env := map[string]string{}
	for k, v := range srv.Env {
		env[k] = v
	}
	for _, name := range srv.Secrets {
		env[name] = "agentry-tool-list-placeholder-not-a-key"
	}
	name := containerName(runID, "toollist")
	run := hardened(dockerx.RunSpec{
		Name:  name,
		Image: srv.Image,
		// The image's entrypoint is the interpreter or nothing; the gateway
		// runs the server's command after it, so does this.
		Argv: append(slices.Clone(srv.Command), srv.Args...),
		Env:  env,
		Mounts: []dockerx.Mount{{
			Source: filepath.Join(nativePath(rd.serversDir), srv.StoreSubdir), Target: spec.MCPStorePrefix + "/" + srv.StoreSubdir,
			ReadOnly: true, Relabel: true,
		}},
		Network:     "none",
		Interactive: true,
		AutoRemove:  true,
		NoLog:       true,
		Runtime:     p.Plan.Runtime(),
		Labels:      runLabels(runID),
	}, p.limits())
	// What the docker filter gives a declared server in a run: every
	// capability dropped but the one that reads its package past the store's
	// mode.
	run.CapAdd = sockfilter.ServerReadCapabilities()

	ctx, cancel := context.WithTimeout(ctx, toolListTimeout)
	defer cancel()
	// Stdin is a pipe of files, not an io.Pipe: the docker client then gets the
	// descriptor itself, and its Wait returns when the client exits instead of
	// waiting for a copy loop that nothing ends.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW := io.Pipe()
	var stderr tailBuffer
	done := make(chan error, 1)
	go func() {
		code, err := dockerx.RunForegroundUnder(ctx, d, run, "MCP server tool listing", stdinR, stdoutW, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("the container exited %d%s", code, stderr.suffix())
		} else if err != nil {
			err = fmt.Errorf("%w%s", err, stderr.suffix())
		}
		_ = stdoutW.CloseWithError(io.EOF)
		done <- err
	}()
	defer func() {
		cancel()
		_ = stdinW.Close()
		_ = stdinR.Close()
		go func() { _, _ = io.Copy(io.Discard, stdoutR) }()
		select {
		case err := <-done:
			_ = err // only that the container ended matters here
		case <-time.After(15 * time.Second):
		}
		_ = d.RemoveForce(context.Background(), name)
	}()

	// The three messages go out together: a server reads them in order, so
	// the listing is asked for after it has been initialized. A write that
	// nobody reads ends when the pipe closes.
	go func() {
		for _, m := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentry","version":"1"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		} {
			if _, err := io.WriteString(stdinW, m+"\n"); err != nil {
				return
			}
		}
	}()
	names, err := readToolListing(stdoutR)
	if err != nil {
		select {
		case derr := <-done:
			done <- derr
			if derr != nil {
				err = fmt.Errorf("%w (%v)", err, derr)
			}
		default:
		}
	}
	return names, err
}

// tailBuffer keeps the last bytes written to it, for an error message.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

const tailKeep = 400

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailKeep {
		t.b = t.b[len(t.b)-tailKeep:]
	}
	return len(p), nil
}

// suffix is ": <the tail, one line>" or "".
func (t *tailBuffer) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := strings.Join(strings.Fields(string(t.b)), " "); s != "" {
		return ": " + s
	}
	return ""
}

// readToolListing reads a server's stdout until its answer to tools/list
// (id 2), ignoring every other line, and returns the names in it.
func readToolListing(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil || string(msg.ID) != "2" {
			continue
		}
		if msg.Error != nil {
			return nil, errors.New("the server refused tools/list")
		}
		names := make([]string, 0, len(msg.Result.Tools))
		for _, t := range msg.Result.Tools {
			if t.Name == "" || len(names) >= maxListedTools {
				continue
			}
			names = append(names, t.Name)
		}
		return names, nil
	}
	return nil, errors.New("the server did not answer tools/list")
}
