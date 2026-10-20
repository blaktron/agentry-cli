// Package procx holds what every process the runner starts has in common
// — one place, so a new spawn site has one call to make rather than a flag
// to remember (nothing enforces the call). Today that is one thing: on
// Windows, a child of a GUI application must not be given a console of
// its own.
package procx

import "os/exec"

// NoWindow marks cmd so that, when the runner is a windowsgui-subsystem
// process — Agentry Desktop is one — the child gets no console window.
// Without it Windows allocates a fresh console for every console-subsystem
// child (docker, a harness's --version, the harness itself), and each one
// flashes a command-prompt window at the person. On every other platform
// it changes nothing. On a Windows terminal it changes one thing: the
// child is detached from the terminal's console, so a Ctrl+C there no
// longer reaches the harness through the shared console group — only
// agentry sees it, and the engine's TerminateProcess ends the harness
// alone, which is the reach configProcess on Windows already declares.
// Attributes the caller already set on cmd.SysProcAttr are kept, which
// is why a caller that replaces SysProcAttr (the engine's configProcess
// on unix) must call this afterwards.
func NoWindow(cmd *exec.Cmd) { noWindow(cmd) }

// Detach marks cmd to outlive the terminal that started it: a process that
// keeps running after its parent returns (a persistent agent's supervisor)
// is not ended by the terminal's hangup or its Ctrl+C. Attributes the caller
// already set on cmd.SysProcAttr are kept.
func Detach(cmd *exec.Cmd) { detach(cmd) }
