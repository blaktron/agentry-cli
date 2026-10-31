// Command genexitclasses writes the exit-class table as exit-classes.json,
// the rendering receipt.ExitClassesJSON makes of receipt.ExitClasses, into
// the file named by its one argument (exit-classes.json in the working
// folder by default). The receipt package's go:generate directive runs it;
// a test there fails when the file on disk is not this rendering.
package main

import (
	"fmt"
	"os"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func main() {
	path := "exit-classes.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if err := os.WriteFile(path, receipt.ExitClassesJSON(), 0o644); err != nil { //nolint:gosec // a generated source file
		fmt.Fprintln(os.Stderr, "genexitclasses:", err)
		os.Exit(1)
	}
}
