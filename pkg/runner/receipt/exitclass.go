package receipt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// ExitClass is one row of the exit-class table: what a run's exit code
// means, in the words every client shows. Code is the process exit code,
// Name the word the envelope's exitClass carries and a client acts on,
// Meaning one phrase a person can read, and RetryLikelyHelps whether
// running again with the same inputs is likely to end differently: true
// where the failure is usually transient (an engine that did not start, a
// model that could not be reached, an answer that did not fit its schema),
// false where the same inputs meet the same refusal or the same limit.
type ExitClass struct {
	Code             int    `json:"code"`
	Name             string `json:"name"`
	Meaning          string `json:"meaning"`
	RetryLikelyHelps bool   `json:"retryLikelyHelps"`
}

// ExitClasses is the table, by code ascending: the one source of each
// class's name, meaning and retry hint, for the envelope, the CLI's own
// output and every client that imports this package. Codes 3 to 9 are
// unused. exit-classes.json beside this file is the same table rendered by
// ExitClassesJSON for readers that are not Go; a test holds the two equal.
var ExitClasses = []ExitClass{
	{Code: 0, Name: "ok", Meaning: "the run finished and its result was collected"},
	{Code: 1, Name: "cli", Meaning: "the CLI itself could not finish the run: it was interrupted, or something it needed was missing"},
	{Code: 2, Name: "usage", Meaning: "the command cannot work as asked; change the flags or how it is called"},
	{Code: 10, Name: "verify", Meaning: "the release could not be verified: its digest, signature or installed files do not match"},
	{Code: 11, Name: "policy", Meaning: "refused by the operator policy or a missing secret before the agent started"},
	{Code: 12, Name: "compile", Meaning: "the QuickAgent's manifest and the policy could not be compiled into a run plan"},
	{Code: 13, Name: "gateway", Meaning: "the container engine or the tool gateway could not be started or reached", RetryLikelyHelps: true},
	{Code: 14, Name: "harness", Meaning: "the agent's harness failed or could not reach its model", RetryLikelyHelps: true},
	{Code: 15, Name: "timeout", Meaning: "the run's time budget ran out"},
	{Code: 16, Name: "budget", Meaning: "a turn, cost or usage budget ran out before the agent finished"},
	{Code: 17, Name: "outputSchema", Meaning: "the agent's final answer did not match the declared output schema", RetryLikelyHelps: true},
	{Code: 18, Name: "blocked", Meaning: "the agent could not do the task with what the run had"},
}

// ExitClassOf is the row for an exit code. A code the table does not hold
// (a process killed by a signal, a code from a newer CLI) is answered as a
// row of its own, named "exit-<code>", so a reader always has words.
func ExitClassOf(code int) ExitClass {
	i := sort.Search(len(ExitClasses), func(i int) bool { return ExitClasses[i].Code >= code })
	if i < len(ExitClasses) && ExitClasses[i].Code == code {
		return ExitClasses[i]
	}
	return ExitClass{Code: code, Name: fmt.Sprintf("exit-%d", code), Meaning: "the run ended with an exit code this CLI does not know"}
}

// ExitClassByName is the row for a class name, and whether the table holds
// it.
func ExitClassByName(name string) (ExitClass, bool) {
	for _, c := range ExitClasses {
		if c.Name == name {
			return c, true
		}
	}
	return ExitClass{}, false
}

// ExitClassName names an exit code as the envelope's exitClass carries it.
func ExitClassName(code int) string { return ExitClassOf(code).Name }

// ExitClassesJSON renders the table as exit-classes.json: one object with
// the rows under "exitClasses", indented, ending in a newline. The bytes are
// deterministic, so the shipped file can be held to them.
func ExitClassesJSON() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// The table is a fixed Go value, so encoding it cannot fail.
	_ = enc.Encode(struct {
		ExitClasses []ExitClass `json:"exitClasses"`
	}{ExitClasses})
	return buf.Bytes()
}

//go:generate go run ./internal/genexitclasses exit-classes.json
