// `agentry files ensure`: pull the files image this CLI pins, ahead of any
// run (plan file-handling.md D4's addendum; agentry-cli#572). A sandbox host
// runs it from the runhost image at each update (agentry-web#946), so
// the first hosted run with inputs does not download the decoders inside a
// paid run. It is the decode step's own pull, alone.
package main

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// filesDocker is the docker client the files commands use (tests replace it).
var filesDocker = func() dockerx.Client { return dockerx.New() }

func newFilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "The files image that turns a run's documents into text",
		Long: "Before an agent starts, the documents a run is given are decoded on this machine in the files image,\n" +
			"which this CLI pins by digest and pulls the first time a run has documents.",
	}
	cmd.AddCommand(newFilesEnsureCmd())
	return cmd
}

func newFilesEnsureCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Pull the files image the runner policy names, so no run has to",
		Long: "Pulls the files image the runner policy names (images.files, else the one this CLI pins), by digest,\n" +
			"unless it is already on this machine, and prints its reference. A reference not pinned by digest is\n" +
			"refused. Nothing is built and no run starts. Without --policy, the policy file under the runner home is\n" +
			"read if there is one, and this CLI's defaults otherwise; nothing is written.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, err := filesPolicy(policyPath)
			if err != nil {
				return err
			}
			ref, err := decode.PullFiles(cmd.Context(), filesDocker(), pol, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
			return err
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "the runner policy file (default: policy.yaml under the runner home, if any)")
	return cmd
}

// filesPolicy is the policy a files command reads: the named file, which
// must exist; else the runner home's, if there is one; else the defaults.
// Unlike a run it never writes a default policy. A policy it cannot use
// exits with the policy class, so a script tells it from a failed pull.
func filesPolicy(flag string) (*policy.Policy, error) {
	f, err := policy.Resolve(flag)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, err)
	}
	pol, _, err := policy.Load(f.Path)
	switch {
	case err == nil:
		return pol, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, engine.Classify(engine.ExitPolicy, err)
	case flag == "":
		return policy.Default(), nil
	default:
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the runner policy %s does not exist", f.Path))
	}
}
