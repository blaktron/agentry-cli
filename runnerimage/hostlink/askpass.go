package hostlink

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Askpass is this binary as OpenSSH's SSH_ASKPASS (no sshpass): ssh
// runs it with its prompt as the one argument and reads a line from its
// standard output. It prints the file AskpassFileEnv names, which must be
// a password or a passphrase file in a target host's folder under
// HostsMount, and nothing else: whoever can run it in the host link's
// container can read those files anyway, and nothing else of the run's is
// there to print. It answers the exit status.
func Askpass(stdout, stderr io.Writer) int {
	return askpass(os.Getenv(AskpassFileEnv), HostsMount, stdout, stderr)
}

func askpass(file, hostsDir string, stdout, stderr io.Writer) int {
	clean := path.Clean(file)
	dir, name := path.Split(clean)
	if file == "" || clean != file || (name != PasswordFile && name != PassphraseFile) ||
		path.Dir(strings.TrimSuffix(dir, "/")) != hostsDir || !ValidID(path.Base(dir)) {
		_, _ = fmt.Fprintln(stderr, "agentry-askpass: no target host's password or passphrase file is named")
		return 1
	}
	b, err := os.ReadFile(file)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "agentry-askpass: cannot read the file it was given")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimRight(string(b), "\r\n"))
	return 0
}
