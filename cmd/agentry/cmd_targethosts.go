package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

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
	// pins are the host keys a schedule was made with (--host-key-pin,
	// hidden: a schedule's fire passes them; agentry-cli#863).
	pins []string
}

func addTargetHostFlags(cmd *cobra.Command, f *targetHostFlags) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.key, "host-key", nil, "sign in to a target host with a private key FILE: id=PATH (repeatable; otherwise the sign-in saved for the account and host in the system keychain, then a prompt on a terminal)")
	fl.StringArrayVar(&f.passphrase, "host-passphrase", nil, "the passphrase of a target host's key: id=ENV_NAME, the name of an environment variable holding it (repeatable)")
	fl.StringArrayVar(&f.password, "host-password", nil, "sign in to a target host with a password: id=ENV_NAME, the name of an environment variable holding it (repeatable)")
	fl.StringArrayVar(&f.sudo, "host-sudo-password", nil, "the sudo password of a target host's account: id=ENV_NAME, the name of an environment variable holding it (repeatable; without one, sudo works only where it asks for none)")
}

// addHostKeyPinFlag is the run's hidden --host-key-pin, which a
// schedule's fire passes: id=SHA256:… refuses any other key the host
// offers (plan runtime-hosts.md D12).
func addHostKeyPinFlag(cmd *cobra.Command, f *targetHostFlags) {
	cmd.Flags().StringArrayVar(&f.pins, hostKeyPinFlag, nil, "refuse a target host whose key is not this one: id=SHA256:… (repeatable; a schedule's runs pass it)")
	_ = cmd.Flags().MarkHidden(hostKeyPinFlag)
}

const hostKeyPinFlag = "host-key-pin"

// parseHostKeyPins reads --host-key-pin's id=SHA256:… values.
func parseHostKeyPins(vals []string) (map[string]string, error) {
	pins := map[string]string{}
	for _, v := range vals {
		id, fp, ok := strings.Cut(v, "=")
		if !ok || id == "" || !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("--%s %q: want id=SHA256:…", hostKeyPinFlag, v)
		}
		pins[id] = fp
	}
	return pins, nil
}

// prepareTargetHost readies one target host; a variable so a test can
// stand in for the network (agentry-cli#863).
var prepareTargetHost = targethost.Prepare

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
	return readyHosts(cmd, answers, plan.Capabilities.TargetHosts, pol, home, opts.hostFlags, false)
}

// readyHosts readies the target hosts ids names. keep is a schedule being
// made: each sign-in is saved in the keychain, where its runs read it.
func readyHosts(cmd *cobra.Command, answers *intake.Answers, ids []string, pol *policy.Policy, home string, f targetHostFlags, keep bool) error {
	flags, err := targethost.ParseFlags(f.key, f.passphrase, f.password, f.sudo)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	pins, err := parseHostKeyPins(f.pins)
	if err != nil {
		return engine.Classify(engine.ExitUsage, err)
	}
	if unknown := flags.Unknown(ids); len(unknown) > 0 {
		slices.Sort(unknown)
		return engine.Classify(engine.ExitUsage, fmt.Errorf("a --host-* flag names %v, which is none of this QuickAgent's target hosts (%v)", unknown, ids))
	}
	for id := range pins {
		if !slices.Contains(ids, id) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--%s names %s, which is none of this QuickAgent's target hosts (%v)", hostKeyPinFlag, id, ids))
		}
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
		o.Pin, o.Keep = pins[id], keep
		ready, err := prepareTargetHost(cmd.Context(), id, h.Answer, o)
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
