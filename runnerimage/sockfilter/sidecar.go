package sockfilter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Where the daemon runs in a VM (Docker Desktop on macOS and Windows), a
// host socket cannot be bind-mounted into its containers, so the filter
// runs in a container of its own beside the gateway: the runner image's
// docker-filter role, holding the daemon's socket and serving this same
// package's filter on a socket in a volume the gateway mounts in place of
// the daemon's (agentry-cli#322). One policy, wherever it runs.

// SidecarReady is the line the docker-filter role prints once its socket
// accepts connections; the engine starts the gateway only after it.
const SidecarReady = "docker-filter: ready"

// MarshalConfig is a filter's configuration as the docker-filter role
// reads it: every field but Listen, Upstream and Log, which belong to the
// container it runs in.
func MarshalConfig(c Config) ([]byte, error) {
	c.Listen, c.Upstream, c.Log = "", "", nil
	return json.Marshal(c)
}

// UnmarshalConfig reads MarshalConfig's form, refusing a field it does not
// know: a configuration from another version is not half-applied.
func UnmarshalConfig(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("docker filter configuration: %w", err)
	}
	return c, nil
}

// CountRefused counts the refusals in a filter's decision log (Filter.
// Refused, for a filter running in another process).
func CountRefused(log io.Reader) int {
	n := 0
	sc := bufio.NewScanner(log)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if _, rest, ok := strings.Cut(sc.Text(), " "); ok && strings.HasPrefix(rest, "refuse ") {
			n++
		}
	}
	return n
}
