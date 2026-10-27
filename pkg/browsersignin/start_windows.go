//go:build windows

package browsersignin

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// start runs the browser with its DevTools pipe on two inherited handles,
// named on its command line by --remote-debugging-io-pipes (read, then
// write), which is how Chromium takes the pipe on Windows.
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
	closeAll := func() {
		for _, f := range []*os.File{toBrowserR, toBrowserW, fromBrowserR, fromBrowserW} {
			_ = f.Close()
		}
	}
	rh, wh := syscall.Handle(toBrowserR.Fd()), syscall.Handle(fromBrowserW.Fd())
	for _, h := range []syscall.Handle{rh, wh} {
		if err := syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
			closeAll()
			return nil, fmt.Errorf("pass the browser its pipe: %w", err)
		}
	}
	pipes := fmt.Sprintf("--remote-debugging-io-pipes=%d,%d", uintptr(rh), uintptr(wh))
	cmd := exec.Command(program, append([]string{"--remote-debugging-pipe", pipes}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{rh, wh}}
	cmd.Env = browserEnv()
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start the browser %s: %w", program, err)
	}
	_ = toBrowserR.Close()
	_ = fromBrowserW.Close()
	b := &browser{cmd: cmd, in: toBrowserW}
	b.begin(fromBrowserR)
	return b, nil
}
