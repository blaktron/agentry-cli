package main

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
)

// Target hosts at `agentry run` (agentry-notes plans/runtime-hosts.md §6;
// agentry-cli#861): a release that declares one asks for a machine when it
// runs, and the run readies each before anything starts: its address, its
// host key (trusted by the person, at a terminal, when no file knows it),
// and its sign-in from --host-key / --host-password, the keychain or a
// prompt. A hosted run refuses one before anything is installed (D11).

// targetHostFlags are the run's target-host flags.
type targetHostFlags struct {
	key, passphrase, password, sudo []string
}

func addTargetHostFlags(cmd *cobra.Command, f *targetHostFlags) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.key, "host-key", nil, "sign in to a target host with a private key FILE: id=PATH (repeatable; otherwise the sign-in saved for the account and host in the system keychain, then a prompt on a terminal)")
	fl.StringArrayVar(&f.passphrase, "host-passphrase", nil, "the passphrase of a target host's key: id=ENV_NAME, the name of an environment variable holding it (repeatable)")
	fl.StringArrayVar(&f.password, "host-password", nil, "sign in to a target host with a password: id=ENV_NAME, the name of an environment variable holding it (repeatable)")
	fl.StringArrayVar(&f.sudo, "host-sudo-password", nil, "the sudo password of a target host's account: id=ENV_NAME, the name of an environment variable holding it (repeatable; without one, sudo works only where it asks for none)")
}

// refuseHostedTargetHosts refuses, before anything is installed, a hosted
// run of a release that declares a target host (plan D11).
func refuseHostedTargetHosts(plan *spec.Plan, opts runOpts) error {
	ids := plan.Capabilities.TargetHosts
	if len(ids) == 0 || (!hostedRun(opts) && !hostedProcess() && opts.reviewInputs == "") {
		return nil
	}
	return engine.HostedTargetHostRefusal(ids)
}

// readyTargetHosts readies each target host the answers name, in the
// plan's order. A refusal is a policy refusal with its reason (exit 11).
func readyTargetHosts(cmd *cobra.Command, answers *intake.Answers, plan *spec.Plan, pol *policy.Policy, home string, opts runOpts) error {
	f := opts.hostFlags
	flags, err := targethost.ParseFlags(f.key, f.passphrase, f.password, f.sudo)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	ids := plan.Capabilities.TargetHosts
	if unknown := flags.Unknown(ids); len(unknown) > 0 {
		slices.Sort(unknown)
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a --host-* flag names %v, which is none of this QuickAgent's target hosts (%v)", unknown, ids))
	}
	if len(ids) == 0 {
		return nil
	}
	var ask targethost.Asker
	if stdinIsTTY(cmd.InOrStdin()) {
		ask = &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
	}
	say := func(msg string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg) }
	o := targethost.Options{
		Hosts: pol.Hosts, KnownHosts: targethost.DefaultKnownHostsFiles(home), Flags: flags,
		Getenv: os.Getenv, Ask: ask, Say: say,
	}
	for _, id := range ids {
		h := answers.Hosts[id]
		if h == nil {
			// Never answered: the engine refuses it by name.
			continue
		}
		ready, err := targethost.Prepare(cmd.Context(), id, h.Answer, o)
		if err != nil {
			var r *targethost.Refusal
			if errors.As(err, &r) {
				return engine.Classify(engine.ExitPolicy, receipt.WithReason(err, receipt.ReasonHostRefused, r.Detail, id))
			}
			return engine.Classify(engine.ExitCLI, err)
		}
		answers.Hosts[id] = ready
		say(fmt.Sprintf("target host %s: %s at %s, host key %s (%s)", id, ready.Answer, ready.Address, ready.HostKey.Fingerprint(), ready.HostKey.From))
	}
	return nil
}
