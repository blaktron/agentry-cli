// `agentry info` and `agentry search` (§11 discovery). search calls the real
// full-text endpoint (GET /search, §10.4) with its facet filters; info shows
// one agent, its latest published version, and its capability contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
)

// newInfoCmd builds the info command.
func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <ns/name>",
		Short: "Show an agent, its latest published version, and its capability contract",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInfo(cmd, args[0])
		},
	}
}

// searchOpts carries the §10.4 facet filters.
type searchOpts struct {
	kind       string
	capability string
	license    string
	verified   bool
	sort       string
	limit      int
}

// newSearchCmd builds the search command.
func newSearchCmd() *cobra.Command {
	var opts searchOpts
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search over public agents, with facet filters",
		Long: "Search public agents via GET /search (§10.4). A query containing \"/\" is\n" +
			"treated as ns/name and shows that agent's info instead.\n\n" +
			"Facets narrow the result set:\n\n" +
			"  --kind        skill | prompt-agent | mcp-server\n" +
			"  --capability  a capability the latest version actually asks for\n" +
			"                (network, filesystem, exec, secrets, dataAccess) —\n" +
			"                a capability declared as \"none\" does not match\n" +
			"  --license     SPDX identifier from the latest published manifest\n" +
			"  --verified    only agents in an org namespace with a verified domain\n" +
			"  --sort        relevance (default) | installs | newest\n\n" +
			"With --json the full response, including facet counts, is printed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.kind, "kind", "", "filter by agent kind: skill | prompt-agent | mcp-server")
	cmd.Flags().StringVar(&opts.capability, "capability", "", "filter by declared capability (network, filesystem, exec, secrets, dataAccess)")
	cmd.Flags().StringVar(&opts.license, "license", "", "filter by SPDX license identifier")
	cmd.Flags().BoolVar(&opts.verified, "verified", false, "only agents in orgs with a verified domain")
	cmd.Flags().StringVar(&opts.sort, "sort", "", "sort order: relevance | installs | newest")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum results (server default when unset)")
	return cmd
}

// errNoPublishedVersion distinguishes "this agent has nothing published" from
// "the registry could not answer". Both used to print the same line, which
// meant a 503 during a release read as an authoring mistake.
var errNoPublishedVersion = errors.New("no published version")

// runInfo fetches and renders one agent.
func runInfo(cmd *cobra.Command, ref string) error {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("info wants ns/name, got %q", ref)
	}
	ns, name := parts[0], parts[1]

	reg := registryURL()
	client, err := newAPIClientChecked(cmd, reg, credentialFor(reg))
	if err != nil {
		return err
	}

	agentResp, err := client.GetAgentWithResponse(cmd.Context(), ns, name)
	if err != nil {
		return fmt.Errorf("get agent %s: %w", ref, err)
	}
	if agentResp.JSON200 == nil {
		return apiProblem("get agent "+ref, agentResp.StatusCode(), agentResp.ApplicationproblemJSONDefault, agentResp.Body)
	}
	agent := agentResp.JSON200

	// Latest published version on the default (stable) channel.
	latest, latestErr := resolveLatest(cmd.Context(), client, ns, name)
	transportFailure := latestErr != nil && !errors.Is(latestErr, errNoPublishedVersion)

	if flags.jsonOut {
		doc := map[string]any{"agent": agent}
		switch {
		case latestErr == nil:
			doc["latest"] = latest
		case transportFailure:
			doc["latestError"] = latestErr.Error()
		default:
			doc["latest"] = nil
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return fmt.Errorf("encode info: %w", err)
		}
	} else {
		renderAgentInfo(cmd.OutOrStdout(), agent, latest, latestErr)
	}

	// A transport or server failure is still a failure, even though the agent
	// record printed: exiting 0 would tell a script the version lookup
	// succeeded and found nothing.
	if transportFailure {
		return latestErr
	}
	return nil
}

// resolveLatest asks for the newest published version, mapping "the registry
// says there is none" to errNoPublishedVersion and leaving every other
// failure — transport error, 5xx, malformed response — as itself.
//
// The distinction matters because the two call for opposite reactions: an
// author with no published version should publish one; an author hitting a
// broken registry should retry, and should certainly not be told their agent
// is empty.
func resolveLatest(ctx context.Context, client *apiclient.ClientWithResponses, ns, name string) (*apiclient.ResolveResult, error) {
	resp, err := client.ResolveRefWithResponse(ctx, &apiclient.ResolveRefParams{Ref: ns + "/" + name})
	if err != nil {
		return nil, fmt.Errorf("resolve %s/%s: %w", ns, name, err)
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	// 404 (and 410 for a fully-yanked agent) mean "nothing published here",
	// which is a fact about the agent. Everything else is a fact about the
	// registry.
	switch resp.StatusCode() {
	case 404, 410:
		return nil, errNoPublishedVersion
	}
	return nil, apiProblem("resolve "+ns+"/"+name, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
}

// renderAgentInfo prints the human-readable info block.
func renderAgentInfo(out io.Writer, agent *apiclient.Agent, latest *apiclient.ResolveResult, latestErr error) {
	_, _ = fmt.Fprintf(out, "%s/%s — %s, %s\n", agent.Namespace, agent.Name, agent.Kind, agent.Visibility)
	if agent.Summary != "" {
		_, _ = fmt.Fprintf(out, "  summary: %s\n", agent.Summary)
	}
	_, _ = fmt.Fprintf(out, "  git:     %s\n", agent.GitHttpUrl)
	_, _ = fmt.Fprintf(out, "  created: %s\n", agent.CreatedAt.Format("2006-01-02"))
	switch {
	case errors.Is(latestErr, errNoPublishedVersion):
		_, _ = fmt.Fprintln(out, "  latest:  no published version")
		return
	case latestErr != nil:
		_, _ = fmt.Fprintf(out, "  latest:  UNKNOWN — the registry could not answer: %v\n", latestErr)
		return
	}
	_, _ = fmt.Fprintf(out, "  latest:  %s (%s)\n", latest.Version, latest.Digest)
	if m, err := manifestFromResolve(latest); err == nil {
		_, _ = fmt.Fprintln(out, "  capabilities:")
		for _, line := range capabilityLines(m.Capabilities) {
			_, _ = fmt.Fprintf(out, "    %s\n", line)
		}
		_, _ = fmt.Fprintf(out, "  install: agentry install %s/%s\n", agent.Namespace, agent.Name)
	}
}

// runSearch queries GET /search, or delegates ns/name queries to info.
func runSearch(cmd *cobra.Command, query string, opts searchOpts) error {
	if strings.Contains(query, "/") {
		return runInfo(cmd, query)
	}

	reg := registryURL()
	client, err := newAPIClientChecked(cmd, reg, credentialFor(reg))
	if err != nil {
		return err
	}

	params := &apiclient.SearchAgentsParams{}
	if query != "" {
		params.Q = &query
	}
	if opts.kind != "" {
		k := apiclient.AgentKind(opts.kind)
		params.Kind = &k
	}
	if opts.capability != "" {
		params.Capability = &opts.capability
	}
	if opts.license != "" {
		params.License = &opts.license
	}
	if opts.verified {
		params.Verified = &opts.verified
	}
	if opts.sort != "" {
		s := apiclient.SearchAgentsParamsSort(opts.sort)
		params.Sort = &s
	}
	if opts.limit > 0 {
		params.Limit = &opts.limit
	}

	resp, err := client.SearchAgentsWithResponse(cmd.Context(), params)
	if err != nil {
		return fmt.Errorf("search %q: %w", query, err)
	}
	if resp.JSON200 == nil {
		return apiProblem("search "+query, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	results := resp.JSON200

	out := cmd.OutOrStdout()
	if flags.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return fmt.Errorf("encode search results: %w", err)
		}
		return nil
	}
	if len(results.Results) == 0 {
		_, _ = fmt.Fprintf(out, "no agents match %q%s\n", query, describeFacets(opts))
		return nil
	}
	for _, hit := range results.Results {
		summary := hit.Summary
		if summary == "" {
			summary = "-"
		}
		installs := ""
		if hit.Installs30d != nil {
			installs = fmt.Sprintf("%d installs/30d", *hit.Installs30d)
		}
		_, _ = fmt.Fprintf(out, "%-40s %-13s %-16s %s\n", hit.Namespace+"/"+hit.Name, hit.Kind, installs, summary)
	}
	if results.Total != nil && *results.Total > len(results.Results) {
		_, _ = fmt.Fprintf(out, "\n%d of %d results shown (--limit to see more)\n", len(results.Results), *results.Total)
	}
	return nil
}

// describeFacets renders the active filters for the empty-result message, so
// "no agents match" never looks like "this registry is empty".
func describeFacets(opts searchOpts) string {
	var parts []string
	if opts.kind != "" {
		parts = append(parts, "kind="+opts.kind)
	}
	if opts.capability != "" {
		parts = append(parts, "capability="+opts.capability)
	}
	if opts.license != "" {
		parts = append(parts, "license="+opts.license)
	}
	if opts.verified {
		parts = append(parts, "verified")
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, ", ")
}
