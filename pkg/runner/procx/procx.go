// Package procx holds what every process the runner starts has in common,
// so that a new spawn site cannot forget it. Today that is one thing: on
// Windows, a child of a GUI application must not be given a console of
// its own (agentry-cli#166).
package procx

import "os/exec"

// NoWindow marks cmd so that, when the runner is a windowsgui-subsystem
// process — Agentry Desktop is one — the child gets no console window.
// Without it Windows allocates a fresh console for every console-subsystem
// child (docker, a harness's --version, the harness itself), and each one
// flashes a command-prompt window at the person. On every other platform,
// and for the CLI on a terminal (where the child inherits the console),
// it changes nothing. Attributes the caller already set on
// cmd.SysProcAttr are kept.
func NoWindow(cmd *exec.Cmd) { noWindow(cmd) }
