package browsersignin

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// downloadsDir is the profile's download folder, inside the profile so a
// file the address answers with is removed with it; openerPage is the page
// the window opens, which sends it on to the address.
const (
	downloadsDir = "downloads"
	openerPage   = "sign-in.html"
)

// signInHint is what the person is told while the window is open. On
// macOS a browser stays running when its last window closes, so it is
// quit instead.
var signInHint = func() string {
	end := "close it"
	if runtime.GOOS == "darwin" {
		end = "quit the browser (Command-Q)"
	}
	return "Sign in in the browser window that opened. It closes by itself once the calendar downloads; if it does not, " + end + " when you are signed in."
}()

// window is the browser the person signs in, which nothing controls.
type window struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// startWindow runs the browser with no DevTools pipe or port.
func startWindow(program string, args []string) (*window, error) {
	cmd := exec.Command(program, args...)
	cmd.Env = browserEnv()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the browser %s: %w", program, err)
	}
	w := &window{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(w.exited)
	}()
	return w, nil
}

// wait returns once the window has ended: closed by the person, or asked to
// quit once a download has landed in downloads. When ctx ends first the
// browser is killed.
func (w *window) wait(ctx context.Context, downloads string) error {
	tick := time.NewTicker(PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			w.kill()
			return fmt.Errorf("the sign-in was not done in time: %w", ctx.Err())
		case <-w.exited:
			return nil
		case <-tick.C:
		}
		if !downloaded(downloads) {
			continue
		}
		// Asked to quit, the browser writes its cookies first; killed, it
		// may not, which the read that follows finds.
		_ = quit(w.cmd)
		select {
		case <-w.exited:
		case <-time.After(QuitWait):
			w.kill()
		case <-ctx.Done():
			w.kill()
			return fmt.Errorf("the sign-in was not done in time: %w", ctx.Err())
		}
		return nil
	}
}

func (w *window) kill() {
	_ = w.cmd.Process.Kill()
	<-w.exited
}

// downloaded says a download has finished in dir: a file there that is not
// one the browser is still writing.
func downloaded(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasSuffix(e.Name(), ".crdownload") {
			return true
		}
	}
	return false
}

// prepareProfile writes what the window starts with into the profile: its
// preferences, which put downloads in the profile's own folder without
// asking, and the page that sends the window on to address, whose path it
// returns. The profile folder is the person's alone (os.MkdirTemp makes it
// 0700), so the address is kept from other users as a command line is not.
func prepareProfile(profile, address string) (string, error) {
	downloads := filepath.Join(profile, downloadsDir)
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		return "", fmt.Errorf("make the browser's profile: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(profile, "Default"), 0o700); err != nil {
		return "", fmt.Errorf("make the browser's profile: %w", err)
	}
	prefs, err := json.Marshal(map[string]any{
		"download": map[string]any{"default_directory": downloads, "prompt_for_download": false, "directory_upgrade": true},
		"savefile": map[string]any{"default_directory": downloads},
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(profile, "Default", "Preferences"), prefs, 0o600); err != nil {
		return "", fmt.Errorf("make the browser's profile: %w", err)
	}
	// json.Marshal escapes <, > and &, so the address cannot end the script.
	target, err := json.Marshal(address)
	if err != nil {
		return "", err
	}
	page := "<!doctype html><meta charset=\"utf-8\"><title>Sign in</title>" +
		"<script>location.replace(" + string(target) + ")</script>" +
		"<p><a href=\"" + html.EscapeString(address) + "\">Continue to the sign-in</a></p>\n"
	opener := filepath.Join(profile, openerPage)
	if err := os.WriteFile(opener, []byte(page), 0o600); err != nil {
		return "", fmt.Errorf("make the browser's profile: %w", err)
	}
	return opener, nil
}
