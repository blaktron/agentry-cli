package toolsrv_test

import (
	"slices"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/browser"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// TestTheCoresToolTableIsTheRunners: the core reads a QuickAgent's
// instruction for runner tools against its own table of which grant gives
// which tool, both at publish and in the Drafter's rules. That table must
// be the tools a run of each grant actually has here, or a release is held
// for a tool the run would have, or passes with one it would not.
func TestTheCoresToolTableIsTheRunners(t *testing.T) {
	// What every run has whatever it declares (write_report) is no grant's.
	always := (&toolsrv.Spec{}).GrantedTools()
	granted := func(s []string) []string {
		out := slices.DeleteFunc(slices.Clone(s), func(n string) bool { return slices.Contains(always, n) })
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		grant toolgrant.Grant
		has   []string
	}{
		{toolgrant.GrantFSRead, (&toolsrv.Spec{FSRead: toolsrv.Access{Enabled: true}}).GrantedTools()},
		{toolgrant.GrantFSWrite, (&toolsrv.Spec{FSWrite: toolsrv.Access{Enabled: true}}).GrantedTools()},
		{toolgrant.GrantEgress, (&toolsrv.Spec{Fetch: toolsrv.Fetch{Enabled: true}}).GrantedTools()},
		{toolgrant.GrantExec, (&toolsrv.Spec{Exec: toolsrv.Exec{Commands: []string{"grep"}}}).GrantedTools()},
		{toolgrant.GrantBrowser, browser.Tools(false)},
		{toolgrant.GrantTargetHosts, hostlink.Tools},
	} {
		if got, want := granted(tc.has), toolgrant.ToolsOf(tc.grant); !slices.Equal(got, want) {
			t.Errorf("%s: the runner gives %q, the core's table %q", tc.grant, got, want)
		}
	}
}
