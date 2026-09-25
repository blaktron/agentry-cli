// `agentry knowledge list | import | upload | download | export | delete`
// (plan knowledge.md §4.3, §4.8; agentry-cli#319): the packs kept sealed in
// the local store the desktop shares, and their copies in Agentry.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/remote"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// knowledgeKeychain is the OS keychain the store caches keys in and keeps
// the machine key in; a seam for tests, which must never touch a person's.
var knowledgeKeychain = func() store.Keychain { return store.OSKeychain{} }

// knowledgeStore is the store under the runner home, wrapping new packs
// by the registry this invocation speaks to (--registry, else the default)
// when it holds a credential for it.
func knowledgeStore(cmd *cobra.Command) (*store.Store, error) {
	return knowledgeStoreFor(cmd, registryURL())
}

// knowledgeStoreFor is the store with current as the registry new packs
// are wrapped by.
func knowledgeStoreFor(cmd *cobra.Command, current string) (*store.Store, error) {
	home, err := runnerHome()
	if err != nil {
		return nil, err
	}
	return store.New(home, store.Options{
		Keys:    knowledgeKeychain(),
		Current: current,
		Registries: func(u string) (store.Registry, error) {
			token := credentialFor(u)
			if token == "" {
				return nil, fmt.Errorf("%s: %w (sign in with `agentry login --registry %s`)", u, store.ErrSignedOut, u)
			}
			if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), u, token); err != nil {
				return nil, err
			}
			return remote.New(u, token, httpClient)
		},
	}), nil
}

// rewrapOnSignIn moves packs kept under this machine's key to the account's
// key once a registry is signed in (plan D20), and says so.
func rewrapOnSignIn(cmd *cobra.Command, s *store.Store) {
	moved, err := s.Rewrap(cmd.Context())
	for _, name := range moved {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "knowledge pack %s: its key is now wrapped to your account, not this computer's\n", name)
	}
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v\n", err)
	}
}

// knowledgeEntryJSON is a kept pack in --json output.
type knowledgeEntryJSON struct {
	Name    string             `json:"name"`
	ID      string             `json:"id"`
	Pages   int                `json:"pages"`
	Bytes   int64              `json:"bytes"`
	SHA256  string             `json:"sha256"`
	Scan    knowledge.Scan     `json:"scan"`
	Where   string             `json:"where"`
	Key     string             `json:"key"`
	Wrapper string             `json:"wrapper"`
	Agentry *store.AgentryCopy `json:"agentry,omitempty"`
}

func entryJSON(e store.Entry) knowledgeEntryJSON {
	return knowledgeEntryJSON{Name: e.Name(), ID: e.ID(), Pages: e.Manifest.Pages, Bytes: e.Manifest.Bytes,
		SHA256: e.Manifest.SHA256, Scan: e.Manifest.Scan, Where: e.Where(), Key: e.KeyHolder(),
		Wrapper: e.Key.Wrapper, Agentry: e.Key.Agentry}
}

func printJSON(out io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	_, _ = fmt.Fprintln(out, string(b))
}

// storeErr classifies a store refusal: a pack that is not kept, or a name
// already kept, is the caller's to fix (usage).
func storeErr(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNameTaken) {
		return engine.Classify(engine.ExitUsage, err)
	}
	return err
}

func newKnowledgeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the knowledge packs kept sealed on this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			entries, lerr := s.List()
			if lerr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agentry: warning: %v\n", lerr)
			}
			out := cmd.OutOrStdout()
			if flags.jsonOut {
				list := make([]knowledgeEntryJSON, 0, len(entries))
				for _, e := range entries {
					list = append(list, entryJSON(e))
				}
				printJSON(out, list)
				return nil
			}
			if len(entries) == 0 {
				_, _ = fmt.Fprintln(out, "No knowledge packs are kept on this computer. Make one with `agentry knowledge create NAME --file …`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tPAGES\tBYTES\tSCAN\tKEPT ON")
			for _, e := range entries {
				_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", e.Name(), e.Manifest.Pages, e.Manifest.Bytes, e.Manifest.Scan.Describe(), e.Where())
			}
			_ = tw.Flush()
			for _, e := range entries {
				_, _ = fmt.Fprintf(out, "%s: encrypted to %s\n", e.Name(), e.KeyHolder())
			}
			return nil
		},
	}
}

func newKnowledgeImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import DIR",
		Short: "Keep a knowledge pack directory sealed on this computer",
		Long: "Seal the knowledge pack in DIR into the store under the runner home, under a key of its own that the\n" +
			"registry you are signed in to wraps (signed out, this computer's key). DIR itself is left as it is:\n" +
			"delete it yourself once the pack is kept.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			rewrapOnSignIn(cmd, s)
			e, err := s.Keep(cmd.Context(), args[0])
			if err != nil {
				return storeErr(err)
			}
			return printKept(cmd, e, "kept")
		},
	}
}

// printKept says a pack is kept: text, or one --json entry.
func printKept(cmd *cobra.Command, e store.Entry, verb string) error {
	if flags.jsonOut {
		printJSON(cmd.OutOrStdout(), entryJSON(e))
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "knowledge pack %s %s on this computer: %s, %d bytes, %s — encrypted to %s\n",
		e.Name(), verb, knowledge.CountPages(e.Manifest.Pages), e.Manifest.Bytes, e.Manifest.Scan.Describe(), e.KeyHolder())
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "attach it to a run with: agentry run <ref> --knowledge %s\n", e.Name())
	return nil
}

func newKnowledgeUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload NAME",
		Short: "Keep a copy of a knowledge pack in Agentry, where it is scanned",
		Long: "Send the kept pack NAME to the registry you are signed in to, sealed and with its wrapped key as they\n" +
			"are. The registry opens it to check and scan it (every prose stage, then AI Review), and keeps it against\n" +
			"your allocation. Uploading again replaces the copy there.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			e, r, err := s.Upload(cmd.Context(), args[0])
			if err != nil {
				return storeErr(err)
			}
			if flags.jsonOut {
				printJSON(cmd.OutOrStdout(), map[string]any{"pack": entryJSON(e), "state": r.State, "id": r.ID, "digest": r.Digest})
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "knowledge pack %s uploaded to %s: %s, %d bytes, %s\n", e.Name(), registryURL(), knowledge.CountPages(r.Pages), r.Bytes, r.State)
			if r.State == store.StateScanning {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Agentry is scanning it; `agentry knowledge download` works once the scan ends.")
			}
			return nil
		},
	}
}

func newKnowledgeDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download NAME",
		Short: "Keep the copy Agentry holds of a knowledge pack on this computer",
		Long: "Download the pack NAME from the registry you are signed in to, sealed as it is kept there, open it with the\n" +
			"key the registry unwraps for your account, check it, and keep it here. A pack kept here with the same id is\n" +
			"replaced by Agentry's copy; a different pack with the same name is not.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			e, err := s.Download(cmd.Context(), args[0])
			if err != nil {
				return storeErr(err)
			}
			return printKept(cmd, e, "downloaded and kept")
		},
	}
}

func newKnowledgeExportCmd() *cobra.Command {
	var to string
	var yes bool
	cmd := &cobra.Command{
		Use:   "export NAME --to DIR",
		Short: "Write a kept knowledge pack out unencrypted, into a new or empty directory",
		Long: "Open the kept pack NAME and write its manifest, pages and documents in the clear into DIR, which must be\n" +
			"new or empty; nothing is written anywhere else. On a terminal you are asked to type \"yes\" first; --yes\n" +
			"skips the question for scripts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return engine.Classify(engine.ExitUsage, errors.New("name the directory to write the pack into with --to DIR"))
			}
			dst, err := filepath.Abs(to)
			if err != nil {
				return err
			}
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			if _, err := s.Find(args[0]); err != nil {
				return storeErr(err)
			}
			if !yes {
				prompt := fmt.Sprintf("export knowledge pack %s unencrypted into %s? Its pages and documents will be written there in the clear. Type \"yes\": ", args[0], dst)
				if err := promptYes(cmd, prompt,
					"export writes a pack in the clear: confirm on a terminal, or pass --yes",
					"export cancelled: nothing was written"); err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
			}
			p, err := s.Open(cmd.Context(), args[0], dst)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				printJSON(cmd.OutOrStdout(), map[string]any{"name": p.Manifest.Name, "dir": dst, "pages": p.Manifest.Pages, "bytes": p.Manifest.Bytes, "sha256": p.Manifest.SHA256})
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "knowledge pack %s exported unencrypted into %s: %s, %d bytes\n", p.Manifest.Name, dst, knowledge.CountPages(p.Manifest.Pages), p.Manifest.Bytes)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the directory to write the pack into (new or empty; required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation (scripts)")
	return cmd
}

func newKnowledgeDeleteCmd() *cobra.Command {
	var yes, alsoRemote bool
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a knowledge pack kept on this computer (--remote: and its copy in Agentry)",
		Long: "Delete the kept pack NAME and its cached key from this computer. A pack that Agentry does not also keep\n" +
			"is gone for good, so on a terminal you are asked to type \"yes\" first (--yes skips it). --remote also\n" +
			"deletes Agentry's copy.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			e, err := s.Find(args[0])
			if err != nil {
				return storeErr(err)
			}
			onlyCopy := e.Key.Agentry == nil || alsoRemote
			if onlyCopy && !yes {
				prompt := fmt.Sprintf("delete knowledge pack %s? No other copy of it will be kept. Type \"yes\": ", e.Name())
				if err := promptYes(cmd, prompt,
					"this is the only copy of the pack: confirm on a terminal, or pass --yes",
					"delete cancelled: nothing was deleted"); err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
			}
			if alsoRemote && e.Key.Agentry != nil {
				reg, err := s.RegistryFor(e.Key.Agentry.Registry)
				if err != nil {
					return err
				}
				if err := reg.Delete(cmd.Context(), e.Key.Agentry.ID); err != nil && !errors.Is(err, store.ErrRemoteNotFound) {
					return err
				}
			}
			if _, err := s.Delete(e.Name()); err != nil {
				return err
			}
			where := "this computer"
			if alsoRemote && e.Key.Agentry != nil {
				where = "this computer and Agentry"
			}
			if flags.jsonOut {
				printJSON(cmd.OutOrStdout(), map[string]any{"name": e.Name(), "deleted": where})
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "knowledge pack %s deleted from %s\n", e.Name(), where)
			if !alsoRemote && e.Key.Agentry != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Agentry still keeps a copy; delete it there with `agentry knowledge download %s` then `delete --remote`, or on the site\n", e.Name())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation (scripts)")
	cmd.Flags().BoolVar(&alsoRemote, "remote", false, "also delete the copy Agentry keeps")
	return cmd
}

// knowledgeArgIsDir says whether a --knowledge value names a directory
// rather than a kept pack: anything with a path separator, or a directory
// that exists.
func knowledgeArgIsDir(arg string) bool {
	if strings.ContainsAny(arg, `/\`) || arg == "." || arg == ".." {
		return true
	}
	fi, err := os.Stat(arg)
	return err == nil && fi.IsDir()
}

// rewrapAfterLogin is the sign-in's half of plan D20: packs kept signed
// out move to the account's key at the registry just signed in to. Never
// fatal: the login stands whatever the store says.
func rewrapAfterLogin(cmd *cobra.Command, reg string) {
	s, err := knowledgeStoreFor(cmd, reg)
	if err != nil {
		return
	}
	rewrapOnSignIn(cmd, s)
}
