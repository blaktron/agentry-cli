package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The json output form of the commands that run a built-in package on a
// sandbox host's behalf — `knowledge create|add` and `draft`: the
// built-in run's envelope on stdout, as `agentry run` writes one, with
// the command's own report beside it.

// envelopePrinted marks a command's error whose envelope is already on
// stdout, so the wrapper does not write a second.
type envelopePrinted struct{ err error }

func (e *envelopePrinted) Error() string { return e.err.Error() }
func (e *envelopePrinted) Unwrap() error { return e.err }

// printBuiltinFailure is the envelope of a command that stopped before the
// built-in ref's run wrote one: the class and the stage, no usage.
func printBuiltinFailure(cmd *cobra.Command, ref, stage string, err error) {
	code := engine.ExitCLI
	var ce *engine.ClassifiedError
	if errors.As(err, &ce) {
		code = ce.Code
	}
	msg := err.Error()
	if cmd.Context().Err() != nil {
		code, msg = engine.ExitCLI, "interrupted"
	}
	printEnvelope(cmd, &receipt.Envelope{
		Agentry: receipt.EnvelopeVersion, Status: "error", ExitCode: code, Error: msg, Stage: stage,
		Ref: ref, CapabilitiesEnforced: map[string]string{}, Result: receipt.TextResult(""),
	}, "json")
}

// printBuiltinEnvelope is the envelope of a command the built-in ran in:
// the run's own, with report beside it under key (none when report is
// nil). A command whose step after the run failed says so in the status,
// class and stage over the run's own; one interrupted (a driver's SIGINT,
// which is how a sandbox host stops a built-in at its cap) says
// "interrupted", and never with exit 0.
func printBuiltinEnvelope(ctx context.Context, out io.Writer, env receipt.Envelope, key string, report any, afterErr error) {
	if ctx.Err() != nil {
		env.Status, env.Error = "error", "interrupted"
		if env.ExitCode == 0 {
			env.ExitCode = engine.ExitCLI
		}
	} else if afterErr != nil {
		env.Status, env.ExitCode, env.Error, env.Stage = "error", engine.ExitCLI, afterErr.Error(), receipt.StageOutput
		var ce *engine.ClassifiedError
		if errors.As(afterErr, &ce) {
			env.ExitCode = ce.Code
		}
	}
	raw, err := env.Marshal()
	if err != nil {
		return
	}
	if report == nil {
		_, _ = out.Write(raw)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	if fields[key], err = json.Marshal(report); err != nil {
		return
	}
	b, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return
	}
	_, _ = out.Write(append(b, '\n'))
}
