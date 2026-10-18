package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADestinationsFileExcludesTheSavedDestinationFlags: a host hands a run its
// destinations in the file alone.
func TestADestinationsFileExcludesTheSavedDestinationFlags(t *testing.T) {
	isolateEnv(t)
	for _, flag := range []string{"--no-destination", "--destination=n8n"} {
		_, _, err := runCLI(t, nil, "run", "acme/digest", "--destinations-file", "d.json", flag, "--plan")
		if err == nil || !strings.Contains(err.Error(), "excludes") {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

// TestTheHostFileTakesItsCredentialsOutOfTheEnvironment: once read, no variable
// that held a credential is left for a process started from here to inherit,
// and a file that is refused leaves the variables alone.
func TestTheHostFileTakesItsCredentialsOutOfTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "destinations.json")
	body := `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/in/abc","secretEnv":"AGENTRY_DEST_SECRET_0","authorizationEnv":"AGENTRY_DEST_AUTH_0"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRY_DEST_SECRET_0", "whsec_ENVsecret0123456789")
	t.Setenv("AGENTRY_DEST_AUTH_0", "Bearer ENVauthorization0123456789")
	t.Setenv("AGENTRY_DEST_UNNAMED", "a value no file names")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(strings.Replace(body, "https://hooks.example.com/in/abc", "https://10.0.0.5/in/abc", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := destinationsFromHostFile(bad); err == nil {
		t.Fatal("a private address was accepted")
	}
	if os.Getenv("AGENTRY_DEST_SECRET_0") == "" {
		t.Error("a refused file removed a variable")
	}
	dests, err := destinationsFromHostFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 || dests[0].Secret != "whsec_ENVsecret0123456789" || dests[0].Authorization != "Bearer ENVauthorization0123456789" || !dests[0].Hosted {
		t.Errorf("destinations = %+v", dests)
	}
	for _, name := range []string{"AGENTRY_DEST_SECRET_0", "AGENTRY_DEST_AUTH_0", "AGENTRY_DEST_UNNAMED"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s is still in the environment (%d bytes)", name, len(v))
		}
	}
}
