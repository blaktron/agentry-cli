package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/clirelease"
	"github.com/blaktron/agentry-cli/pkg/runhost/setup"
)

// updatingRunhost is a scripted service that runs CLI version and takes an
// update to a newer one, restarting into it at once.
type updatingRunhost struct {
	mu      sync.Mutex
	version string
	gvisor  string
	got     []runhost.UpdateHeader
	doctor  int
	noVerb  bool
}

func (u *updatingRunhost) handlers() map[string]runhost.Handler {
	h := map[string]runhost.Handler{
		"version": func(_ context.Context, _ runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			return 0, fw.Result(map[string]any{"version": u.version, "arch": "amd64"})
		},
		"status": func(_ context.Context, _ runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			return 0, fw.Result(map[string]any{"gvisor": u.gvisor})
		},
		"doctor": func(_ context.Context, _ runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.doctor != 0 {
				return u.doctor, errors.New("docker:runtime: runsc is not registered")
			}
			return 0, fw.Result(map[string]any{})
		},
	}
	if !u.noVerb {
		h["update"] = func(_ context.Context, req runhost.Request, in io.Reader, fw *runhost.FrameWriter) (int, error) {
			var hd runhost.UpdateHeader
			dec := json.NewDecoder(in)
			if err := dec.Decode(&hd); err != nil {
				return 2, err
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			u.got = append(u.got, hd)
			_ = fw.Out("checked")
			switch req.Args[0] {
			case "cli":
				from := u.version
				u.version = req.Args[1]
				return 0, fw.Result(runhost.Updated{What: "cli", From: from, To: req.Args[1], Installed: true})
			default:
				from := u.gvisor
				u.gvisor = req.Args[1]
				return 0, fw.Result(runhost.Updated{What: "gvisor", From: from, To: req.Args[1], Installed: true})
			}
		}
	}
	return h
}

func updateOptions(fetched *[]string) UpdateOptions {
	return UpdateOptions{
		Version: "1.2.4",
		FetchCLI: func(_ context.Context, v, arch string) (*clirelease.Binary, error) {
			*fetched = append(*fetched, "cli "+v+" "+arch)
			return &clirelease.Binary{Version: v, Arch: arch, Name: clirelease.BinaryName(v, arch), Bytes: []byte("cli"), Sums: []byte("sums"), Sig: []byte("sig")}, nil
		},
		FetchGVisor: func(_ context.Context, arch string) ([]byte, error) {
			*fetched = append(*fetched, "gvisor "+arch)
			return []byte("archive"), nil
		},
		Restart: 10 * time.Second,
	}
}

func TestUpdateBringsARunhostToThisDevicesVersion(t *testing.T) {
	u := &updatingRunhost{version: "1.2.3", gvisor: "old"}
	l := newLabWith(t, u.handlers())
	var fetched []string
	rec := l.rec
	out, err := Update(context.Background(), &rec, l.keys, updateOptions(&fetched))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Updated || out.From != "1.2.3" || out.To != "1.2.4" || !out.GVisorUpdated || out.GVisor != setup.GVisorRelease || len(out.NeedsAttention) != 0 {
		t.Fatalf("outcome %+v", out)
	}
	if rec.CLIVersion != "1.2.4" || rec.GVisor != setup.GVisorRelease {
		t.Fatalf("the record says %s, gVisor %s", rec.CLIVersion, rec.GVisor)
	}
	if strings.Join(fetched, ",") != "cli 1.2.4 amd64,gvisor x86_64" {
		t.Fatalf("fetched %v", fetched)
	}
	if len(u.got) != 2 || u.got[0].Name != "agentry-1.2.4-linux-amd64" || string(u.got[0].Sig) != "sig" || u.got[0].Size != 3 || u.got[1].Size != 7 {
		t.Fatalf("the runhost was sent %+v", u.got)
	}

	// A second update has nothing to send, and only the doctor's verdict is
	// left: a failing check is what only the admin sign-in can repair.
	u.mu.Lock()
	u.doctor = 1
	u.mu.Unlock()
	fetched = nil
	out, err = Update(context.Background(), &rec, l.keys, updateOptions(&fetched))
	if err != nil || out.Updated || out.GVisorUpdated || len(fetched) != 0 {
		t.Fatalf("an up-to-date runhost: %+v %v %v", out, err, fetched)
	}
	if len(out.NeedsAttention) != 1 || !strings.Contains(out.NeedsAttention[0], "runsc is not registered") || !strings.Contains(out.NeedsAttention[0], "agentry runhost add lab") {
		t.Fatalf("a runhost whose doctor fails: %+v", out)
	}
}

func TestANewerRunhostIsLeftForThisDeviceToCatchUp(t *testing.T) {
	u := &updatingRunhost{version: "1.3.0", gvisor: setup.GVisorRelease}
	l := newLabWith(t, u.handlers())
	var fetched []string
	rec := l.rec
	out, err := Update(context.Background(), &rec, l.keys, updateOptions(&fetched))
	if err != nil || out.DeviceBehind != "1.3.0" || out.Updated || len(fetched) != 0 || len(u.got) != 0 {
		t.Fatalf("a newer runhost: %+v %v %v", out, err, fetched)
	}
	if err := VersionSkew("lab", "1.3.0", "1.2.4"); !strings.Contains(err.Error(), "update this device's CLI to 1.3.0") {
		t.Fatalf("a device behind its runhost was told: %v", err)
	}
	if err := VersionSkew("lab", "1.2.3", "1.2.4"); !strings.Contains(err.Error(), "agentry runhost update lab") || !strings.Contains(err.Error(), "1.2.3") {
		t.Fatalf("a runhost behind its device was told: %v", err)
	}
}

// A runhost whose CLI predates the update verb answers at once and ends the
// session while the release is still being sent; the device reports what
// the admin sign-in must do instead of the broken upload. The release is
// larger than an SSH channel's window, so the upload is always cut short.
func TestWhatOnlyTheAdminSignInCanDoNeedsAttention(t *testing.T) {
	u := &updatingRunhost{version: "1.2.3", noVerb: true}
	l := newLabWith(t, u.handlers())
	o := updateOptions(new([]string))
	o.FetchCLI = func(_ context.Context, v, arch string) (*clirelease.Binary, error) {
		return &clirelease.Binary{Version: v, Arch: arch, Name: clirelease.BinaryName(v, arch), Bytes: make([]byte, 4<<20), Sums: []byte("sums"), Sig: []byte("sig")}, nil
	}
	rec := l.rec
	out, err := Update(context.Background(), &rec, l.keys, o)
	if err != nil || out.Updated || len(out.NeedsAttention) != 1 || !strings.Contains(out.NeedsAttention[0], "takes no update over the run key") || !strings.Contains(out.NeedsAttention[0], "agentry runhost add lab") {
		t.Fatalf("a runhost without the update verb: %+v %v", out, err)
	}
}

func TestUnpairAsksTheRunhostToLetThisDeviceGo(t *testing.T) {
	var asked runhost.Request
	l := newLabWith(t, map[string]runhost.Handler{
		"unpair": func(_ context.Context, req runhost.Request, _ io.Reader, fw *runhost.FrameWriter) (int, error) {
			asked = req
			return 0, fw.Result(Unpaired{Device: testDevice, Credits: CreditsRemoved{Removed: true, Revoked: true}})
		},
	})
	old := testServeDevice
	testServeDevice = testDevice
	defer func() { testServeDevice = old }()
	out, err := Unpair(context.Background(), l.rec, l.keys, nil)
	if err != nil || out.Device != testDevice || !out.Credits.Revoked {
		t.Fatalf("unpair: %+v %v", out, err)
	}
	if asked.Device != testDevice {
		t.Fatalf("the service was told the device is %q", asked.Device)
	}
}
