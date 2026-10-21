package mcpinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestTheServerLockKeepsOneWriter: while one holder has a server's lock, a
// second waits for it, until its context ends; Installing says an install is
// under way for that server alone, and the lock file stays outside the
// server's directory, whose tree the store's digest covers.
func TestTheServerLockKeepsOneWriter(t *testing.T) {
	store := t.TempDir()
	unlock, err := LockServer(t.Context(), store, "pypi-a")
	if err != nil {
		t.Fatal(err)
	}
	if !Installing(store, "pypi-a") || Installing(store, "pypi-b") {
		t.Fatalf("installing: a %v, b %v; want a alone", Installing(store, "pypi-a"), Installing(store, "pypi-b"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*lockPoll)
	defer cancel()
	if _, err := LockServer(ctx, store, "pypi-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second writer took a held lock: %v", err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := LockServer(t.Context(), store, "pypi-a")
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	time.Sleep(2 * lockPoll)
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting writer never took the lock once it was let go")
	}
	if Installing(store, "pypi-a") {
		t.Error("a lock let go still reads as an install under way")
	}
	if _, err := os.Stat(filepath.Join(store, "pypi-a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock made the server's directory: %v", err)
	}
	if Installing(filepath.Join(store, "missing"), "pypi-a") {
		t.Error("a store that does not exist reads as installing")
	}
}

// TestInstalledIsTheStoredPin: the store holds a server once its record
// names the same package and version; a remote server needs no install.
func TestInstalledIsTheStoredPin(t *testing.T) {
	store := t.TempDir()
	srv := manifest.MCPServer{PyPI: "office-word-mcp-server", Version: "1.1.11"}
	if Installed(store, srv) {
		t.Fatal("an empty store holds the server")
	}
	rec := `{"source":"pypi","package":"office-word-mcp-server","version":"1.1.10","id":"pypi-office-word-mcp-server"}`
	if err := os.WriteFile(filepath.Join(store, "pypi-office-word-mcp-server.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if Installed(store, srv) {
		t.Error("a record of another version holds the pin")
	}
	srv.Version = "1.1.10"
	if !Installed(store, srv) {
		t.Error("the record of the pin does not hold it")
	}
	if !Installed(store, manifest.MCPServer{URL: "https://mcp.example.com/mcp"}) {
		t.Error("a remote server reads as not installed")
	}
}

// TestACancelledInstallLeavesNothing: an install cancelled while its
// builder runs removes the builder, the proxy and the install's network,
// under a context that outlives the cancel, and records nothing.
func TestACancelledInstallLeavesNothing(t *testing.T) {
	started := make(chan struct{})
	fake := withRunner(&dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"agentry-mcpinstall-": {Block: true, Before: func(dockerx.RunSpec) { close(started) }},
	}})
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	fake.ExistingImages["node:test"] = true
	regs := httpsRegs(policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"})
	res := []Resolved{{Source: "npm", Package: "@scope/server-x", Version: "1.0.0", ID: "npm-scope-server-x", BinName: "server-x"}}
	store := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- EnsureInstalled(ctx, fake, res, store, regs, images, policy.DockerPolicy{}, io.Discard)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the builder never started")
	}
	cancel()
	select {
	case err := <-done:
		_ = err // cancelled: the install's error is the cancel's
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled install did not return")
	}
	removed := map[string]bool{}
	for _, c := range fake.Calls {
		if c.Op == "RemoveForce" {
			removed[c.Arg] = true
		}
	}
	if !removed["agentry-mcpinstall-npm-scope-server-x"] || !removed[installProxyName("npm-scope-server-x")] {
		t.Errorf("removed %v: want the builder and the proxy", removed)
	}
	for _, n := range fake.NetworkState() {
		if strings.HasPrefix(n.Name, "agentry-mcpinstall-") {
			t.Errorf("the install's network %s is left", n.Name)
		}
	}
	if Installed(store, manifest.MCPServer{NPM: "@scope/server-x", Version: "1.0.0"}) {
		t.Error("a cancelled install recorded the server as installed")
	}
}
