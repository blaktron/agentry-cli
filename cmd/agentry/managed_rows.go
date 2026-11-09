package main

import "github.com/blaktron/agentry-cli/pkg/managed"

// The agentry command starts no run In Agentry and sends no bug report, so
// the rows that govern those paths are held wherever it has any: runs here,
// on a runhost and to target hosts, through the runner.
func init() { managed.Enforce(managed.ClientRows...) }
