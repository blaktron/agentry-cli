// `agentry files ensure`: pull the files image this CLI pins, and refresh the
// ClamAV signatures, ahead of any run. A sandbox host runs it from the runhost
// image at each update, so the first hosted run with inputs downloads neither
// the decoders nor the signatures inside a paid run. It is the decode step's
// own pull and refresh, alone.
package main

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// filesDocker is the docker client the files commands use, and
// filesSignatures the signatures' refresh (tests replace both).
var (
	filesDocker     = func() dockerx.Client { return dockerx.New() }
	filesSignatures = clamdb.Ensure
)

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
		Short: "Pull the files image and refresh the ClamAV signatures, so no run has to",
		Long: "Pulls the files image the runner policy names (images.files, else the one this CLI pins), by digest,\n" +
			"unless it is already on this machine, and prints its reference. A reference not pinned by digest is\n" +
			"refused. It then refreshes the ClamAV signatures the malware check reads (images.clamavSignatures,\n" +
			"else this CLI's default) under the runner home when they were last fetched over a day ago, verifying\n" +
			"their signature, and says on stderr which it holds; a refresh that fails is said, not an error, since a\n" +
			"run uses the signatures it has. Nothing is built and no run starts. Without --policy, the policy file\n" +
			"under the runner home is read if there is one, and this CLI's defaults otherwise; nothing is written.",
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
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), ref); err != nil {
				return err
			}
			home, err := policy.Home()
			if err != nil {
				return engine.Classify(engine.ExitPolicy, err)
			}
			stderr := cmd.ErrOrStderr()
			db := filesSignatures(cmd.Context(), home, pol.Images.ClamAVSignaturesRef(), stderr)
			if !db.Held() {
				_, err = fmt.Fprintf(stderr, "clamav: %s from %s; runs check no file until a refresh succeeds\n", db.Problem, db.Ref)
				return err
			}
			line := "clamav: holding " + db.Describe()
			if db.Problem != "" {
				line += "; " + db.Problem + ", and runs use these until one succeeds"
			}
			_, err = fmt.Fprintln(stderr, line)
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
