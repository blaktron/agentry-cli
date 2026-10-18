package send

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// KindS3 is a destination that is a bucket of an S3 API: Amazon's, or any other
// service that speaks it (Cloudflare R2, MinIO, Backblaze B2, Wasabi).
const KindS3 = "s3"

// DefaultMaxObject is the most one object is, in bytes: the largest file a run
// can write (an exported PDF), so a deliverable the run made is never left out
// for its size alone.
const DefaultMaxObject int64 = 64 << 20

// The bounds on what a person types for a bucket, and on the key built from it.
const (
	maxPrefixBytes  = 200
	maxKeyBytes     = 1024
	maxSegmentBytes = 200
	maxRunIDBytes   = 64
)

var (
	bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)
	runIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// accessKeyRE is an access key id as services write them: no slash, which
	// the signature's scope reads as a separator, and no space.
	accessKeyRE = regexp.MustCompile(`^[A-Za-z0-9._@+:-]{1,128}$`)
)

// S3Spec is a bucket destination's non-secret fields.
type S3Spec struct {
	// Bucket and Prefix name where objects go: <Bucket>/<Prefix>/<run>/<file>.
	// The prefix may be empty.
	Bucket string
	Prefix string
	// Region is the signing region ("us-east-1", or "auto" for R2).
	Region string
	// Endpoint is the service's address when it is not Amazon's; empty is
	// Amazon S3 in Region. OnMyNetwork lets an endpoint be on the person's own
	// network, and http.
	Endpoint    string
	OnMyNetwork bool
}

// S3Target is a bucket destination checked: the address requests go to and
// what the proxy in front of them must permit.
type S3Target struct {
	S3Spec
	// Scheme is https, or http for an endpoint on the person's own network.
	Scheme string
	// Host is the Host header (and the signed host), HostPort the pair a proxy's
	// allowlist writes.
	Host     string
	HostPort string
	// PathStyle says the bucket is the first path segment, which every
	// endpoint a person names takes; Amazon's own is addressed by host name.
	PathStyle bool
	// Plain says the address is http.
	Plain bool
}

// ValidAccessKeyID says whether s can be an access key id.
func ValidAccessKeyID(s string) bool { return accessKeyRE.MatchString(s) }

// ValidRunID says whether s can be the run segment of an object key: the
// runner's own id, never anything that reads as a path.
func ValidRunID(s string) bool { return len(s) <= maxRunIDBytes && runIDRE.MatchString(s) }

// CheckPrefix holds a key prefix to what is safe to write on any S3 service:
// plain segments of letters, digits, '.', '_' and '-', separated by one slash,
// none empty, '.' or '..', and no slash at either end. An empty prefix is no
// prefix.
func CheckPrefix(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > maxPrefixBytes {
		return fmt.Errorf("the prefix is longer than %d bytes", maxPrefixBytes)
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return errors.New("the prefix holds an empty segment, '.' or '..': write it as folder/subfolder with no slash at either end")
		case strings.Trim(seg, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") != "":
			return errors.New("the prefix may hold letters, digits, '.', '_' and '-' only")
		}
	}
	return nil
}

// CheckS3 holds a bucket destination to its rules and answers where requests
// for it go. The bucket is a DNS-style name; the region and prefix are plain;
// Amazon's own address is made from the bucket and region, never typed, and an
// endpoint takes the webhook's address rule (CheckURL) with no path or query,
// so it is judged as a webhook's address is: https on port 443, at a public
// address, unless it is on the person's own network.
func CheckS3(s S3Spec) (S3Target, error) {
	var t S3Target
	switch {
	case !bucketRE.MatchString(s.Bucket) || strings.Contains(s.Bucket, ".."):
		return t, errors.New("the bucket must be 3 to 63 lower-case letters, digits, '.' and '-', beginning and ending with a letter or digit")
	case canonicalIPv4(strings.Split(s.Bucket, ".")):
		return t, errors.New("the bucket is written like an IP address, which no service takes as a bucket name")
	case !regionRE.MatchString(s.Region):
		return t, errors.New("the region must be lower-case letters, digits and '-' (us-east-1, or auto for R2)")
	}
	if err := CheckPrefix(s.Prefix); err != nil {
		return t, err
	}
	t.S3Spec = s
	if s.Endpoint == "" {
		if s.OnMyNetwork {
			return t, errors.New("a bucket is on your own network only through an endpoint: give --endpoint")
		}
		if strings.Contains(s.Bucket, ".") {
			return t, errors.New("a bucket with a '.' in its name cannot be reached at Amazon's own address over https: give the service's --endpoint")
		}
		t.Scheme, t.Host = "https", s.Bucket+".s3."+s.Region+".amazonaws.com"
		t.HostPort = t.Host + ":443"
		return t, nil
	}
	e, err := CheckURL(s.Endpoint, s.OnMyNetwork)
	if err != nil {
		return t, err
	}
	if (e.URL.Path != "" && e.URL.Path != "/") || e.URL.RawQuery != "" || e.URL.ForceQuery || e.URL.RawPath != "" {
		return t, errors.New("the endpoint is a host (https://host or https://host:port), with no path or query")
	}
	t.Scheme, t.PathStyle, t.Plain = e.URL.Scheme, true, e.Plain
	t.Host = e.URL.Host
	t.HostPort = e.HostPort
	return t, nil
}

// SameService says two checked destinations are the same service for the
// purpose of keeping a credential saved for the first: both Amazon's (an access
// key reaches every bucket and region of its account), or both the same
// endpoint, scheme and rule.
func SameService(a, b S3Target) bool {
	if !a.PathStyle || !b.PathStyle {
		return !a.PathStyle && !b.PathStyle
	}
	return a.Scheme == b.Scheme && a.HostPort == b.HostPort && a.OnMyNetwork == b.OnMyNetwork
}

// ObjectKeys makes the key of each deliverable path, <prefix>/<run id>/<path
// below reports/>, or "" for one it will not name. The agent chose the file's
// name, so each segment is made plain: a character other than a letter, digit,
// mark, '.', '_', '-', '(', ')' or a space becomes '_' (a separator, control,
// format and bidirectional character among them), leading and trailing spaces
// and dots go, and a segment with nothing left is "deliverable". The key can
// therefore never leave the prefix and run folder, whatever the path says. Two
// paths that become one key keep both, the later one as "name (2).ext". A path
// that is not beneath reports/, or a key past 1,024 bytes, is "". The run id
// must be ValidRunID and the prefix CheckPrefix, or every key is "".
func ObjectKeys(prefix, runID string, paths []string) []string {
	keys := make([]string, len(paths))
	if !ValidRunID(runID) || CheckPrefix(prefix) != nil {
		return keys
	}
	head := runID + "/"
	if prefix != "" {
		head = prefix + "/" + head
	}
	taken := map[string]bool{}
	for i, p := range paths {
		if !plainReportPath(p) {
			continue
		}
		segs := strings.Split(strings.TrimPrefix(p, "reports/"), "/")
		for j := range segs {
			segs[j] = plainSegment(segs[j])
		}
		key := head + strings.Join(segs, "/")
		for n := 2; taken[key] && n < 1000; n++ {
			last := segs[len(segs)-1]
			ext := path.Ext(last)
			alt := append(append([]string{}, segs[:len(segs)-1]...), fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(last, ext), n, ext))
			key = head + strings.Join(alt, "/")
		}
		if len(key) > maxKeyBytes || taken[key] {
			continue
		}
		taken[key] = true
		keys[i] = key
	}
	return keys
}

// plainSegment makes one path segment of a key from a name the agent chose.
func plainSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '.' || r == '_' || r == '-' || r == '(' || r == ')':
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), " .")
	if name == "" {
		return "deliverable"
	}
	if len(name) > maxSegmentBytes {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:maxSegmentBytes-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimRight(stem, " .") + ext
	}
	return name
}

// escapeKey is the key as a request path: every byte but an unreserved
// character and the slash percent-encoded, once, which is how S3 canonicalizes a
// path.
func escapeKey(key string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// objectURL is the address one key is put to, with the request path as it is
// sent and signed.
func (t S3Target) objectURL(key string) (*url.URL, string) {
	p := "/" + key
	if t.PathStyle {
		p = "/" + t.Bucket + p
	}
	esc := escapeKey(p)
	u := &url.URL{Scheme: t.Scheme, Host: t.Host}
	u.Path, _ = url.PathUnescape(esc)
	u.RawPath = esc
	return u, esc
}
