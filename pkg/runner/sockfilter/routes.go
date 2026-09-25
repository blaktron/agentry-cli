package sockfilter

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// Routes. An object id or name is one path segment; an image name may
// hold slashes, so image routes match on their suffix.
var (
	reContainer    = regexp.MustCompile(`^/containers/([^/]+)(/[a-z/]+)?$`)
	reExec         = regexp.MustCompile(`^/exec/([^/]+)/(start|resize|json)$`)
	reNetwork      = regexp.MustCompile(`^/networks/([^/]+)(/(connect|disconnect))?$`)
	reVolume       = regexp.MustCompile(`^/volumes/([^/]+)$`)
	reImageInspect = regexp.MustCompile(`^/images/.+/json$`)
	reDistribution = regexp.MustCompile(`^/distribution/.+/json$`)
)

// containerOps are the verbs on one of the run's containers the gateway
// may use. update, rename, commit and export are not among them: none
// launches or stops a server, and update could lift the run's limits.
var containerOps = map[string]bool{
	"": true, "/json": true, "/start": true, "/stop": true, "/kill": true,
	"/wait": true, "/restart": true, "/logs": true, "/attach": true,
	"/attach/ws": true, "/resize": true, "/top": true, "/stats": true,
	"/archive": true, "/changes": true, "/pause": true, "/unpause": true,
	"/exec": true,
}

// relayForbidden are the verbs that would put code or files into a relay,
// the one kind of container with a way off the host.
var relayForbidden = map[string]bool{"/exec": true, "/attach": true, "/attach/ws": true}

// imagePullKeys are the query keys a pull may carry: a reference to pull
// from a registry. fromSrc (an import from a URL or the request body),
// repo, message and changes would make an image here with a name of the
// caller's choosing.
var imagePullKeys = map[string]bool{"fromImage": true, "tag": true, "platform": true}

// decide judges a request and returns the path to forward it under: the
// one judged, except that a container named by its name is forwarded by
// the full id the check resolved, so it cannot be swapped between the two.
func (f *Filter) decide(r *http.Request, p string) (string, error) {
	fwd, err := f.judge(r, p)
	if fwd == "" {
		fwd = p
	}
	return fwd, err
}

func (f *Filter) judge(r *http.Request, p string) (string, error) {
	ctx := r.Context()
	switch {
	case p == "/_ping" || p == "/version" || p == "/info":
		return "", readOnly(r)
	case p == "/images/json" || reImageInspect.MatchString(p) || reDistribution.MatchString(p):
		return "", readOnly(r)
	case p == "/images/create":
		if err := method(r, http.MethodPost); err != nil {
			return "", err
		}
		for k := range r.URL.Query() {
			if !imagePullKeys[k] {
				return "", deny("an image may only be pulled by reference, not made with %q", k)
			}
		}
		if r.URL.Query().Get("fromImage") == "" {
			return "", deny("an image pull names no image")
		}
		return "", nil
	case p == "/containers/create":
		if err := method(r, http.MethodPost); err != nil {
			return "", err
		}
		return "", f.rewrite(r, func(body []byte) (any, error) { return f.containerCreate(ctx, body) })
	case p == "/containers/json" || p == "/networks" || p == "/volumes":
		return "", readOnly(r)
	case p == "/networks/create":
		if err := method(r, http.MethodPost); err != nil {
			return "", err
		}
		return "", f.rewrite(r, func(body []byte) (any, error) { return f.networkCreate(ctx, body) })
	case p == "/volumes/create":
		if err := method(r, http.MethodPost); err != nil {
			return "", err
		}
		return "", f.rewrite(r, f.volumeCreate)
	case p == "/events":
		if err := readOnly(r); err != nil {
			return "", err
		}
		f.scopeEvents(r)
		return "", nil
	}
	if m := reContainer.FindStringSubmatch(p); m != nil && m[1] != "create" && m[1] != "json" && m[1] != "prune" {
		return f.containerOp(r, m[1], m[2])
	}
	if m := reExec.FindStringSubmatch(p); m != nil {
		f.mu.Lock()
		ok := f.execs[m[1]]
		f.mu.Unlock()
		if !ok {
			return "", deny("exec %s was not created through this run's filter", short(m[1]))
		}
		return "", nil
	}
	if m := reNetwork.FindStringSubmatch(p); m != nil && m[1] != "create" && m[1] != "prune" {
		m[1] = f.runNetwork(m[1])
		switch {
		case m[3] != "": // connect, disconnect
			if err := method(r, http.MethodPost); err != nil {
				return "", err
			}
			n, err := f.ownNetwork(ctx, m[1], true)
			if err != nil {
				return "", err
			}
			return "/networks/" + n.id + m[2], f.connect(r, n.name, m[3] == "connect")
		case r.Method == http.MethodDelete:
			n, err := f.ownNetwork(ctx, m[1], false)
			return "/networks/" + n.id, err
		default:
			if err := readOnly(r); err != nil {
				return "", err
			}
			n, err := f.ownNetwork(ctx, m[1], true)
			return "/networks/" + n.id, err
		}
	}
	if m := reVolume.FindStringSubmatch(p); m != nil && m[1] != "create" && m[1] != "prune" {
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			return "", deny("method %s on a volume", r.Method)
		}
		return "", f.ownVolume(ctx, m[1])
	}
	return "", deny("%s %s is not an operation a run's gateway needs", r.Method, p)
}

// containerOp decides a verb on one container.
func (f *Filter) containerOp(r *http.Request, id, op string) (string, error) {
	if !containerOps[op] {
		return "", deny("the container operation %q is not one a gateway needs", strings.TrimPrefix(op, "/"))
	}
	if op == "" && r.Method != http.MethodDelete {
		return "", deny("method %s on a container", r.Method)
	}
	c, err := f.ownContainer(r.Context(), id)
	if err != nil {
		return "", err
	}
	intoRelay := relayForbidden[op] || (op == "/archive" && r.Method == http.MethodPut)
	if intoRelay {
		relay, err := f.isRelay(r.Context(), c.imageID)
		if err != nil {
			return "", err
		}
		if relay {
			return "", deny("nothing may exec into, attach to or write into a relay (%s)", short(id))
		}
	}
	if op == "/exec" {
		if err := f.rewrite(r, f.execCreate); err != nil {
			return "", err
		}
	}
	return "/containers/" + c.id + op, nil
}

func readOnly(r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return deny("method %s is read-only here", r.Method)
	}
	return nil
}

func method(r *http.Request, want string) error {
	if r.Method != want {
		return deny("method %s, want %s", r.Method, want)
	}
	return nil
}

// scopeEvents adds the run's label to the event stream's filters.
func (f *Filter) scopeEvents(r *http.Request) {
	q := r.URL.Query()
	filters := map[string][]string{}
	if raw := q.Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &filters)
	}
	filters["label"] = append(filters["label"], f.cfg.OwnerLabel+"="+f.cfg.RunID)
	b, _ := json.Marshal(filters)
	q.Set("filters", string(b))
	r.URL.RawQuery = q.Encode()
}
