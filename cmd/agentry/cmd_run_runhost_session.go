package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/payload"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/tui"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A session on a remote runhost, as this device's chat or session protocol
// drives it: the session itself runs on the runhost under its session
// protocol, its commands go over the job's input and its events come back as
// frames, so the TUI and the session protocol drive it as they drive a local
// session.Session.

// runOnSession runs an interactive session of the release on the runhost:
// the terminal chat drives it, or the session protocol does with
// --session-protocol, exactly as they drive a local session. A connection
// lost mid-session leaves the session on the runhost to its idle timeout, as
// a local session whose chat goes quiet; the device follows it to its end and
// collects it.
func runOnSession(cmd *cobra.Command, client *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys, j *job.Job, archive []byte,
	files *payload.Packed, lj remote.LocalJob, m *manifest.Manifest, pol *policy.Policy, opts runOpts, carried *runOnCarried, say func(string)) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	var started bool
	s := remote.NewSession(func(ctx context.Context, res runhostJobResult, err error, next int) (*receipt.Envelope, error) {
		carried.finishProfile(errOut)
		c := client
		if err != nil && !started {
			if home, herr := runnerHome(); herr == nil {
				remote.DropJob(home, j.ID)
			}
			return nil, engine.Classify(remote.SendClass(err), err)
		}
		if err != nil {
			var c2 *runhost.Client
			res, c2, err = remote.Reattach(ctx, rec, keys, j.ID, next, errOut, say, nil)
			if err != nil {
				return nil, engine.Classify(engine.ExitGateway, fmt.Errorf("%w: %v; the session goes on there to its idle timeout: agentry runhost attach %s %s collects it", errSessionLost, err, rec.Name, j.ID))
			}
			defer func() { _ = c2.Close() }()
			c = c2
		}
		return runOnFinishEnv(cmd, c, rec, keys, lj, res.ExitCode)
	})
	go func() {
		res, st, next, err := remote.Start(ctx, client, j, archive, files, false, say, s.Frame, s.Input)
		started = st
		s.Over(res, err, next)
	}()
	defer func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = s.End(endCtx, false)
	}()
	if opts.sessionProtocol {
		return runProtocol(cmd, s, opts)
	}
	topts := tui.Options{
		Ref: lj.Ref, Model: pol.Model.Provider + "/" + pol.Model.Name, Caps: capabilityLines(m.Contract()),
		MaxTurns: opts.maxTurns, MaxCostUSD: opts.maxCost, HasTask: strings.TrimSpace(j.Run.Task) != "",
		Budgets: spec.SessionBudgets{IdleTimeoutSeconds: pol.Sessions.IdleTimeoutSeconds, MaxWallSeconds: pol.Sessions.MaxWallSeconds, MaxUserTurns: pol.Sessions.MaxUserTurns},
	}
	if m.Interactive != nil {
		topts.Opening = m.Interactive.Opening
	}
	outcome, uiErr := runChat(ctx, s, topts, cmd.InOrStdin(), cmd.OutOrStdout())
	if uiErr != nil {
		_, _ = fmt.Fprintf(errOut, "chat closed with an error: %v\n", uiErr)
	}
	env, runErr := s.End(ctx, outcome.Finish)
	if env != nil {
		if _, reason := s.Ended(); reason != "" {
			_, _ = fmt.Fprintf(errOut, "session ended: %s\n", reason)
		}
		_, _ = fmt.Fprintf(errOut, "transcript, reports and receipts: %s\n", env.OutDir)
		printDeliverySummary(errOut, env)
	}
	if runErr == nil && uiErr != nil {
		return engine.Classify(engine.ExitCLI, uiErr)
	}
	return runErr
}

var errSessionLost = errors.New("the connection to the runhost was lost during the session")
