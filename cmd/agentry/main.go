// agentry is the developer CLI (§11): init, validate, login, publish,
// install, verify, search, info, and yank against an Agentry registry.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/internal/version"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// defaultRegistryURL is the fallback registry when neither --registry nor the
// config file names one (a local dev agentryd). It is a var, not a const, so
// a release build can bake its own registry in — binaries served from a
// site's /download page connect to that site by default:
//
//	go build -ldflags "-X main.defaultRegistryURL=https://registry.example.test" ./cmd/agentry
var defaultRegistryURL = "http://localhost:8080"

// globalFlags carries the §11 global flags shared by every subcommand.
type globalFlags struct {
	// registry is the --registry override; empty falls back to the config
	// file default and then defaultRegistryURL.
	registry string
	// jsonOut requests machine-readable output (info, search, verify).
	jsonOut bool
}

// flags is the process-wide flag state bound by newRootCmd.
var flags globalFlags

func main() {
	installRunGrant()
	// SIGINT and SIGTERM cancel the command's context (agentry-cli#124): a
	// run's docker calls take it, the engine's teardown runs on every path,
	// and the result envelope is written with the failure — an interrupted
	// run leaves its receipts, where a process killed by the signal left
	// nothing. Once the context is cancelled the handler is removed, so a
	// second signal kills the process the ordinary way.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agentry: "+err.Error())
		// `agentry run` classifies its failures (RUNNER-PLAN §R8) so
		// pipelines and cron can branch on the exit code; everything
		// else keeps the traditional 1.
		var classified *engine.ClassifiedError
		if errors.As(err, &classified) {
			os.Exit(classified.Code)
		}
		if code, ok := exitCodeOf(err); ok {
			os.Exit(code)
		}
		os.Exit(1)
	}
}

// newRootCmd assembles the CLI command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentry",
		Short:         "Agentry CLI — develop, validate, publish, install, and run QuickAgents (single-purpose AI agents)",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// The second line names the registry this binary falls back to when
	// nothing is configured: a release bakes production's or dev's by the
	// branch it was cut from (agentry-cli#240), and this is where a person
	// sees which.
	root.SetVersionTemplate("agentry version {{.Version}}\ndefault registry: " + defaultRegistryURL + "\n")
	root.PersistentFlags().StringVar(&flags.registry, "registry", "",
		"registry base URL (default: config defaultRegistry, else "+defaultRegistryURL+")")
	root.PersistentFlags().BoolVar(&flags.jsonOut, "json", false,
		"machine-readable JSON output on stdout (search, info, verify, doctor, install, uninstall, list, builtins, validate, setup, run --plan); narration stays on stderr")

	root.AddCommand(
		newInitCmd(),
		newValidateCmd(),
		newLoginCmd(),
		newWhoamiCmd(),
		newPublishCmd(),
		newInstallCmd(),
		newSetupCmd(),
		newDoctorCmd(),
		newFilesCmd(),
		newRunCmd(),
		newKnowledgeCmd(),
		newVoiceCmd(),
		newDraftCmd(),
		newReviewCmd(),
		newBuiltinsCmd(),
		newProfilesCmd(),
		newUninstallCmd(),
		newListCmd(),
		newVerifyCmd(),
		newSearchCmd(),
		newInfoCmd(),
		newYankCmd(),
		newRunnerToolsCmd(),
		newRunnerConfineCmd(),
	)
	return root
}

// registryURL resolves the effective registry base URL: --registry flag,
// then the config file's defaultRegistry, then the local dev default.
// The returned URL never has a trailing slash.
func registryURL() string {
	if flags.registry != "" {
		return strings.TrimRight(flags.registry, "/")
	}
	if cfg, err := loadCLIConfig(); err == nil && cfg.DefaultRegistry != "" {
		return strings.TrimRight(cfg.DefaultRegistry, "/")
	} else if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agentry: warning: "+err.Error())
	}
	return defaultRegistryURL
}
