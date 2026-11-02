package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	rhsetup "github.com/blaktron/agentry-cli/pkg/runhost/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// A runhost's CLI follows its devices': `runhost update` sends this CLI's
// signed release over the run key, and the first runhost command after this
// CLI changes version does the same for every runhost (the post-update
// step). `pair` adds another of the owner's devices with the admin sign-in;
// `unpair` takes this one off over its run key.

// runhostUpdatedFile records the CLI version the post-update step last ran
// for, under the runner home.
const runhostUpdatedFile = "runhost-updated-for"

// runhostUpdate is remote.Update, a variable so tests can stand in for it.
var runhostUpdate = remote.Update

func newRunhostUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [<name>...]",
		Short: "Bring runhosts to this CLI's version, over their run keys",
		Long: "Sends each runhost this CLI's own signed release for its architecture, over\n" +
			"this computer's run key: the runhost checks its signature against the keys\n" +
			"its own CLI carries, that it is for the server's architecture and that it is\n" +
			"no older than what it runs, installs it, and restarts its service once its\n" +
			"running jobs finish. gVisor follows the release this CLI pins. A runhost\n" +
			"that already runs a newer CLI is left as it is: update this computer's CLI\n" +
			"instead. What only the admin sign-in can change is reported as needing\n" +
			"attention. With no names, every runhost this computer pairs with.\n\n" +
			"The first runhost command after this CLI changes version updates every\n" +
			"runhost the same way.",
		RunE: func(cmd *cobra.Command, args []string) error {
			outs, err := updateRunhosts(cmd.Context(), cmd, args)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"runhosts": outs})
			}
			for _, o := range outs {
				printUpdateOutcome(cmd.OutOrStdout(), o)
			}
			for _, o := range outs {
				if o.Err != "" {
					return engine.Classify(engine.ExitGateway, errors.New("not every runhost could be updated"))
				}
			}
			return nil
		},
	}
}

// runhostUpdateResult is one runhost's update as the command reports it.
type runhostUpdateResult struct {
	remote.UpdateOutcome
	Err string `json:"error,omitempty"`
}

// updateRunhosts updates the named runhosts, or every one, and saves what
// changed in their records.
func updateRunhosts(ctx context.Context, cmd *cobra.Command, names []string) ([]runhostUpdateResult, error) {
	if !clirelease.ValidVersion(version.Version) {
		return nil, engine.Classify(engine.ExitUsage, rhsetup.ErrNotARelease)
	}
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		for _, r := range recs.Runhosts {
			names = append(names, r.Name)
		}
	}
	keys, err := clirelease.Carried()
	if err != nil {
		return nil, err
	}
	var outs []runhostUpdateResult
	for _, name := range names {
		rec, ok := recs.Find(name)
		if !ok {
			return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost is named %q on this device: agentry runhost list shows them", name))
		}
		res := runhostUpdateResult{UpdateOutcome: remote.UpdateOutcome{Name: name}}
		dk, err := runhost.LoadKeys(name)
		if err == nil {
			say := func(s string) { _, _ = fmt.Fprintf(narration(cmd), "%s: %s\n", name, s) }
			res.UpdateOutcome, err = runhostUpdate(ctx, rec, dk, remote.UpdateOptions{Keys: keys, Say: say})
		}
		if err != nil {
			res.Err = err.Error()
		}
		outs = append(outs, res)
	}
	if err := recs.Save(home); err != nil {
		return outs, err
	}
	return outs, nil
}

func printUpdateOutcome(w io.Writer, o runhostUpdateResult) {
	switch {
	case o.Err != "":
		_, _ = fmt.Fprintf(w, "runhost %s was not updated: %s\n", o.Name, o.Err)
	case o.DeviceBehind != "":
		_, _ = fmt.Fprintf(w, "runhost %s runs CLI %s, newer than this computer's %s: update this computer's CLI to run there\n", o.Name, o.DeviceBehind, version.Version)
	case o.Updated && o.WaitsForJobs > 0:
		_, _ = fmt.Fprintf(w, "runhost %s took CLI %s (from %s); its service restarts into it once its %d running job(s) finish\n", o.Name, o.To, o.From, o.WaitsForJobs)
	case o.Updated:
		_, _ = fmt.Fprintf(w, "runhost %s runs CLI %s (from %s)\n", o.Name, o.To, o.From)
	case len(o.NeedsAttention) == 0:
		_, _ = fmt.Fprintf(w, "runhost %s already runs CLI %s\n", o.Name, o.To)
	}
	if o.GVisorUpdated {
		_, _ = fmt.Fprintf(w, "runhost %s runs gVisor %s\n", o.Name, o.GVisor)
	}
	for _, a := range o.NeedsAttention {
		_, _ = fmt.Fprintf(w, "runhost %s needs attention: %s\n", o.Name, a)
	}
}

// runhostPostUpdate is the post-update step: the first time this CLI's
// version runs a runhost command or a run on a runhost, it updates every
// runhost this computer pairs with. A runhost it cannot reach is said, and
// left for `agentry runhost update`.
func runhostPostUpdate(cmd *cobra.Command) {
	if onRunhostServer() || !clirelease.ValidVersion(version.Version) {
		return
	}
	home, err := runnerHome()
	if err != nil {
		return
	}
	marker := filepath.Join(home, runhostUpdatedFile)
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == version.Version { //nolint:gosec // the runner home's own file
		return
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return
	}
	var stale []string
	for _, r := range recs.Runhosts {
		if r.CLIVersion != version.Version {
			stale = append(stale, r.Name)
		}
	}
	if len(stale) > 0 {
		_, _ = fmt.Fprintf(narration(cmd), "this CLI is now %s: updating %s\n", version.Version, strings.Join(stale, ", "))
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		outs, err := updateRunhosts(ctx, cmd, stale)
		cancel()
		w := narration(cmd)
		for _, o := range outs {
			if o.Err != "" {
				_, _ = fmt.Fprintf(w, "warning: runhost %s was not updated (%s): agentry runhost update %s tries again\n", o.Name, o.Err, o.Name)
				continue
			}
			printUpdateOutcome(w, o)
		}
		if err != nil {
			_, _ = fmt.Fprintf(w, "warning: the runhosts' update: %v\n", err)
		}
	}
	_ = os.WriteFile(marker, []byte(version.Version+"\n"), 0o600)
}

func newRunhostPairCmd() *cobra.Command {
	var af adminFlags
	cmd := &cobra.Command{
		Use:   "pair <name> ssh://[user@]host[:port]",
		Short: "Pair this computer with a runhost set up from another of yours, with the admin sign-in",
		Long: "Signs in to the server with the admin sign-in and installs this computer's own\n" +
			"run key and sealing key, kept in its system keychain, and nothing else. The\n" +
			"runhost must belong to the Agentry account this computer is signed in to; a\n" +
			"runhost set up signed out is bound to that account now. The server's host key\n" +
			"is checked as at setup. Then, over the new run key: a runhost on an older CLI\n" +
			"than this computer's is updated to it, and one on a newer CLI asks this\n" +
			"computer to update first. The admin sign-in is kept nowhere.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunhostPair(cmd, args[0], args[1], &af)
		},
	}
	af.bind(cmd)
	return cmd
}

func runRunhostPair(cmd *cobra.Command, name, target string, af *adminFlags) error {
	if err := runhost.ValidName(name); err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	answer, err := parseSSHTarget(target)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	recs, err := runhost.LoadRecords(home)
	if err != nil {
		return err
	}
	if _, ok := recs.Find(name); ok {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("this computer already pairs with a runhost named %s", name))
	}
	ask := terminalAsker(cmd)
	auth, err := af.resolve(ask, answer.String())
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	deviceID, err := runhost.DeviceID(home)
	if err != nil {
		return err
	}
	label, _ := os.Hostname()
	say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
	out, err := rhsetup.Pair(cmd.Context(), rhsetup.PairOptions{
		Name: name, Answer: answer, Auth: auth,
		KnownHosts: targethost.DefaultKnownHostsFiles(home),
		Ask:        ask, Say: say,
		Version: version.Version, Owner: runhostOwner(cmd),
		DeviceID: deviceID, DeviceLabel: label,
	})
	if out != nil && out.Record.Name != "" {
		recs.Put(out.Record)
		if serr := recs.Save(home); serr != nil {
			return errors.Join(err, serr)
		}
	}
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	if out.Bound {
		say(fmt.Sprintf("runhost %s now belongs to %s", name, out.Record.Owner))
	}
	var up *runhostUpdateResult
	if out.Record.CLIVersion != version.Version {
		outs, err := updateRunhosts(cmd.Context(), cmd, []string{name})
		if err != nil {
			return err
		}
		up = &outs[0]
	}
	if flags.jsonOut {
		rec, _ := recs.Find(name)
		doc := map[string]any{"runhost": rec, "bound": out.Bound}
		if up != nil {
			doc["update"] = up
		}
		return emitJSON(cmd.OutOrStdout(), doc)
	}
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "this computer is paired with runhost %s (%s, owned by %s)\n", name, out.Record.Host, out.Record.Owner)
	if up != nil {
		printUpdateOutcome(w, *up)
	}
	return nil
}

func newRunhostUnpairCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "unpair <name>",
		Short: "Take this computer off a runhost, over its run key, and forget the runhost here",
		Long: "Asks the runhost to drop this computer's run key and sealing key, and to\n" +
			"revoke the Agentry credits token this computer gave it, if it did. The\n" +
			"runhost and its other devices go on as before. Then this computer's keys and\n" +
			"its record of the runhost are removed. The last device of a runhost cannot\n" +
			"unpair: agentry runhost remove takes the runhost off its server instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			home, err := runnerHome()
			if err != nil {
				return err
			}
			recs, err := runhost.LoadRecords(home)
			if err != nil {
				return err
			}
			rec, ok := recs.Find(name)
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no runhost named %s on this computer", name))
			}
			if !yes {
				ask := terminalAsker(cmd)
				if ask == nil {
					return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
				}
				ok, err := ask.Confirm(fmt.Sprintf("Unpair this computer from runhost %s?", name), false)
				if err != nil || !ok {
					return errors.Join(err, errors.New("nothing changed"))
				}
			}
			keys, err := runhost.LoadKeys(name)
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			say := func(s string) { _, _ = fmt.Fprintln(narration(cmd), s) }
			out, err := remote.Unpair(cmd.Context(), *rec, keys, say)
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			if out.Credits.Credits != nil {
				var b strings.Builder
				printCreditsRemoved(&b, name, out.Credits)
				say(strings.TrimSpace(b.String()))
			}
			if err := runhost.DeleteKeys(name); err != nil {
				say("warning: this computer's keys for " + name + " could not be removed from the system keychain: " + err.Error())
			}
			if jobs, err := remote.LoadJobs(home); err == nil {
				for _, j := range jobs {
					if j.Runhost == name {
						remote.DropJob(home, j.Job)
					}
				}
			}
			recs.Drop(name)
			if err := recs.Save(home); err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"unpaired": name, "credits": out.Credits})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "this computer is no longer paired with runhost %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "unpair without asking")
	return cmd
}
