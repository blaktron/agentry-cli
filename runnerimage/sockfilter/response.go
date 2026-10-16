package sockfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// modifyResponse keeps lists to the run's objects and learns the execs
// the run creates.
func (f *Filter) modifyResponse(res *http.Response) error {
	p := canonical(res.Request.URL.Path)
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return nil
	}
	switch {
	case p == "/containers/create":
		return f.verifyCreated(res)
	case p == "/containers/json":
		return rewriteBody(res, func(body []byte) []byte {
			return f.keepItems(body, func(item map[string]any) bool { return f.oursAny(item["Labels"]) })
		})
	case p == "/networks":
		return rewriteBody(res, func(body []byte) []byte {
			kept := f.keepItems(body, func(item map[string]any) bool {
				name, _ := item["Name"].(string)
				return f.oursAny(item["Labels"]) || f.shared(name)
			})
			if len(f.cfg.NetworkNames) == 0 {
				return kept
			}
			var items []map[string]any
			if json.Unmarshal(kept, &items) != nil {
				return kept
			}
			for _, it := range items {
				if name, ok := it["Name"].(string); ok {
					it["Name"] = f.gatewayNetwork(name)
				}
			}
			out, _ := json.Marshal(items)
			return out
		})
	case p == "/volumes":
		return rewriteBody(res, f.keepVolumes)
	case len(f.cfg.NetworkNames) > 0 && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json") && p != "/containers/json":
		// A container's networks under the names the gateway knows them by:
		// it reads a relay's address on its relay network by that name.
		return rewriteBody(res, f.containerNetworksForGateway)
	case len(f.cfg.NetworkNames) > 0 && strings.HasPrefix(p, "/networks/"):
		return rewriteBody(res, func(body []byte) []byte {
			var doc map[string]any
			if json.Unmarshal(body, &doc) != nil {
				return body
			}
			if name, ok := doc["Name"].(string); ok {
				doc["Name"] = f.gatewayNetwork(name)
			}
			out, _ := json.Marshal(doc)
			return out
		})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/exec"):
		return rewriteBody(res, func(body []byte) []byte {
			var e struct {
				ID string `json:"Id"`
			}
			if json.Unmarshal(body, &e) == nil && e.ID != "" {
				f.mu.Lock()
				f.execs[e.ID] = true
				f.mu.Unlock()
			}
			return body
		})
	}
	return nil
}

// containerNetworksForGateway renames a container inspect's networks, and
// its network mode, to the gateway's names.
func (f *Filter) containerNetworksForGateway(body []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	if ns, ok := doc["NetworkSettings"].(map[string]any); ok {
		if nets, ok := ns["Networks"].(map[string]any); ok {
			renamed := map[string]any{}
			for name, v := range nets {
				renamed[f.gatewayNetwork(name)] = v
			}
			ns["Networks"] = renamed
		}
	}
	if hc, ok := doc["HostConfig"].(map[string]any); ok {
		if mode, ok := hc["NetworkMode"].(string); ok {
			hc["NetworkMode"] = f.gatewayNetwork(mode)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// oursAny reads a decoded labels object.
func (f *Filter) oursAny(labels any) bool {
	m, _ := labels.(map[string]any)
	v, _ := m[f.cfg.OwnerLabel].(string)
	return v == f.cfg.RunID
}

// keepItems keeps the items of a JSON array that keep says to; anything
// unreadable becomes an empty list, never the daemon's whole one.
func (f *Filter) keepItems(body []byte, keep func(map[string]any) bool) []byte {
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		return []byte("[]")
	}
	kept := []map[string]any{}
	for _, it := range items {
		if keep(it) {
			kept = append(kept, it)
		}
	}
	out, _ := json.Marshal(kept)
	return out
}

func (f *Filter) keepVolumes(body []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return []byte(`{"Volumes":[],"Warnings":null}`)
	}
	vols, _ := doc["Volumes"].([]any)
	kept := []any{}
	for _, v := range vols {
		m, _ := v.(map[string]any)
		if f.oursAny(m["Labels"]) {
			kept = append(kept, v)
		}
	}
	doc["Volumes"] = kept
	out, _ := json.Marshal(doc)
	return out
}

// rewriteBody reads a response body (bounded) and replaces it with edit's.
func rewriteBody(res *http.Response, edit func([]byte) []byte) error {
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	_ = res.Body.Close()
	if err != nil {
		return err
	}
	out := edit(body)
	res.Body = io.NopCloser(bytes.NewReader(out))
	res.ContentLength = int64(len(out))
	res.Header.Set("Content-Length", fmt.Sprint(len(out)))
	res.Header.Del("Transfer-Encoding")
	return nil
}

// verifyCreated reads a created container's runtime back from the engine
// (Config.Verify) when the create had to put it under one. A mismatch is
// kept (RuntimeMismatch), counted and logged as a refusal, and returned,
// so the gateway's create fails instead of answering with a container the
// receipt would misname.
func (f *Filter) verifyCreated(res *http.Response) error {
	m, _ := res.Request.Context().Value(createMetaKey{}).(*createMeta)
	if m == nil || m.runtime == "" {
		return nil
	}
	var id string
	err := rewriteBody(res, func(body []byte) []byte {
		var c struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(body, &c) == nil {
			id = c.ID
		}
		return body
	})
	if err != nil || id == "" {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verify := f.cfg.Verify
	if verify == nil {
		verify = f.verifyUpstream
	}
	if verr := verify(ctx, id, m.class, m.runtime); verr != nil {
		rr := &runtimeRefusal{err: verr}
		f.mismatch.CompareAndSwap(nil, rr)
		f.refused.Add(1)
		f.logf("refuse POST %s: %s", canonical(res.Request.URL.Path), verr.Error())
		return rr
	}
	f.logf("verified %s %s under %s", m.class, short(id), m.runtime)
	return nil
}

// verifyUpstream is the check where no engine CLI is at hand (the filter
// container beside a daemon in a VM): the daemon's own inspect, which
// Docker Engine answers with the runtime it applied. A mismatched
// container is removed through the daemon before the gateway hears of
// it. podman's Docker-compatible API answers "oci" for every container,
// so on podman only a native Verify can judge, and a run under a runtime
// on podman is refused wherever the CLI does not serve the filter.
func (f *Filter) verifyUpstream(ctx context.Context, id, class, want string) error {
	var c struct {
		HostConfig struct {
			Runtime string `json:"Runtime"`
		} `json:"HostConfig"`
	}
	code, err := f.inspect(ctx, "/containers/"+id+"/json", &c)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("read the runtime the %s the gateway launched (%s) was created with: daemon answered %d: %v", class, short(id), code, err)
	}
	if got := c.HostConfig.Runtime; got != want {
		f.removeUpstream(ctx, id)
		return fmt.Errorf("the %s the gateway launched (%s) was created under %q, not the policy's docker.runtime %q: the engine did not apply the runtime it was asked for, so the run stops rather than carry a receipt naming a sandbox it did not have", class, short(id), got, want)
	}
	return nil
}

// removeUpstream force-removes a container, with its anonymous volumes,
// through the daemon.
func (f *Filter) removeUpstream(ctx context.Context, id string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://docker/containers/"+id+"?force=1&v=1", nil)
	if err != nil {
		return
	}
	if res, err := f.upstream.Do(req); err == nil {
		_ = res.Body.Close()
	}
}
