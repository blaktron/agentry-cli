package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
)

// The doctor's rows for the person's computer: Agentry Desktop answering
// or not, and each catalogue application's listener. With no QuickAgent
// that uses one, nothing is a failure; with one that uses Blender, Agentry
// Desktop not running is.
func TestDoctorReportsThePersonsComputer(t *testing.T) {
	home, err := os.MkdirTemp("", "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	row := func(rep *doctorReport, name string) doctorCheck {
		for _, c := range rep.Checks {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no row %q in %+v", name, rep.Checks)
		return doctorCheck{}
	}
	rep := &doctorReport{}
	doctorDesktop(rep, home, nil)
	if c := row(rep, "desktop link"); c.Status != "ok" || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("no app, no QuickAgent: %+v", c)
	}
	if c := row(rep, "desktop: Blender"); c.Status != "ok" {
		t.Fatalf("Blender's row with no QuickAgent: %+v", c)
	}
	uses := &manifest.Manifest{Capabilities: manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}}}}}
	rep = &doctorReport{}
	doctorDesktop(rep, home, uses)
	if c := row(rep, "desktop link"); c.Status != "fail" {
		t.Fatalf("no app, a QuickAgent that uses Blender: %+v", c)
	}
	if err := os.MkdirAll(filepath.Dir(desktop.SocketPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", desktop.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	rep = &doctorReport{}
	doctorDesktop(rep, home, uses)
	if c := row(rep, "desktop link"); c.Status != "ok" || !strings.Contains(c.Detail, "answers") {
		t.Fatalf("the app answering: %+v", c)
	}
}
