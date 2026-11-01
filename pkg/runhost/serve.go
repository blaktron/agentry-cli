package runhost

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Serve is the forced command of every run key. sshd runs it whatever the
// device asked for, with the asked command in SSH_ORIGINAL_COMMAND; Serve
// relays the session to the service's socket only when that command is one
// of Verbs, and otherwise answers with a refusal and exit status 2. It never
// runs anything itself. It returns the exit status the session ends with:
// the service's exit frame's.
func Serve(ctx context.Context, original string, stdin io.Reader, stdout io.Writer, socket string) int {
	fw := NewFrameWriter(stdout)
	fields := strings.Fields(original)
	if len(fields) == 0 || !IsVerb(fields[0]) {
		asked := original
		if asked == "" {
			asked = "a shell"
		}
		_ = fw.Exit(2, fmt.Errorf("this key reaches only the runhost's service; it cannot run %q", asked))
		return 2
	}
	for _, a := range fields[1:] {
		if len(a) > 256 {
			_ = fw.Exit(2, fmt.Errorf("an argument longer than 256 characters"))
			return 2
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		_ = fw.Exit(3, fmt.Errorf("the runhost's service is not answering (%v): `agentry runhost doctor` with the admin sign-in says why", err))
		return 3
	}
	defer func() { _ = conn.Close() }()
	if err := WriteRequest(conn, Request{Verb: fields[0], Args: fields[1:]}); err != nil {
		_ = fw.Exit(3, err)
		return 3
	}
	go func() {
		_, _ = io.Copy(conn, stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	// Frames pass through unchanged; the exit frame's code is the session's.
	code, sawExit := 0, false
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, err := readLine(br)
		if err != nil {
			break
		}
		var f Frame
		if json.Unmarshal(line, &f) == nil && f.Type == FrameExit {
			code, sawExit = f.Code, true
		}
		if _, err := stdout.Write(append(line, '\n')); err != nil {
			return 3
		}
		if sawExit {
			break
		}
	}
	if !sawExit {
		_ = fw.Exit(3, ErrNoExit)
		return 3
	}
	return code
}

// ServeSocket is the service's socket as serve finds it: under
// XDG_RUNTIME_DIR, which the SSH session's login sets, or else under
// /run/user for this uid.
func ServeSocket() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d + "/" + SocketName
	}
	return SocketPath(os.Getuid())
}

// LoginShell decides what the CLI does when it runs as the agentry
// account's login shell, ShellPath, which sshd and su know it by. sshd runs a
// session's command as `shell -c command`, and the forced command makes that
// `-c` + ServeCommand whatever the device asked. Under that name everything
// else is refused: a login, an interactive shell, and any other -c command,
// so the account has no shell even if a key were installed without its
// forced command. handled is false when the CLI runs under any other name;
// serve is true when the command is serve.
func LoginShell(args []string) (handled, serve bool, refusal string) {
	if len(args) == 0 || strings.TrimPrefix(filepath.Base(args[0]), "-") != ShellName {
		return false, false, ""
	}
	if len(args) == 3 && args[1] == "-c" && args[2] == ServeCommand {
		return true, true, ""
	}
	return true, false, "the agentry account has no shell: it runs nothing but the runhost's service"
}
