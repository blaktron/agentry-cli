package hostlink

import (
	"path"
	"strconv"
	"strings"
)

// The ssh and sftp arguments.
// The host link reaches a target host with OpenSSH's own client and no
// configuration of anyone's: no ~/.ssh/config and no system file (-F
// /dev/null), no agent, no forwarding of any kind (agent, X11, TCP, stream
// local, tunnel), no ProxyJump or ProxyCommand, no connection sharing, no
// local command, and the one host key the engine checked, under an alias,
// in a file of that host's alone. ssh keeps the FIRST value it is given for
// an option, so each option is given exactly once,
// here, before anything else on the line: a later -o can never undo one,
// and the builders below never add a second (TestTheHostLinkForwardsNothing).

// sshOptions are the options every ssh and sftp call of a target host
// carries, in a fixed order. dir is the host's folder (HostsMount/<id>).
// tty asks the host for a pseudo-terminal whatever this side has (a shell
// session); every other call has none.
func sshOptions(t Target, dir string, tty bool) []string {
	key, pubkey, password, prefer := "none", "no", "yes", "keyboard-interactive,password"
	requestTTY := "no"
	if tty {
		requestTTY = "force"
	}
	if t.Auth == AuthKey {
		key, pubkey, password, prefer = path.Join(dir, KeyFile), "yes", "no", "publickey"
	}
	opts := [][2]string{
		// Who and where: the relay's address is the destination, the
		// port is the host's own (the relay listens on it).
		{"User", t.User},
		{"Port", strconv.Itoa(t.Port)},
		{"AddressFamily", "inet"},
		{"CanonicalizeHostname", "no"},
		{"ConnectTimeout", "20"},
		{"ServerAliveInterval", "30"},
		{"ServerAliveCountMax", "3"},
		// The host key: the one the engine checked, under the alias,
		// in the host's own file; nothing is learned or written.
		{"HostKeyAlias", HostKeyAlias},
		{"UserKnownHostsFile", path.Join(dir, KnownHostsFile)},
		{"GlobalKnownHostsFile", "/dev/null"},
		{"KnownHostsCommand", "none"},
		{"StrictHostKeyChecking", "yes"},
		{"UpdateHostKeys", "no"},
		{"CheckHostIP", "no"},
		{"HostKeyAlgorithms", t.HostKeyAlgorithm},
		{"VerifyHostKeyDNS", "no"},
		{"VisualHostKey", "no"},
		// Sign-in: one way, the person's, and nothing of an agent's.
		{"BatchMode", "no"},
		{"IdentityAgent", "none"},
		{"AddKeysToAgent", "no"},
		{"IdentitiesOnly", "yes"},
		{"IdentityFile", key},
		{"PKCS11Provider", "none"},
		{"PubkeyAuthentication", pubkey},
		{"PasswordAuthentication", password},
		{"KbdInteractiveAuthentication", password},
		{"HostbasedAuthentication", "no"},
		{"PreferredAuthentications", prefer},
		{"NumberOfPasswordPrompts", "1"},
		// No forwarding of any kind.
		{"ForwardAgent", "no"},
		{"ForwardX11", "no"},
		{"ForwardX11Trusted", "no"},
		{"ClearAllForwardings", "yes"},
		{"GatewayPorts", "no"},
		{"Tunnel", "no"},
		{"ExitOnForwardFailure", "yes"},
		{"StreamLocalBindUnlink", "no"},
		// No other hop, no shared connection, no local command.
		{"ProxyCommand", "none"},
		{"ProxyJump", "none"},
		{"ControlMaster", "no"},
		{"ControlPath", "none"},
		{"PermitLocalCommand", "no"},
		{"LocalCommand", "none"},
		{"RemoteCommand", "none"},
		{"RequestTTY", requestTTY},
		{"EscapeChar", "none"},
		{"LogLevel", "ERROR"},
	}
	out := []string{"-F", "/dev/null"}
	for _, o := range opts {
		out = append(out, "-o", o[0]+"="+o[1])
	}
	return out
}

// sshArgv runs command, one command line, in the account's login shell on
// the host behind relay (the relay's address on the host link's network).
// With sudo, the line runs under sudo -S, which reads the password the
// host link writes to its standard input (never the agent's).
func sshArgv(t Target, dir, relay, command string, sudo bool) []string {
	if sudo {
		command = "sudo -S -p '' -- sh -c " + shellQuote(command)
	}
	argv := append([]string{"ssh"}, sshOptions(t, dir, false)...)
	return append(argv, "-T", "--", relay, command)
}

// shellArgv opens the account's login shell on a pseudo-terminal on the
// host behind relay: no command, and RequestTTY=force (ssh -tt), since the
// host link's own side of the session is a pipe.
func shellArgv(t Target, dir, relay string) []string {
	argv := append([]string{"ssh"}, sshOptions(t, dir, true)...)
	return append(argv, "--", relay)
}

// sftpArgv runs sftp in batch mode, its commands on standard input. sftp
// -b turns on BatchMode, which would switch off SSH_ASKPASS and so a
// password or a passphrase; the BatchMode=no in sshOptions comes first and
// ssh keeps the first value.
func sftpArgv(t Target, dir, relay string) []string {
	argv := append([]string{"sftp"}, sshOptions(t, dir, false)...)
	return append(argv, "-b", "-", "--", relay)
}

// askpassFile is the file ssh's askpass prints for t: the password, or
// the key's passphrase; "" when the key has none.
func askpassFile(t Target, dir string) string {
	switch {
	case t.Auth == AuthPassword:
		return path.Join(dir, PasswordFile)
	case t.Passphrase:
		return path.Join(dir, PassphraseFile)
	}
	return ""
}

// shellTerm is the terminal type a shell session tells the host it has.
const shellTerm = "xterm-256color"

// shellEnv is a shell session's ssh environment: sshEnv's, and the TERM ssh
// sends with its pseudo-terminal request.
func shellEnv(t Target, dir string) []string {
	return append(sshEnv(t, dir), "TERM="+shellTerm)
}

// sshEnv is ssh's whole environment: nothing of this process's, a PATH,
// and, where there is a secret to type, OpenSSH's askpass forced on and
// pointed at this binary and the one file it may print.
func sshEnv(t Target, dir string) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "LC_ALL=C.UTF-8"}
	if f := askpassFile(t, dir); f != "" {
		env = append(env, "SSH_ASKPASS="+AskpassProgram, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=agentry:0", AskpassFileEnv+"="+f)
	}
	return env
}

// shellQuote quotes s for a POSIX shell: one word, every byte literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sftpQuote quotes a path for an sftp batch command. sftp reads a quoted
// argument literally, with \ escaping the next byte, and does not expand a
// glob character inside quotes; a path with a line end is refused before
// it gets here (checkPath).
func sftpQuote(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}
