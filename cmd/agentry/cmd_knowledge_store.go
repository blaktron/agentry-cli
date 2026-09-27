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
			return knowledgeClient(cmd, u)
		},
		// An organization's shared packs (Knowledge M8): the same account's
		// client, under the organization's routes.
		Shared: func(u, org string) (store.Shared, error) {
			c, err := knowledgeClient(cmd, u)
			if err != nil {
				return nil, err
			}
			return c.Shared(org), nil
		},
	}), nil
}

// knowledgeClient is the signed-in client of the registry at u, past the
// registry's minimum-version gate.
func knowledgeClient(cmd *cobra.Command, u string) (*remote.Client, error) {
	token := credentialFor(u)
	if token == "" {
		return nil, fmt.Errorf("%s: %w (sign in with `agentry login --registry %s`)", u, store.ErrSignedOut, u)
	}
	if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), u, token); err != nil {
		return nil, err
	}
	return remote.New(u, token, httpClient)
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
	Name    string         `json:"name"`
	ID      string         `json:"id"`
	Pages   int            `json:"pages"`
	Bytes   int64          `json:"bytes"`
	SHA256  string         `json:"sha256"`
	Scan    knowledge.Scan `json:"scan"`
	Where   string         `json:"where"`
	Key     string         `json:"key"`
	Wrapper store.Wrapper  `json:"wrapper"`
	// Org is the organization whose shared pack this is (Knowledge M8).
	Org     string             `json:"org,omitempty"`
	Agentry *store.AgentryCopy `json:"agentry,omitempty"`
}

func entryJSON(e store.Entry) knowledgeEntryJSON {
	return knowledgeEntryJSON{Name: e.Name(), ID: e.ID(), Pages: e.Manifest.Pages, Bytes: e.Manifest.Bytes,
		SHA256: e.Manifest.SHA256, Scan: e.Manifest.Scan, Where: e.Where(), Key: e.KeyHolder(),
		Wrapper: e.Key.Wrapper, Org: e.Key.Org, Agentry: e.Key.Agentry}
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
	var org string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the knowledge packs kept sealed on this computer",
		Long: "List the knowledge packs kept sealed on this computer: your own, and any organization's shared packs you\n" +
			"downloaded. With --org ORG, list instead the packs ORG shares in Agentry, which you can download while you\n" +
			"are one of its members.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org != "" {
				return listShared(cmd, org)
			}
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
	cmd.Flags().StringVar(&org, "org", "", "list the packs this organization shares in Agentry instead")
	return cmd
}

// listShared prints the packs org shares at the registry this invocation
// speaks to, for one of its members.
func listShared(cmd *cobra.Command, org string) error {
	c, err := knowledgeClient(cmd, registryURL())
	if err != nil {
		return err
	}
	packs, err := c.Shared(org).List(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if flags.jsonOut {
		type sharedJSON struct {
			Name       string `json:"name"`
			ID         string `json:"id"`
			State      string `json:"state"`
			Pages      int    `json:"pages"`
			Bytes      int64  `json:"bytes"`
			Org        string `json:"org"`
			Exportable bool   `json:"exportable"`
		}
		list := make([]sharedJSON, 0, len(packs))
		for _, p := range packs {
			list = append(list, sharedJSON{Name: p.Name, ID: p.ID, State: p.State, Pages: p.Pages, Bytes: p.Bytes, Org: org, Exportable: p.Exportable})
		}
		printJSON(out, list)
		return nil
	}
	if len(packs) == 0 {
		_, _ = fmt.Fprintf(out, "%s shares no knowledge packs in Agentry.\n", org)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPAGES\tBYTES\tSTATE\tMEMBERS MAY EXPORT")
	for _, p := range packs {
		may := "no (admins only)"
		if p.Exportable {
			may = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", p.Name, p.Pages, p.Bytes, p.State, may)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(out, "Keep one on this computer with `agentry knowledge download --org %s NAME`. Its key is %s's, which Agentry hands only to its members, so it opens only while you are one.\n", org, org)
	return nil
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
	var org string
	cmd := &cobra.Command{
		Use:   "download NAME",
		Short: "Keep the copy Agentry holds of a knowledge pack on this computer",
		Long: "Download the pack NAME from the registry you are signed in to, sealed as it is kept there, open it with the\n" +
			"key the registry unwraps for your account, check it, and keep it here. A pack kept here with the same id is\n" +
			"replaced by Agentry's copy; a different pack with the same name is not.\n\n" +
			"With --org ORG, download ORG's shared pack NAME instead. Its key stays the organization's: it is asked for\n" +
			"every time the pack is opened, never kept in the keychain, so the pack opens only while you are a member and\n" +
			"signed in.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := knowledgeStore(cmd)
			if err != nil {
				return err
			}
			var e store.Entry
			if org != "" {
				e, err = s.DownloadShared(cmd.Context(), org, args[0])
			} else {
				e, err = s.Download(cmd.Context(), args[0])
			}
			if err != nil {
				return storeErr(err)
			}
			return printKept(cmd, e, "downloaded and kept")
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "download this organization's shared pack")
	return cmd
}

func newKnowledgeExportCmd() *cobra.Command {
	var to string
	var yes bool
	cmd := &cobra.Command{
		Use:   "export NAME --to DIR",
		Short: "Write a kept knowledge pack out unencrypted, into a new or empty directory",
		Long: "Open the kept pack NAME and write its manifest, pages and documents in the clear into DIR, which must be\n" +
			"new or empty; nothing is written anywhere else. On a terminal you are asked to type \"yes\" first; --yes\n" +
			"skips the question for scripts. An organization's shared pack is exported by Agentry, which allows it to\n" +
			"the organization's admins, and to its members once an admin marks the pack exportable.",
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
			p, err := s.Export(cmd.Context(), args[0], dst)
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
			noCopyRemains := e.Key.Agentry == nil || alsoRemote
			if noCopyRemains && !yes {
				prompt := fmt.Sprintf("delete knowledge pack %s? No copy of it will be left. Type \"yes\": ", e.Name())
				if err := promptYes(cmd, prompt,
					"no copy of the pack would be left: confirm on a terminal, or pass --yes",
					"delete cancelled: nothing was deleted"); err != nil {
					return engine.Classify(engine.ExitUsage, err)
				}
			}
			if _, err := s.Delete(cmd.Context(), e.Name(), alsoRemote); err != nil {
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
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Agentry still keeps a copy of it: `agentry knowledge download %s` brings it back\n", e.Name())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation (scripts)")
	cmd.Flags().BoolVar(&alsoRemote, "remote", false, "also delete the copy Agentry keeps")
	return cmd
}

// knowledgeArgIsPath says whether a --knowledge (or add) value is written
// as a path: it holds a path separator, or is "." or "..". Anything else is
// a pack's name, looked up in the store first (a directory of that name in
// the working directory is taken only when the store keeps no such pack,
// as M1 took every value).
func knowledgeArgIsPath(arg string) bool {
	return strings.ContainsAny(arg, `/\`) || arg == "." || arg == ".."
}

// isDir says whether path is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
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
