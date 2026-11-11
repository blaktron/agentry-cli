package main

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// `agentry agent connect` makes the agent's connection for its remote
// OAuth server once, keyed to the agent, prints no token, says it is
// connected the second time, and --remove removes it.
func TestAgentConnectKeepsAConnectionForTheAgent(t *testing.T) {
	v, _, _, opened := connectionFixture(t)
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	srv := manifest.MCPServer{URL: v.ServerURL(), Auth: manifest.AuthOAuth, Scopes: []string{"read"}}
	if err := persistent.SaveConfig(home, "assistant", persistent.Config{Version: persistent.ConfigVersion, Enabled: true, Servers: []manifest.MCPServer{srv}}); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, nil, "agent", "connect", "assistant")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, errOut)
	}
	if *opened != 1 || !strings.Contains(errOut, "The assistant agent needs you to sign in to "+v.ServerURL()) || !strings.Contains(errOut, "kept on this machine for the agent") {
		t.Fatalf("opened %d\n%s\n%s", *opened, out, errOut)
	}
	store := connection.Open(home, nil)
	c, err := store.Get(persistent.ConnectionKey("assistant", srv))
	if err != nil || c.Issuer != v.URL {
		t.Fatalf("the agent's connection: %+v (%v)", c, err)
	}
	if strings.Contains(out+errOut, "PROBE-") || strings.Contains(out+errOut, probeClientSecret) {
		t.Fatalf("a secret was printed:\n%s\n%s", out, errOut)
	}
	out, _, err = runCLI(t, nil, "agent", "connect", "assistant")
	if err != nil || *opened != 1 || !strings.Contains(out, v.ServerURL()+": connected") {
		t.Fatalf("second connect: %q (%v), opened %d", out, err, *opened)
	}
	out, _, err = runCLI(t, nil, "agent", "connect", "assistant", "--remove")
	if err != nil || !strings.Contains(out, "removed the assistant agent's connection to "+v.ServerURL()) {
		t.Fatalf("remove: %q (%v)", out, err)
	}
	if _, err := store.Get(persistent.ConnectionKey("assistant", srv)); err == nil {
		t.Fatal("the connection stayed")
	}
}

// An agent with no server that signs in has nothing to connect, and a
// server address it does not have is refused.
func TestAgentConnectWithNothingToConnect(t *testing.T) {
	_, _, _, _ = connectionFixture(t)
	out, _, err := runCLI(t, nil, "agent", "connect", "assistant")
	if err != nil || !strings.Contains(out, "nothing to connect") {
		t.Fatalf("%q (%v)", out, err)
	}
	if _, _, err := runCLI(t, nil, "agent", "connect", "assistant", "https://nowhere.example/mcp"); err == nil {
		t.Fatal("an address the agent does not have was connected")
	}
}
