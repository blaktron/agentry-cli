package desktop

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// handler answers binds, calls and unbinds and nothing else, as an Agentry
// Desktop older than the listing does.
type handler struct{}

func (handler) Bind(context.Context, BindRequest) (Bound, *Refusal) {
	return Bound{Token: "t"}, nil
}

func (handler) Call(context.Context, CallRequest) (desktoplink.Answer, *Refusal) {
	return desktoplink.Answer{}, nil
}

func (handler) Unbind(context.Context, UnbindRequest) *Refusal { return nil }

// lister is a handler that also lists the person's applications.
type lister struct {
	handler
	apps []Application
}

func (l lister) List(context.Context) (ApplicationList, *Refusal) {
	return ApplicationList{Applications: l.apps}, nil
}

func serve(t *testing.T, h Handler) Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, ln, h) }()
	return Client{Socket: sock}
}

// A link service that lists answers the person's applications; one that
// does not answers the listing as no message it takes, which the client
// reads as ErrNoListing.
func TestTheListingIsAskedAndAnswered(t *testing.T) {
	want := []Application{{ID: "blender", Name: "Blender", From: FromLibrary, MCP: true, Ready: true}, {ID: "my-cad", Name: "My CAD", From: FromPerson, Computer: true}}
	got, err := serve(t, lister{apps: want}).List(context.Background())
	if err != nil || len(got.Applications) != 2 || got.Applications[1] != want[1] {
		t.Fatalf("listed %+v, %v", got, err)
	}
	if _, err := serve(t, handler{}).List(context.Background()); !errors.Is(err, ErrNoListing) {
		t.Fatalf("a link service that does not list: %v", err)
	}
}

// A relay carries a bound run's calls and its unbind alone: never a
// listing, nor its answer.
func TestARelayDoesNotCarryAListing(t *testing.T) {
	for name, m := range map[string]Message{
		"a listing":    {List: &ListRequest{}},
		"its answer":   {Listed: &ApplicationList{}},
		"a call too":   {List: &ListRequest{}, Call: &CallRequest{Run: "R", Token: "t"}},
		"an unbind to": {Listed: &ApplicationList{}, Unbind: &UnbindRequest{Run: "R", Token: "t"}},
	} {
		if RelayedCallOnly(m, "R") {
			t.Errorf("%s is carried", name)
		}
	}
	if !RelayedCallOnly(Message{Call: &CallRequest{Run: "R", Token: "t"}}, "R") {
		t.Fatal("a call of the run's is not carried")
	}
}

// Anything but the library's own set-up, an unknown answer and none
// included, reads as the person's: the strict way for the policy.
func TestSetByPersonReadsTheStrictWay(t *testing.T) {
	for from, want := range map[string]bool{FromLibrary: false, FromLibraryEdited: true, FromPerson: true, "": true, "something-new": true} {
		if got := SetByPerson(from); got != want {
			t.Errorf("SetByPerson(%q) = %v", from, got)
		}
	}
}
