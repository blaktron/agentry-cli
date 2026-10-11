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
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/source"
)

// The input sources' fetch step (plan input-sources.md §5, D4–D8;
// agentry-cli#833), before the decode. Each source the intake answered is
// fetched by the runner image's `source` role in the files container (the
// decode image), on a network of its own whose only route out is a CONNECT
// proxy to the source's host on port 443, at public addresses alone. The
// token, when there is one, reaches the step on its stdin: never in an
// environment, an argument or a file. Then the proxy and the network are
// removed, the tree the step wrote is held to the source's rules on the
// host (regular files and folders only, no .git, within the caps, the
// counts the report gives), and it moves to /inputs/<id>/, where the decode
// treats it as it treats uploads, with the source's own cap and malware
// rule. The agent container never sees a token, a source mount of its own
// or a route to the source's host.

// sourceTimeout bounds one source's fetch step.
const sourceTimeout = 20 * time.Minute

// maxSourceReport bounds the fetch step's report.
const maxSourceReport = 1 << 20

// sourceProxyAlias is the source proxy's name on its network.
const sourceProxyAlias = "source-proxy"

// sourceRun is one source fetched: the source, what its step reported, and
// how long it took.
type sourceRun struct {
	src     sources.Source
	fetched sources.Fetched
}

// fetchSources fetches every input source the run was given into
// /inputs/<id>/, and rewrites intake.json with what each resolved to. A
// run with no source does nothing here.
func fetchSources(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) ([]sourceRun, error) {
	srcs := p.Intake.SourceList()
	if len(srcs) == 0 {
		return nil, nil
	}
	// A hosted run's sources arrive with their own caps, job credentials
	// and AI Review of a source's Markdown (agentry-enterprise#516).
	if p.InputReview != nil {
		return nil, Classify(ExitPolicy, errors.New("this run was given an input source, which a hosted run does not take yet (agentry-enterprise#516)"))
	}
	if p.Policy == nil {
		return nil, Classify(ExitCLI, errors.New("the input sources cannot be fetched: no policy names the files image"))
	}
	img, err := ensureDecode(ctx, d, p.Plan.Proxy.Image, p.Policy, filepath.Join(rd.root, "image"), p.Stderr)
	if err != nil {
		return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the input sources could not be fetched: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeImage))
	}
	// Each source's staging folder goes as its fetch ends; their parent
	// with the last of them.
	defer func() { _ = os.Remove(filepath.Join(rd.root, "sources")) }()
	var runs []sourceRun
	for _, s := range srcs {
		f, err := fetchSource(ctx, d, p, rd, td, runID, img.Tag, *s)
		if err != nil {
			return nil, err
		}
		runs = append(runs, sourceRun{src: *s, fetched: f})
	}
	if p.Intake.Fetched == nil {
		p.Intake.Fetched = map[string]sources.Fetched{}
	}
	for _, r := range runs {
		p.Intake.Fetched[r.src.ID] = r.fetched
	}
	if err := writeIntake(p, rd); err != nil {
		return nil, err
	}
	return runs, nil
}

// fetchSource fetches one source, its network and proxy made for it and
// removed before it returns.
func fetchSource(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID, image string, s sources.Source) (sources.Fetched, error) {
	kind, ok := sources.Lookup(s.Kind)
	if !ok {
		return sources.Fetched{}, Classify(ExitCLI, sources.Unfetched(s.ID, s.Kind))
	}
	dest := filepath.Join(rd.inputs, s.ID)
	if _, err := os.Lstat(dest); err == nil {
		return sources.Fetched{}, Classify(ExitCLI, inputRefused(receipt.InputNameClash, fmt.Errorf("the input source %s collides with an input of the same name under %s", s.ID, spec.InputsMount)))
	}
	base := filepath.Join(rd.root, "sources", s.ID)
	tree, work := filepath.Join(base, "tree"), filepath.Join(base, "work")
	for _, dir := range []string{tree, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return sources.Fetched{}, Classify(ExitCLI, fmt.Errorf("make the input source's folders: %w", err))
		}
	}
	defer func() { _ = os.RemoveAll(base) }()
	caps := sources.LocalCaps

	// The network and the proxy are the step's own, removed as soon as it
	// ends (and by the teardown if the run dies first).
	labels := runLabels(runID)
	netName := sourceNetworkName(runID, s.ID)
	proxyName := containerName(runID, "source-proxy-"+s.ID)
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("create the input source's network: %w", err))
	}
	removed := false
	cleanup := func(c context.Context) {
		if removed {
			return
		}
		removed = true
		_ = d.RemoveForce(c, proxyName)
		_ = d.NetworkRemove(c, netName)
	}
	td.add(cleanup)
	defer cleanup(context.WithoutCancel(ctx))
	proxySpec := dockerx.RunSpec{
		Name:    proxyName,
		Image:   p.Plan.Proxy.Image,
		Argv:    []string{"proxy", "--allow", s.Host + ":443", "--public-only", "--addr", fmt.Sprintf(":%d", spec.ProxyPort)},
		Network: netName,
		Alias:   sourceProxyAlias,
		Labels:  labels,
	}
	proxySpec = readOnlyRoot(hardened(proxySpec, p.limits()), "/tmp")
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("start the input source's proxy: %w", err))
	}
	// Its route out, as the llm proxy's: the daemon's default bridge.
	if err := d.NetworkConnect(ctx, "bridge", proxyName); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("connect the input source's proxy to the bridge: %w", err))
	}

	name := containerName(runID, "source-"+s.ID)
	fetchSpec := dockerx.RunSpec{
		Name:    name,
		Image:   image,
		Argv:    kind.Argv(s, caps, fmt.Sprintf("http://%s:%d", sourceProxyAlias, spec.ProxyPort)),
		Network: netName,
		Mounts: []dockerx.Mount{
			{Source: tree, Target: "/out", Relabel: true},
			{Source: work, Target: "/work", Relabel: true},
		},
		// The files belong to the person who ran the command, and the step
		// runs with no more than they have.
		User:        sourceUser(),
		Interactive: s.Token != "",
		AutoRemove:  true,
		Labels:      labels,
	}
	fetchSpec = readOnlyRoot(hardened(fetchSpec, p.limits()), "/tmp:size=64m,mode=1777")
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
	if logs, lerr := d.Logs(context.WithoutCancel(ctx), proxyName, 200); lerr == nil && logs != "" {
		_ = os.WriteFile(filepath.Join(rd.receiptsDir, "source-proxy-"+s.ID+".log"), []byte(scrubToken(logs, s.Token)), 0o644) //nolint:gosec // receipt
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
		return sources.Fetched{}, Classify(ExitPolicy, inputRefused(receipt.InputSourceCap, fmt.Errorf("the run was refused before the agent started: the input source %s: %s", s.ID, rep.Refusal)))
	case code != 0 || rep.Error != "":
		why := rep.Error
		if why == "" {
			why = fmt.Sprintf("the fetch step failed (exit %d)", code)
		}
		return sources.Fetched{}, Classify(ExitCLI, inputRefused(receipt.InputSourceFetch, fmt.Errorf("the input source %s (%s) could not be fetched: %s", s.ID, s.URL, why)))
	}
	if err := checkSourceTree(tree, rep, caps); err != nil {
		return sources.Fetched{}, Classify(ExitGateway, fmt.Errorf("the input source %s: %w", s.ID, err))
	}
	if err := os.Rename(tree, dest); err != nil {
		return sources.Fetched{}, Classify(ExitCLI, fmt.Errorf("stage the input source %s: %w", s.ID, err))
	}
	relabelForToolContainers(context.WithoutCancel(ctx), p.Stderr, relabelTarget{dir: dest, recursive: true})
	return sources.Fetched{Source: rep, Took: took}, nil
}

// sourceNetworkName is one source's network.
func sourceNetworkName(runID, id string) string { return "agentry-" + runID + "-src-" + id }

// sourceUser runs the fetch step as the invoking user, so the files are
// theirs to remove; "" where the platform has no uid (Windows), which
// leaves the image's own.
func sourceUser() string {
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

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

// sourceLines puts each source's receipt line into enforced, under
// source.<id>, with the malware check of its files from the decode.
func sourceLines(enforced map[string]string, runs []sourceRun, dec *inputsDecoder) {
	var files []runnerdecode.File
	if dec != nil {
		dec.mu.Lock()
		files = dec.before.Report.Files
		dec.mu.Unlock()
	}
	for _, r := range runs {
		enforced["source."+r.src.ID] = sources.Line(r.src, r.fetched, sources.Checks(r.src.ID, files))
	}
}

// sourceIDs are the folders under /inputs that are input sources.
func sourceIDs(runs []sourceRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.src.ID)
	}
	return out
}

// scrubToken removes a token from text that may echo it.
func scrubToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[token]")
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

// writeIntake rewrites intake.json, in /inputs and the receipts, once the
// sources resolved.
func writeIntake(p Params, rd *runDirs) error {
	return stageIntakeDoc(p.Intake, rd)
}
