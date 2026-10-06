package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// inputsDecoder is the decode step on a run's own inputs (plan
// file-handling.md §5, D2; agentry-cli#560): every run with inputs, from
// any client, has them decoded before the agent starts, and a session's
// file added mid-run before it is announced. The originals stay at
// /inputs; the host writes the index and the decoded texts to the run's
// inputs-text directory (toolsrv's layout), copying each text only through
// decode.Result's checked, capped reads, so nothing the decode container
// wrote reaches the tools container unchecked. The words the index shows
// the model come from the decode's fixed vocabulary, never from the
// report's free text (decode.UnreadReason). A run with no inputs pulls
// nothing and has no decode line. Every file is checked by ClamAV before
// any decoder reads it (plan file-handling.md §7; agentry-cli#563): a
// finding refuses the run before the agent starts, with the policy class,
// so a hosted run settles nothing (D9), and refuses a session's added file
// alone.
type inputsDecoder struct {
	d  dockerx.Client
	p  Params
	rd *runDirs

	mu  sync.Mutex
	img *decode.Image
	// lookup asks Hybrid Analysis about the risky inputs by hash (plan
	// file-handling.md §7, agentry-cli#564), with the policy's key or none.
	lookup *hybrid.Client
	// seq numbers the decode directories under the run root.
	seq int
	// before is what was decoded before the agent started, added what a
	// session added after: the receipt says when each was decoded.
	before, added decode.Result
	// refused is each refusal of a session's added file, for the receipt.
	refused []string
	index   toolsrv.InputsIndex
}

// ensureDecode, ensureSignatures and runDecode are the decode step's
// calls, variables so the engine's tests can decode without a daemon or a
// registry.
var (
	ensureDecode     = decode.Ensure
	ensureSignatures = clamdb.Ensure
	runDecode        = decode.Run
	hybridClient     = hybrid.FromPolicy
)

// decodeInputs decodes what the run staged under /inputs. It answers nil
// when the caller decoded the inputs itself or staged only its own files
// (Params.Decoded, Params.TextInputs: every built-in), and otherwise a
// decoder, which has decoded nothing when nothing was given, so a
// session's first added file is decoded on its own.
func decodeInputs(ctx context.Context, d dockerx.Client, p Params, rd *runDirs) (*inputsDecoder, error) {
	if p.Decoded != nil || p.TextInputs {
		return nil, nil
	}
	x := &inputsDecoder{d: d, p: p, rd: rd, index: toolsrv.InputsIndex{Schema: toolsrv.InputsIndexSchema, Files: []toolsrv.InputEntry{}}}
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
	res, err := x.decode(ctx, rd.inputs)
	if err != nil {
		return nil, err
	}
	if res.Refused() {
		return nil, Classify(ExitPolicy, inputRefused(receipt.InputMalware, fmt.Errorf("the run was refused before the agent started: %s", res.Refusal())))
	}
	entries, err := x.stageTexts(res)
	if err != nil {
		return nil, err
	}
	x.before = merged(x.before, res)
	return x, x.commit(entries)
}

// inputRefused names why an input was refused, in the run's reason; never
// which input, since its name is the person's (agentry-cli#628).
func inputRefused(why string, err error) error {
	return receipt.WithReason(err, receipt.ReasonInputRefused, why)
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
func (x *inputsDecoder) decode(ctx context.Context, in string) (*decode.Result, error) {
	if x.p.Policy == nil {
		return nil, Classify(ExitCLI, errors.New("the inputs cannot be decoded: no policy names the files image"))
	}
	if x.img == nil {
		img, err := ensureDecode(ctx, x.d, x.p.Plan.Proxy.Image, x.p.Policy, filepath.Join(x.rd.root, "image"), x.p.Stderr)
		if err != nil {
			return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the inputs could not be decoded: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeImage))
		}
		// With no home to keep it in there is no bundle, and the receipt
		// says no file was checked (clamdb.Ensure answers ProblemNone).
		home := x.p.RunnerHome
		if home == "" {
			home, _ = policy.Home()
		}
		img.Signatures = ensureSignatures(ctx, home, x.p.Policy.Images.ClamAVSignaturesRef(), x.p.Stderr)
		// The Hybrid Analysis key, the person's or a sandbox host's: none
		// asks nothing, and the receipt says so (D8); a keychain the
		// policy names that cannot be read stops the run, as for judge.
		lookup, err := hybridClient(x.p.Policy)
		if err != nil {
			return nil, Classify(ExitPolicy, fmt.Errorf("the Hybrid Analysis key: %w", err))
		}
		x.img, x.lookup = &img, lookup
	}
	out := filepath.Join(x.rd.root, "decode", strconv.Itoa(x.seq))
	x.seq++
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("make the decode's output directory: %w", err))
	}
	res, err := runDecode(ctx, x.d, *x.img, in, out, x.p.limits(), x.p.Stderr)
	if err != nil {
		return nil, Classify(ExitGateway, receipt.WithReason(fmt.Errorf("the inputs could not be decoded: %w", err), receipt.ReasonInputsNotDecoded, receipt.DecodeRun))
	}
	res.LookUp(ctx, x.lookup)
	return res, nil
}

// stageTexts copies each decoded text into inputs-text/text/<path>, and
// each Markdown view of HTML into inputs-text/converted/<path>
// (agentry-cli#631), and answers every file's index entry, for commit to
// publish. The caller holds x.mu.
func (x *inputsDecoder) stageTexts(res *decode.Result) ([]toolsrv.InputEntry, error) {
	textRoot := filepath.Join(x.rd.inputsText, toolsrv.InputsTextSubdir)
	convertedRoot := filepath.Join(x.rd.inputsText, toolsrv.InputsConvertedSubdir)
	entries := make([]toolsrv.InputEntry, 0, len(res.Report.Files))
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
			e.Converted, e.ConvertedBy, e.ConvertedBytes = true, f.Tool, fi.Size()
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
// so the caller announces it only once all three are in place (plan
// file-handling.md §5, D2). It answers the file's index entry.
func (x *inputsDecoder) add(ctx context.Context, base string, data []byte) (toolsrv.InputEntry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	in := filepath.Join(x.rd.root, "decode", "add-"+strconv.Itoa(x.seq))
	if err := os.MkdirAll(in, 0o755); err != nil {
		return toolsrv.InputEntry{}, fmt.Errorf("stage %s for the decode: %w", base, err)
	}
	if err := os.WriteFile(filepath.Join(in, base), data, 0o644); err != nil { //nolint:gosec // read by the decode container
		return toolsrv.InputEntry{}, fmt.Errorf("stage %s for the decode: %w", base, err)
	}
	res, err := x.decode(ctx, in)
	if err != nil {
		return toolsrv.InputEntry{}, err
	}
	// A finding refuses this file alone: nothing of it is staged, the
	// agent is not told of it, and the session goes on (D9).
	if res.Refused() {
		x.refused = append(x.refused, res.Refusal())
		return toolsrv.InputEntry{}, fmt.Errorf("refused: %s; it was not added, and the session goes on without it", res.Refusal())
	}
	entries, err := x.stageTexts(res)
	if err != nil {
		return toolsrv.InputEntry{}, err
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
	// A file refused during the session is named, never dropped in
	// silence (D9).
	if len(x.refused) > 0 {
		if line != "" {
			line += "; "
		}
		line += "files added during the session and refused before the agent was told of them: " + strings.Join(x.refused, "; ")
	}
	return line
}
