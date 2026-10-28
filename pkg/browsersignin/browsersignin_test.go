package browsersignin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary is the browser too, run with fakeBrowserEnv set. Started
// without --remote-debugging-pipe it is the sign-in window, which acts as
// the variable's mode says; started with it, it is the headless read and
// speaks the DevTools pipe on descriptors 3 and 4, answering with the
// cookies the window left in its profile. Each start writes its command
// line to the file fakeArgsEnv names, the read's with ".read" appended.
const (
	fakeBrowserEnv = "BROWSERSIGNIN_FAKE_BROWSER"
	fakeArgsEnv    = "BROWSERSIGNIN_FAKE_ARGS"
	fakeCookies    = "fake-cookies.json"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeBrowserEnv); mode != "" {
		if slices.Contains(os.Args[1:], "--remote-debugging-pipe") {
			fakeRead(mode)
		} else {
			fakeWindow(mode)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// signedIn are the cookies a sign-in leaves: the account's, the calendar
// host's, and one kept only until the browser closes.
var signedIn = []Cookie{
	{Name: "SID", Value: "signed-in-0123456789", Domain: ".example.com", Path: "/", Secure: true},
	{Name: "OSID", Value: "calendar-0123456789", Domain: "calendar.example.com", Path: "/", Secure: true},
	{Name: "SESSION", Value: "until-closed-0123456789", Domain: "calendar.example.com", Path: "/feed", Secure: true},
}

func fakeArg(prefix string) string {
	for _, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

func writeFakeArgs(suffix string) {
	if f := os.Getenv(fakeArgsEnv); f != "" {
		_ = os.WriteFile(f+suffix, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		_ = os.WriteFile(f+suffix+".env", []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
}

// fakeWindow is the sign-in window. In mode "signs-in" the person signs in
// and the address's download lands, and it waits to be asked to quit; in
// "signs-in-shown" the address is shown, not downloaded, and the person
// closes the window; in "closes" the person closes it without signing in;
// in "waits" nothing happens until it is killed.
func fakeWindow(mode string) {
	writeFakeArgs("")
	profile := fakeArg("--user-data-dir=")
	opener := os.Args[len(os.Args)-1]
	page, _ := os.ReadFile(opener)
	if f := os.Getenv(fakeArgsEnv); f != "" {
		_ = os.WriteFile(f+".page", page, 0o600)
	}
	var prefs struct {
		Download struct {
			Dir    string `json:"default_directory"`
			Prompt bool   `json:"prompt_for_download"`
		} `json:"download"`
	}
	b, _ := os.ReadFile(filepath.Join(profile, "Default", "Preferences"))
	_ = json.Unmarshal(b, &prefs)
	signIn := func() {
		b, _ := json.Marshal(signedIn)
		_ = os.WriteFile(filepath.Join(profile, fakeCookies), b, 0o600)
	}
	switch mode {
	case "signs-in":
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM)
		signIn()
		_ = os.WriteFile(filepath.Join(prefs.Download.Dir, "Unconfirmed 1.crdownload"), []byte("BEGIN:VCAL"), 0o600)
		time.Sleep(50 * time.Millisecond)
		_ = os.Rename(filepath.Join(prefs.Download.Dir, "Unconfirmed 1.crdownload"), filepath.Join(prefs.Download.Dir, "basic.ics"))
		<-quit
		if f := os.Getenv(fakeArgsEnv); f != "" {
			_ = os.WriteFile(f+".quit", nil, 0o600)
		}
	case "signs-in-shown":
		signIn()
		time.Sleep(100 * time.Millisecond)
	case "closes":
	case "waits":
		select {}
	}
}

// fakeRead answers the commands the headless read sends; in mode
// "downloads" it refuses to deny downloads.
func fakeRead(mode string) {
	writeFakeArgs(".read")
	cookies := []Cookie{{Name: "stranger", Value: "x", Domain: "other.example", Path: "/"}}
	if b, err := os.ReadFile(filepath.Join(fakeArg("--user-data-dir="), fakeCookies)); err == nil {
		var kept []Cookie
		_ = json.Unmarshal(b, &kept)
		cookies = append(cookies, kept...)
	}
	in, out := os.NewFile(3, "in"), os.NewFile(4, "out")
	r := bufio.NewReader(in)
	for {
		msg, err := r.ReadBytes(0)
		if err != nil {
			return
		}
		var m struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(msg[:len(msg)-1], &m)
		result := any(map[string]any{})
		var fail string
		switch m.Method {
		case "Browser.setDownloadBehavior":
			if mode == "downloads" || m.Params["behavior"] != "deny" {
				fail = "refused"
			}
		case "Storage.getCookies":
			result = map[string]any{"cookies": cookies}
		case "Browser.close":
			b, _ := json.Marshal(map[string]any{"id": m.ID, "result": result})
			_, _ = out.Write(append(b, 0))
			return
		}
		var b []byte
		if fail != "" {
			b, _ = json.Marshal(map[string]any{"id": m.ID, "error": map[string]any{"message": fail}})
		} else {
			// An event first, which the read must pass over.
			ev, _ := json.Marshal(map[string]any{"method": "Target.targetInfoChanged", "params": map[string]any{}})
			_, _ = out.Write(append(ev, 0))
			b, _ = json.Marshal(map[string]any{"id": m.ID, "result": result})
		}
		_, _ = out.Write(append(b, 0))
	}
}

const fakeFeed = "https://calendar.example.com/feed/basic.ics?a=1&b=<x>"

func fakeOptions(t *testing.T, mode string, ready func(context.Context, string) (bool, error)) (Options, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser reads descriptors 3 and 4 and is asked to quit by a signal")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := t.TempDir() + "/args"
	t.Setenv(fakeBrowserEnv, mode)
	t.Setenv(fakeArgsEnv, args)
	old, oldQuit := PollEvery, QuitWait
	PollEvery, QuitWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { PollEvery, QuitWait = old, oldQuit })
	return Options{URL: fakeFeed, Browser: exe, Ready: ready}, args
}

func readArgs(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunReadsTheSignInOnceTheDownloadLands(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "model-key-0123456789")
	t.Setenv("XAUTHORITY", "/run/user/1000/xauth")
	var headers []string
	o, args := fakeOptions(t, "signs-in", func(_ context.Context, h string) (bool, error) {
		headers = append(headers, h)
		// A fetch that fails for a moment is tried again.
		return len(headers) > 1, nil
	})
	var told []string
	o.Progress = func(s string) { told = append(told, s) }
	got, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if h := Header(got); h != "SESSION=until-closed-0123456789; SID=signed-in-0123456789; OSID=calendar-0123456789" {
		t.Errorf("cookies %q", h)
	}
	if len(headers) != 2 || strings.Contains(headers[0], "stranger") {
		t.Errorf("Ready was asked %q", headers)
	}
	if _, err := os.Stat(args + ".quit"); err != nil {
		t.Error("the window was not asked to quit once the download landed")
	}
	if len(told) == 0 || !strings.Contains(told[0], "closes by itself") {
		t.Errorf("progress %q", told)
	}

	window, read := readArgs(t, args), readArgs(t, args+".read")
	for _, want := range []string{"--user-data-dir=", "--no-first-run", "--password-store=basic", "--new-window", openerPage} {
		if !strings.Contains(window, want) {
			t.Errorf("the window's command line lacks %s:\n%s", want, window)
		}
	}
	if strings.Contains(window, "--remote-debugging") || strings.Contains(window, "--headless") {
		t.Errorf("the window is controlled:\n%s", window)
	}
	for _, want := range []string{"--remote-debugging-pipe", "--headless=new", "--restore-last-session", "about:blank"} {
		if !strings.Contains(read, want) {
			t.Errorf("the read's command line lacks %s:\n%s", want, read)
		}
	}
	if strings.Contains(read, "--remote-debugging-port") {
		t.Errorf("the read listens on a port:\n%s", read)
	}
	if strings.Contains(window+read, "calendar.example.com") {
		t.Errorf("the address is on a command line:\n%s\n%s", window, read)
	}

	// The opener page sends the window on to the address, which cannot
	// end its script.
	page := readArgs(t, args+".page")
	target, _ := json.Marshal(fakeFeed)
	if !strings.Contains(page, "location.replace("+string(target)+")") || strings.Contains(page, "<x>") {
		t.Errorf("the opener page:\n%s", page)
	}

	for _, f := range []string{args + ".env", args + ".read.env"} {
		env := readArgs(t, f)
		if strings.Contains(env, "model-key-0123") || !strings.Contains(env, "XAUTHORITY=") {
			t.Errorf("the browser's environment:\n%s", env)
		}
	}
	// The profile, its download and the opener page are removed afterwards.
	for _, line := range strings.Split(window, "\n") {
		if dir, ok := strings.CutPrefix(line, "--user-data-dir="); ok {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the profile %s is left: %v", dir, err)
			}
		}
	}
}

func TestRunReadsTheSignInWhenThePersonClosesTheWindow(t *testing.T) {
	o, args := fakeOptions(t, "signs-in-shown", func(_ context.Context, h string) (bool, error) {
		return strings.Contains(h, "SID="), nil
	})
	got, err := Run(context.Background(), o)
	if err != nil || len(got) != 3 {
		t.Fatalf("Run = %+v, %v", got, err)
	}
	if _, err := os.Stat(args + ".quit"); err == nil {
		t.Error("a window with no download was asked to quit")
	}
}

func TestRunStopsWhenTheWindowClosesWithoutASignIn(t *testing.T) {
	asked := false
	o, _ := fakeOptions(t, "closes", func(context.Context, string) (bool, error) { asked = true; return true, nil })
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run = %v", err)
	}
	if asked {
		t.Error("Ready was asked with no cookie for the address")
	}
}

func TestRunStopsWhenTheSignInCannotReadTheAddress(t *testing.T) {
	asked := 0
	o, _ := fakeOptions(t, "signs-in-shown", func(context.Context, string) (bool, error) { asked++; return false, nil })
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run = %v", err)
	}
	if asked != ReadyTries {
		t.Errorf("Ready was asked %d times", asked)
	}
}

func TestRunStopsOnReadyError(t *testing.T) {
	stop := errors.New("this account cannot read the calendar")
	o, _ := fakeOptions(t, "signs-in", func(context.Context, string) (bool, error) { return false, stop })
	if _, err := Run(context.Background(), o); !errors.Is(err, stop) {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunNeedsDownloadsDeniedInTheRead(t *testing.T) {
	o, _ := fakeOptions(t, "downloads", func(context.Context, string) (bool, error) { return true, nil })
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunEndsWithTheContext(t *testing.T) {
	o, args := fakeOptions(t, "waits", func(context.Context, string) (bool, error) { return true, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, o); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if _, err := os.Stat(args + ".read"); err == nil {
		t.Error("the profile was read after the sign-in was given up")
	}
}

func TestRunOpensHTTPSAlone(t *testing.T) {
	for _, u := range []string{"http://calendar.example.com/x.ics", "webcal://calendar.example.com/x.ics", "https:///x.ics"} {
		if _, err := Run(context.Background(), Options{URL: u, Ready: func(context.Context, string) (bool, error) { return true, nil }}); err == nil {
			t.Errorf("%s was opened", u)
		}
	}
}

func TestMatch(t *testing.T) {
	u, _ := url.Parse("https://calendar.google.com/calendar/ical/a%40b.com/public/basic.ics")
	cookies := []Cookie{
		{Name: "SID", Value: "a", Domain: ".google.com", Path: "/"},
		{Name: "OSID", Value: "b", Domain: "calendar.google.com", Path: "/"},
		{Name: "deep", Value: "c", Domain: "calendar.google.com", Path: "/calendar"},
		{Name: "other-path", Value: "d", Domain: "calendar.google.com", Path: "/calendars"},
		{Name: "mail", Value: "e", Domain: "mail.google.com", Path: "/"},
		{Name: "host-only-parent", Value: "f", Domain: "google.com", Path: "/"},
		{Name: "suffix", Value: "g", Domain: ".gle.com", Path: "/"},
		{Name: "partitioned", Value: "h", Domain: ".google.com", Path: "/", PartitionKey: map[string]any{"topLevelSite": "https://x.example"}},
		{Name: "bad", Value: "semi;colon", Domain: ".google.com", Path: "/"},
		{Name: "", Value: "nameless", Domain: ".google.com", Path: "/"},
	}
	var names []string
	for _, c := range Match(cookies, u) {
		names = append(names, c.Name)
	}
	if want := []string{"deep", "SID", "OSID"}; !slices.Equal(names, want) {
		t.Errorf("matched %v, want %v", names, want)
	}
	plain, _ := url.Parse("http://calendar.google.com/x")
	if got := Match([]Cookie{{Name: "s", Value: "v", Domain: "calendar.google.com", Path: "/", Secure: true}}, plain); len(got) != 0 {
		t.Errorf("a secure cookie matched http: %+v", got)
	}
	if got := Header(Match(cookies[:2], u)); got != "SID=a; OSID=b" {
		t.Errorf("Header = %q", got)
	}
}

func TestFindHonoursTheVariable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBrowser, exe)
	if got, err := Find(); err != nil || got != exe {
		t.Errorf("Find = %q, %v", got, err)
	}
	t.Setenv(EnvBrowser, "/nowhere/chrome")
	if _, err := Find(); err == nil || !strings.Contains(err.Error(), EnvBrowser) {
		t.Errorf("a missing program: %v", err)
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if len(candidates(goos)) == 0 && goos != "windows" {
			t.Errorf("%s has no candidates", goos)
		}
	}
}

func TestCredentialName(t *testing.T) {
	for name, want := range map[string]bool{
		"ANTHROPIC_API_KEY": true, "AGENTRY_TOKEN": true, "GITHUB_TOKEN": true, "MY_SERVICE_SECRET": true, "AWS_PROFILE": true, "NPM_AUTH_KEY": true, "NPM_CONFIG__AUTH": true,
		"AGENTRY_BROWSER": false, "XAUTHORITY": false, "DISPLAY": false, "HOME": false, "PATH": false, "DBUS_SESSION_BUS_ADDRESS": false, "LOCALAPPDATA": false,
	} {
		if got := credentialName(name); got != want {
			t.Errorf("credentialName(%s) = %v", name, got)
		}
	}
}

func TestAKilledRunsProfileIsRemovedByTheNext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	stale, fresh := filepath.Join(tmp, profilePrefix+"stale"), filepath.Join(tmp, profilePrefix+"fresh")
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleProfile)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	removeStaleProfiles()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale profile was left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("another sign-in's profile was removed")
	}
}
