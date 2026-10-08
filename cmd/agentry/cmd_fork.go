// `agentry fork` (plan forks-and-publisher.md §5, §6; agentry-cli#752): fork
// a public QuickAgent into the signed-in account's namespace, or an
// organization it publishes for, over the core's POST /agents/{ns}/{name}/fork
// (agentry#943, DEV-220). The core decides every rule: the origin public,
// not quarantined or blocked and with a published version; the caller able
// to create in the target; the name free; the quota and the private-agent
// cap. The fork's first version is a submission through the pipeline,
// charged like any. Fork needs agent:publish, which the sign-in grant
// carries.
//
// What a fork is, and every refusal, are said in the site's words
// (agentry-web lib/fork.ts FORK_TERMS and forkRefusal), as the desktop says
// them (services_fork.go).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// forkTerms are what a fork is, said before it is made: the site's
// FORK_TERMS, the third from DEV-224.
var forkTerms = []string{
	"A fork stays private for good. It can be changed and given new versions, but it can never be made public.",
	"Its first version is a submission through the pipeline, and it costs credits as any submission does.",
	"Hosted runs of the fork pay the original's publisher their Publisher Earnings share, unless the fork is in their own namespace or their organization's. Runs on your own computer pay nobody.",
}

const forkNotForked = "Not forked:"

// forkWaitPoll and forkWaitLimit pace --wait: the copy is a worker job
// that usually takes seconds, and a submission's pipeline is not waited
// for at all.
var (
	forkWaitPoll  = 2 * time.Second
	forkWaitLimit = 10 * time.Minute
)

func newForkCmd() *cobra.Command {
	var into, name string
	var yes, wait bool
	cmd := &cobra.Command{
		Use:   "fork <ns/name>",
		Short: "Fork a public QuickAgent into your namespace or an organization's (private for good)",
		Long: "Fork the public QuickAgent ns/name: a private copy of it, with its whole history, in your own namespace\n" +
			"(or --into an organization you publish for), under the same name unless --name gives another. Its first\n" +
			"version is the origin's newest published version, renamed, submitted through the pipeline and charged\n" +
			"like any submission. On a terminal you are told what a fork is and asked to type \"yes\" (--yes skips it).\n" +
			"--wait follows the copy until the first version is submitted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFork(cmd, args[0], into, name, yes, wait)
		},
	}
	cmd.Flags().StringVar(&into, "into", "", "the namespace the fork goes into (default: your own)")
	cmd.Flags().StringVar(&name, "name", "", "the fork's name (default: the origin's)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation (scripts)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the copy is made and its first version submitted, or has failed")
	return cmd
}

func runFork(cmd *cobra.Command, ref, into, newName string, yes, wait bool) error {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || !manifest.ValidNamespace(ns) || !manifest.ValidName(name) {
		if strings.Contains(ref, "@") {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("fork takes ns/name without a version: a fork starts from the newest published version"))
		}
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not a QuickAgent's name (ns/name)", ref))
	}
	if into != "" && !manifest.ValidNamespace(into) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--into %q is not a namespace", into))
	}
	if newName != "" && !manifest.ValidName(newName) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--name %q is not a name a QuickAgent can have: lowercase letters, digits and hyphens, starting and ending with a letter or digit", newName))
	}

	reg := registryURL()
	token := credentialFor(reg)
	if token == "" {
		return fmt.Errorf("no credential for %s — run `agentry login` or set %s", reg, envToken)
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	if into == "" {
		u, err := client.GetCurrentUserWithResponse(cmd.Context())
		if err != nil {
			return errors.New(forkUnreachable())
		}
		if u.StatusCode() == http.StatusUnauthorized {
			return errors.New(forkRefusal(ref, "", name, http.StatusUnauthorized, "", ""))
		}
		if u.JSON200 == nil {
			return apiProblem("read your account to fork into your namespace (or pass --into)", u.StatusCode(), u.ApplicationproblemJSONDefault, u.Body)
		}
		into = u.JSON200.Handle
	}
	forkName := forkNameOr(newName, name)
	fork := into + "/" + forkName

	if !yes {
		w := cmd.ErrOrStderr()
		_, _ = fmt.Fprintf(w, "Fork %s into %s?\n\n", ref, fork)
		for _, t := range forkTerms {
			_, _ = fmt.Fprintf(w, "  %s\n", t)
		}
		_, _ = fmt.Fprintln(w)
		if err := promptYes(cmd, "Type \"yes\" to fork: ",
			"a fork's first version is a submission charged like any: confirm on a terminal, or pass --yes",
			"fork cancelled: nothing was sent"); err != nil {
			return engine.Classify(engine.ExitUsage, err)
		}
	}

	body := apiclient.ForkAgentJSONRequestBody{Namespace: into}
	if newName != "" {
		body.Name = &newName
	}
	res, err := client.ForkAgentWithResponse(cmd.Context(), ns, name, body)
	if err != nil {
		return errors.New(forkUnreachable())
	}
	if res.JSON202 == nil {
		slug, detail := problemParts(res.StatusCode(), res.ApplicationproblemJSONDefault, res.Body)
		return errors.New(forkRefusal(ref, into, forkName, res.StatusCode(), slug, detail))
	}
	made := res.JSON202
	fork = made.Namespace + "/" + made.Name
	out := cmd.OutOrStdout()
	if !flags.jsonOut {
		_, _ = fmt.Fprintf(out, "Forked %s into %s.\n", ref, fork)
	}
	if !wait {
		if flags.jsonOut {
			printJSON(out, made)
			return nil
		}
		_, _ = fmt.Fprintf(out, "Its copy is under way; then its first version goes through the pipeline like any submission. `agentry info %s` shows the fork.\n", fork)
		return nil
	}
	final, err := waitForFork(cmd, client, made)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		printJSON(out, final)
	}
	if final.ForkState != nil && *final.ForkState == apiclient.AgentForkStateFailed {
		detail := "the registry gave no reason"
		if final.ForkDetail != nil && *final.ForkDetail != "" {
			detail = *final.ForkDetail
		}
		return fmt.Errorf("the fork %s was made, but its copy failed: %s", fork, detail)
	}
	if !flags.jsonOut {
		v := ""
		if final.ForkedFrom != nil && final.ForkedFrom.Version != "" {
			v = " " + final.ForkedFrom.Version
		}
		_, _ = fmt.Fprintf(out, "The copy is made, and its first version%s is in the pipeline. `agentry info %s` shows it.\n", v, fork)
	}
	return nil
}

// waitForFork reads the fork until its copy is ready or has failed.
func waitForFork(cmd *cobra.Command, client *apiclient.ClientWithResponses, a *apiclient.Agent) (*apiclient.Agent, error) {
	deadline := time.Now().Add(forkWaitLimit)
	cur := a
	if cur.ForkState == nil {
		// A registry that does not report a fork's copy cannot be waited on.
		return nil, fmt.Errorf("the registry does not say how the copy of %s/%s is going; `agentry info %s/%s` shows the fork", a.Namespace, a.Name, a.Namespace, a.Name)
	}
	for cur.ForkState != nil && *cur.ForkState == apiclient.AgentForkStateCopying {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the fork %s/%s is still being copied after %s; `agentry info %s/%s` shows when it is ready", a.Namespace, a.Name, forkWaitLimit, a.Namespace, a.Name)
		}
		select {
		case <-cmd.Context().Done():
			return nil, cmd.Context().Err()
		case <-time.After(forkWaitPoll):
		}
		res, err := client.GetAgentWithResponse(cmd.Context(), a.Namespace, a.Name)
		if err != nil {
			continue // a dropped read is retried until the deadline
		}
		if res.JSON200 == nil {
			return nil, apiProblem("read the fork "+a.Namespace+"/"+a.Name, res.StatusCode(), res.ApplicationproblemJSONDefault, res.Body)
		}
		cur = res.JSON200
	}
	return cur, nil
}

func forkNameOr(newName, name string) string {
	if newName != "" {
		return newName
	}
	return name
}

// problemParts is a refusal's problem slug (its type's last segment) and
// detail, read from the parsed problem or, for a status the generated
// client types on its own, from a body that is a problem for that status
// (apiProblem's rule).
func problemParts(status int, p *apiclient.Problem, body []byte) (slug, detail string) {
	if p == nil {
		var parsed apiclient.Problem
		if json.Unmarshal(body, &parsed) != nil || parsed.Title == "" || parsed.Status != status {
			return "", ""
		}
		p = &parsed
	}
	slug = p.Type[strings.LastIndex(p.Type, "/")+1:]
	if p.Detail != nil {
		detail = *p.Detail
	}
	return slug, detail
}

func forkUnreachable() string {
	return forkNotForked + " the registry did not answer. Try again in a moment."
}

// forkRefusal says why a fork was not made, in the site's words
// (forkRefusal in agentry-web lib/fork.ts, as the desktop's). Never a status
// code alone.
func forkRefusal(origin, target, name string, status int, slug, detail string) string {
	nf := forkNotForked
	switch {
	case slug == "account-unactivated" && detail != "":
		return detail
	case status == http.StatusUnauthorized:
		return nf + " the registry did not accept your credential. Run `agentry login`, then try again."
	case slug == "validation" && strings.Contains(strings.ToLower(detail), "quota"):
		// The namespace's agent quota is the core's validation refusal,
		// named in its detail.
		return fmt.Sprintf("%s %s already holds as many QuickAgents as it may. Delete one there, then try again.", nf, target)
	case slug == "name-taken":
		return fmt.Sprintf("%s %s already has a QuickAgent called %s. Choose another name for the fork with --name.", nf, target, name)
	case slug == "name-held":
		return fmt.Sprintf("%s %s belonged to a QuickAgent deleted from %s, and that name is never used again. Choose another name for the fork with --name.", nf, name, target)
	case slug == "fork-source":
		return fmt.Sprintf("%s only a public QuickAgent that is not quarantined or blocked can be forked, and %s is not one now.", nf, origin)
	case slug == "nothing-to-fork":
		return fmt.Sprintf("%s %s has no published version yet, and a fork starts from one.", nf, origin)
	case slug == "private-agent-cap":
		return fmt.Sprintf("%s %s already holds as many private QuickAgents as its subscription allows, and a fork is private. Retire one, or upgrade, then try again.", nf, target)
	case status == http.StatusForbidden && detail != "":
		return nf + " " + detail
	case status == http.StatusForbidden:
		return fmt.Sprintf("%s you cannot create QuickAgents in %s.", nf, target)
	case status == http.StatusNotFound && detail != "" && slug != "not-found":
		return nf + " " + detail
	case status == http.StatusNotFound:
		return fmt.Sprintf("%s %s or %s is not there any more.", nf, origin, target)
	case status == http.StatusTooManyRequests:
		return nf + " too many requests. Wait a minute, then try again."
	case status >= 500:
		return forkUnreachable()
	case detail != "":
		return nf + " " + detail
	}
	return nf + " the registry refused it."
}
