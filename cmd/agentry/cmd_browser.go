// `agentry browser ensure`: pull the browser image this CLI pins ahead of
// any run. A sandbox host runs it from the runhost image as it is set up
// and at each release roll, as it runs `files ensure`, so no hosted run of
// a QuickAgent that declares capabilities.browser downloads Chromium inside
// a paid run.
package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// browserDocker is the docker client the browser commands use (tests
// replace it).
var browserDocker = func() dockerx.Client { return dockerx.New() }

// browserPullAttempts is how often ensure tries a pull before it gives
// up, waiting browserPullBackoff times the attempt between tries.
const (
	browserPullAttempts = 3
	browserPullBackoff  = 5 * time.Second
)

func newBrowserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "The browser image a QuickAgent that tests websites runs",
		Long: "A QuickAgent that declares capabilities.browser gets a headless Chromium in the browser image,\n" +
			"which this CLI pins by digest and pulls the first time such a run starts.",
	}
	cmd.AddCommand(newBrowserEnsureCmd())
	return cmd
}

func newBrowserEnsureCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Pull the browser image, so no run has to",
		Long: "Pulls the browser image the runner policy names (images.browser, else the one this CLI pins), by\n" +
			"digest, unless it is already on this machine, and prints its reference. A reference not pinned by\n" +
			"digest is refused. Nothing is built and no run starts. Without --policy, the policy file under the\n" +
			"runner home is read if there is one, and this CLI's defaults otherwise; nothing is written.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, err := filesPolicy(policyPath)
			if err != nil {
				return err
			}
			ref := pol.Images.BrowserImage()
			if !policy.PinnedByDigest(ref) {
				return engine.Classify(engine.ExitPolicy, fmt.Errorf("the browser image %q is not pinned by digest; images.browser must be <name>@sha256:<digest>", ref))
			}
			d := browserDocker()
			ok, err := d.ImageExists(cmd.Context(), ref)
			if err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			if !ok {
				stderr := cmd.ErrOrStderr()
				_, _ = fmt.Fprintf(stderr, "pulling the browser image %s (once on this machine)\n", ref)
				for i := 1; ; i++ {
					if err = d.Pull(cmd.Context(), ref); err == nil {
						break
					}
					if i == browserPullAttempts {
						return engine.Classify(engine.ExitGateway, fmt.Errorf("could not pull the browser image %s, in which a QuickAgent that tests websites runs its browser, after %d attempts: %w", ref, browserPullAttempts, err))
					}
					_, _ = fmt.Fprintf(stderr, "pull failed (%v); trying again\n", err)
					select {
					case <-cmd.Context().Done():
						return cmd.Context().Err()
					case <-time.After(browserPullBackoff * time.Duration(i)):
					}
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
			return err
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "the runner policy file (default: policy.yaml under the runner home, if any)")
	return cmd
}
