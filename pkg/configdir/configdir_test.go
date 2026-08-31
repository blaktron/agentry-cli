// Resolution tests: the one environment variable that relocates every
// client file must actually win, and the default must be the XDG-ish
// ~/.config/agentry every client shares.
package configdir

import (
	"path/filepath"
	"testing"
)

func TestDirHonorsOverride(t *testing.T) {
	t.Setenv(EnvVar, "/relocated/agentry")
	got, err := Dir()
	if err != nil || got != "/relocated/agentry" {
		t.Fatalf("Dir() = (%q, %v)", got, err)
	}
}

func TestDirDefaultsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", home)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "agentry"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestDirErrorsWithoutHome(t *testing.T) {
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", "")
	if _, err := Dir(); err == nil {
		t.Fatal("Dir() with no HOME and no override must error, not guess")
	}
}
