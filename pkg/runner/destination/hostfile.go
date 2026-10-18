package destination

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"

	"github.com/blaktron/agentry-cli/runnerimage/send"
)

// MaxFileBytes bounds a destinations file.
const MaxFileBytes = 64 << 10

// envNameRE is a variable a destinations file may name for a credential.
var envNameRE = regexp.MustCompile(`^AGENTRY_DEST_[A-Z0-9_]{1,100}$`)

// fileEntry is one destination as a destinations file writes it: the
// non-secret fields, and the names of the variables that hold its credentials.
// A webhook has a url, a signing secret in secretEnv and optionally an
// Authorization value; a bucket has bucket, prefix, region and endpoint, its
// secret access key in secretEnv and its access key id in accessKeyIdEnv.
// There is no field for On my network: a host reaches public addresses alone.
type fileEntry struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	URL              string `json:"url,omitempty"`
	Bucket           string `json:"bucket,omitempty"`
	Prefix           string `json:"prefix,omitempty"`
	Region           string `json:"region,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	SecretEnv        string `json:"secretEnv"`
	AccessKeyIDEnv   string `json:"accessKeyIdEnv,omitempty"`
	AuthorizationEnv string `json:"authorizationEnv,omitempty"`
}

type fileDoc struct {
	Destinations []fileEntry `json:"destinations"`
}

// FromFile reads the destinations a driver such as a sandbox host hands a run:
// a JSON document of non-secret fields (name, kind, and a webhook's url or a
// bucket's bucket, prefix, region and endpoint) with, for each, the name of the
// environment variable holding its signing secret (a bucket's secret access
// key) and, when the receiver wants one, the one holding a webhook's
// Authorization value (a bucket's access key id). It answers the
// destinations with their credentials read through getenv, and the names of
// the variables it read, which the caller removes from its own environment so
// that no process it starts inherits them.
//
// Every address must pass the rule of a destination that is not on the
// person's network, so a delivery reaches public addresses on port 443
// alone. A missing, empty or malformed value, a name that is not a valid
// destination name, a kind this CLI does not send to, an unknown field or the
// same name twice refuses the file as a whole; a delivery is never unsigned
// and never silently goes nowhere. No error carries a credential's value.
func FromFile(path string, getenv func(string) string) (dests []Resolved, envNames []string, err error) {
	// A file that is not a regular one is refused before it is opened: opening
	// a FIFO with no writer would wait for ever.
	if pi, perr := os.Stat(path); perr == nil && !pi.Mode().IsRegular() {
		return nil, nil, errors.New("the destinations file is not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // a path the driver named
	if err != nil {
		return nil, nil, fmt.Errorf("read the destinations file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("read the destinations file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, errors.New("the destinations file is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read the destinations file: %w", err)
	}
	if len(raw) > MaxFileBytes {
		return nil, nil, fmt.Errorf("the destinations file is more than %d bytes", MaxFileBytes)
	}
	var doc fileDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, errors.New("the destinations file is not a document of the shape this CLI reads")
	}
	if _, terr := dec.Token(); terr != io.EOF {
		return nil, nil, errors.New("the destinations file holds more than one document")
	}
	if len(doc.Destinations) == 0 {
		return nil, nil, errors.New("the destinations file names no destination")
	}
	if len(doc.Destinations) > MaxPerRun {
		return nil, nil, fmt.Errorf("a run delivers to at most %d destinations", MaxPerRun)
	}
	seen := map[string]bool{}
	for _, e := range doc.Destinations {
		d := Destination{Name: e.Name, Kind: e.Kind, URL: e.URL}
		switch e.Kind {
		case KindWebhook:
			if e.AccessKeyIDEnv != "" {
				return nil, nil, fmt.Errorf("destination %q: accessKeyIdEnv is for a bucket", e.Name)
			}
			if e.Bucket != "" || e.Prefix != "" || e.Region != "" || e.Endpoint != "" {
				return nil, nil, fmt.Errorf("destination %q: a webhook has an address and no bucket, prefix, region or endpoint", e.Name)
			}
		case KindS3:
			if e.URL != "" {
				return nil, nil, fmt.Errorf("destination %q: a bucket has an endpoint, not an address of its own", e.Name)
			}
			if e.AuthorizationEnv != "" {
				return nil, nil, fmt.Errorf("destination %q: a bucket is signed with its key pair, not an Authorization value", e.Name)
			}
			d = Destination{Name: e.Name, Kind: KindS3, Bucket: e.Bucket, Prefix: e.Prefix, Region: e.Region, Endpoint: e.Endpoint}
			if d.Region == "" && d.Endpoint != "" {
				d.Region = "us-east-1"
			}
		default:
			return nil, nil, fmt.Errorf("destination %q: a host hands a run webhooks and buckets only", e.Name)
		}
		if err := d.Check(); err != nil {
			return nil, nil, fmt.Errorf("destination %q: %w", e.Name, err)
		}
		if seen[d.Name] {
			return nil, nil, fmt.Errorf("destination %q is listed twice", d.Name)
		}
		seen[d.Name] = true
		r := Resolved{Destination: d, Hosted: true}
		if !envNameRE.MatchString(e.SecretEnv) {
			return nil, nil, fmt.Errorf("destination %q: secretEnv names a variable that does not begin with AGENTRY_DEST_", d.Name)
		}
		if r.Secret = getenv(e.SecretEnv); !send.ValidSecret(r.Secret) {
			return nil, nil, fmt.Errorf("destination %q: the variable %s holds no signing secret, or one that is not a plain line", d.Name, e.SecretEnv)
		}
		envNames = append(envNames, e.SecretEnv)
		if d.Kind == KindS3 {
			if !envNameRE.MatchString(e.AccessKeyIDEnv) {
				return nil, nil, fmt.Errorf("destination %q: accessKeyIdEnv names a variable that does not begin with AGENTRY_DEST_", d.Name)
			}
			if r.AccessKeyID = getenv(e.AccessKeyIDEnv); !send.ValidAccessKeyID(r.AccessKeyID) {
				return nil, nil, fmt.Errorf("destination %q: the variable %s holds no access key id, or one a request cannot be signed with", d.Name, e.AccessKeyIDEnv)
			}
			envNames = append(envNames, e.AccessKeyIDEnv)
		}
		if e.AuthorizationEnv != "" {
			if !envNameRE.MatchString(e.AuthorizationEnv) {
				return nil, nil, fmt.Errorf("destination %q: authorizationEnv names a variable that does not begin with AGENTRY_DEST_", d.Name)
			}
			if r.Authorization = getenv(e.AuthorizationEnv); r.Authorization == "" || !send.ValidHeaderValue(r.Authorization) {
				return nil, nil, fmt.Errorf("destination %q: the variable %s holds no Authorization value, or one that is not a valid header value", d.Name, e.AuthorizationEnv)
			}
			envNames = append(envNames, e.AuthorizationEnv)
		}
		dests = append(dests, r)
	}
	slices.Sort(envNames)
	return dests, slices.Compact(envNames), nil
}
