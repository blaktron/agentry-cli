package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/apiclient"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// cliHelper answers a persistent agent's helper tools from the CLI: the
// registry read with the registry client, and each install, schedule and
// schedule change a command of this binary's, as the person would run it.
// The supervisor asks the person before each of those that needs it, so a
// command here asks nothing again.
type cliHelper struct {
	policyPath string
}

// maxHelperOutput bounds what a command of the helper's prints that is read.
const maxHelperOutput = 4 << 20

// command runs this binary with args and answers what it printed.
func (h cliHelper) command(ctx context.Context, args ...string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // this binary, with arguments the supervisor checked
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &limitBuffer{b: &out, n: maxHelperOutput}, &limitBuffer{b: &errOut, n: 64 << 10}, nil
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = strings.TrimSpace(msg[i+1:])
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(strings.TrimPrefix(msg, "agentry: "))
	}
	return out.Bytes(), nil
}

// limitBuffer keeps at most n bytes and drops the rest.
type limitBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (h cliHelper) client() (*apiclient.ClientWithResponses, string, error) {
	reg := registryURL()
	token, _ := credentialFor(reg)
	c, err := newAPIClient(reg, token)
	return c, reg, err
}

func (h cliHelper) Search(ctx context.Context, query string) (string, []persistent.HelperHit, error) {
	c, reg, err := h.client()
	if err != nil {
		return "", nil, err
	}
	limit := 16
	resp, err := c.SearchAgentsWithResponse(ctx, &apiclient.SearchAgentsParams{Q: &query, Limit: &limit})
	if err != nil {
		return "", nil, fmt.Errorf("the registry could not be searched: %w", err)
	}
	if resp.JSON200 == nil {
		return "", nil, apiProblem("search", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	var hits []persistent.HelperHit
	for _, r := range resp.JSON200.Results {
		hit := persistent.HelperHit{Ref: r.Namespace + "/" + r.Name, Summary: r.Summary, Quarantined: r.QuarantineState != nil && *r.QuarantineState != ""}
		for _, m := range r.Modes {
			hit.Modes = append(hit.Modes, string(m))
		}
		if r.Installs30d != nil {
			hit.Installs30d = *r.Installs30d
		}
		hits = append(hits, hit)
	}
	return reg, hits, nil
}

func (h cliHelper) Show(ctx context.Context, ref string) (string, error) {
	out, err := h.command(ctx, "info", ref, "--json")
	if err != nil {
		return "", err
	}
	var doc struct {
		Agent struct {
			Summary string   `json:"summary"`
			Modes   []string `json:"modes"`
		} `json:"agent"`
		Latest *struct {
			Version string `json:"version"`
		} `json:"latest"`
		Run *infoRun `json:"run"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", fmt.Errorf("its details do not read: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s on %s\nSummary (its publisher's words): %q\nModes: %s\n", ref, registryURL(), strings.TrimSpace(doc.Agent.Summary), strings.Join(doc.Agent.Modes, ", "))
	if doc.Latest == nil || doc.Latest.Version == "" {
		b.WriteString("No version is published.\n")
	} else {
		fmt.Fprintf(&b, "Latest version: %s\n", doc.Latest.Version)
	}
	items, _ := h.Installed(ctx)
	shown := false
	for _, it := range items {
		if it.Ref == ref {
			fmt.Fprintf(&b, "Installed on this computer: %s\n", it.Version)
			shown = true
		}
	}
	if !shown {
		b.WriteString("Not installed on this computer.\n")
	}
	if r := doc.Run; r != nil {
		b.WriteString("How to run it (the latest version's):\n")
		if len(r.Intake) == 0 {
			b.WriteString("- It asks no questions.\n")
		}
		for _, q := range r.Intake {
			req := "optional"
			if q.Required {
				req = "required"
			}
			fmt.Fprintf(&b, "- Question %s (%s, %s): %q; %s\n", q.ID, q.Type, req, q.Prompt, q.Answer)
		}
		if len(r.Takes) > 0 {
			fmt.Fprintf(&b, "- Files it takes: %s\n", strings.Join(r.Takes, ", "))
		}
		for _, s := range r.Secrets {
			fmt.Fprintf(&b, "- Key it needs: %s (the person's to set)\n", s.Name)
		}
	}
	return b.String(), nil
}

func (h cliHelper) Installed(context.Context) ([]persistent.HelperInstalled, error) {
	st, err := installed.Load()
	if err != nil {
		return nil, err
	}
	var out []persistent.HelperInstalled
	for _, r := range st.Records {
		if r.Target != installdir.Target {
			continue
		}
		// A record keyed by its install target, not by an address, ran from
		// this computer's default registry.
		reg := r.Registry
		if !strings.HasPrefix(reg, "https://") && !strings.HasPrefix(reg, "http://") {
			reg = ""
		}
		out = append(out, persistent.HelperInstalled{Ref: r.Namespace + "/" + r.Name, Version: r.Version, Registry: reg, Local: r.Source == installed.SourceLocal})
	}
	return out, nil
}

func (h cliHelper) Install(ctx context.Context, ref, _ string) (string, error) {
	out, err := h.command(ctx, "install", ref, "--yes", "--json")
	if err != nil {
		return "", err
	}
	var res struct {
		Ref string `json:"ref"`
	}
	if json.Unmarshal(out, &res) != nil || res.Ref == "" {
		return ref + " is installed on this computer.", nil
	}
	return res.Ref + " is installed on this computer.", nil
}

func (h cliHelper) Schedules(ctx context.Context) ([]persistent.HelperSchedule, error) {
	out, err := h.command(ctx, "schedule", "list", "--json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Schedules []struct {
			ID       string     `json:"id"`
			Name     string     `json:"name"`
			Ref      string     `json:"ref"`
			Version  string     `json:"version"`
			Paused   bool       `json:"paused"`
			Describe string     `json:"describe"`
			Next     *time.Time `json:"next"`
			Ended    bool       `json:"ended"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("the schedules do not read: %w", err)
	}
	var all []persistent.HelperSchedule
	for _, s := range doc.Schedules {
		hs := persistent.HelperSchedule{ID: s.ID, Name: s.Name, Ref: s.Ref + "@" + s.Version, When: s.Describe, Paused: s.Paused, Ended: s.Ended}
		if s.Next != nil {
			hs.Next = s.Next.Local().Format("2006-01-02 15:04")
		}
		all = append(all, hs)
	}
	return all, nil
}

func (h cliHelper) AddSchedule(ctx context.Context, s persistent.HelperScheduleSpec, _ string) (string, error) {
	when, err := s.When.Args()
	if err != nil {
		return "", err
	}
	args := append([]string{"schedule", "add", s.Exact}, when...)
	if s.Registry != "" {
		args = append(args, "--registry", s.Registry)
	}
	if s.Task != "" {
		args = append(args, "--task", s.Task)
	}
	for id, v := range s.Answers {
		args = append(args, "--intake", id+"="+v)
	}
	if h.policyPath != "" {
		args = append(args, "--policy", h.policyPath)
	}
	out, err := h.command(ctx, append(args, "--yes", "--json")...)
	if err != nil {
		return "", err
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &made); err != nil || made.ID == "" {
		return "", errors.New("the schedule was made, and its id could not be read: schedule_list names it")
	}
	return made.ID, nil
}

func (h cliHelper) PauseSchedule(ctx context.Context, id string, paused bool) error {
	verb := "resume"
	if paused {
		verb = "pause"
	}
	_, err := h.command(ctx, "schedule", verb, id)
	return err
}

func (h cliHelper) RemoveSchedule(ctx context.Context, id, _ string) error {
	_, err := h.command(ctx, "schedule", "remove", id)
	return err
}

func (h cliHelper) Docs(ctx context.Context, page string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, persistent.DocsBase+page, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "agentry-cli")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return body, resp.StatusCode, err
}
