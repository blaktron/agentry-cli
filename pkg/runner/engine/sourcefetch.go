package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// SourceFetcher runs the input-source fetch step: the runner image's
// `source` role in the files container, on a network of its own whose only
// route out is a CONNECT proxy to the source's host on port 443, at public
// addresses alone, with the token, when there is one, on the step's stdin
// and nowhere else. A run fetches each source the intake answered this way
// before its decode (fetchSources), and a knowledge build fetches each
// repository or address it was given the same way before its own
// (pkg/knowledge/build): one step, one posture, one set of checks on what
// the step wrote, whoever asked for the fetch.
type SourceFetcher struct {
	Docker dockerx.Client
	// ProxyImage is the runner image, whose proxy role guards the step;
	// FilesImage is the files image, whose source role fetches.
	ProxyImage, FilesImage string
	// Limits bound the step's and the proxy's containers.
	Limits policy.Limits
	// User is the person's user on this engine (dockerx.PersonUser): the
	// files the step writes are theirs, and it runs with no more than they
	// have.
	User string
	// RunID names and labels the step's containers and network, so the
	// run's own teardown and the next run's sweep find them.
	RunID string
	// Work is a folder of the caller's: each source's tree and report live
	// under it while the step runs and go when it ends. Receipts, when
	// set, takes the proxy's log as source-proxy-<id>.log.
	Work, Receipts string
	Stderr         io.Writer
	// Later registers a cleanup with the caller's teardown, for a step the
	// process dies in the middle of; Network registers a network the step
	// made, for a sweep once every container is gone. Either may be nil:
	// the step removes its own containers and network as it ends, and a
	// sweep by the run's label removes what a killed process left.
	Later   func(func(context.Context))
	Network func(string)
}

// sourceTimeout bounds one source's fetch step.
const sourceTimeout = 20 * time.Minute

// maxSourceReport bounds the fetch step's report.
const maxSourceReport = 1 << 20

// sourceProxyAlias is the source proxy's name on its network.
const sourceProxyAlias = "source-proxy"

// Fetch fetches one source within caps into dest, which must not exist: the
// tree the step wrote is held to the source's rules (regular files and
// folders only, no .git, within the caps, the counts the report gives) and
// moves to dest whole, or nothing is staged. The network and the proxy are
// the step's own, made for it and removed before Fetch returns.
func (f SourceFetcher) Fetch(ctx context.Context, s sources.Source, caps sources.Caps, dest string) (sources.Fetched, error) {
	kind, ok := sources.Lookup(s.Kind)
	if !ok {
		return sources.Fetched{}, Classify(ExitCLI, sources.Unfetched(s.ID, s.Kind))
	}
	d := f.Docker
	base := filepath.Join(f.Work, s.ID)
	tree, work := filepath.Join(base, "tree"), filepath.Join(base, "work")
	for _, dir := range []string{tree, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return sources.Fetched{}, Classify(ExitCLI, fmt.Errorf("make the input source's folders: %w", err))
		}
	}
	defer func() { _ = os.RemoveAll(base) }()

	labels := runLabels(f.RunID)
	netName := sourceNetworkName(f.RunID, s.ID)
	proxyName := containerName(f.RunID, "source-proxy-"+s.ID)
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("create the input source's network: %w", err))
	}
	// The teardown's sweep removes the network again after every container
	// carrying the run's label is gone, should a step killed mid-way have
	// held it here.
	if f.Network != nil {
		f.Network(netName)
	}
	name := containerName(f.RunID, "source-"+s.ID)
	removed := false
	cleanup := func(c context.Context) {
		if removed {
			return
		}
		// The step first: a docker run killed on the timeout leaves its
		// container holding the network.
		_ = d.RemoveForce(c, name)
		_ = d.RemoveForce(c, proxyName)
		removed = d.NetworkRemove(c, netName) == nil
	}
	if f.Later != nil {
		f.Later(cleanup)
	}
	defer cleanup(context.WithoutCancel(ctx))
	proxySpec := dockerx.RunSpec{
		Name:    proxyName,
		Image:   f.ProxyImage,
		Argv:    []string{"proxy", "--allow", s.Host + ":443", "--public-only", "--addr", fmt.Sprintf(":%d", spec.ProxyPort)},
		Network: netName,
		Alias:   sourceProxyAlias,
		Labels:  labels,
	}
	proxySpec = readOnlyRoot(hardened(proxySpec, f.Limits), "/tmp")
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("start the input source's proxy: %w", err))
	}
	// Its route out, as the llm proxy's: the daemon's default bridge.
	if err := d.NetworkConnect(ctx, "bridge", proxyName); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("connect the input source's proxy to the bridge: %w", err))
	}

	fetchSpec := dockerx.RunSpec{
		Name:    name,
		Image:   f.FilesImage,
		Argv:    kind.Argv(s, caps, fmt.Sprintf("http://%s:%d", sourceProxyAlias, spec.ProxyPort)),
		Network: netName,
		Mounts: []dockerx.Mount{
			{Source: tree, Target: "/out", Relabel: true},
			{Source: work, Target: "/work", Relabel: true},
		},
		// The files belong to the person who ran the command, and the step
		// runs with no more than they have: their ids on this engine.
		User:        f.User,
		Interactive: s.Token != "",
		AutoRemove:  true,
		Labels:      labels,
	}
	fetchSpec = readOnlyRoot(hardened(fetchSpec, f.Limits), "/tmp:size=64m,mode=1777")
	var stdin io.Reader
	if s.Token != "" {
		stdin = strings.NewReader(s.Token + "\n")
	}
	fctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	start := time.Now()
	var errOut bytes.Buffer
	code, err := d.RunForeground(fctx, fetchSpec, stdin, io.Discard, &limitedBuffer{b: &errOut, n: 64 << 10})
	took := time.Since(start)
	// The proxy's decisions are the evidence of what the step reached.
	if logs, lerr := d.Logs(context.WithoutCancel(ctx), proxyName, 200); lerr == nil && logs != "" && f.Receipts != "" {
		_ = os.WriteFile(filepath.Join(f.Receipts, "source-proxy-"+s.ID+".log"), []byte(scrubToken(logs, s.Token)), 0o644) //nolint:gosec // receipt
	}
	cleanup(context.WithoutCancel(ctx))
	if err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: the fetch step: %s", s.ID, scrubToken(err.Error(), s.Token)))
	}
	raw, rerr := readSourceReport(work)
	if rerr != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: %w (exit %d: %s)", s.ID, rerr, code, scrubToken(strings.TrimSpace(tailString(errOut.String(), 300)), s.Token)))
	}
	rep, err := sources.CheckReport(raw, s, caps)
	if err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: %w", s.ID, err))
	}
	switch {
	case rep.Refusal != "":
		return sources.Fetched{}, Classify(ExitPolicy, receipt.WithCount(fmt.Errorf("the run was refused before the agent started: the input source %s: %s", s.ID, rep.Refusal),
			receipt.ReasonInputRefused, receipt.InputSourceCap, overCount(rep.Over, caps), s.ID))
	case code != 0 || rep.Error != "":
		why := rep.Error
		if why == "" {
			why = fmt.Sprintf("the fetch step failed (exit %d)", code)
		}
		return sources.Fetched{}, Classify(ExitCLI, receipt.WithReason(fmt.Errorf("the input source %s (%s) could not be fetched: %s", s.ID, s.Shown(), why),
			receipt.ReasonInputRefused, receipt.InputSourceFetch, s.ID))
	}
	if err := checkSourceTree(tree, rep, caps); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: %w", s.ID, err))
	}
	if err := checkSourceFile(tree, rep); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: %w", s.ID, err))
	}
	if err := os.Rename(tree, dest); err != nil {
		return sources.Fetched{}, Classify(ExitCLI, fmt.Errorf("stage the input source %s: %w", s.ID, err))
	}
	return sources.Fetched{Source: rep, Took: took}, nil
}

// overCount is a cap refusal's count in the run's reason: the report's
// words and count, against the value of the cap this run fetched with,
// never one the report gives; nil when the report gave no count.
func overCount(o *source.Over, caps sources.Caps) *receipt.ReasonCount {
	if o == nil {
		return nil
	}
	var limit int64
	switch o.Cap {
	case source.CapFiles:
		limit = int64(caps.Files)
	case source.CapBytes:
		limit = caps.Bytes
	default:
		return nil
	}
	// The report's words are the receipt's (TestTheFetchStepCountsInTheReceiptsWords).
	return &receipt.ReasonCount{Cap: o.Cap, Limit: limit, Of: o.Of, Count: o.Count, More: o.More}
}

// sourceNetworkName is one source's network.
func sourceNetworkName(runID, id string) string { return "agentry-" + runID + "-src-" + id }

// readSourceReport reads the step's report from its work folder, a regular
// file within the bound.
func readSourceReport(work string) ([]byte, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(source.ReportFile)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSourceReport {
		return nil, errors.New("the fetch step wrote no readable report")
	}
	f, err := root.Open(source.ReportFile)
	if err != nil {
		return nil, errors.New("the fetch step wrote no readable report")
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxSourceReport))
}

// checkSourceTree holds the tree the step wrote to the source's rules: only
// regular files and folders, nothing inside a .git folder, no more than the
// caps, and exactly the files and bytes the report gives. A tree that
// breaks any of them was not written by the step this CLI ships, and is
// not staged.
func checkSourceTree(tree string, rep source.Report, caps sources.Caps) error {
	files, bytesSeen := 0, int64(0)
	err := filepath.WalkDir(tree, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == tree {
			return nil
		}
		rel, err := filepath.Rel(tree, p)
		if err != nil {
			return err
		}
		for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
			if runnerdecode.InRepository(seg+"/x") || strings.EqualFold(strings.TrimRight(seg, ". "), ".git") {
				return fmt.Errorf("the fetch step staged %s, inside a .git folder", filepath.ToSlash(rel))
			}
		}
		switch {
		case e.IsDir():
			return nil
		case !e.Type().IsRegular():
			return fmt.Errorf("the fetch step staged %s, which is not a regular file", filepath.ToSlash(rel))
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		files++
		bytesSeen += fi.Size()
		if files > caps.Files || bytesSeen > caps.Bytes {
			return errors.New("the fetch step staged more than the source's caps")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if files != rep.Files || bytesSeen != rep.Bytes {
		return fmt.Errorf("the fetch step staged %d files and %d bytes, but reported %d and %d", files, bytesSeen, rep.Files, rep.Bytes)
	}
	return nil
}

// checkSourceFile holds a one-file source (a url source) to its report:
// the one file the tree holds is the one the report names, at its top.
func checkSourceFile(tree string, rep source.Report) error {
	if rep.Name == "" {
		return nil
	}
	entries, err := os.ReadDir(tree)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != rep.Name || !entries[0].Type().IsRegular() {
		return errors.New("the fetch step staged something other than the one file it reported")
	}
	return nil
}

// scrubToken removes a token from text that may echo it.
func scrubToken(s, token string) string {
	for _, f := range source.TokenForms(token) {
		s = strings.ReplaceAll(s, f, "[token]")
	}
	return s
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// limitedBuffer keeps the first n bytes written to it.
type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
