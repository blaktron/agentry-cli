//go:build !linux

package toolsrv

// PrepareMounts holds nothing off Linux, where the tools server is
// only `agentry runner-tools`, a debugging path that never runs a
// QuickAgent.
func PrepareMounts(*Spec) error { return nil }
