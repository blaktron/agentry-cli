package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// The Docker daemon's configuration is edited on the device, as JSON, and
// the whole file is staged back: a server need have no JSON tool, and every
// other setting the person made is kept as it was.

// runtimes are gVisor's two registrations, as setup makes them.
func runtimes() map[string]any {
	return map[string]any{
		runhost.RunscRuntime:    map[string]any{"path": runhost.RunscPath},
		runhost.RunscUDSRuntime: map[string]any{"path": runhost.RunscPath, "runtimeArgs": []any{dockerx.HostUDSOpen}},
	}
}

func parseDaemonJSON(existing []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("/etc/docker/daemon.json is not readable JSON: %w", err)
	}
	return doc, nil
}

func renderDaemonJSON(doc map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// MergeRuntimes adds gVisor's two runtimes to the daemon's configuration.
// changed is false when they are already there as setup makes them, so the
// daemon is not reloaded for nothing. A runtime of either name that the
// person registered otherwise is refused, rather than taken over.
func MergeRuntimes(existing []byte) (out []byte, changed bool, err error) {
	doc, err := parseDaemonJSON(existing)
	if err != nil {
		return nil, false, err
	}
	have := map[string]any{}
	if rt, ok := doc["runtimes"]; ok {
		m, ok := rt.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("/etc/docker/daemon.json: runtimes is not an object")
		}
		have = m
	}
	for name, want := range runtimes() {
		cur, ok := have[name]
		if !ok {
			have[name] = want
			changed = true
			continue
		}
		if !reflect.DeepEqual(normalise(cur), normalise(want)) {
			return nil, false, fmt.Errorf("the Docker daemon already registers a runtime named %s (%s); setup registers its own pinned gVisor under that name, so remove that registration from /etc/docker/daemon.json first", name, compact(cur))
		}
	}
	if !changed {
		return existing, false, nil
	}
	doc["runtimes"] = have
	out, err = renderDaemonJSON(doc)
	return out, err == nil, err
}

// RemoveRuntimes takes gVisor's two runtimes back out, when they are the
// ones setup made. changed is false when there is nothing to take out.
func RemoveRuntimes(existing []byte) (out []byte, changed bool, err error) {
	doc, err := parseDaemonJSON(existing)
	if err != nil {
		return nil, false, err
	}
	have, _ := doc["runtimes"].(map[string]any)
	for name, want := range runtimes() {
		if cur, ok := have[name]; ok && reflect.DeepEqual(normalise(cur), normalise(want)) {
			delete(have, name)
			changed = true
		}
	}
	if !changed {
		return existing, false, nil
	}
	if len(have) == 0 {
		delete(doc, "runtimes")
	}
	out, err = renderDaemonJSON(doc)
	return out, err == nil, err
}

// normalise round-trips a value through JSON, so a value built here and one
// read from the file compare equal when they say the same thing.
func normalise(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
