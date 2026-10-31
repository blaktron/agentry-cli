package engine

import "github.com/blaktron/agentry-cli/pkg/runner/receipt"

// ExitClasses is the exit-class table: for each exit code above, its name,
// its one-phrase meaning and whether a retry with the same inputs is likely
// to help. The rows live in the receipt package, which renders them into
// every envelope and which this package depends on; they are reachable here
// beside the codes they describe, and the test in this package holds the
// table and the constants to one another so neither gains an entry alone.
var ExitClasses = receipt.ExitClasses

// ExitClassOf is the table's row for an exit code (receipt.ExitClassOf).
func ExitClassOf(code int) receipt.ExitClass { return receipt.ExitClassOf(code) }
