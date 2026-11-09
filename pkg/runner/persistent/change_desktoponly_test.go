package persistent

import (
	"errors"
	"testing"
)

// Agentry Helper's configuration is Agentry Desktop's, except the saved
// model it runs on: a change that sets the model alone is applied, and any
// other change, or one beside the model, is refused.
func TestAgentryHelperTakesAModelChangeAndNothingElse(t *testing.T) {
	helper, err := Lookup("main")
	if err != nil || !helper.DesktopOnly {
		t.Fatalf("Agentry Helper is %+v", helper)
	}
	home := t.TempDir()
	var c Config
	id := "m-0123456789abcdef"
	if _, err := ApplyChange(home, helper, &c, Change{Model: &id}); err != nil {
		t.Fatalf("a model change: %v", err)
	}
	name := "Helper"
	if _, err := ApplyChange(home, helper, &c, Change{Model: &id, Name: &name}); !errors.Is(err, ErrDesktopOnly) {
		t.Fatalf("a model change beside a name: %v", err)
	}
	if _, err := ApplyChange(home, helper, &c, Change{Name: &name}); !errors.Is(err, ErrDesktopOnly) {
		t.Fatalf("a name change: %v", err)
	}
}
