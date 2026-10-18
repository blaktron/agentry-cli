package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// inputsDecoder is the decode step on a run's own inputs: every run with
// inputs, from any client, has them decoded before the agent starts, and a
// session's file added mid-run before it is announced. The originals stay
// at /inputs; the host writes the index and the decoded texts to the run's
// inputs-text directory (toolsrv's layout), copying each text only through
// decode.Result's checked, capped reads, so nothing the decode container
// wrote reaches the tools container unchecked. The words the index shows
// the model come from the decode's fixed vocabulary, never from the
// report's free text (decode.UnreadReason). A run with no inputs pulls
// nothing and has no decode line. Every input is staged with no execute
// bit. The malware check reads what checkScope says before any decoder
// does: every upload and a source's risky files on a sandbox host, or
// where the policy turns the check on; the executables alone on this
// computer otherwise, and only for a QuickAgent that declares commands. A
// finding refuses a run that can execute (canExecute) before the agent
// starts, with the policy class, so a hosted run settles nothing, and
// refuses a session's added file alone. A run that runs no commands is
// given the flagged file anyway: never decoded, readable only as
// uploaded, named as flagged in the index and the receipt. On a hosted run, AI Review then reads every
// text the model could read of them (review.go), and refuses the run, or
// the added file, on one it flags.
type inputsDecoder struct {
	d  dockerx.Client
	p  Params
	rd *runDirs

	mu  sync.Mutex
	img *decode.Image
	// lookup asks Hybrid Analysis about the risky inputs by hash, with the
	// policy's key or none.
	lookup *hybrid.Client
	// scope is how much the malware check reads (checkScope).
	scope checkScope
	// review is AI Review of the inputs' text, nil on a local run.
	review *inputReview
	// seq numbers the decode directories under the run root.
	seq int
	// before is what was decoded before the agent started, added what a
	// session added after: the receipt says when each was decoded.
	before, added decode.Result
	// refused is each refusal of a session's added file, for the receipt.
	refused []string
	index   toolsrv.InputsIndex
	// sourceReviewed is how many Markdown files of each source AI Review
	// read, for its receipt line.
	sourceReviewed map[string]int
}

// ensureDecode, ensureSignatures and runDecode are the decode step's
// calls, variables so the engine's tests can decode without a daemon or a
// registry.
var (
	ensureDecode     = decode.Ensure
	ensureSignatures = clamdb.Ensure
	runDecode        = decode.RunWith
	hybridClient     = hybrid.FromPolicy
)

// checkScope is how much of a run's inputs the malware check reads.
type checkScope int

const (
	// checkAll checks every upload and an input source's executable and
	// binary risky files, as a sandbox host does.
	checkAll checkScope = iota
	// checkExecutables checks the executables alone.
	checkExecutables
	// checkOff checks nothing: no signatures are fetched, nothing is
	// looked up, and the receipt says the check was off.
	checkOff
)

// checkOffProblem is the receipt's reason no file was checked.
const checkOffProblem = "the malware check is off on this computer (the runner policy's malwareCheck), and the QuickAgent runs no commands"

// canExecute says the QuickAgent declares commands, with which its run
// could run an input.
func (p Params) canExecute() bool {
	return p.Plan != nil && (len(p.Plan.Capabilities.ExecCommands) > 0 || p.Plan.ExecImage != nil)
}

// checkScope is the malware check this run takes: all of it on a sandbox
// host, whatever the policy says, and where the policy turns it on; on
// this computer otherwise the executables of a run that can execute, and
// nothing for one that runs no commands.
func (p Params) checkScope() checkScope {
	switch {
	case p.Hosted || p.Policy.MalwareCheckOn():
		return checkAll
	case p.canExecute():
		return checkExecutables
	}
	return checkOff
}

// clearExecBits takes the execute bits off every regular file staged under
// dir, so an input nobody meant to run cannot be run as it is.
func clearExecBits(dir string) error {
	return filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		if m := fi.Mode().Perm(); m&0o111 != 0 {
			return os.Chmod(p, m&^0o111)
		}
		return nil
	})
}

// decodeInputs decodes what the run staged under /inputs. It answers nil
// when the caller decoded the inputs itself or staged only its own files
// (Params.Decoded, Params.TextInputs: every built-in), and otherwise a
// decoder, which has decoded nothing when nothing was given, so a
// session's first added file is decoded on its own.
func decodeInputs(ctx context.Context, d dockerx.Client, p Params, rd *runDirs) (*inputsDecoder, error) {
	if err := clearExecBits(rd.inputs); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("stage the inputs with no execute bit: %w", err))
	}
	if p.Decoded != nil || p.TextInputs {
		return nil, nil
	}
	x := &inputsDecoder{d: d, p: p, rd: rd, scope: p.checkScope(), review: newInputReview(p.InputReview), index: toolsrv.InputsIndex{Schema: toolsrv.InputsIndexSchema, Files: []toolsrv.InputEntry{}}}
	if p.Plan != nil {
		// The release's own default view, which the tools serve an
		// input in when the model asks for none.
		x.index.DefaultView = p.Plan.InputView
	}
	given, err := hasGivenInputs(rd.inputs)
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !given {
		// The intake answers alone are the engine's own text: indexed as
		// read, never decoded, so a session that adds a file later still
		// lists them.
		return x, x.indexIntake()
	}
	res, err := x.decode(ctx, rd.inputs, rd.sources)
	if err != nil {
		return nil, err
	}
	if res.Refused() && x.p.canExecute() {
		return nil, Classify(ExitPolicy, receipt.WithRefusedInputs(inputRefused(receipt.InputMalware, fmt.Errorf("the run was refused before the agent started: %s", res.Refusal())), malwareRefused(res)))
	}
	entries, err := x.stageTexts(res)
	if err != nil {
		return nil, err
	}
	entries, refusal, err := x.reviewEntries(ctx, res, entries, false)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.refusedRun()
	}
	x.before = merged(x.before, res)
	return x, x.commit(entries)
}

// reviewEntries has AI Review read the texts of a decode's files, once
// they are staged and before the index names them, and answers the
// entries as the index is to name them: a file withheld past what AI
// Review reads of a run is listed as not read, with no view, and its
// staged texts are taken back. The caller holds x.mu.
func (x *inputsDecoder) reviewEntries(ctx context.Context, res *decode.Result, entries []toolsrv.InputEntry, added bool) ([]toolsrv.InputEntry, *reviewRefusal, error) {
	if x.review == nil {
		return entries, nil, nil
	}
	texts := reviewTexts(entries, res.In, x.textRoot(), x.convertedRoot())
	texts, markdown := sourceReviewTexts(texts, x.rd.sources)
	withheld, refusal, err := x.review.review(ctx, texts, added)
	if err != nil {
		return nil, nil, Classify(ExitCLI, err)
	}
	if refusal == nil {
		if err := sourceNotReviewed(withheld, x.rd.sources, markdown); err != nil {
			return nil, nil, Classify(ExitPolicy, err)
		}
	}
	for id, n := range markdown {
		if x.sourceReviewed == nil {
			x.sourceReviewed = map[string]int{}
		}
		x.sourceReviewed[id] = n
	}
	for i, e := range entries {
		if !slices.Contains(withheld, e.Path) {
			continue
		}
		x.dropTexts(e.Path)
		entries[i] = toolsrv.InputEntry{Path: e.Path, Type: e.Type, InArchive: e.InArchive, MIME: e.MIME, Bytes: e.Bytes, Unread: notReviewed}
	}
	return entries, refusal, nil
}

// textRoot and convertedRoot are where the run's decoded texts and
// Markdown views are staged (toolsrv's layout).
func (x *inputsDecoder) textRoot() string {
	return filepath.Join(x.rd.inputsText, toolsrv.InputsTextSubdir)
}

func (x *inputsDecoder) convertedRoot() string {
	return filepath.Join(x.rd.inputsText, toolsrv.InputsConvertedSubdir)
}

// dropTexts takes back what was staged of one path: its text, its
// Markdown view, and, for an archive, its members'.
func (x *inputsDecoder) dropTexts(path string) {
	for _, root := range []string{x.textRoot(), x.convertedRoot()} {
		_ = os.RemoveAll(filepath.Join(root, filepath.FromSlash(path)))
	}
}

// inputRefused names why an input was refused, in the run's reason; never
// which input, since its name is the person's. A source's refusals name
// the source by its question id, the release's own word, and carry their
// count (receipt.WithCount).
func inputRefused(why string, err error) error {
	return receipt.WithReason(err, receipt.ReasonInputRefused, why)
}

// malwareRefused names, in typed fields, each file a decode's checks
// refused: ClamAV's findings with their signatures, in the report's order,
// then each file Hybrid Analysis called malicious with its threat score.
func malwareRefused(res *decode.Result) []receipt.RefusedInput {
	var out []receipt.RefusedInput
	for _, f := range res.Found() {
		out = append(out, receipt.RefusedInput{Path: f.Path, By: receipt.RefusedByClamAV, Signature: f.Signature})
	}
	for _, f := range res.Malicious() {
		l := res.Lookups[f.Path]
		out = append(out, receipt.RefusedInput{Path: f.Path, By: receipt.RefusedByHybridAnalysis, Verdict: l.VerdictWord(), ThreatScore: l.ThreatScore})
	}
	return out
}

// hasGivenInputs says whether the person gave the run anything: the
// intake answers the engine writes there are not a reason to decode.
func hasGivenInputs(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, Classify(ExitCLI, fmt.Errorf("read the staged inputs: %w", err))
	}
	for _, e := range entries {
		if e.Name() != intakeFile {
			return true, nil
		}
	}
	return false, nil
}

// indexIntake indexes the intake answers, when the run has them, as text.
// The caller holds x.mu.
func (x *inputsDecoder) indexIntake() error {
	fi, err := os.Lstat(filepath.Join(x.rd.inputs, intakeFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("index the intake answers: %w", err))
	}
	return x.commit([]toolsrv.InputEntry{{Path: intakeFile, Type: string(runnerdecode.FamilyText), Bytes: fi.Size(), TextBytes: fi.Size()}})
}

// decode runs the decode container over in, a directory this run made,
// into a fresh output directory under the run root. The decode image and
// the signatures are ensured on first use: the files image is pulled, and
// the signature bundle refreshed, only for a run that has something to
// decode. The caller holds x.mu.
func (x *inputsDecoder) decode(ctx context.Context, in string, sources []string) (*decode.Result, error) {
	if x.p.Policy == nil {
		return nil, Classify(ExitCLI, errors.New("the inputs cannot be decoded: no policy names the files image"))
	}
	if x.img == nil {
		img, err := ensureDecode(ctx, x.d, x.p.Plan.Proxy.Image, x.p.Policy, filepath.Join(x.rd.root, "image"), x.p.Stderr)
		if err != nil {
			return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the inputs could not be decoded: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeImage))
		}
		if x.scope == checkOff {
			// Nothing is checked, so nothing is fetched or looked up.
			img.Signatures = clamdb.DB{Problem: checkOffProblem}
			x.img = &img
			return x.run(ctx, in, sources)
		}
		// With no home to keep it in there is no bundle, and the receipt
		// says no file was checked (clamdb.Ensure answers ProblemNone).
		home := x.p.RunnerHome
		if home == "" {
			home, _ = policy.Home()
		}
		img.Signatures = ensureSignatures(ctx, home, x.p.Policy.Images.ClamAVSignaturesRef(), x.p.Stderr)
		// The Hybrid Analysis key, the person's or a sandbox host's: none
		// asks nothing, and the receipt says so; a keychain the policy
		// names that cannot be read stops the run, as for judge.
		lookup, err := hybridClient(x.p.Policy)
		if err != nil {
			return nil, Classify(ExitPolicy, fmt.Errorf("the Hybrid Analysis key: %w", err))
		}
		x.img, x.lookup = &img, lookup
	}
	return x.run(ctx, in, sources)
}

// run is one decode with the image in hand, and the Hybrid Analysis
// lookup unless the check is off.
func (x *inputsDecoder) run(ctx context.Context, in string, sources []string) (*decode.Result, error) {
	out := filepath.Join(x.rd.root, "decode", strconv.Itoa(x.seq))
	x.seq++
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("make the decode's output directory: %w", err))
	}
	opt := decode.Options{Sources: sources, ExecutablesOnly: x.scope == checkExecutables}
	res, err := runDecode(ctx, x.d, *x.img, in, out, x.p.limits(), opt, x.p.Stderr)
	if err != nil {
		return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the inputs could not be decoded: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeRun))
	}
	if x.scope != checkOff {
		res.LookUp(ctx, x.lookup)
	}
	return res, nil
}

// stageTexts copies each decoded text into inputs-text/text/<path>, and
// each Markdown view of HTML into inputs-text/converted/<path>, and
// answers every file's index entry, for commit to publish. The caller
// holds x.mu.
func (x *inputsDecoder) stageTexts(res *decode.Result) ([]toolsrv.InputEntry, error) {
	textRoot, convertedRoot := x.textRoot(), x.convertedRoot()
	entries := make([]toolsrv.InputEntry, 0, len(res.Report.Files))
	flags := res.Flags()
	for _, f := range res.Report.Files {
		e := toolsrv.InputEntry{Path: f.Path, Type: string(f.Family), InArchive: f.In, MIME: f.MIME}
		if f.In != "" {
			// A member is not staged: its size is the one it unpacked to.
			e.Bytes = f.Size
		} else if fi, err := os.Lstat(filepath.Join(res.In, filepath.FromSlash(f.Path))); err == nil {
			// res.In is a directory this run staged, never the decode's
			// output, so its sizes are the inputs' own.
			e.Bytes = fi.Size()
		}
		if flag := flags[f.Path]; flag != "" {
			// Handed over by a run that runs no commands: no text of it is
			// staged, and only the file as uploaded, with no execute bit,
			// can be read; a member stayed in its archive.
			e.Unread = "flagged: " + flag + "; it was not decoded"
			if f.In == "" {
				e.Unread += ", and it can be read only as uploaded (it cannot be executed)"
				e.Original = true
			} else {
				e.Unread += ", and it stayed in " + f.In
			}
			entries = append(entries, e)
			continue
		}
		switch {
		case f.HasText():
			// A member's text is served from inputs-text whether it was
			// decoded or read as it is: the member is not under /inputs.
			dst := filepath.Join(textRoot, filepath.FromSlash(f.Path))
			if err := res.CopyText(f.Path, dst); err != nil {
				return nil, Classify(ExitGateway, fmt.Errorf("the text decoded from %s: %w", f.Path, err))
			}
			fi, err := os.Stat(dst)
			if err != nil {
				return nil, Classify(ExitCLI, fmt.Errorf("the text decoded from %s: %w", f.Path, err))
			}
			e.TextBytes = fi.Size()
			if f.Outcome == runnerdecode.OutcomeDecoded {
				e.Decoded, e.DecodedBy = true, f.Tool
			}
		case f.Outcome == runnerdecode.OutcomeText:
			e.TextBytes = e.Bytes
		case f.Outcome == runnerdecode.OutcomeUnpacked:
			e.Unpacked, e.Partial = true, decode.UnpackedInPart(f)
		default:
			e.Unread = decode.UnreadReason(f)
			// A file as staged that no reader took is still there to read
			// as uploaded; one ClamAV found, one refused and one inside
			// a .git folder are not.
			e.Original = f.In == "" && f.Outcome == runnerdecode.OutcomeUndecodable &&
				f.Why != runnerdecode.WhyFound && f.Why != runnerdecode.WhyRepository
		}
		if f.Markdown != "" && e.Unread == "" {
			dst := filepath.Join(convertedRoot, filepath.FromSlash(f.Path))
			if err := res.CopyMarkdown(f.Path, dst); err != nil {
				return nil, Classify(ExitGateway, fmt.Errorf("the Markdown converted from %s: %w", f.Path, err))
			}
			fi, err := os.Stat(dst)
			if err != nil {
				return nil, Classify(ExitCLI, fmt.Errorf("the Markdown converted from %s: %w", f.Path, err))
			}
			e.Converted, e.ConvertedBy, e.ConvertedBytes = true, f.MarkdownBy, fi.Size()
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// commit adds entries to the index and writes it in one rename, so the
// tools container never reads half of one. The caller holds x.mu.
func (x *inputsDecoder) commit(entries []toolsrv.InputEntry) error {
	x.index.Files = append(x.index.Files, entries...)
	sort.Slice(x.index.Files, func(i, j int) bool { return x.index.Files[i].Path < x.index.Files[j].Path })
	b, err := json.MarshalIndent(x.index, "", "  ")
	if err != nil {
		return Classify(ExitCLI, fmt.Errorf("encode the inputs index: %w", err))
	}
	tmp := filepath.Join(x.rd.inputsText, "."+toolsrv.InputsIndexFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil { //nolint:gosec // read by the tools container
		return Classify(ExitCLI, fmt.Errorf("write the inputs index: %w", err))
	}
	if err := os.Rename(tmp, filepath.Join(x.rd.inputsText, toolsrv.InputsIndexFile)); err != nil {
		return Classify(ExitCLI, fmt.Errorf("write the inputs index: %w", err))
	}
	return nil
}

// add decodes one file a session adds mid-run, from a directory of its
// own, stages its text, then the original under /inputs, then the index,
// so the caller announces it only once all three are in place. It
// answers the file's index entry.
func (x *inputsDecoder) add(ctx context.Context, base string, data []byte) (toolsrv.InputEntry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	// Two adds of one name could both pass the caller's check before
	// either is staged; the second would write over the first's texts.
	for _, e := range x.index.Files {
		if e.Path == base || strings.HasPrefix(e.Path, base+"/") {
			return toolsrv.InputEntry{}, fmt.Errorf("an input named %q is already staged; rename the file", base)
		}
	}
	in := filepath.Join(x.rd.root, "decode", "add-"+strconv.Itoa(x.seq))
	if err := os.MkdirAll(in, 0o755); err != nil {
		return toolsrv.InputEntry{}, fmt.Errorf("stage %s for the decode: %w", base, err)
	}
	if err := os.WriteFile(filepath.Join(in, base), data, 0o644); err != nil { //nolint:gosec // read by the decode container
		return toolsrv.InputEntry{}, fmt.Errorf("stage %s for the decode: %w", base, err)
	}
	res, err := x.decode(ctx, in, nil)
	if err != nil {
		return toolsrv.InputEntry{}, err
	}
	// A finding refuses this file alone: nothing of it is staged, the
	// agent is not told of it, and the session goes on.
	if res.Refused() && x.p.canExecute() {
		x.refused = append(x.refused, res.Refusal())
		return toolsrv.InputEntry{}, fmt.Errorf("refused: %s; it was not added, and the session goes on without it", res.Refusal())
	}
	entries, err := x.stageTexts(res)
	if err != nil {
		return toolsrv.InputEntry{}, err
	}
	// AI Review's flag refuses this file alone, as a finding does: its
	// staged texts are taken back, and nothing names it to the agent.
	entries, refusal, err := x.reviewEntries(ctx, res, entries, true)
	if err != nil {
		return toolsrv.InputEntry{}, err
	}
	if refusal != nil {
		x.dropTexts(base)
		x.refused = append(x.refused, refusal.why)
		return toolsrv.InputEntry{}, fmt.Errorf("refused: %s; it was not added, and the session goes on without it", refusal.why)
	}
	// An archive brings its members, each under its path.
	for _, e := range entries {
		if e.Path != base && !strings.HasPrefix(e.Path, base+"/") {
			return toolsrv.InputEntry{}, fmt.Errorf("the decode reported %s, which is not %s", e.Path, base)
		}
	}
	if len(entries) == 0 || entries[0].Path != base {
		return toolsrv.InputEntry{}, fmt.Errorf("the decode did not report %s", base)
	}
	if err := os.WriteFile(filepath.Join(x.rd.inputs, base), data, 0o644); err != nil { //nolint:gosec // inputs are agent-readable by design
		return toolsrv.InputEntry{}, fmt.Errorf("stage input %s: %w", base, err)
	}
	if err := x.commit(entries); err != nil {
		return toolsrv.InputEntry{}, err
	}
	x.added = merged(x.added, res)
	return entries[0], nil
}

// merged adds one decode's files to an account of several.
func merged(into decode.Result, res *decode.Result) decode.Result {
	into.Image = res.Image
	into.ExecutablesOnly = res.ExecutablesOnly
	into.Sources = append(into.Sources, res.Sources...)
	into.Report.Files = append(into.Report.Files, res.Report.Files...)
	if res.Lookups != nil {
		if into.Lookups == nil {
			into.Lookups = map[string]hybrid.Result{}
		}
		for p, l := range res.Lookups {
			into.Lookups[p] = l
		}
	}
	return into
}

// lines puts the receipt's decode and inputReview lines into enforced,
// when the run has them.
func (x *inputsDecoder) lines(enforced map[string]string) {
	if line := x.line(); line != "" {
		enforced["decode"] = line
	}
	if x == nil {
		return
	}
	if line := x.review.line(); line != "" {
		enforced["inputReview"] = line
	}
}

// line is the receipt's decode line for every decode this run made, empty
// when it made none: what was decoded before the run, and what a session
// added and had decoded before the agent was told of it.
func (x *inputsDecoder) line() string {
	if x == nil {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	const addedWhen = "the files added during the session were each decoded before the agent was told of them"
	var line string
	switch {
	case len(x.before.Report.Files) == 0 && len(x.added.Report.Files) == 0:
	case len(x.added.Report.Files) == 0:
		line = x.before.Line()
	case len(x.before.Report.Files) == 0:
		line = x.added.LineAs(addedWhen)
	default:
		line = strings.Join([]string{x.before.Line(), addedWhen + ": " + x.added.Counts(), x.added.Checks()}, "; ")
	}
	// A flagged file a run that runs no commands was given is named.
	for _, h := range []string{x.before.HandedOver(), x.added.HandedOver()} {
		if h != "" {
			if line != "" {
				line += "; "
			}
			line += h
		}
	}
	// A file refused during the session is named, never dropped in
	// silence.
	if len(x.refused) > 0 {
		if line != "" {
			line += "; "
		}
		line += "files added during the session and refused before the agent was told of them: " + strings.Join(x.refused, "; ")
	}
	return line
}
