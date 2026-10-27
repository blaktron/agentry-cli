package browsersignin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// EnvBrowser names a Chromium-family browser's program to use instead of
// the ones Find looks for.
const EnvBrowser = "AGENTRY_BROWSER"

// ErrNoBrowser says no Chromium-family browser was found.
var ErrNoBrowser = errors.New("no Chrome, Edge, Chromium or Brave was found on this computer: install one, or set " + EnvBrowser + " to its program")

// Find is the program of a Chromium-family browser on this computer: the
// one EnvBrowser names, else the first of Chrome, Edge, Chromium and Brave
// found where each installs itself.
func Find() (string, error) {
	if p := os.Getenv(EnvBrowser); p != "" {
		if _, err := os.Stat(p); err != nil {
			if lp, lerr := exec.LookPath(p); lerr == nil {
				return lp, nil
			}
			return "", fmt.Errorf("%s names %s, which was not found", EnvBrowser, p)
		}
		return p, nil
	}
	for _, c := range candidates(runtime.GOOS) {
		if filepath.IsAbs(c) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", ErrNoBrowser
}

// candidates are where each browser installs itself, in the order tried.
func candidates(goos string) []string {
	switch goos {
	case "darwin":
		var out []string
		for _, app := range []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Chromium.app/Contents/MacOS/Chromium",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
		} {
			out = append(out, filepath.Join("/Applications", app))
			if home, err := os.UserHomeDir(); err == nil {
				out = append(out, filepath.Join(home, "Applications", app))
			}
		}
		return out
	case "windows":
		var out []string
		for _, rel := range []string{
			`Google\Chrome\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`Chromium\Application\chrome.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
		} {
			for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
				if base := os.Getenv(env); base != "" {
					out = append(out, filepath.Join(base, rel))
				}
			}
		}
		return out
	default:
		return []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "microsoft-edge-stable", "chromium", "chromium-browser", "brave-browser"}
	}
}
