package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localinstall"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry/pkg/apiclient"
)

// errLocalUnsigned is the refusal a local install without --allow-unsigned
// meets: a folder has no attestation.
var errLocalUnsigned = errors.New("a local folder is unsigned by definition; pass --allow-unsigned to install one")

// runLocalInstall is `agentry install --local DIR`: a
// QuickAgent folder on this machine, never published, installed into the
// runner home, where it lists with the installed QuickAgents and runs by
// name. It is unsigned, so the install and every run of it need
// --allow-unsigned, as `run --local` does. The folder is staged as publish
// packs it; the contract shown and consented to is the staged manifest's,
// and that same staged tree is laid down, sealed under the runner home's
// integrity key, so a run refuses the installed copy once its files
// change, as it refuses an installed release's; --repair lays it down
// again from the folder. --plan and --json answer as a registry install's.
func runLocalInstall(cmd *cobra.Command, dir string, opts installOpts) error {
	if opts.target != "" && opts.target != installdir.Target || opts.provider != "" || opts.dir != "" {
		return errors.New("--local installs into the runner home only: --target, --provider and --dir do not apply")
	}
	if opts.allowUnscanned {
		return errors.New("--allow-unscanned is for registry releases; a local folder is unsigned, and --allow-unsigned is its consent")
	}
	if !opts.allowUnsigned && !opts.plan {
		return engine.Classify(engine.ExitVerify, errLocalUnsigned)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("--local %s: not a directory", dir)
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if flags.jsonOut {
		// stdout carries the result; the narration moves to stderr.
		out = errOut
	}

	staged, err := localinstall.Stage(abs, installExtractLimits())
	if err != nil {
		if errors.Is(err, localinstall.ErrInvalidManifest) {
			return engine.Classify(engine.ExitCompile, err)
		}
		return err
	}
	defer func() { _ = staged.Close() }()
	for _, w := range staged.Warnings {
		_, _ = fmt.Fprintf(errOut, "warning: %v\n", w)
	}
	m := staged.Manifest
	_, _ = fmt.Fprintf(out, "%s (%s), local and unsigned, from %s (%s)\n",
		staged.Ref(), modesLabel(m.SupportedModes()), abs, staged.Digest)
	if m.Summary != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", m.Summary)
	}
	_, _ = fmt.Fprintln(out, "capabilities:")
	for _, line := range capabilityLinesSized(m.Contract(), nil) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}

	state, serr := installed.Load()
	if serr != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %v — treating this as a first install\n", serr)
		state = installed.State{}
	}
	key := installdir.ConsentKey(home, m.Namespace, m.Name)
	next := installed.Snapshot(m.Contract())
	att := trust.AttestationResult{State: trust.StateLocal}
	if opts.plan {
		var refusal error
		if !opts.allowUnsigned {
			refusal = errLocalUnsigned
		}
		res := &apiclient.ResolveResult{Namespace: m.Namespace, Name: m.Name, Version: m.Version}
		return writeInstallPlan(cmd, res, m, staged.Digest, att, refusal, &state, key, next, installdir.Target,
			installPaths{dest: key.Dir, managed: true})
	}
	if prev := state.Find(key); prev != nil && !prev.Local() {
		_, _ = fmt.Fprintf(errOut, "note: this replaces %s/%s@%s installed from a registry\n", m.Namespace, m.Name, prev.Version)
	}
	if err := confirmCapabilityChange(cmd, out, &state, key, m.Version, next, m.Contract(), opts.yes); err != nil {
		return err
	}
	style, warning := installdir.StyleForHome(home)
	if warning != "" {
		_, _ = fmt.Fprintf(errOut, "warning: %s\n", warning)
	}
	lay := staged.Install
	if opts.repair {
		lay = staged.Repair
	}
	releaseDir, err := lay(home, style)
	if err != nil {
		var changed *installdir.ChangedError
		if errors.As(err, &changed) {
			return engine.Classify(engine.ExitVerify, changed)
		}
		return err
	}
	_, _ = fmt.Fprintf(out, "installed %s (local, unsigned) into %s\n", staged.Ref(), releaseDir)
	_, _ = fmt.Fprintf(out, "run it with: agentry run %s/%s --allow-unsigned\n", m.Namespace, m.Name)
	if flags.jsonOut {
		contract, cdigest, err := contractJSON(m.Contract())
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), installResult{
			Ref: staged.Ref(), Namespace: m.Namespace, Name: m.Name, Version: m.Version, Digest: staged.Digest,
			Target: installdir.Target, Dir: key.Dir, Managed: true,
			Attestation: trust.StateLocal, Signed: false,
			Contract: contract, ContractDigest: cdigest, Recorded: true, Repaired: opts.repair,
		})
	}
	return nil
}
