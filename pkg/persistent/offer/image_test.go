package offer

import (
	"slices"
	"strings"
	"testing"
)

func goodImageServer() ImageServer {
	return ImageServer{
		Key: "gog", Name: "Google Workspace (gog)", Image: "ghcr.io/example/gog:1@sha256:" + strings.Repeat("a", 64),
		Command: []string{"mcp"}, Egress: []string{"gmail.googleapis.com", "oauth2.googleapis.com:443"},
		VolumeTarget: "/persist/gogcli", SignIn: "sign it in",
		Switches: []ImageSwitch{
			{Key: "writes", Name: "Allow changes", Line: "changes", Args: []string{"--allow-write"}},
			{Key: "send", Name: "Allow sending", Line: "sends", Args: []string{"--allow-gmail-send"}, Requires: "writes"},
		},
	}
}

// TestAnImageServerHoldsToItsRules: a key and a name, an image pinned by
// digest, a command, named hosts on 443, an absolute volume target, a
// sign-in, and switches that are each one, requiring only one the server has.
func TestAnImageServerHoldsToItsRules(t *testing.T) {
	if err := goodImageServer().Check(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ImageServer){
		"no key":                    func(s *ImageServer) { s.Key = "" },
		"an image by tag":           func(s *ImageServer) { s.Image = "ghcr.io/example/gog:1" },
		"no command":                func(s *ImageServer) { s.Command = nil },
		"no host":                   func(s *ImageServer) { s.Egress = nil },
		"any host":                  func(s *ImageServer) { s.Egress = []string{"*"} },
		"a wildcard host":           func(s *ImageServer) { s.Egress = []string{"*.googleapis.com"} },
		"an address":                func(s *ImageServer) { s.Egress = []string{"10.0.0.1"} },
		"another port":              func(s *ImageServer) { s.Egress = []string{"gmail.googleapis.com:8443"} },
		"a relative target":         func(s *ImageServer) { s.VolumeTarget = "persist" },
		"no sign-in":                func(s *ImageServer) { s.SignIn = "" },
		"a switch with no args":     func(s *ImageServer) { s.Switches[0].Args = nil },
		"a switch twice":            func(s *ImageServer) { s.Switches[1].Key = "writes"; s.Switches[1].Requires = "" },
		"a missing requirement":     func(s *ImageServer) { s.Switches[1].Requires = "admin" },
		"a switch requiring itself": func(s *ImageServer) { s.Switches[1].Requires = "send" },
	} {
		s := goodImageServer()
		mutate(&s)
		if err := s.Check(); err == nil {
			t.Errorf("%s: offered", name)
		}
	}
	if err := CheckImageServers([]ImageServer{goodImageServer(), goodImageServer()}); err == nil {
		t.Error("the same image server offered twice")
	}
}

// TestAnImageServersSwitchesAddTheirArgs: the command takes each switch
// that is on, in the switches' order; one whose requirement is off is left
// out.
func TestAnImageServersSwitchesAddTheirArgs(t *testing.T) {
	s := goodImageServer()
	for _, tc := range []struct {
		on   []string
		want []string
	}{
		{nil, []string{"mcp"}},
		{[]string{"writes"}, []string{"mcp", "--allow-write"}},
		{[]string{"send"}, []string{"mcp"}},
		{[]string{"send", "writes"}, []string{"mcp", "--allow-write", "--allow-gmail-send"}},
	} {
		if got := s.Args(func(k string) bool { return slices.Contains(tc.on, k) }); !slices.Equal(got, tc.want) {
			t.Errorf("on %v: %q, want %q", tc.on, got, tc.want)
		}
	}
	if !slices.Equal(s.Command, []string{"mcp"}) {
		t.Fatal("Args changed the command")
	}
}
