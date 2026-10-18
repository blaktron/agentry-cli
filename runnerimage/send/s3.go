package send

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// s3RequestTimeout bounds one object's PUT: a deliverable can be 64 MiB.
// s3Budget bounds the whole delivery, inside the caller's own limit on the
// step, so that the step still writes its report: a file not sent when it ends
// is failed with "timeout", and the files sent are told.
const (
	s3RequestTimeout = 8 * time.Minute
	s3Budget         = 10 * time.Minute
)

// deliverS3 puts each file of the job to the bucket, one request each, signed
// with the access key (AWS Signature Version 4, the payload hash being the
// digest the run's envelope names, which the step has just shown the staged
// bytes to have). Only the key id and the signature leave the step, in the
// request's headers; the secret never does. A request that fails to connect, or
// is refused for who the sender is, where it went or that the bucket is not
// there, ends the delivery: the files after it are failed with the same reason
// and nothing more is tried. A file the step cannot send is refused with its
// reason and the others still go.
func deliverS3(ctx context.Context, o Options, rep *Report) {
	failAll := func(reason string) {
		rep.Status, rep.Error = StatusFailed, reason
		for i := range rep.Files {
			if rep.Files[i].Status == "" {
				rep.Files[i].Status, rep.Files[i].Error = StatusFailed, reason
			}
		}
	}
	spec := S3Spec{Bucket: o.Job.Bucket, Prefix: o.Job.Prefix, Region: o.Job.Region, Endpoint: o.Job.Endpoint, OnMyNetwork: o.Job.OnMyNetwork}
	target, err := CheckS3(spec)
	if o.unchecked {
		u, perr := url.Parse(spec.Endpoint)
		if perr == nil && spec.Endpoint != "" {
			target, err = S3Target{S3Spec: spec, Scheme: u.Scheme, Host: u.Host, HostPort: u.Host, PathStyle: true, Plain: u.Scheme == "http"}, nil
		}
	}
	if err != nil || !ValidRunID(o.Job.RunID) {
		failAll(ReasonDestination)
		return
	}
	rep.Host = target.HostPort
	if !ValidAccessKeyID(o.Cred.AccessKeyID) || !ValidSecret(o.Cred.SecretAccessKey) {
		failAll(ReasonCredential)
		return
	}
	paths := make([]string, len(o.Job.Files))
	for i, f := range o.Job.Files {
		paths[i] = f.Path
	}
	keys := ObjectKeys(spec.Prefix, o.Job.RunID, paths)
	maxObject := o.Job.MaxObject
	if maxObject <= 0 {
		maxObject = DefaultMaxObject
	}
	root, rerr := os.OpenRoot(o.Staging)
	if rerr == nil {
		defer func() { _ = root.Close() }()
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	budget := s3Budget
	if o.Budget > 0 {
		budget = o.Budget
	}
	ctx, cancelBudget := context.WithTimeout(ctx, budget)
	defer cancelBudget()
	hc := client(o)
	hc.Timeout = s3RequestTimeout
	stop := ""
	firstBad := ""
	mark := func(i int, status, reason string) {
		rep.Files[i].Status, rep.Files[i].Error = status, reason
		if status != StatusDelivered && firstBad == "" {
			firstBad = reason
		}
	}
	for i, f := range o.Job.Files {
		switch {
		case !plainReportPath(f.Path) || keys[i] == "":
			mark(i, StatusRefused, ReasonOutside)
			continue
		case f.Omitted != "":
			mark(i, StatusRefused, f.Omitted)
			continue
		case f.Bytes < 0:
			mark(i, StatusRefused, ReasonUnreadable)
			continue
		case f.Bytes > maxObject:
			mark(i, StatusRefused, ReasonTooLarge)
			continue
		case f.Staged == "" || rerr != nil:
			mark(i, StatusRefused, ReasonMissing)
			continue
		case stop != "":
			mark(i, StatusFailed, stop)
			continue
		}
		file, reason := openStaged(root, f)
		if reason != "" {
			mark(i, StatusRefused, reason)
			continue
		}
		status, reason, fatal := putObject(ctx, hc, o, target, keys[i], f, file, now())
		_ = file.Close()
		mark(i, status, reason)
		if fatal {
			stop = reason
		}
	}
	rep.Status, rep.Error = StatusDelivered, ""
	for _, fr := range rep.Files {
		if fr.Status != StatusDelivered {
			rep.Status, rep.Error = StatusFailed, firstBad
			break
		}
	}
}

// putObject sends one file and answers its outcome, and whether it ends the
// delivery (nothing was reached, or the service refused the sender, the
// address or the bucket, which the next file would meet too). The service's
// answer is read to a bound and never kept.
func putObject(ctx context.Context, hc *http.Client, o Options, t S3Target, key string, f File, body io.Reader, at time.Time) (status, reason string, fatal bool) {
	u, esc := t.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.NopCloser(io.LimitReader(body, f.Bytes)))
	if err != nil {
		return StatusFailed, ReasonDestination, true
	}
	req.URL = u
	req.Host = t.Host
	req.ContentLength = f.Bytes
	amzDate := at.UTC().Format("20060102T150405Z")
	hash := strings.ToLower(f.SHA256)
	ct := contentTypeOf(f.Path)
	headers := map[string]string{
		"content-type":         ct,
		"host":                 t.Host,
		"x-amz-content-sha256": hash,
		"x-amz-date":           amzDate,
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Amz-Content-Sha256", hash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", signV4(o.Cred.AccessKeyID, o.Cred.SecretAccessKey, t.Region, at, http.MethodPut, esc, "", headers, hash))
	resp, err := hc.Do(req)
	if err != nil {
		return StatusFailed, classify(err), true
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return StatusDelivered, "", false
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return StatusFailed, ReasonRedirect, true
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		return StatusFailed, ReasonReceiverSaid, true
	}
	return StatusFailed, ReasonReceiverSaid, false
}

// openStaged opens a staged file for sending, after reading it once as a
// regular file reached by no link and showing it to be the bytes of the file's
// size and SHA-256. The handle is back at the start.
func openStaged(root *os.Root, f File) (*os.File, string) {
	if f.Staged == "" || strings.ContainsAny(f.Staged, `/\`) || f.Staged == "." || f.Staged == ".." {
		return nil, ReasonMissing
	}
	fi, err := root.Lstat(f.Staged)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ReasonMissing
	case err != nil:
		return nil, ReasonUnreadable
	case fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular():
		return nil, ReasonLink
	case fi.Size() != f.Bytes:
		return nil, ReasonChanged
	}
	h, err := root.Open(f.Staged)
	if err != nil {
		return nil, ReasonUnreadable
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(h, f.Bytes+1))
	if err != nil {
		_ = h.Close()
		return nil, ReasonUnreadable
	}
	if n != f.Bytes || hex.EncodeToString(sum.Sum(nil)) != strings.ToLower(f.SHA256) {
		_ = h.Close()
		return nil, ReasonChanged
	}
	if _, err := h.Seek(0, io.SeekStart); err != nil {
		_ = h.Close()
		return nil, ReasonUnreadable
	}
	return h, ""
}

// contentTypeOf is the type an object is stored with: the types a person's
// tools open as themselves, and a download for anything else, so a page the
// agent wrote is never served as one from the person's own bucket.
func contentTypeOf(p string) string {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(p), ".")) {
	case "pdf":
		return "application/pdf"
	case "md":
		return "text/markdown; charset=utf-8"
	case "txt":
		return "text/plain; charset=utf-8"
	case "csv":
		return "text/csv; charset=utf-8"
	case "json":
		return "application/json"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return "application/octet-stream"
}

// signV4 is the Authorization header value of a request signed with AWS
// Signature Version 4 for the s3 service: the canonical request is the method,
// the path as sent (already percent-encoded once), the query, each header
// (names lower case, in order) and the payload's SHA-256, hex.
func signV4(accessKeyID, secret, region string, at time.Time, method, escPath, query string, headers map[string]string, payloadHash string) string {
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + strings.TrimSpace(headers[n]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := method + "\n" + escPath + "\n" + query + "\n" + canonHeaders.String() + "\n" + signed + "\n" + payloadHash
	date := at.UTC().Format("20060102")
	scope := date + "/" + region + "/s3/aws4_request"
	digest := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + at.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + accessKeyID + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
