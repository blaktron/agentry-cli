package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/output"
	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// runOnCollect fetches the job's sealed output and opens it with this
// device's sealing key into outDir. An output that does not open, or holds
// anything a run's output does not, leaves nothing in outDir.
func CollectOutput(ctx context.Context, c *runhost.Client, jobID, device string, keys *runhost.DeviceKeys, outDir string) error {
	sealed, err := os.CreateTemp(filepath.Dir(outDir), ".runhost-output-*")
	if err != nil {
		return err
	}
	defer func() { _ = sealed.Close(); _ = os.Remove(sealed.Name()) }()
	var written int64
	a, err := c.Call(ctx, "collect", []string{jobID}, nil, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		if written += int64(len(f.Data)); written > output.MaxOutput+output.MaxHeader+(output.MaxOutput/output.ChunkSize+1)*20 {
			return errors.New("the sealed output is larger than any run's")
		}
		_, err := sealed.Write(f.Data)
		return err
	})
	if err != nil {
		return err
	}
	if a.Code != 0 {
		return errors.New(nonEmptyOr(a.Error, "the runhost would not hand the output over"))
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(outDir), ".runhost-open-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	pr, pw := io.Pipe()
	opened := make(chan error, 1)
	go func() {
		err := output.Open(pw, sealed, device, keys.Seal, jobID)
		pw.CloseWithError(err)
		opened <- err
	}()
	uerr := output.Unpack(pr, filepath.Join(staging, "out"))
	// Whatever Unpack did not read is drained, so Open reaches the end of
	// the sealed output and says whether all of it opened.
	_, _ = io.Copy(io.Discard, pr)
	oerr := <-opened
	if uerr != nil {
		return uerr
	}
	if oerr != nil {
		return oerr
	}
	return moveInto(filepath.Join(staging, "out"), outDir)
}

// moveInto moves every entry of src into dst, which exists.
func moveInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		to := filepath.Join(dst, e.Name())
		if err := os.RemoveAll(to); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}

// Envelope takes the runhost CLI's envelope as this device's record:
// its folders are this device's, and it names the runhost it ran on.
func Envelope(env *receipt.Envelope, rec *runhost.Record, outDir, jobID string) {
	if env.OutDir != "" {
		if env.Receipts.Dir != "" {
			if rel, err := filepath.Rel(env.OutDir, env.Receipts.Dir); err == nil && !strings.HasPrefix(rel, "..") {
				env.Receipts.Dir = filepath.Join(outDir, rel)
			}
		}
	}
	env.OutDir = outDir
	if env.Receipts.Dir == "" {
		env.Receipts.Dir = filepath.Join(outDir, "receipts")
	}
	env.Runhost = &receipt.Runhost{Name: rec.Name, Host: rec.Host, HostKeyFingerprint: rec.HostKeyFingerprint, CLIVersion: rec.CLIVersion, Job: jobID}
}

// Deliver copies a successful run's deliverables to the folder the device
// delivers to, as a local run does at its end, recording each copy in the
// envelope and the deliveries ledger. It answers a warning when the ledger
// could not be written.
func Deliver(env *receipt.Envelope, outDir, deliverTo string) string {
	if deliverTo == "" || env.ExitCode != 0 || env.Deliverables == nil {
		return ""
	}
	ds := deliver.ToFolder(outDir, env.Deliverables.Files, deliver.FolderOptions{
		Root: deliverTo, Agent: deliver.AgentName(env.Ref), Label: deliver.Label(env.Deliverables.Files, env.RunID),
	})
	env.Deliveries = append(env.Deliveries, ds...)
	if err := deliver.WriteLedger(outDir, ds, true); err != nil {
		return fmt.Sprintf("warning: the deliveries could not be recorded in receipts/%s: %v", deliver.LedgerFile, err)
	}
	return ""
}

// CollectEnvelope fetches a finished job's sealed envelope and opens it
// with this device's sealing key.
func CollectEnvelope(ctx context.Context, c *runhost.Client, jobID, device string, keys *runhost.DeviceKeys) ([]byte, error) {
	var sealed bytes.Buffer
	a, err := c.Call(ctx, "collect", []string{jobID, "envelope"}, nil, func(f runhost.Frame) error {
		if f.Type != runhost.FrameData {
			return nil
		}
		if sealed.Len()+len(f.Data) > runhost.MaxEnvelope+output.MaxHeader+(runhost.MaxEnvelope/output.ChunkSize+1)*20 {
			return errors.New("the sealed envelope is larger than any run's")
		}
		sealed.Write(f.Data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if a.Code != 0 {
		return nil, errors.New(nonEmptyOr(a.Error, "the runhost would not hand the envelope over"))
	}
	var plain bytes.Buffer
	if err := output.Open(&plain, &sealed, device, keys.Seal, runhost.SealedEnvelopeJob(jobID)); err != nil {
		return nil, err
	}
	return plain.Bytes(), nil
}
