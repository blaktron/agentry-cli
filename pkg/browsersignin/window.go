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
// quit once a download has landed in downloads, which is removed at once.
// killed says a window asked to quit did not within QuitWait and was
// killed, so it may not have written its cookies. When ctx ends first the
// browser is killed.
func (w *window) wait(ctx context.Context, downloads string) (killed bool, err error) {
	tick := time.NewTicker(PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			w.kill()
			return false, notInTime(ctx)
		case <-w.exited:
			return false, nil
		case <-tick.C:
		}
		if !removeDownloads(downloads) {
			continue
		}
		// Asked to quit, the browser writes its cookies first.
		_ = quit(w.cmd)
		select {
		case <-w.exited:
			return false, nil
		case <-time.After(QuitWait):
			w.kill()
			return true, nil
		case <-ctx.Done():
			w.kill()
			return false, notInTime(ctx)
		}
	}
}

func (w *window) kill() {
	_ = w.cmd.Process.Kill()
	<-w.exited
}

// removeDownloads removes every finished download in dir, a file there
// that is not one the browser is still writing, so the calendar it holds
// is not left behind by a run that is killed, and says whether there was
// one.
func removeDownloads(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := false
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasSuffix(e.Name(), ".crdownload") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			found = true
		}
	}
	return found
}

// prepareProfile writes what the window starts with into the profile: its
// preferences, which put downloads in the profile's own folder without
// asking, and the page that sends the window on to address, whose path it
// returns. The profile folder is the person's alone (os.MkdirTemp makes it
// 0700 on Unix; on Windows it is in the person's own temporary folder), so
// other users cannot read the address there as they can a command line.
func prepareProfile(profile, address string) (string, error) {
	fail := func(err error) (string, error) { return "", fmt.Errorf("make the browser's profile: %w", err) }
	downloads := filepath.Join(profile, downloadsDir)
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Join(profile, "Default"), 0o700); err != nil {
		return fail(err)
	}
	prefs, err := json.Marshal(map[string]any{
		"download": map[string]any{"default_directory": downloads, "prompt_for_download": false, "directory_upgrade": true},
		"savefile": map[string]any{"default_directory": downloads},
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(profile, "Default", "Preferences"), prefs, 0o600); err != nil {
		return fail(err)
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
		return fail(err)
	}
	return opener, nil
}
