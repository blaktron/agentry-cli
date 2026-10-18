//go:build !windows

package engine

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/destination"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// TestADeliveryFolderIsReadableByItsOwnerAlone: while the step runs, its folder,
// the three it is given and every file in them are the person's alone (0700 and
// 0600), whatever the umask: a CLI killed there leaves nothing another user on
// the host can read.
func TestADeliveryFolderIsReadableByItsOwnerAlone(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	p, f := fixture(t)
	happyFake(f)
	agentWrites(p, f, map[string]string{"reports/brief.md": "# Brief\n"}, `{"path":"reports/brief.md","title":"The brief","marked":true}`)
	saw := deliveryStepPlays(t, f, delivered)
	step := f.Foreground["-delivery"].Handler
	modes := map[string]fs.FileMode{}
	f.Foreground["-delivery"].Handler = func(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, o, e io.Writer) (int, error) {
		for _, m := range r.Mounts {
			base := filepath.Dir(m.Source)
			_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
				if fi, serr := os.Lstat(path); err == nil && serr == nil {
					modes[path] = fi.Mode()
				}
				return nil
			})
		}
		return step(ctx, r, stdin, o, e)
	}
	p.Destinations = []destination.Resolved{hook("n8n", "https://hooks.example.com/in/abc", false)}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	if len(saw.job.Files) != 1 {
		t.Fatalf("job: %+v", saw.job)
	}
	var dirs, files int
	for path, mode := range modes {
		switch {
		case mode.IsDir():
			dirs++
			if mode.Perm() != 0o700 {
				t.Errorf("%s is %v, want 0700", path, mode.Perm())
			}
		case mode.IsRegular():
			files++
			if mode.Perm() != 0o600 {
				t.Errorf("%s is %v, want 0600", path, mode.Perm())
			}
		}
	}
	// The folder, its staged, job and work folders; the lock, the job file and
	// the staged copy.
	if dirs != 4 || files < 3 {
		t.Errorf("saw %d folders and %d files: %v", dirs, files, modes)
	}
}
