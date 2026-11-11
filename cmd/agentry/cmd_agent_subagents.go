package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// cliSubagents runs a persistent agent's subagents from the CLI: it reads a
// subagent's version on its registry with the registry client, and runs it
// as `agentry run` of this binary, an ordinary autonomous run with its own
// verification, policy check and receipts. The person agreed to each run of
// it when they turned the subagent on, so the run asks nothing.
type cliSubagents struct {
	// policyPath is the policy file the supervisor runs under, which each
	// run is held to as well; "" is the default.
	policyPath string
}

// Check reads exact on registry and says how to run it, or why it may not
// run now: it is quarantined, its publisher withdrew it, it is not in the
// registry, or it is no longer an autonomous QuickAgent.
func (c cliSubagents) Check(ctx context.Context, registry, exact string) (persistent.SubagentUsage, error) {
	var none persistent.SubagentUsage
	ns, name, version, err := splitExact(exact)
	if err != nil {
		return none, err
	}
	if registry == "" {
		registry = registryURL()
	}
	token, _ := credentialFor(registry)
	client, err := newAPIClient(registry, token)
	if err != nil {
		return none, err
	}
	// A quarantine is the agent's, whatever each version's own status.
	ag, err := client.GetAgentWithResponse(ctx, ns, name)
	if err != nil {
		return none, fmt.Errorf("the registry could not be asked about it: %w", err)
	}
	if ag.StatusCode() == http.StatusNotFound || ag.StatusCode() == http.StatusGone {
		return none, errors.New("it is no longer in the registry")
	}
	if ag.JSON200 == nil {
		return none, apiProblem("read "+ns+"/"+name, ag.StatusCode(), ag.ApplicationproblemJSONDefault, ag.Body)
	}
	if q := ag.JSON200.QuarantineState; q != "" && q != apiclient.AgentQuarantineStateNone {
		return none, errors.New("the registry quarantined it, so it runs no more")
	}
	info, err := client.GetVersionInfoWithResponse(ctx, ns, name, version)
	if err != nil {
		return none, fmt.Errorf("the registry could not be asked about it: %w", err)
	}
	if info.StatusCode() == http.StatusNotFound || info.StatusCode() == http.StatusGone {
		return none, errors.New("it is no longer in the registry")
	}
	if info.JSON200 == nil {
		return none, apiProblem("read "+exact, info.StatusCode(), info.ApplicationproblemJSONDefault, info.Body)
	}
	switch info.JSON200.Status {
	case apiclient.VersionStatusPublished:
	case apiclient.VersionStatusQuarantined, apiclient.VersionStatusBlocked:
		return none, errors.New("the registry quarantined it, so it runs no more")
	case apiclient.VersionStatusYanked:
		return none, errors.New("its publisher withdrew it, so it runs no more")
	default:
		return none, fmt.Errorf("it is %s on the registry, not published", info.JSON200.Status)
	}
	mr, err := client.GetVersionManifestWithResponse(ctx, ns, name, version)
	if err != nil {
		return none, fmt.Errorf("its manifest could not be read: %w", err)
	}
	if mr.JSON200 == nil {
		return none, apiProblem("read the manifest of "+exact, mr.StatusCode(), mr.ApplicationproblemJSONDefault, mr.Body)
	}
	raw, err := json.Marshal(*mr.JSON200)
	if err != nil {
		return none, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return none, fmt.Errorf("its manifest does not read: %w", err)
	}
	if m.IsPersistent() || !m.Supports(manifest.ModeAutonomous) {
		return none, errors.New("it does not run as an autonomous QuickAgent, so it never runs as a subagent")
	}
	u := persistent.SubagentUsage{Desktop: desktopUse(m.Capabilities.Desktop)}
	for _, q := range howToRun(&m).Intake {
		if q.PersonsOwn {
			continue
		}
		u.Questions = append(u.Questions, persistent.SubagentQuestion{ID: q.ID, Type: q.Type, Required: q.Required, Prompt: q.Prompt, Choices: q.Choices, Answer: q.Answer})
	}
	return u, nil
}

// Run runs r.Exact as `agentry run`, with the task in a file beside its
// output folder, and answers the envelope the run printed, which it prints
// whether the run succeeded or not.
func (c cliSubagents) Run(ctx context.Context, r persistent.SubagentRunSpec) (*receipt.Envelope, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(r.OutDir)
	task := filepath.Join(dir, "task.txt")
	if err := os.WriteFile(task, []byte(r.Task), 0o600); err != nil {
		return nil, err
	}
	args := c.runArgs(r, task)
	log, err := os.OpenFile(filepath.Join(dir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the run's own folder
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // this binary, with arguments the supervisor checked
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, log, nil
	runErr := cmd.Run()
	var env receipt.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil || env.Status == "" {
		if runErr != nil {
			return nil, fmt.Errorf("the run did not start: %w (its log: %s)", runErr, filepath.Join(dir, "run.log"))
		}
		return nil, fmt.Errorf("the run printed no envelope (its log: %s)", filepath.Join(dir, "run.log"))
	}
	return &env, nil
}

// runArgs are the arguments of a subagent's run: an autonomous run of the
// exact version, asking nothing, with the task file, the staged files and
// the answers, sending its result nowhere and taking no Writing Voice.
func (c cliSubagents) runArgs(r persistent.SubagentRunSpec, task string) []string {
	args := []string{"run", r.Exact, "--mode", "autonomous", "--yes", "--output-format", "json", "--out", r.OutDir,
		"--input", task, "--no-destination", "--no-voice", "--quiet"}
	if r.Registry != "" {
		args = append(args, "--registry", r.Registry)
	}
	if c.policyPath != "" {
		args = append(args, "--policy", c.policyPath)
	}
	for _, f := range r.Files {
		args = append(args, "--file", f)
	}
	ids := make([]string, 0, len(r.Answers))
	for id := range r.Answers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		args = append(args, "--intake", id+"="+r.Answers[id])
	}
	return args
}

// desktopUse is what a desktop line uses of the person's computer, in the
// install's words: each application, then computer use.
func desktopUse(d *manifest.Desktop) []string {
	if d == nil {
		return nil
	}
	var use []string
	for _, srv := range d.Servers {
		use = append(use, manifest.DescribeDesktopServer(srv))
	}
	if words := manifest.DescribeComputerUse(d); words != "" {
		use = append(use, words)
	}
	return use
}

// splitExact reads ns/name@version.
func splitExact(exact string) (ns, name, version string, err error) {
	base, version, ok := strings.Cut(exact, "@")
	ns, name, ok2 := strings.Cut(base, "/")
	if !ok || !ok2 || ns == "" || name == "" || version == "" {
		return "", "", "", fmt.Errorf("%q is not namespace/name@version", exact)
	}
	return ns, name, version, nil
}

// installedSubagent is the QuickAgent ref installed on this computer as a
// subagent the person adds: a registry install for the runner, run as an
// autonomous QuickAgent and not persistent, at the version it is installed
// at, with what its contract reaches.
func installedSubagent(home, ref string) (persistent.AddedSubagent, error) {
	ns, name, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "@") {
		return persistent.AddedSubagent{}, fmt.Errorf("%q is not namespace/name", ref)
	}
	st, err := installed.Load()
	if err != nil {
		return persistent.AddedSubagent{}, err
	}
	i := slices.IndexFunc(st.Records, func(r installed.Record) bool {
		return r.Target == installdir.Target && r.Namespace == ns && r.Name == name
	})
	if i < 0 {
		return persistent.AddedSubagent{}, fmt.Errorf("%s/%s is not installed on this computer: install it first (agentry install %s/%s)", ns, name, ns, name)
	}
	r := st.Records[i]
	if r.Source == installed.SourceLocal {
		return persistent.AddedSubagent{}, fmt.Errorf("%s/%s was installed from a folder, and a subagent is a release from the registry", ns, name)
	}
	m, err := localrun.ManifestFrom(filepath.Join(installdir.ReleaseDir(home, ns, name, r.Version, r.Digest), "workspace"))
	if err != nil {
		return persistent.AddedSubagent{}, fmt.Errorf("%s/%s@%s: %w", ns, name, r.Version, err)
	}
	if m.IsPersistent() || !m.Supports(manifest.ModeAutonomous) {
		return persistent.AddedSubagent{}, fmt.Errorf("%s/%s does not run as an autonomous QuickAgent, so it is never a subagent", ns, name)
	}
	return persistent.AddedSubagent{Ref: ns + "/" + name, Version: r.Version, Reach: subagentReach(m.Capabilities)}, nil
}
