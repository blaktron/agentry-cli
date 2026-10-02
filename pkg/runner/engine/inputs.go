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
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
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
// wrote reaches the tools container unchecked. A run with no inputs pulls
// nothing and has no decode line.
type inputsDecoder struct {
	d  dockerx.Client
	p  Params
	rd *runDirs

	mu    sync.Mutex
	img   *decode.Image
	n     int
	all   decode.Result
	index toolsrv.InputsIndex
}

// ensureDecode and runDecode are the decode step's two calls, variables
// so the engine's tests can decode without a daemon.
var (
	ensureDecode = decode.Ensure
	runDecode    = decode.Run
)

// decodeInputs decodes what the run staged under /inputs. It answers nil
// when the caller decoded the inputs itself or staged only text it wrote
// (Params.Decoded, Params.TextInputs: the Knowledge Creator and the Voice
// Writer), and a decoder
// that has decoded nothing yet when nothing was given, so a session's
// first added file is decoded on its own.
func decodeInputs(ctx context.Context, d dockerx.Client, p Params, rd *runDirs) (*inputsDecoder, error) {
	if p.Decoded != nil || p.TextInputs {
		return nil, nil
	}
	x := &inputsDecoder{d: d, p: p, rd: rd, index: toolsrv.InputsIndex{Schema: toolsrv.InputsIndexSchema, Files: []toolsrv.InputEntry{}}}
	given, err := hasGivenInputs(rd.inputs)
	if err != nil || !given {
		return x, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	res, err := x.decode(ctx, rd.inputs)
	if err != nil {
		return nil, err
	}
	if err := x.stage(res); err != nil {
		return nil, err
	}
	return x, nil
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

// decode runs the decode container over in, a directory this run made,
// into a fresh output directory under the run root. The decode image is
// ensured on first use: the files image is pulled only for a run that has
// something to decode. The caller holds x.mu.
func (x *inputsDecoder) decode(ctx context.Context, in string) (*decode.Result, error) {
	if x.p.Policy == nil {
		return nil, Classify(ExitCLI, errors.New("the inputs cannot be decoded: no policy names the files image"))
	}
	if x.img == nil {
		img, err := ensureDecode(ctx, x.d, x.p.Plan.Proxy.Image, x.p.Policy, filepath.Join(x.rd.root, "image"), x.p.Stderr)
		if err != nil {
			return nil, Classify(ExitGateway, fmt.Errorf("the inputs could not be decoded: %w", err))
		}
		x.img = &img
	}
	out := filepath.Join(x.rd.root, "decode", strconv.Itoa(x.n))
	x.n++
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, Classify(ExitCLI, fmt.Errorf("the decode's output directory: %w", err))
	}
	res, err := runDecode(ctx, x.d, *x.img, in, out, x.p.limits(), x.p.Stderr)
	if err != nil {
		return nil, Classify(ExitGateway, fmt.Errorf("the inputs could not be decoded: %w", err))
	}
	return res, nil
}

// stage copies each decoded text into inputs-text/text/<path>, adds every
// file the decode reported to the index and writes the index. The caller
// holds x.mu.
func (x *inputsDecoder) stage(res *decode.Result) error {
	textRoot := filepath.Join(x.rd.inputsText, toolsrv.InputsTextSubdir)
	for _, f := range res.Report.Files {
		e := toolsrv.InputEntry{Path: f.Path, Type: string(f.Family)}
		// res.In is a directory this run staged, never the decode's
		// output, so its sizes are the inputs' own.
		if fi, err := os.Lstat(filepath.Join(res.In, filepath.FromSlash(f.Path))); err == nil {
			e.Bytes = fi.Size()
		}
		switch f.Outcome {
		case runnerdecode.OutcomeDecoded:
			dst := filepath.Join(textRoot, filepath.FromSlash(f.Path))
			if err := res.CopyText(f.Path, dst); err != nil {
				return Classify(ExitGateway, fmt.Errorf("the text decoded from %s: %w", f.Path, err))
			}
			fi, err := os.Stat(dst)
			if err != nil {
				return Classify(ExitCLI, err)
			}
			e.Decoded, e.DecodedBy, e.TextBytes = true, f.Tool, fi.Size()
		case runnerdecode.OutcomeText:
			e.TextBytes = e.Bytes
		default:
			e.Unread = f.Reason
			if e.Unread == "" {
				e.Unread = string(f.Outcome)
			}
		}
		x.index.Files = append(x.index.Files, e)
		x.all.Report.Files = append(x.all.Report.Files, f)
	}
	x.all.Image = res.Image
	sort.Slice(x.index.Files, func(i, j int) bool { return x.index.Files[i].Path < x.index.Files[j].Path })
	return x.writeIndex()
}

// writeIndex replaces the index in one rename, so the tools container
// never reads half of one.
func (x *inputsDecoder) writeIndex() error {
	b, err := json.MarshalIndent(x.index, "", "  ")
	if err != nil {
		return Classify(ExitCLI, err)
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
// own, then stages the original under /inputs and updates the index,
// before the caller announces it (plan file-handling.md §5, D2). It
// answers the file's index entry.
func (x *inputsDecoder) add(ctx context.Context, base string, data []byte) (toolsrv.InputEntry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	in := filepath.Join(x.rd.root, "decode", "add-"+strconv.Itoa(x.n))
	if err := os.MkdirAll(in, 0o755); err != nil {
		return toolsrv.InputEntry{}, err
	}
	if err := os.WriteFile(filepath.Join(in, base), data, 0o644); err != nil { //nolint:gosec // read by the decode container
		return toolsrv.InputEntry{}, err
	}
	res, err := x.decode(ctx, in)
	if err != nil {
		return toolsrv.InputEntry{}, err
	}
	if err := os.WriteFile(filepath.Join(x.rd.inputs, base), data, 0o644); err != nil { //nolint:gosec // inputs are agent-readable by design
		return toolsrv.InputEntry{}, fmt.Errorf("stage input %s: %w", base, err)
	}
	if err := x.stage(res); err != nil {
		return toolsrv.InputEntry{}, err
	}
	for _, e := range x.index.Files {
		if e.Path == base {
			return e, nil
		}
	}
	return toolsrv.InputEntry{}, fmt.Errorf("the decode did not report %s", base)
}

// line is the receipt's decode line for every decode this run made, empty
// when it made none.
func (x *inputsDecoder) line() string {
	if x == nil {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.all.Report.Files) == 0 {
		return ""
	}
	return x.all.Line()
}
