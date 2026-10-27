package browsersignin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The test binary is the browser too: run with fakeBrowserEnv set, it
// speaks the DevTools pipe on descriptors 3 and 4 as the variable's mode
// says, and writes its command line to the file fakeArgsEnv names.
const (
	fakeBrowserEnv = "BROWSERSIGNIN_FAKE_BROWSER"
	fakeArgsEnv    = "BROWSERSIGNIN_FAKE_ARGS"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeBrowserEnv); mode != "" {
		fakeBrowser(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeBrowser answers the commands Run sends. In mode "signs-in" the
// profile has a stranger's cookie at first, then the sign-in's after the
// second read; in "closes" the window is gone after the address opens; in
// "downloads" the download behaviour is refused.
func fakeBrowser(mode string) {
	if f := os.Getenv(fakeArgsEnv); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	in, out := os.NewFile(3, "in"), os.NewFile(4, "out")
	r := bufio.NewReader(in)
	reads, opened := 0, false
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
		case "Target.getTargets":
			pages := []any{map[string]any{"targetId": "blank", "type": "page"}}
			if opened {
				pages = []any{map[string]any{"targetId": "feed", "type": "page"}, map[string]any{"targetId": "w", "type": "service_worker"}}
				if mode == "closes" {
					pages = pages[1:]
				}
			}
			result = map[string]any{"targetInfos": pages}
		case "Target.createTarget":
			opened = m.Params["url"] == "https://calendar.example.com/feed/basic.ics"
			result = map[string]any{"targetId": "feed"}
		case "Storage.getCookies":
			reads++
			cookies := []Cookie{{Name: "stranger", Value: "x", Domain: "other.example", Path: "/"}}
			if reads >= 2 {
				cookies = append(cookies,
					Cookie{Name: "SID", Value: "signed-in-0123456789", Domain: ".example.com", Path: "/", Secure: true},
					Cookie{Name: "OSID", Value: "calendar-0123456789", Domain: "calendar.example.com", Path: "/", Secure: true})
			}
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
			// An event first, which Run must pass over.
			ev, _ := json.Marshal(map[string]any{"method": "Target.targetInfoChanged", "params": map[string]any{}})
			_, _ = out.Write(append(ev, 0))
			b, _ = json.Marshal(map[string]any{"id": m.ID, "result": result})
		}
		_, _ = out.Write(append(b, 0))
	}
}

func fakeOptions(t *testing.T, mode string, ready func(context.Context, string) (bool, error)) (Options, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser reads descriptors 3 and 4")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := t.TempDir() + "/args"
	t.Setenv(fakeBrowserEnv, mode)
	t.Setenv(fakeArgsEnv, args)
	old := PollEvery
	PollEvery = 10 * time.Millisecond
	t.Cleanup(func() { PollEvery = old })
	return Options{URL: "https://calendar.example.com/feed/basic.ics", Browser: exe, Ready: ready}, args
}

func TestRunReturnsTheCookiesOnceTheAddressAnswers(t *testing.T) {
	var headers []string
	o, args := fakeOptions(t, "signs-in", func(_ context.Context, h string) (bool, error) {
		headers = append(headers, h)
		return strings.Contains(h, "SID="), nil
	})
	got, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if h := Header(got); h != "SID=signed-in-0123456789; OSID=calendar-0123456789" && h != "OSID=calendar-0123456789; SID=signed-in-0123456789" {
		t.Errorf("cookies %+v", got)
	}
	if slices.ContainsFunc(headers, func(h string) bool { return strings.Contains(h, "stranger") }) {
		t.Errorf("another site's cookie was sent: %v", headers)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	cmdline := string(b)
	for _, want := range []string{"--remote-debugging-pipe", "--user-data-dir=", "--no-first-run", "about:blank"} {
		if !strings.Contains(cmdline, want) {
			t.Errorf("the command line lacks %s:\n%s", want, cmdline)
		}
	}
	if strings.Contains(cmdline, "calendar.example.com") || strings.Contains(cmdline, "--remote-debugging-port") {
		t.Errorf("the address or a debugging port is on the command line:\n%s", cmdline)
	}
	// The profile is removed afterwards.
	for _, line := range strings.Split(cmdline, "\n") {
		if dir, ok := strings.CutPrefix(line, "--user-data-dir="); ok {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the profile %s is left: %v", dir, err)
			}
		}
	}
}

func TestRunStopsWhenTheWindowCloses(t *testing.T) {
	o, _ := fakeOptions(t, "closes", func(context.Context, string) (bool, error) { return false, nil })
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunStopsOnReadyError(t *testing.T) {
	stop := errors.New("this account cannot read the calendar")
	o, _ := fakeOptions(t, "signs-in", func(context.Context, string) (bool, error) { return false, stop })
	if _, err := Run(context.Background(), o); !errors.Is(err, stop) {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunNeedsDownloadsDenied(t *testing.T) {
	o, _ := fakeOptions(t, "downloads", func(context.Context, string) (bool, error) { return true, nil })
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunEndsWithTheContext(t *testing.T) {
	o, _ := fakeOptions(t, "signs-in", func(context.Context, string) (bool, error) { return false, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, o); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
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
