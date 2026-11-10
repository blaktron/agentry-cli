// Which commands answer --json. The root flag is one for the whole tree;
// each command that honours it is marked here, the root flag's help lists
// them from the tree, and a test runs every marked command under --json and
// holds its stdout to one JSON object (login's NDJSON aside). A path named
// here that the tree does not have is a programming error, caught the first
// time the tree is built.
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// jsonAnnotation marks a command that answers --json; its value says, in a
// few words, what the JSON is.
const jsonAnnotation = "json"

// jsonCommands is every command path that answers --json, with what it
// prints. A list prints an object with a named array, never a bare array.
var jsonCommands = map[string]string{
	"search":                    "{results, total, facets}; with --org {results, total}",
	"info":                      "{agent, latest, run}",
	"verify":                    "the verification report",
	"doctor":                    "{ref, checks[]}, each check with its name, status, detail and remedy",
	"install":                   "the install record, or the plan with --plan",
	"uninstall":                 "what was removed",
	"list":                      "{path, records[]}",
	"builtins":                  "{builtins[]}",
	"validate":                  "the validation report",
	"setup":                     "the policy as configured",
	"login":                     "NDJSON: a pending event, then the typed result",
	"logout":                    "the scoped sign-out",
	"whoami":                    "the account",
	"managed show":              "{documents[], problems[]}",
	"run":                       "the result envelope; the plan with --plan; the plan in words with --dry-run",
	"runs":                      "{dir, runs[]}",
	"runhost add":               "{runhost, preflight[], runKeyRefused[], status, doctor}",
	"runhost list":              "{runhosts[]}",
	"runhost status":            "the runhost's state",
	"runhost doctor":            "{checks[]}, the runhost's checks",
	"runhost remove":            "{removed, images, docker}",
	"fork":                      "the fork as the registry records it",
	"review":                    "the Examiner's report",
	"draft":                     "the draft",
	"deliver":                   "{deliveries[]}",
	"destinations list":         "{destinations[]}",
	"connections list":          "{connections[]}",
	"connections remove":        "{removed[]}",
	"model-signins list":        "{modelSignIns[]} (deprecated: providers list)",
	"model-signins remove":      "the removal (deprecated: providers remove --sign-in or --connect)",
	"model-signins add":         "the sign-in (deprecated: providers add)",
	"models list":               "{default, max, models[]}",
	"models add":                "the saved model",
	"models edit":               "the saved model",
	"models default":            "the saved model",
	"models check":              "{model, ok, contextWindow, warning}",
	"models remove":             "{removed[], moved[], default}",
	"providers list":            "{providers[], signIns[]}",
	"providers add":             "the provider, with signedInAgain when it signed in or connected again",
	"providers remove":          "{provider, providerRemoved, credential, signIn, revocation, removed[], moved[], default}",
	"voice show":                "the voice's record",
	"voice upload":              "the account's voice as the registry answers it",
	"voice status":              "the account's voice and its review",
	"connect":                   "the connected account",
	"report-design show":        "the report design a run takes, or one of the library",
	"report-design list":        "{registry, default, designs[]}",
	"schedule add":              "the schedule",
	"schedule list":             "{schedules[]}",
	"schedule show":             "the schedule",
	"schedule check-signed-out": "the signed-out check",
	"schedule reseal":           "what was resealed",
	"knowledge add":             "the pack added",
	"knowledge list":            "{packs[]}",
	"knowledge upload":          "the upload",
	"knowledge export":          "the export",
	"knowledge import":          "the pack imported",
	"knowledge delete":          "the deletion",
	"knowledge ship":            "the shipment",
	"agent list":                "{agents[]}",
	"agent show":                "the agent's state",
	"agent install":             "{agent, ref, version, source, digest, persona, packs, subagents, enabled, recorded}",
	"agent uninstall":           "{agent, ref, dataRemoved, release}",
	"agent start":               "the agent's state",
	"agent stop":                "the agent's state",
	"agent attach":              "the agent's state",
	"agent configure":           "the configuration's outcome",
	"agent grant":               "the grant's outcome",
	"agent compact":             "the compaction",
	"agent memory export":       "the export",
	"agent secret list":         "{secrets[]}",
	"agent skill list":          "{skills[]}",
	"agent tasks":               "{tasks[]}",
}

// markJSONCommands annotates every command in jsonCommands on root's tree.
// A path with no command panics: the table and the tree disagree.
func markJSONCommands(root *cobra.Command) {
	for path, what := range jsonCommands {
		c := findCommand(root, path)
		if c == nil {
			panic(fmt.Sprintf("jsonCommands names %q, which the command tree does not have", path))
		}
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[jsonAnnotation] = what
	}
}

// findCommand walks root to the command at path ("schedule list"), nil
// when there is none.
func findCommand(root *cobra.Command, path string) *cobra.Command {
	c := root
	for _, name := range strings.Fields(path) {
		var next *cobra.Command
		for _, sub := range c.Commands() {
			if sub.Name() == name || sub.HasAlias(name) {
				next = sub
				break
			}
		}
		if next == nil {
			return nil
		}
		c = next
	}
	return c
}

// jsonCapableCommands lists the marked commands under root by path, sorted,
// leaving out hidden ones.
func jsonCapableCommands(root *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			if sub.Hidden {
				// A hidden command, a deprecated alias say, is left out of
				// the help, and so is everything under it.
				continue
			}
			path := strings.TrimSpace(prefix + " " + sub.Name())
			if _, ok := sub.Annotations[jsonAnnotation]; ok {
				out = append(out, path)
			}
			walk(sub, path)
		}
	}
	walk(root, "")
	sort.Strings(out)
	return out
}

// jsonFlagHelp is the root --json flag's help, listing the marked commands
// from the tree.
func jsonFlagHelp(root *cobra.Command) string {
	return "machine-readable JSON output on stdout, one object (a list is an object with a named array); narration stays on stderr. Answered by: " +
		strings.Join(jsonCapableCommands(root), ", ") + "; login writes NDJSON lifecycle events; run also takes --output-format json"
}
