// The --json surface as one contract: the root flag's help lists every
// command that answers it, from the tree, and each of those commands prints
// exactly one JSON object on stdout (login's NDJSON aside). A command that
// cannot run in a test says why.
package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRootHelpListsEveryJSONCommand: the help is generated from the marked
// commands, and the marks are the table's paths, each of which the tree
// has (a missing one would have stopped newRootCmd).
func TestRootHelpListsEveryJSONCommand(t *testing.T) {
	root := newRootCmd()
	marked := jsonCapableCommands(root)
	want := make([]string, 0, len(jsonCommands))
	for p := range jsonCommands {
		want = append(want, p)
	}
	sort.Strings(want)
	if strings.Join(marked, ",") != strings.Join(want, ",") {
		t.Fatalf("marked %v\nwant   %v", marked, want)
	}
	help := root.PersistentFlags().Lookup("json").Usage
	for _, p := range marked {
		if !strings.Contains(help, p) {
			t.Errorf("the --json help does not name %q:\n%s", p, help)
		}
	}
	if findCommand(root, "no such") != nil || findCommand(root, "schedule nothing") != nil {
		t.Error("findCommand found a command that is not there")
	}
}

// jsonFixture runs one marked command under --json in an isolated
// environment and returns its stdout; a nil fixture is a command the test
// cannot run, with the reason beside it.
type jsonFixture struct {
	run    func(t *testing.T) (stdout string)
	reason string
}

// TestEveryJSONCommandEmitsOneObject: each marked command, run under
// --json, writes exactly one JSON value on stdout and that value is an
// object, so a reader can always index a named field; a list is an object
// with a named array. Commands that need a browser, a model or a hosted
// service are named with the reason they are not run here.
func TestEveryJSONCommandEmitsOneObject(t *testing.T) {
	withRegistry := func(args ...string) func(*testing.T) string {
		return func(t *testing.T) string {
			isolateEnv(t)
			isolateRunnerHome(t)
			fr := newFakeRegistry(t)
			fr.addAgent("acme", "demo", "autonomous", "reads things").addVersion(t, "1.0.0", skillFiles("demo", ""))
			stdout, stderr, err := runCLI(t, strings.NewReader(""), append([]string{"--registry", fr.url(), "--json"}, args...)...)
			if err != nil && strings.TrimSpace(stdout) == "" {
				t.Fatalf("%v: %v (stderr %q)", args, err, stderr)
			}
			return stdout
		}
	}
	local := func(args ...string) func(*testing.T) string {
		return func(t *testing.T) string {
			isolateEnv(t)
			_, policyPath := isolateRunnerHome(t)
			for i, a := range args {
				if a == "POLICY" {
					args[i] = policyPath
				}
			}
			stdout, stderr, err := runCLI(t, strings.NewReader(""), append([]string{"--json"}, args...)...)
			if err != nil && strings.TrimSpace(stdout) == "" {
				t.Fatalf("%v: %v (stderr %q)", args, err, stderr)
			}
			return stdout
		}
	}
	fixtures := map[string]jsonFixture{
		"search":  {run: withRegistry("search", "demo")},
		"info":    {run: withRegistry("info", "acme/demo")},
		"verify":  {run: withRegistry("verify", "acme/demo@1.0.0")},
		"install": {run: withRegistry("install", "acme/demo@1.0.0", "--allow-unsigned")},
		"uninstall": {run: func(t *testing.T) string {
			isolateEnv(t)
			isolateRunnerHome(t)
			fr := newFakeRegistry(t)
			fr.addAgent("acme", "demo", "autonomous", "reads things").addVersion(t, "1.0.0", skillFiles("demo", ""))
			if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
				t.Fatal(err)
			}
			stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "uninstall", "acme/demo")
			if err != nil && strings.TrimSpace(stdout) == "" {
				t.Fatal(err)
			}
			return stdout
		}},
		"doctor": {run: func(t *testing.T) string {
			policyPath, _ := doctorFixture(t)
			fr := newFakeRegistry(t)
			stdout, _, _ := runCLI(t, nil, "--json", "doctor", "--registry", fr.url(), "--policy", policyPath)
			return stdout
		}},
		"list":     {run: withRegistry("list")},
		"builtins": {run: local("builtins")},
		"validate": {run: func(t *testing.T) string {
			isolateEnv(t)
			stdout, _, err := runCLI(t, nil, "--json", "validate", writeTree(t, publishableSkillFiles("demo", "")))
			if err != nil {
				t.Fatal(err)
			}
			return stdout
		}},
		"setup":  {run: local("setup", "--skip-images", "--yes", "--provider", "openai", "--policy", "POLICY")},
		"logout": {run: withRegistry("logout")},
		"whoami": {run: func(t *testing.T) string {
			isolateEnv(t)
			fr := newFakeRegistry(t)
			if _, _, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login", "--paste"); err != nil {
				t.Fatal(err)
			}
			stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "whoami")
			if err != nil {
				t.Fatal(err)
			}
			return stdout
		}},
		"run": {run: local("run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--dry-run", "--policy", "POLICY")},
		"runs": {run: func(t *testing.T) string {
			stdout, _, err := runCLI(t, nil, "--json", "runs", "--dir", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return stdout
		}},
		"destinations list":         {run: local("destinations", "list")},
		"connections list":          {run: local("connections", "list")},
		"connections remove":        {run: local("connections", "remove", "--all")},
		"model-signins list":        {run: local("model-signins", "list")},
		"schedule list":             {run: local("schedule", "list")},
		"schedule check-signed-out": {run: local("schedule", "check-signed-out")},
		"schedule reseal":           {run: local("schedule", "reseal")},
		"knowledge list":            {run: local("knowledge", "list")},
		"agent list":                {run: local("agent", "list")},

		"runhost list":         {run: local("runhost", "list")},
		"runhost add":          {reason: "sets up a server over SSH"},
		"runhost status":       {reason: "needs a runhost"},
		"runhost doctor":       {reason: "needs a runhost"},
		"runhost remove":       {reason: "needs a runhost and its admin sign-in"},
		"login":                {reason: "signs in through a browser on a loopback listener; its NDJSON events are covered by the login tests"},
		"fork":                 {reason: "needs a registry that forks"},
		"review":               {reason: "runs the Examiner against a model"},
		"draft":                {reason: "runs the Drafter against a model"},
		"deliver":              {reason: "needs a finished run's folder and its destinations"},
		"model-signins remove": {reason: "needs a stored model sign-in"},
		"model-signins add":    {reason: "signs in through a browser"},
		"connect":              {reason: "signs in through a browser"},
		"report-design show":   {reason: "needs a signed-in account with a report design"},
		"schedule add":         {reason: "needs an installed release and the machine's scheduler"},
		"schedule show":        {reason: "needs a schedule"},
		"knowledge add":        {reason: "builds a pack with a model"},
		"knowledge upload":     {reason: "needs a built pack and a signed-in account"},
		"knowledge export":     {reason: "needs a built pack"},
		"knowledge import":     {reason: "needs an exported pack"},
		"knowledge delete":     {reason: "needs a built pack"},
		"knowledge ship":       {reason: "needs a built pack and a release"},
		"agent show":           {reason: "needs a persistent agent"},
		"agent start":          {reason: "needs a persistent agent and the engine"},
		"agent stop":           {reason: "needs a running persistent agent"},
		"agent attach":         {reason: "needs a running persistent agent"},
		"agent configure":      {reason: "needs a persistent agent"},
		"agent grant":          {reason: "needs a persistent agent"},
		"agent compact":        {reason: "needs a persistent agent with a transcript"},
		"agent memory export":  {reason: "needs a persistent agent and the engine"},
		"agent secret list":    {reason: "needs a persistent agent"},
		"agent skill list":     {reason: "needs a persistent agent"},
		"agent tasks":          {reason: "needs a persistent agent"},
		"voice show":           {reason: "needs a Writing Voice kept on this computer"},
		"voice upload":         {reason: "needs a voice and a signed-in account"},
		"voice status":         {reason: "needs a signed-in account"},
	}
	for _, path := range jsonCapableCommands(newRootCmd()) {
		f, ok := fixtures[path]
		if !ok {
			t.Errorf("%q answers --json and has neither a fixture nor a reason here", path)
			continue
		}
		if f.run == nil {
			if f.reason == "" {
				t.Errorf("%q: no fixture and no reason", path)
			}
			continue
		}
		t.Run(path, func(t *testing.T) {
			stdout := f.run(t)
			dec := json.NewDecoder(strings.NewReader(stdout))
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
			}
			if dec.More() {
				t.Fatalf("stdout carries more than one JSON value:\n%s", stdout)
			}
		})
	}
	for path, f := range fixtures {
		if f.run == nil && f.reason == "" {
			t.Errorf("%q: a fixture entry with neither run nor reason", path)
		}
		if _, ok := jsonCommands[path]; !ok {
			t.Errorf("%q has a fixture but is not a marked command", path)
		}
	}
	_ = filepath.Join
}
