package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// exitConstants reads the Exit* constants of this package from engine.go,
// by name and value, so a constant added or renumbered without the table,
// or a table row without a constant, fails the test below.
func exitConstants(t *testing.T) map[int]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Exit") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s is not an integer literal", n.Name)
				}
				code, _ := strconv.Atoi(lit.Value)
				if prev, dup := out[code]; dup {
					t.Fatalf("%s and %s are both %d", prev, n.Name, code)
				}
				out[code] = n.Name
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Exit* constants found in engine.go")
	}
	return out
}

// The exit codes this package defines and the exit-class table are one
// set: every constant has a row with the name the envelope carries, and
// every row has a constant.
func TestTheExitClassTableMatchesTheConstants(t *testing.T) {
	consts := exitConstants(t)
	want := map[int]string{
		ExitOK: "ok", ExitCLI: "cli", ExitUsage: "usage", ExitVerify: "verify", ExitPolicy: "policy",
		ExitCompile: "compile", ExitGateway: "gateway", ExitHarness: "harness", ExitTimeout: "timeout",
		ExitBudget: "budget", ExitOutputSchema: "outputSchema", ExitBlocked: "blocked",
	}
	rows := map[int]receipt.ExitClass{}
	for _, c := range ExitClasses {
		rows[c.Code] = c
	}
	for code, name := range consts {
		row, ok := rows[code]
		if !ok {
			t.Errorf("%s = %d has no row in the exit-class table", name, code)
			continue
		}
		if row.Name != want[code] {
			t.Errorf("%s = %d: the table names it %q, want %q", name, code, row.Name, want[code])
		}
		if got := receipt.ExitClassName(code); got != row.Name {
			t.Errorf("code %d: ExitClassName %q, table %q", code, got, row.Name)
		}
	}
	for code := range rows {
		if _, ok := consts[code]; !ok {
			t.Errorf("the table's code %d has no Exit* constant", code)
		}
	}
	if len(want) != len(consts) {
		t.Errorf("this test knows %d classes, engine.go defines %d: add the new class here too", len(want), len(consts))
	}
	if got := receipt.ExitClassName(99); got != "exit-99" {
		t.Errorf("unknown code: %q", got)
	}
	if ExitClassOf(ExitGateway).Name != "gateway" {
		t.Error("ExitClassOf does not read the table")
	}
}
