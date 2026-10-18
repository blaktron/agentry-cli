package engine

// The delivery step: after a run has succeeded and teardown has stopped and
// removed every container it started, each destination the person gave the run
// is sent the run's result and its deliverables by a step of its own.
//
// A webhook is sent one signed POST of the result and the files; a bucket is sent
// each file as an object of its own, and nothing else (a run with no file leaves
// it nothing).
//
// The step runs the runner image's `deliver` role in a container that has
// nothing of the run: it gets copies of the deliverables, staged read-only by the
// host after each was shown to be the bytes of the digest the run's envelope
// names, a job file, and a folder for its report. Its network is internal, with
// one proxy on it, which reaches the destination's host and port alone and, for
// an ordinary destination, only at a public address (and for one saved with "On
// my network", at a public or private-network address, never link-local). The
// destination's credential arrives on the step's stdin and nowhere else: not an
// environment, an argument, a mount, a log, a receipt or the envelope. A run
// given no destination makes no such container, and a destination is never named
// by a release. What the receiver says is kept nowhere: each file's outcome is
// one word from the receipt's closed list.
//
// A failed delivery never changes the run: it is a record (receipts/
// deliveries.ndjson and the envelope's deliveries block).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/send"
)

// deliveryTimeout bounds one webhook's step, and bucketDeliveryTimeout one
// bucket's, which puts a file at a time: its own requests are bounded well
// inside them.
const (
	deliveryTimeout       = 6 * time.Minute
	bucketDeliveryTimeout = 12 * time.Minute
)

// deliveryTotalTimeout bounds every destination's step of one delivery together.
const deliveryTotalTimeout = 24 * time.Minute

// MaxObjectBytes is the most one object sent to a bucket is.
const MaxObjectBytes = send.DefaultMaxObject

// MaxBucketStaged is the most bytes of deliverables a bucket's step is given
// copies of in all; a file past what is left is listed as too large.
const MaxBucketStaged int64 = 1 << 30

// maxDeliveryReport bounds the step's report.
const maxDeliveryReport = 1 << 20

// deliveryProxyAlias is the delivery proxy's name on its network.
const deliveryProxyAlias = "delivery-proxy"

// MaxWebhookBody is the most a webhook body is, in bytes.
const MaxWebhookBody = send.DefaultMaxBody

// deliveryRun is what a delivery to destinations needs of the run that made
// them or of the folder a later delivery reads.
type deliveryRun struct {
	// RunDir is the output folder, RunID, Ref and Digest the envelope's
	// own, Result its result and Files the deliverables it lists.
	RunDir string
	RunID  string
	Ref    string
	Digest string
	Result json.RawMessage
	Files  []receipt.Deliverable
	// Image is the runner image the step and its proxy run in, Limits the
	// posture they get, Engine the container engine, Root where the step's
	// own folders are made (the runner home), Stderr the progress writer.
	Image  string
	Limits policy.Limits
	Engine dockerx.Engine
	Root   string
	Stderr io.Writer
}

// sendToDestinations delivers the run to each destination in turn and answers
// every record, one per file per destination (one for the result alone when
// the run left no file). It never fails the caller.
func sendToDestinations(ctx context.Context, d dockerx.Client, r deliveryRun, dests []destination.Resolved) []receipt.Delivery {
	// Every destination together has a time of its own, so that several slow
	// receivers cannot hold a finished run's process for the sum of their limits.
	ctx, cancel := context.WithTimeout(ctx, deliveryTotalTimeout)
	defer cancel()
	var out []receipt.Delivery
	for _, dest := range dests {
		out = append(out, sendToOne(ctx, d, r, dest)...)
	}
	return out
}

// kindOf is the receipt's word for a destination's kind.
func kindOf(dest destination.Resolved) string {
	if dest.Kind == destination.KindS3 {
		return receipt.DeliveryKindS3
	}
	return receipt.DeliveryKindWebhook
}

// objectsFor names, for each deliverable, what a record calls the thing sent:
// its path for a webhook, and for a bucket the object written, <bucket>/<key>
// (the path again for one the step will not name).
func objectsFor(r deliveryRun, dest destination.Resolved) []string {
	out := make([]string, len(r.Files))
	var keys []string
	if dest.Kind == destination.KindS3 {
		paths := make([]string, len(r.Files))
		for i, f := range r.Files {
			paths[i] = f.Path
		}
		keys = send.ObjectKeys(dest.Prefix, r.RunID, paths)
	}
	for i, f := range r.Files {
		out[i] = f.Path
		if keys != nil && keys[i] != "" {
			out[i] = dest.Bucket + "/" + keys[i]
		} else if keys != nil {
			// A path the step will not name stays in the record, but as
			// text no terminal acts on.
			out[i] = strconv.QuoteToASCII(f.Path)
		}
	}
	return out
}

// recordsFor is every record of a destination answered one way: a failure
// before the step could say more. A bucket is sent files alone, so a run that
// left none has nothing to record for it.
func recordsFor(r deliveryRun, dest destination.Resolved, host, status, reason string) []receipt.Delivery {
	var out []receipt.Delivery
	rec := func(object, sum string) {
		out = append(out, receipt.Delivery{Destination: dest.Name, Kind: kindOf(dest), Host: host, Object: object, SHA256: sum, Status: status, Error: reason, OnMyNetwork: dest.OnMyNetwork})
	}
	objects := objectsFor(r, dest)
	for i, f := range r.Files {
		rec(objects[i], f.SHA256)
	}
	if len(r.Files) == 0 && dest.Kind != destination.KindS3 {
		rec("result", resultSum(r.Result))
	}
	return out
}

// resultSum is the SHA-256 of a result as it is sent, empty when there is none.
func resultSum(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	return sha256Hex(bytes.TrimSpace(raw))
}

// sendToOne delivers to one destination.
func sendToOne(ctx context.Context, d dockerx.Client, r deliveryRun, dest destination.Resolved) (records []receipt.Delivery) {
	failed := func(host, reason string) []receipt.Delivery {
		return recordsFor(r, dest, host, receipt.DeliveryFailed, reason)
	}
	// What the kind decides: the host and port the proxy admits, whether it is
	// plain http, the credential the step is given, and how long it has.
	var (
		target  send.Target
		cred    send.Credential
		limit   = deliveryTimeout
		bucketS send.S3Target
		isS3    = dest.Kind == destination.KindS3
	)
	switch dest.Kind {
	case destination.KindWebhook:
		var err error
		if target, err = send.CheckURL(dest.URL, dest.OnMyNetwork); err != nil {
			return failed("", receipt.DeliveryErrDestination)
		}
		if !send.ValidSecret(dest.Secret) || !send.ValidHeaderValue(dest.Authorization) {
			return failed(target.HostPort, receipt.DeliveryErrCredential)
		}
		cred = send.Credential{Secret: dest.Secret, Authorization: dest.Authorization}
	case destination.KindS3:
		var err error
		if bucketS, err = send.CheckS3(dest.S3Spec()); err != nil || !send.ValidRunID(r.RunID) {
			return failed("", receipt.DeliveryErrDestination)
		}
		target = send.Target{HostPort: bucketS.HostPort, Plain: bucketS.Plain}
		if !send.ValidAccessKeyID(dest.AccessKeyID) || !send.ValidSecret(dest.Secret) {
			return failed(target.HostPort, receipt.DeliveryErrCredential)
		}
		if len(r.Files) == 0 {
			return nil
		}
		cred = send.Credential{AccessKeyID: dest.AccessKeyID, SecretAccessKey: dest.Secret}
		limit = bucketDeliveryTimeout
	default:
		return failed("", receipt.DeliveryErrDestination)
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return failed(target.HostPort, receipt.DeliveryErrTimeout)
		}
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	sweepStaleDeliveryFolders(r.Root)
	// The step's folders: the staged copies (read-only to the step), the
	// job (read-only) and the report's (the one place it writes).
	folder, err := newDeliveryFolder(r.Root)
	if err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	defer folder.remove()
	base := folder.path
	staged, jobDir, work := filepath.Join(base, "staged"), filepath.Join(base, "job"), filepath.Join(base, "work")
	for _, dir := range []string{staged, jobDir, work} {
		if err := os.Mkdir(dir, deliveryDirMode); err != nil {
			return failed(target.HostPort, receipt.DeliveryErrStep)
		}
	}
	var job send.Job
	var stagedFiles []deliver.Staged
	if isS3 {
		job = send.Job{
			Kind: send.KindS3, Bucket: dest.Bucket, Prefix: dest.Prefix, Region: dest.Region, Endpoint: dest.Endpoint, OnMyNetwork: dest.OnMyNetwork,
			RunID: r.RunID, Ref: r.Ref, Digest: r.Digest, MaxObject: send.DefaultMaxObject,
		}
		stagedFiles = deliver.StageRaw(r.RunDir, r.Files, staged, send.DefaultMaxObject, MaxBucketStaged)
	} else {
		job = send.Job{
			Kind: send.KindWebhook, URL: dest.URL, OnMyNetwork: dest.OnMyNetwork,
			RunID: r.RunID, Ref: r.Ref, Digest: r.Digest, Result: r.Result, MaxBody: MaxWebhookBody,
		}
		stagedFiles = deliver.Stage(r.RunDir, r.Files, staged, MaxWebhookBody)
	}
	for i, st := range stagedFiles {
		f := r.Files[i]
		jf := send.File{Path: f.Path, Title: f.Title, SHA256: f.SHA256, Bytes: f.Bytes, Type: f.Type, Marked: f.Marked}
		if st.Name != "" {
			jf.Staged = st.Name
		} else {
			jf.Omitted = st.Reason
		}
		job.Files = append(job.Files, jf)
	}
	jb, err := json.Marshal(job)
	if err != nil || os.WriteFile(filepath.Join(jobDir, send.JobFile), jb, deliveryFileMode) != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}

	id, err := NewRunID()
	if err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	labels := runLabels(id)
	netName := "agentry-" + id + "-dlv"
	proxyName := containerName(id, "delivery-proxy")
	stepName := containerName(id, "delivery")
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	removed := false
	cleanup := func(c context.Context) {
		if removed {
			return
		}
		// The step first: a docker run killed on the timeout leaves its
		// container holding the network.
		_ = d.RemoveForce(c, stepName)
		_ = d.RemoveForce(c, proxyName)
		removed = d.NetworkRemove(c, netName) == nil
	}
	defer cleanup(context.WithoutCancel(ctx))

	argv := []string{"proxy", "--allow", target.HostPort, "--addr", fmt.Sprintf(":%d", spec.ProxyPort)}
	if dest.OnMyNetwork {
		argv = append(argv, "--network-ok")
		if target.Plain {
			argv = append(argv, "--plain-http", target.HostPort)
		}
	} else {
		argv = append(argv, "--public-only")
	}
	proxySpec := readOnlyRoot(hardened(dockerx.RunSpec{
		Name: proxyName, Image: r.Image, Argv: argv, Network: netName, Alias: deliveryProxyAlias, Labels: labels,
	}, r.Limits), "/tmp")
	if err := d.RunDetached(ctx, proxySpec); err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	// Its route out, as the other proxies': the daemon's default bridge.
	if err := d.NetworkConnect(ctx, "bridge", proxyName); err != nil {
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}

	stepSpec := dockerx.RunSpec{
		Name:  stepName,
		Image: r.Image,
		Argv: []string{"deliver", "--job", "/job", "--staged", "/staged", "--work", "/work",
			"--proxy", fmt.Sprintf("http://%s:%d", deliveryProxyAlias, spec.ProxyPort)},
		Network: netName,
		Mounts: []dockerx.Mount{
			{Source: staged, Target: "/staged", ReadOnly: true, Relabel: true},
			{Source: jobDir, Target: "/job", ReadOnly: true, Relabel: true},
			{Source: work, Target: "/work", Relabel: true},
		},
		// The files belong to the person who ran the command, and the step
		// runs with no more than they have.
		User:        r.Engine.PersonUser(),
		Interactive: true,
		AutoRemove:  true,
		Labels:      labels,
	}
	stepSpec = readOnlyRoot(hardened(stepSpec, r.Limits), "/tmp:size=64m,mode=1777")
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var errOut bytes.Buffer
	code, rerr := d.RunForeground(sctx, stepSpec, bytes.NewReader(credJSON), io.Discard, &limitedBuffer{b: &errOut, n: 4 << 10})
	// The proxy's decisions are the evidence of what the step reached: host
	// and port and the verdict, never a credential or a body.
	if logs, lerr := d.Logs(context.WithoutCancel(ctx), proxyName, 100); lerr == nil && logs != "" {
		appendDeliveryProxyLog(r.RunDir, dest.Name, logs)
	}
	cleanup(context.WithoutCancel(ctx))
	if rerr != nil {
		say(r.Stderr, dest, "the delivery step could not run: "+scrubCredential(rerr.Error(), dest))
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	rep, ok := readDeliveryReport(work)
	if !ok {
		say(r.Stderr, dest, fmt.Sprintf("the delivery step left no report (exit %d)", code))
		return failed(target.HostPort, receipt.DeliveryErrStep)
	}
	return recordsFromReport(r, dest, target.HostPort, rep)
}

// deliveryDirMode and deliveryFileMode are the modes of a delivery's folders and
// files: the job file holds the destination's address, whose path or query may
// carry the receiver's own token, and the staged copies are the run's
// deliverables, so a process that dies in the middle of a step leaves them
// readable by the person who ran it and no one else. The step runs as that
// person (dockerx.Engine.PersonUser), or as the container's root, which a
// rootless engine maps to them. A umask only removes bits from these. Windows
// has no such modes: the folder there is as private as the runner home it is
// made in, whose access list it inherits, and the CLI sets none of its own.
const (
	deliveryDirMode  os.FileMode = 0o700
	deliveryFileMode os.FileMode = 0o600
)

// deliveryLockFile is the lock a delivery holds on its folder for as long as the
// folder exists, so that a sweep can tell a folder whose process died (the lock
// is free: the system releases it however the process ends) from a delivery in
// flight, whatever the folder's age. It is in the folder's top, which no
// container is given.
const deliveryLockFile = "delivery.lock"

// deliveryFolder is a delivery's working folder under the runner home and the
// lock that keeps it from the sweep.
type deliveryFolder struct {
	path string
	lock *os.File
}

// newDeliveryFolder makes a delivery's folder in root, private to the person
// (os.MkdirTemp makes it deliveryDirMode), and takes its lock before anything is
// put in it.
func newDeliveryFolder(root string) (*deliveryFolder, error) {
	path, err := os.MkdirTemp(root, "delivery-")
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(path, deliveryLockFile), os.O_CREATE|os.O_EXCL|os.O_RDWR, deliveryFileMode)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := tryLockFile(lock); err != nil {
		_ = lock.Close()
		_ = os.RemoveAll(path)
		return nil, err
	}
	return &deliveryFolder{path: path, lock: lock}, nil
}

// remove deletes the folder and lets go of its lock (first: a file held open
// cannot be deleted on Windows).
func (f *deliveryFolder) remove() {
	_ = f.lock.Close()
	_ = os.RemoveAll(f.path)
}

// staleDelivery is how old a delivery folder with no lock file is when no
// delivery can still be using it: every destination's step together is bounded
// at deliveryTotalTimeout. Such a folder is judged by its age alone: its process
// may take no lock, or be between making the folder and locking it.
const staleDelivery = time.Hour

// deliveryFolderName is what os.MkdirTemp makes of "delivery-": the prefix and a
// decimal number, and nothing else a person might have named a folder.
var deliveryFolderName = regexp.MustCompile(`^delivery-[0-9]+$`)

// sweepStaleDeliveryFolders removes the delivery folders in root that a process
// that died left behind, at the start of every run and delivery. A folder whose
// lock is free belongs to a process that is gone and is removed at once; one
// whose lock is held is in flight and is left alone; one with no lock file is
// removed once it is older than staleDelivery. Best effort. An empty root is
// not swept: folders made there are in the system's temporary folder, shared
// with names that are not the runner's.
func sweepStaleDeliveryFolders(root string) {
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !deliveryFolderName.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		lock, err := os.OpenFile(filepath.Join(dir, deliveryLockFile), os.O_RDWR, 0)
		switch {
		case err == nil:
			if tryLockFile(lock) == nil {
				_ = lock.Close()
				_ = os.RemoveAll(dir)
				continue
			}
			_ = lock.Close()
		case errors.Is(err, fs.ErrNotExist):
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > staleDelivery {
				_ = os.RemoveAll(dir)
			}
		}
	}
}

// say writes one line about a destination to the progress writer, with the
// credential masked.
func say(w io.Writer, dest destination.Resolved, msg string) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "warning: delivery to %s: %s\n", dest.Name, scrubCredential(msg, dest))
}

// scrubCredential masks the destination's credential from text that may echo
// it.
func scrubCredential(s string, dest destination.Resolved) string {
	for _, v := range []string{dest.Secret, dest.Authorization, dest.AccessKeyID} {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[credential]")
		}
	}
	return s
}

// readDeliveryReport reads the step's report from its work folder: a regular
// file within the bound that decodes as the step's own format.
func readDeliveryReport(work string) (send.Report, bool) {
	var rep send.Report
	root, err := os.OpenRoot(work)
	if err != nil {
		return rep, false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(send.ReportFile)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxDeliveryReport {
		return rep, false
	}
	f, err := root.Open(send.ReportFile)
	if err != nil {
		return rep, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxDeliveryReport))
	if err != nil || json.Unmarshal(b, &rep) != nil || rep.Schema != send.ReportSchema {
		return rep, false
	}
	return rep, true
}

// recordsFromReport turns the step's report into the receipt's records. The
// report is read as the word of a program that held the credential, no more:
// each file's status and reason are taken only from the closed lists, and
// anything else is the word "step".
func recordsFromReport(r deliveryRun, dest destination.Resolved, host string, rep send.Report) []receipt.Delivery {
	byPath := map[string]send.FileResult{}
	for _, f := range rep.Files {
		byPath[f.Path] = f
	}
	var out []receipt.Delivery
	objects := objectsFor(r, dest)
	for i, f := range r.Files {
		fr, ok := byPath[f.Path]
		status, reason := closedOutcome(fr.Status, fr.Error)
		if !ok {
			status, reason = receipt.DeliveryFailed, receipt.DeliveryErrStep
		}
		out = append(out, receipt.Delivery{Destination: dest.Name, Kind: kindOf(dest), Host: host, Object: objects[i], SHA256: f.SHA256, Status: status, Error: reason, OnMyNetwork: dest.OnMyNetwork})
	}
	if len(r.Files) == 0 && dest.Kind != destination.KindS3 {
		// The result was delivered when the POST was and the body held it; a
		// result the body left out is refused with the reason, and one the POST
		// did not reach is failed with the POST's.
		status, reason := closedOutcome(rep.Status, rep.Error)
		switch {
		case rep.Status == send.StatusDelivered && rep.Result == send.StatusDelivered:
			status, reason = receipt.DeliveryDelivered, ""
		case rep.Status == send.StatusDelivered && (rep.Result == send.ReasonTooLarge || rep.Result == send.ReasonUnreadable):
			status, reason = closedOutcome(receipt.DeliveryRefused, rep.Result)
		case rep.Status == send.StatusDelivered:
			status, reason = receipt.DeliveryFailed, receipt.DeliveryErrStep
		}
		out = append(out, receipt.Delivery{Destination: dest.Name, Kind: kindOf(dest), Host: host, Object: "result", SHA256: resultSum(r.Result), Status: status, Error: reason, OnMyNetwork: dest.OnMyNetwork})
	}
	return out
}

// closedOutcome holds a status and a reason to the receipt's closed lists.
func closedOutcome(status, reason string) (string, string) {
	switch status {
	case receipt.DeliveryDelivered:
		return status, ""
	case receipt.DeliveryRefused, receipt.DeliveryFailed:
		for _, w := range receipt.DeliveryErrors {
			if w == reason && w != receipt.DeliveryErrNotMarked {
				return status, reason
			}
		}
	}
	return receipt.DeliveryFailed, receipt.DeliveryErrStep
}

// appendDeliveryProxyLog adds the proxy's decisions for one delivery to
// receipts/delivery-proxy-<destination>.log, a file of its own so that a later
// delivery of the same folder adds to it and nothing is rewritten.
func appendDeliveryProxyLog(runDir, name, logs string) {
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("receipts", 0o755); err != nil {
		return
	}
	rel := "receipts/delivery-proxy-" + name + ".log"
	if fi, err := root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
		return
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "# %s\n%s", time.Now().UTC().Format(time.RFC3339), logs)
	if !strings.HasSuffix(logs, "\n") {
		_, _ = f.WriteString("\n")
	}
	_ = f.Close()
}

// RedeliverParams is a later delivery of a finished run's folder.
type RedeliverParams struct {
	// RunDir is the run's output folder.
	RunDir string
	// Destinations are the places to send it, credentials read.
	Destinations []destination.Resolved
	// Policy is the runner policy (its limits), RunnerImage the runner
	// image's tag, present on the machine, and RunRoot the runner home.
	Policy      *policy.Policy
	RunnerImage string
	RunRoot     string
	Stderr      io.Writer
}

// Redeliver sends the result and the deliverables an existing run folder's
// envelope lists to the destinations, and adds the records to its receipts/
// deliveries.ndjson. result.json and every other file of the folder are left as
// they are. An error is returned only when there is nothing to deliver: a folder
// that is not a run's, a run that did not succeed.
func Redeliver(ctx context.Context, d dockerx.Client, p RedeliverParams) ([]receipt.Delivery, error) {
	env, err := deliver.ReadEnvelope(p.RunDir)
	if err != nil {
		return nil, err
	}
	if env.Status != "ok" {
		return nil, fmt.Errorf("the run in %s did not succeed (%s), so it delivers nothing", p.RunDir, env.ExitClass)
	}
	var files []receipt.Deliverable
	if env.Deliverables != nil {
		files = env.Deliverables.Files
	}
	if len(files) > receipt.MaxDeliverables {
		return nil, fmt.Errorf("the run in %s lists more than %d deliverables, which no run's envelope does", p.RunDir, receipt.MaxDeliverables)
	}
	if len(files) == 0 && resultSum(env.Result) == "" {
		return nil, fmt.Errorf("the run in %s lists no deliverables and has no result", p.RunDir)
	}
	e, err := dockerx.DescribeEngine(ctx, d)
	if err != nil {
		return nil, Classify(ExitGateway, err)
	}
	lim := policy.Limits{}.WithDefaults()
	if p.Policy != nil {
		lim = p.Policy.Docker.Limits.WithDefaults()
	}
	ds := sendToDestinations(ctx, d, deliveryRun{
		RunDir: p.RunDir, RunID: env.RunID, Ref: env.Ref, Digest: env.Digest, Result: resultToSend(env), Files: files,
		Image: p.RunnerImage, Limits: lim, Engine: e, Root: p.RunRoot, Stderr: p.Stderr,
	}, p.Destinations)
	if err := deliver.WriteLedger(p.RunDir, ds, true); err != nil {
		return ds, fmt.Errorf("the deliveries were made, but %s could not be recorded in the run's receipts: %w", deliver.LedgerFile, err)
	}
	return ds, nil
}

// resultToSend is the envelope's result for a body: absent when it holds none.
func resultToSend(env *receipt.Envelope) json.RawMessage {
	if len(bytes.TrimSpace(env.Result)) == 0 || string(bytes.TrimSpace(env.Result)) == "null" {
		return nil
	}
	return env.Result
}
