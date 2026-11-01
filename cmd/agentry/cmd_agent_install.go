package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// agentInstall is what installing a release as a persistent agent adds to
// an install: the checks before anything is downloaded or asked, the
// persona and the subagents shown with the contract, and the agent's record
// once the release is laid down.
type agentInstall struct {
	home string
	pol  *policy.Policy
	// pins are the release's subagents as describe resolved them.
	pins []persistent.SubagentPin
	// local says the release is an author's folder, whose subagents are
	// never run.
	local bool
}

// check refuses what cannot be a persistent agent here: a release that does
// not declare the persistent mode, a built-in's reference, a policy that
// allows no persistent release, and a release that names an image server or
// a mail provider this build does not carry. A running agent is refused
// too: a new version is installed while it is stopped.
func (a *agentInstall) check(m *manifest.Manifest) error {
	ref := m.Namespace + "/" + m.Name
	if !m.IsPersistent() {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is not a persistent QuickAgent (its modes: %s): install it with agentry install, and run it with agentry run", ref, modesLabel(m.SupportedModes())))
	}
	if persistent.IsBuiltinRef(ref) {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is built into Agentry: enable it with agentry agent enable %s", ref, m.Name))
	}
	if !a.pol.PersistentReleasesAllowed() {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: policy.PersistentReleasesRefused})
	}
	// What a segment refuses at its start is refused here, before the person
	// is asked: a persistent agent's files are its share and data folders,
	// and it has no intake to name a target host.
	if fs := m.Capabilities.Filesystem; declaresFiles(fs.Read) || declaresFiles(fs.Write) {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: ref + " declares filesystem access, and a persistent agent's files are its share folder and its data volume alone"})
	}
	if len(m.Capabilities.TargetHosts) > 0 {
		return engine.Classify(engine.ExitPolicy, &compile.PolicyRefusal{Reason: ref + " declares a target host, which a persistent agent is never given: a target host is named in a run's intake"})
	}
	if err := localrun.CheckPersistentNames(m.Capabilities.Persistent); err != nil {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("%s cannot run on this version of Agentry: %w", ref, err))
	}
	if held, h := persistent.Held(a.home, persistent.Key(m.Namespace, m.Name)); held {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%w: stop it first (agentry agent stop %s), then install again", &persistent.HeldError{Agent: ref, Holder: h}, ref))
	}
	return nil
}

// persistentAgentsRelease refuses a plain install of a release that is
// installed as a persistent agent: the install would replace the version
// the agent is pinned to, which `agentry agent install` moves with its
// record.
func persistentAgentsRelease(home string, opts installOpts, namespace, name string) error {
	if opts.agent != nil {
		return nil
	}
	if _, err := persistent.LoadRelease(home, persistent.Key(namespace, name)); err == nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s/%s is installed as a persistent agent: update it with agentry agent install %s/%s", namespace, name, namespace, name))
	}
	return nil
}

// declaresFiles says a filesystem line grants access: declared, and not none.
func declaresFiles(a manifest.FSAccess) bool { return !a.IsZero() && a.Mode != manifest.FSModeNone }

// describe says, beside the contract, what a persistent agent is given that
// the contract does not list: the persona it takes on, and each subagent
// with the version it is pinned to now. resolve asks the registry for a
// subagent's newest version within its constraint; nil (an author's folder)
// runs none.
func (a *agentInstall) describe(ctx context.Context, w io.Writer, m *manifest.Manifest, resolve func(string) (*apiclient.ResolveResult, error)) {
	_, _ = fmt.Fprintln(w, "as a persistent agent:")
	_, _ = fmt.Fprintln(w, "  it keeps its conversation, its files and a share folder with you between starts, until you reset it")
	if p := m.Persona; p != nil {
		lock := "you may rename it and replace its personality"
		if !p.IsEditable() {
			lock = "you may rename it; its personality is locked by its publisher"
		}
		_, _ = fmt.Fprintf(w, "  persona: %s, from %s (%s)\n", safeTerminalText(p.Name, 64), m.PersonaFile(), lock)
	}
	subs := m.Subagents()
	if len(subs) == 0 {
		return
	}
	if resolve == nil {
		a.local = true
		_, _ = fmt.Fprintln(w, "  subagents: a folder installed from this computer runs none of the subagents it declares")
		return
	}
	a.pins = nil
	for _, dep := range subs {
		pin := persistent.SubagentPin{Ref: dep.Ref, Constraint: dep.Version, Why: dep.Why}
		pin.Version, pin.Unresolved = resolveSubagent(ctx, dep, resolve)
		a.pins = append(a.pins, pin)
		if pin.Version != "" {
			_, _ = fmt.Fprintf(w, "  subagent %s@%s (%s), pinned until you update the agent\n", dep.Ref, pin.Version, dep.Version)
		} else {
			_, _ = fmt.Fprintf(w, "  subagent %s (%s) resolves to no version it can run: %s\n", dep.Ref, dep.Version, pin.Unresolved)
		}
	}
}

// resolveSubagent is the version a subagent's constraint resolves to now,
// or why none does: the newest published version within it must run as an
// ordinary autonomous QuickAgent and not be persistent.
func resolveSubagent(ctx context.Context, dep manifest.AgentDependency, resolve func(string) (*apiclient.ResolveResult, error)) (version, unresolved string) {
	if ctx.Err() != nil {
		return "", ctx.Err().Error()
	}
	res, err := resolve(dep.Ref + "@" + dep.Version)
	if err != nil {
		return "", safeTerminalText(err.Error(), 300)
	}
	sm, err := manifestFromResolve(res)
	if err != nil {
		return "", safeTerminalText(err.Error(), 300)
	}
	switch {
	case res.Yanked:
		return "", res.Version + " is withdrawn"
	case sm.IsPersistent():
		return "", res.Version + " is a persistent QuickAgent, which never runs as a subagent"
	case !sm.Supports(manifest.ModeAutonomous):
		return "", res.Version + " does not run autonomously"
	case !dep.SubagentAllows(res.Version):
		return "", res.Version + " is outside the constraint " + dep.Version
	}
	return res.Version, ""
}

// agentInstallResult is the `agent install --json` shape.
type agentInstallResult struct {
	Agent     string                   `json:"agent"`
	Ref       string                   `json:"ref"`
	Version   string                   `json:"version"`
	Source    string                   `json:"source"`
	Digest    string                   `json:"digest"`
	Persona   string                   `json:"persona,omitempty"`
	Packs     []string                 `json:"packs,omitempty"`
	Subagents []persistent.SubagentPin `json:"subagents,omitempty"`
	Enabled   bool                     `json:"enabled"`
	// Recorded is false when the install's consent record could not be
	// saved: the next install asks again.
	Recorded bool `json:"recorded"`
}

// record writes the agent's install record once its release is laid down,
// enabling it, and says how to start it.
func (a *agentInstall) record(cmd *cobra.Command, r persistent.Release, m *manifest.Manifest, recorded bool) error {
	r.InstalledAt = time.Now().UTC()
	if !a.local {
		r.Subagents = a.pins
	}
	ag, err := persistent.SaveRelease(a.home, r)
	if err != nil {
		return engine.Classify(engine.ExitCLI, err)
	}
	cfg, err := persistent.LoadConfig(a.home, ag.Name)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		res := agentInstallResult{
			Agent: ag.Name, Ref: ag.Ref, Version: ag.Version, Source: ag.Source, Digest: r.Digest,
			Packs: ag.Packs, Subagents: ag.Subagents, Enabled: cfg.Enabled, Recorded: recorded,
		}
		if ag.Persona != nil {
			res.Persona = ag.Persona.Name
		}
		return emitJSON(cmd.OutOrStdout(), res)
	}
	out := cmd.OutOrStdout()
	state := "enabled"
	if !cfg.Enabled {
		state = "disabled, as you left it (agentry agent enable " + ag.Ref + ")"
	}
	_, _ = fmt.Fprintf(out, "%s is installed as a persistent agent, %s. It goes by %s.\n", ag.PackageRef(), state, safeTerminalText(ag.DisplayName(cfg), 64))
	printNextSteps(cmd.ErrOrStderr(),
		nextStep{command: "agentry agent start " + ag.Ref},
		nextStep{command: "agentry agent chat " + ag.Ref},
	)
	return nil
}

const agentInstallLong = "Installs a persistent QuickAgent from the registry, or from a folder on this\n" +
	"computer with --local, so it runs as a persistent agent beside the ones built\n" +
	"into Agentry. The release is verified as agentry install verifies it (its\n" +
	"digest and its signature), and you agree to what it declares: its\n" +
	"capabilities, the knowledge it ships, the persona it takes on and the\n" +
	"subagents it declares, each pinned to the newest version its constraint allows.\n\n" +
	"The agent is enabled and left stopped. Its conversation, its files and its share\n" +
	"folder are kept between starts and across updates. Installing it again\n" +
	"updates it: to the newest version, or the one you name, asking again if the\n" +
	"new version declares more. Stop it before you update it.\n\n" +
	"A folder installed with --local is unsigned, so it needs --allow-unsigned, and\n" +
	"it runs none of the subagents it declares.\n\n" +
	"A runner policy that sets allowPersistentReleases: false refuses every\n" +
	"persistent agent but the ones built into Agentry.\n\n" +
	"  agentry agent install acme/helper\n" +
	"  agentry agent install acme/helper@1.2.0\n" +
	"  agentry agent install --local ./my-agent --allow-unsigned"

func newAgentInstallCmd() *cobra.Command {
	var (
		opts       installOpts
		policyPath string
	)
	cmd := &cobra.Command{
		Use:   "install <ns/name[@version]> | --local <folder>",
		Short: "Install a persistent QuickAgent from the registry or a folder, to run as a persistent agent",
		Long:  agentInstallLong,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case opts.local != "" && len(args) > 0:
				return engine.Classify(engine.ExitUsage, errors.New("install from the registry or from a folder (--local), not both"))
			case opts.local == "" && len(args) == 0:
				return engine.Classify(engine.ExitUsage, errors.New("name the QuickAgent to install (ns/name), or a folder with --local"))
			case opts.local == "" && opts.allowUnsigned:
				return engine.Classify(engine.ExitUsage, errors.New("--allow-unsigned is for a folder installed with --local: a persistent agent from the registry is a signed release"))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			pol, _, _, err := agentPolicy(policyPath)
			if err != nil {
				return err
			}
			opts.agent = &agentInstall{home: home, pol: pol}
			if opts.local != "" {
				return runLocalInstall(cmd, opts.local, opts)
			}
			return runInstall(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.local, "local", "", "install a QuickAgent folder on this computer instead of a release from the registry")
	cmd.Flags().BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "with --local: agree to install the folder, which is unsigned")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "agree to what the agent declares without being asked")
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml, created on first use)")
	return cmd
}

// agentUninstallResult is the `agent uninstall --json` shape.
type agentUninstallResult struct {
	Agent       string          `json:"agent"`
	Ref         string          `json:"ref"`
	DataRemoved bool            `json:"dataRemoved"`
	Release     uninstallOutput `json:"release"`
}

func newAgentUninstallCmd() *cobra.Command {
	var removeData bool
	var confirm string
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Uninstall a persistent agent installed from the registry or a folder; its conversation and files are kept unless you remove them",
		Long: "Uninstalls a persistent agent you installed: it is no longer listed or\n" +
			"started, and its release, its installed MCP servers and your agreement to it\n" +
			"are removed from this computer. Its conversation, its files and everything\n" +
			"you chose for it are kept, and installing it again takes them up. With\n" +
			"--remove-data they go too: its volumes and its folder under the runner home.\n" +
			"The files in its share folder are yours and are always kept. Stop it first.\n" +
			"With --remove-data, type its reference when asked, or pass it as --confirm when\n" +
			"there is no terminal to ask on. The agents built into Agentry are disabled,\n" +
			"not uninstalled.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := agentNamed(args[0])
			if err != nil {
				return err
			}
			if !a.Published() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%s is built into Agentry and is not uninstalled: agentry agent disable %s", a.Title, a.Name))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			if held, h := persistent.Held(home, a.Name); held {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%w: stop it first (agentry agent stop %s)", &persistent.HeldError{Agent: a.Ref, Holder: h}, a.Ref))
			}
			if removeData {
				if confirm == "" {
					if !stdinIsTTY(cmd.InOrStdin()) {
						return engine.Classify(engine.ExitUsage, fmt.Errorf("--remove-data asks you to type the agent's reference, and there is no terminal: pass --confirm %s", a.Ref))
					}
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "This uninstalls %s and wipes its conversation, its files and everything you chose for it. The files in its share folder are kept. Type its reference (%s) to go on: ", a.Ref, a.Ref)
					line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
					confirm = strings.TrimSpace(line)
				}
				if confirm != a.Ref {
					return engine.Classify(engine.ExitUsage, errors.New("the reference did not match; nothing was uninstalled"))
				}
			}
			if err := persistent.Uninstall(cmd.Context(), home, a.Name, agentDocker(), removeData); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			out := cmd.OutOrStdout()
			if flags.jsonOut {
				out = cmd.ErrOrStderr()
			}
			release, err := uninstallRelease(cmd, out, a.Ref, false, false)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), agentUninstallResult{Agent: a.Name, Ref: a.Ref, DataRemoved: removeData, Release: release})
			}
			if removeData {
				_, _ = fmt.Fprintf(out, "%s is uninstalled, and its conversation and files are removed. The files in its share folder are kept.\n", a.Ref)
			} else {
				_, _ = fmt.Fprintf(out, "%s is uninstalled. Its conversation and files are kept: installing it again takes them up (agentry agent install %s).\n", a.Ref, a.Ref)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&removeData, "remove-data", false, "also remove its conversation, its files and everything you chose for it")
	cmd.Flags().StringVar(&confirm, "confirm", "", "with --remove-data: the agent's reference, to confirm without a terminal")
	return cmd
}
