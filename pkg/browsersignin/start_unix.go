//go:build !windows

package browsersignin

import (
	"fmt"
	"os"
	"os/exec"
)

// start runs the browser with its DevTools pipe on file descriptors 3 (it
// reads) and 4 (it writes), which --remote-debugging-pipe uses.
func start(program string, args []string) (*browser, error) {
	toBrowserR, toBrowserW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromBrowserR, fromBrowserW, err := os.Pipe()
	if err != nil {
		_ = toBrowserR.Close()
		_ = toBrowserW.Close()
		return nil, err
	}
	cmd := exec.Command(program, append([]string{"--remote-debugging-pipe"}, args...)...)
	cmd.ExtraFiles = []*os.File{toBrowserR, fromBrowserW}
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{toBrowserR, toBrowserW, fromBrowserR, fromBrowserW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("start the browser %s: %w", program, err)
	}
	// The browser holds its own ends now.
	_ = toBrowserR.Close()
	_ = fromBrowserW.Close()
	b := &browser{cmd: cmd, in: toBrowserW}
	b.begin(fromBrowserR)
	return b, nil
}
