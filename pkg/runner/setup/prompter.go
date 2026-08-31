// TTYPrompter: the CLI's Prompter over stdin/stderr. Secrets are read with
// terminal echo disabled via stty against the controlling terminal —
// stdlib-only, unix-only, and honest when it cannot: the fallback prompts
// visibly and says so rather than silently echoing what the operator
// believed was hidden. The desktop app implements Prompter as a form and
// never comes through here (RUNNER-PLAN-2 §P8).
package setup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// TTYPrompter prompts on Err (stderr, keeping stdout pipeline-clean, §R8)
// and reads answers from In.
type TTYPrompter struct {
	In  io.Reader
	Err io.Writer

	reader *bufio.Reader
}

func (t *TTYPrompter) buf() *bufio.Reader {
	if t.reader == nil {
		t.reader = bufio.NewReader(t.In)
	}
	return t.reader
}

// Ask implements Prompter.
func (t *TTYPrompter) Ask(prompt, def string) (string, error) {
	if def != "" {
		_, _ = fmt.Fprintf(t.Err, "%s [%s]: ", prompt, def)
	} else {
		_, _ = fmt.Fprintf(t.Err, "%s: ", prompt)
	}
	line, err := t.buf().ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("read answer: %w", err)
	}
	ans := strings.TrimSpace(line)
	if ans == "" {
		return def, nil
	}
	return ans, nil
}

// AskSecret implements Prompter with echo disabled where possible.
func (t *TTYPrompter) AskSecret(prompt string) (string, error) {
	_, _ = fmt.Fprintf(t.Err, "%s: ", prompt)
	restore, ok := disableEcho()
	if !ok {
		_, _ = fmt.Fprint(t.Err, "(warning: input will be visible) ")
	}
	line, err := t.buf().ReadString('\n')
	if restore != nil {
		restore()
	}
	_, _ = fmt.Fprintln(t.Err)
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// Confirm implements Prompter.
func (t *TTYPrompter) Confirm(prompt string, def bool) (bool, error) {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	ans, err := t.Ask(fmt.Sprintf("%s [%s]", prompt, hint), "")
	if err != nil {
		return def, err
	}
	switch strings.ToLower(ans) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return def, nil
	}
}

// Say implements Prompter.
func (t *TTYPrompter) Say(text string) { _, _ = fmt.Fprintln(t.Err, text) }

// disableEcho turns terminal echo off via stty on the controlling
// terminal, returning the restore function. ok=false means echo could not
// be disabled (no tty, no stty, non-unix) and the caller should warn.
func disableEcho() (restore func(), ok bool) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return nil, false
	}
	run := func(arg string) error {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = tty
		return cmd.Run()
	}
	if err := run("-echo"); err != nil {
		_ = tty.Close()
		return nil, false
	}
	return func() {
		_ = run("echo")
		_ = tty.Close()
	}, true
}
