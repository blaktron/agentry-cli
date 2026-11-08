package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
)

// The doctor's rows for the person's computer: Agentry Desktop answering
// or not, and each application the person set up, as Agentry Desktop lists
// it. With no QuickAgent that uses one, nothing is a failure; with one, an
// application it names that is missing, not approved for what it uses or
// not ready is, and so is an Agentry Desktop that does not list them.
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
	var list desktop.ApplicationList
	var listErr error
	prev := doctorList
	doctorList = func(context.Context, string) (desktop.ApplicationList, error) { return list, listErr }
	t.Cleanup(func() { doctorList = prev })

	rep := &doctorReport{}
	doctorDesktop(rep, home, nil)
	if c := row(rep, "desktop link"); c.Status != "ok" || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("no app, no QuickAgent: %+v", c)
	}
	uses := &manifest.Manifest{Capabilities: manifest.Capabilities{Desktop: &manifest.Desktop{
		Servers: []manifest.DesktopServer{{ID: "blender"}, {ID: "kdenlive", Use: manifest.DesktopUseComputer}}, ComputerUse: manifest.ComputerUseApp}}}
	rep = &doctorReport{}
	doctorDesktop(rep, home, uses)
	if c := row(rep, "desktop link"); c.Status != "fail" {
		t.Fatalf("no app, a QuickAgent that uses blender: %+v", c)
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

	// An Agentry Desktop that does not list: a failure only for a
	// QuickAgent that uses an application.
	listErr = desktop.ErrNoListing
	rep = &doctorReport{}
	doctorDesktop(rep, home, uses)
	if c := row(rep, "desktop link"); c.Status != "ok" || !strings.Contains(c.Detail, "answers") {
		t.Fatalf("the app answering: %+v", c)
	}
	if c := row(rep, "desktop: applications"); c.Status != "fail" || c.Remedy != "update Agentry Desktop" {
		t.Fatalf("an Agentry Desktop that does not list, for a QuickAgent that uses one: %+v", c)
	}
	rep = &doctorReport{}
	doctorDesktop(rep, home, nil)
	if c := row(rep, "desktop: applications"); c.Status != "ok" {
		t.Fatalf("an Agentry Desktop that does not list, for no QuickAgent: %+v", c)
	}

	// The person's applications, each a row; what the QuickAgent needs and
	// does not have, a failure naming the step.
	listErr = nil
	list = desktop.ApplicationList{Applications: []desktop.Application{
		{ID: "blender", Name: "Blender", From: desktop.FromLibrary, MCP: true, Ready: true},
		{ID: "kdenlive", Name: "Kdenlive", From: desktop.FromPerson, MCP: false, Computer: false, Ready: true},
	}}
	rep = &doctorReport{}
	doctorDesktop(rep, home, uses)
	if c := row(rep, "desktop: Blender"); c.Status != "ok" || !strings.Contains(c.Detail, "from Agentry Desktop's library") || !strings.Contains(c.Detail, "its MCP server approved") {
		t.Fatalf("blender's row: %+v", c)
	}
	if c := row(rep, "desktop: kdenlive"); c.Status != "fail" || !strings.Contains(c.Detail, "works Kdenlive's windows") || !strings.Contains(c.Remedy, "approve its windows") {
		t.Fatalf("kdenlive used for its windows, not approved: %+v", c)
	}
	missing := &manifest.Manifest{Capabilities: manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "my-cad"}}}}}
	rep = &doctorReport{}
	doctorDesktop(rep, home, missing)
	if c := row(rep, "desktop: my-cad"); c.Status != "fail" || !strings.Contains(c.Detail, "not set up on this computer") {
		t.Fatalf("an application that is not set up: %+v", c)
	}
}
