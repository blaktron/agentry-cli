package messenger

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Without the relay, as add and test call a service from the CLI itself,
// the client dials public addresses alone: a homeserver's name that
// resolves into the server's own network is never sent the credential.
func TestADirectCallDialsPublicAddressesAlone(t *testing.T) {
	c, err := newHTTPClient("", callTimeout)
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"https://10.0.0.1/", "https://169.254.169.254/", "https://[fd00::1]/", "https://100.64.0.1/"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
		_, err := c.Do(req)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Errorf("%s: %v", addr, err)
		}
	}
}
