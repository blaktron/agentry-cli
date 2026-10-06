package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// A remote server's bearer (plan mcp-oauth-connections.md §5;
// agentry-cli#663) is resolved as any declared secret is, then written into
// the server's own connection folder for its bridge, never into the
// gateway's secrets file; the receipts mask it, and teardown deletes it.

const remoteBearer = "ghp-remote-bearer-0123456789"

func withRemoteServer(p Params) (Params, string) {
	srv := spec.MCPServerSpec{
		Name: "mcp-url-api-githubcopilot-com-mcp", Source: spec.SourceURL,
		Package: "https://api.githubcopilot.com/mcp/",
		Egress:  []string{"api.githubcopilot.com"},
		Secrets: []string{"REMOTE_TOKEN"},
		Auth:    "secret",
	}
	plan := *p.Plan
	plan.MCPServers = append([]spec.MCPServerSpec(nil), srv)
	p.Plan = &plan
	return p, srv.Name
}

func TestRemoteBearerReachesItsBridgeAloneAndDiesWithTheRun(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p, name := withRemoteServer(p)
	t.Setenv("REMOTE_TOKEN", remoteBearer)
	cfg := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg")
	bearer := filepath.Join(cfg, spec.ConnectionsDir, name, spec.BearerFile)

	var secretsFile, bearerFile string
	var bearerMode, dirMode os.FileMode
	f.Foreground["-probe"].Before = func(dockerx.RunSpec) {
		// The gateway is up: what it and the bridge were given is on disk.
		b, _ := os.ReadFile(filepath.Join(cfg, "gateway", "secrets.env")) //nolint:gosec // the test's run dir
		secretsFile = string(b)
		b, _ = os.ReadFile(bearer) //nolint:gosec // the test's run dir
		bearerFile = string(b)
		if fi, err := os.Stat(bearer); err == nil {
			bearerMode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Dir(bearer)); err == nil {
			dirMode = fi.Mode().Perm()
		}
	}
	// A gateway log line that quotes the value must not reach a receipt.
	f.LogsByName["agentry-cafe0000-gateway"] = "- " + name + ": Authorization: Bearer " + remoteBearer + "\n"

	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	if bearerFile != remoteBearer {
		t.Fatalf("the bridge's credential file holds %q", bearerFile)
	}
	if bearerMode != 0o600 || dirMode != 0o700 {
		t.Fatalf("credential file mode %v in a folder of mode %v, want 0600 in 0700", bearerMode, dirMode)
	}
	if strings.Contains(secretsFile, remoteBearer) || strings.Contains(secretsFile, compile.ServerSecretKey(name, "REMOTE_TOKEN")) {
		t.Fatalf("the gateway's secrets file holds the remote server's bearer:\n%s", secretsFile)
	}
	if !strings.Contains(secretsFile, compile.SecretKey("ENGINE_TEST_TOKEN")+"=") {
		t.Fatalf("the release's own secret is still the gateway's:\n%s", secretsFile)
	}
	if _, err := os.Stat(filepath.Join(cfg, spec.ConnectionsDir)); !os.IsNotExist(err) {
		t.Fatalf("the connection folders outlived the run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, "gateway.log"))
	if err != nil || strings.Contains(string(b), remoteBearer) || !strings.Contains(string(b), "Bearer [redacted]") {
		t.Fatalf("gateway.log = %q (%v)", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.OutDir, "result.json")); strings.Contains(string(b), remoteBearer) { //nolint:gosec // the test's out dir
		t.Fatal("result.json holds the bearer")
	}
}

func TestAKeptRunDirKeepsNoRemoteBearer(t *testing.T) {
	// A run dir kept for debugging still loses its credentials.
	p, f := fixture(t)
	happyFake(f)
	p, name := withRemoteServer(p)
	p.KeepRunDir = true
	t.Setenv("REMOTE_TOKEN", remoteBearer)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg")
	if _, err := os.Stat(filepath.Join(cfg, spec.ConnectionsDir, name)); !os.IsNotExist(err) {
		t.Fatalf("a kept run dir kept the bearer: %v", err)
	}
}

func TestAMissingRemoteBearerRefusesTheRunByName(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p, _ = withRemoteServer(p)
	_ = os.Unsetenv("REMOTE_TOKEN")
	_, err := Run(context.Background(), f, p)
	if err == nil || !strings.Contains(err.Error(), "REMOTE_TOKEN") {
		t.Fatalf("a missing bearer must refuse the run naming it: %v", err)
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy {
		t.Fatalf("want exit %d, got %v", ExitPolicy, err)
	}
}

func TestAnOptionalRemoteBearerIsWrittenEmpty(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p, name := withRemoteServer(p)
	plan := *p.Plan
	plan.OptionalSecrets = []string{"REMOTE_TOKEN"}
	p.Plan = &plan
	_ = os.Unsetenv("REMOTE_TOKEN")
	bearer := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", spec.ConnectionsDir, name, spec.BearerFile)
	seen := "unread"
	f.Foreground["-probe"].Before = func(dockerx.RunSpec) {
		if b, err := os.ReadFile(bearer); err == nil { //nolint:gosec // the test's run dir
			seen = string(b)
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	if seen != "" {
		t.Fatalf("an absent optional bearer was written as %q, want an empty file (no Authorization header)", seen)
	}
}

// TestNoPackageServerNoStore is agentry-cli#329, which a remote-only
// release (agentry-cli#663) meets on every run: with no package server the
// signature's store is neither required, nor labelled, nor named, whether
// the release declares remote servers or none at all.
func TestNoPackageServerNoStore(t *testing.T) {
	for name, remote := range map[string]bool{"no servers": false, "a remote server": true} {
		t.Run(name, func(t *testing.T) {
			p, _ := fixture(t)
			if remote {
				p, _ = withRemoteServer(p)
			}
			p.ServersDir = filepath.Join(t.TempDir(), "servers-that-do-not-exist")
			td := &teardown{stderr: io.Discard}
			defer td.run()
			rd, err := prepareRunDirs(p, "cafe0001", td)
			if err != nil {
				t.Fatal(err)
			}
			if rd.serversDir != "" {
				t.Fatalf("serversDir = %q, want none", rd.serversDir)
			}
			for _, tgt := range relabelTargets(rd) {
				if tgt.dir == "" || strings.Contains(tgt.dir, "servers-that-do-not-exist") {
					t.Fatalf("relabel target %+v", tgt)
				}
			}
		})
	}
}
