// How a command's failure reaches the person: one line on stderr that says
// what happened in the product's words and what to do, and beneath it, only
// with --verbose, the client library's own error. Two failures have raw
// forms a person cannot act on, a registry that cannot be reached and a
// container engine that does not answer; each is carried as an error that
// renders the line itself and keeps the raw text as its detail.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// detailedError is an error whose text is the line for a person and whose
// Detail is the client's own error beneath it.
type detailedError interface {
	error
	Detail() string
}

// registryUnreachable is a request that never got an answer from the
// registry: the name did not resolve, the connection was refused or timed
// out, or the TLS handshake failed.
type registryUnreachable struct {
	registry string
	cause    string
	err      error
}

func (e *registryUnreachable) Error() string {
	return "cannot reach " + e.registry + " (" + e.cause + "): check the network connection, or pass --registry to use another registry"
}
func (e *registryUnreachable) Unwrap() error  { return e.err }
func (e *registryUnreachable) Detail() string { return e.err.Error() }

// transportCause is a transport failure in a few words.
func transportCause(err error) string {
	var dns *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dns):
		return "the name " + dns.Name + " does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "timed out"
	case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
		return "its TLS certificate was not accepted: " + strings.TrimSpace(err.Error())
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

// unreachableTransport turns a transport failure into registryUnreachable
// for the registry the request was for. An interrupted request stays an
// interruption. It sends through the process's default transport as it is
// at the time of the request, so a transport installed at start-up (a
// hosted run's grant) is kept in the chain.
type unreachableTransport struct{}

func (unreachableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil || errors.Is(err, context.Canceled) {
		return resp, err
	}
	return nil, &registryUnreachable{registry: req.URL.Scheme + "://" + req.URL.Host, cause: transportCause(err), err: err}
}

// describeError is what a failed command prints: the line for a person and
// the detail --verbose adds. An error with no detailed form prints as it is.
func describeError(err error) (line, detail string) {
	var d detailedError
	if errors.As(err, &d) {
		return d.Error(), d.Detail()
	}
	return err.Error(), ""
}

// printCommandError writes a command's failure on w: the line, and the
// detail under --verbose.
func printCommandError(w io.Writer, err error) {
	line, detail := describeError(err)
	_, _ = fmt.Fprintln(w, "agentry: "+line)
	if flags.verbose && detail != "" && detail != line {
		_, _ = fmt.Fprintln(w, "  detail: "+detail)
	}
}
