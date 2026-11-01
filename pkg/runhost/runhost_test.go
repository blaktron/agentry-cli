package runhost

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestAuthorizedKeyLineForcesServeAndRestricts(t *testing.T) {
	line := AuthorizedKeyLine("ssh-ed25519 AAAAC3Nza", "dev-1")
	want := `restrict,command="/var/lib/agentry/bin/agentry runhost serve" ssh-ed25519 AAAAC3Nza agentry-runhost-device:dev-1`
	if line != want {
		t.Fatalf("got  %s\nwant %s", line, want)
	}
}

func TestLoginShellRunsServeAndNothingElse(t *testing.T) {
	cases := []struct {
		args            []string
		handled, serves bool
	}{
		{[]string{"agentry-shell", "-c", ServeCommand}, true, true},
		{[]string{ShellPath, "-c", ServeCommand}, true, true},
		{[]string{"agentry-shell", "-c", "id"}, true, false},
		{[]string{"agentry-shell", "-c", ServeCommand + "; id"}, true, false},
		{[]string{"-agentry-shell"}, true, false},
		{[]string{"-agentry-shell", "-c", "id"}, true, false},
		{[]string{"agentry-shell"}, true, false},
		{[]string{"agentry-shell", "-c"}, true, false},
		{[]string{"agentry-shell", "runhost", "serve"}, true, false},
		// Under its own name the CLI is the CLI.
		{[]string{"agentry", "-c", ServeCommand}, false, false},
		{[]string{"agentry", "runhost", "status", "lab"}, false, false},
		{[]string{"/var/lib/agentry/bin/agentry"}, false, false},
	}
	for _, c := range cases {
		handled, serves, refusal := LoginShell(c.args)
		if handled != c.handled || serves != c.serves {
			t.Errorf("%q: handled=%v serve=%v (%s)", c.args, handled, serves, refusal)
		}
		if handled && !serves && refusal == "" {
			t.Errorf("%q: refused without a reason", c.args)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"lab", "a", "home-server-2"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Lab", "-x", "x-", "a b", "a/b", strings.Repeat("a", 33), "lab:ssh"} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short; t.TempDir's can be too long.
	d, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func startService(t *testing.T, handlers map[string]Handler) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), SocketName)
	s := &Service{Socket: sock, Handlers: handlers}
	l, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func TestTheSocketIsOwnerOnly(t *testing.T) {
	sock := startService(t, nil)
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode().Perm())
	}
}

func TestTheServiceRefusesAnOpenRuntimeDirectory(t *testing.T) {
	d := shortTempDir(t)
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Service{Socket: filepath.Join(d, SocketName)}
	if l, err := s.Listen(); err == nil {
		_ = l.Close()
		t.Fatal("the service listened in a directory others can enter")
	}
}

func TestServeRelaysAVerbAndItsExitCode(t *testing.T) {
	sock := startService(t, map[string]Handler{
		"status": func(_ context.Context, req Request, _ net.Conn, fw *FrameWriter) (int, error) {
			_ = fw.Out("checking %s", strings.Join(req.Args, ","))
			_ = fw.Result(map[string]string{"state": "ready"})
			return 0, nil
		},
		"doctor": func(context.Context, Request, net.Conn, *FrameWriter) (int, error) {
			return 1, os.ErrPermission
		},
	})
	var out bytes.Buffer
	code := Serve(context.Background(), "status --brief", strings.NewReader(""), &out, sock)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	a, err := ReadAnswer(&out, nil)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]string
	if err := json.Unmarshal(a.Result, &res); err != nil || res["state"] != "ready" || len(a.Out) != 1 || a.Out[0] != "checking --brief" {
		t.Fatalf("answer %+v", a)
	}

	out.Reset()
	if code := Serve(context.Background(), "doctor", strings.NewReader(""), &out, sock); code != 1 {
		t.Fatalf("a failing verb's exit code was %d", code)
	}
}

func TestServeRefusesWhatIsNotAVerbWithoutDialling(t *testing.T) {
	// "status;" is not a verb, so a command chained after one is refused too.
	for _, asked := range []string{"", "id", "bash -i", "sh -c 'curl x | sh'", "status; id"} {
		var out bytes.Buffer
		// No service: a refusal must not need one.
		code := Serve(context.Background(), asked, strings.NewReader(""), &out, "/nonexistent/sock")
		if code != 2 {
			t.Errorf("%q: exit %d", asked, code)
		}
		a, err := ReadAnswer(&out, nil)
		if err != nil || a.Error == "" {
			t.Errorf("%q: answer %+v (%v)", asked, a, err)
		}
	}
}

func TestAVerbTheServiceDoesNotKnowIsAnsweredNotDropped(t *testing.T) {
	sock := startService(t, map[string]Handler{})
	var out bytes.Buffer
	if code := Serve(context.Background(), "job", strings.NewReader(""), &out, sock); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestServeSaysWhenTheServiceIsDown(t *testing.T) {
	var out bytes.Buffer
	if code := Serve(context.Background(), "status", strings.NewReader(""), &out, filepath.Join(shortTempDir(t), "none.sock")); code != 3 {
		t.Fatalf("exit %d", code)
	}
}

func TestAnAnswerCutShortIsAnError(t *testing.T) {
	sock := startService(t, map[string]Handler{
		"status": func(_ context.Context, _ Request, conn net.Conn, fw *FrameWriter) (int, error) {
			_ = fw.Out("half")
			_ = conn.Close()
			time.Sleep(10 * time.Millisecond)
			return 0, nil
		},
	})
	var out bytes.Buffer
	if code := Serve(context.Background(), "status", strings.NewReader(""), &out, sock); code != 3 {
		t.Fatalf("exit %d", code)
	}
}

func TestRecordsKeepNoSecretAndRoundTrip(t *testing.T) {
	home := t.TempDir()
	r, err := LoadRecords(home)
	if err != nil || len(r.Runhosts) != 0 {
		t.Fatalf("an empty home: %+v %v", r, err)
	}
	r.Put(Record{Name: "lab", Host: "192.0.2.10", Port: 22, HostKey: "ssh-ed25519 AAAA", Owner: Owner{Kind: OwnerDevices}})
	r.Put(Record{Name: "lab", Host: "192.0.2.11", Port: 2222})
	r.Put(Record{Name: "edge", Host: "192.0.2.12", Port: 22})
	if err := r.Save(home); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(home, RecordsFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("records file mode %v (%v)", st.Mode().Perm(), err)
	}
	back, err := LoadRecords(home)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := back.Find("lab"); !ok || got.Host != "192.0.2.11" || len(back.Runhosts) != 2 {
		t.Fatalf("records %+v", back.Runhosts)
	}
	back.Drop("lab")
	if _, ok := back.Find("lab"); ok || len(back.Runhosts) != 1 {
		t.Fatalf("after drop %+v", back.Runhosts)
	}
}

func TestDeviceIDIsMadeOnceAndKept(t *testing.T) {
	home := t.TempDir()
	a, err := DeviceID(home)
	if err != nil || len(a) != 16 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := DeviceID(home)
	if a != b {
		t.Fatalf("the device id changed: %s then %s", a, b)
	}
}

func TestDeviceKeysRoundTripThroughTheKeychain(t *testing.T) {
	keyring.MockInit()
	k, err := NewDeviceKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveKeys("lab", k); err != nil {
		t.Fatal(err)
	}
	back, err := LoadKeys("lab")
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := k.RunPublic()
	pb, _ := back.RunPublic()
	if pa != pb || k.SealPublic() != back.SealPublic() || !strings.HasPrefix(pa, "ssh-ed25519 ") {
		t.Fatalf("keys changed through the keychain: %s / %s", pa, pb)
	}
	if err := DeleteKeys("lab"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteKeys("lab"); err != nil {
		t.Fatalf("deleting keys already gone: %v", err)
	}
	if _, err := LoadKeys("lab"); err == nil {
		t.Fatal("keys were read after they were deleted")
	}
}
