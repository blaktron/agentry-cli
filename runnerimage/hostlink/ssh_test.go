package hostlink

import (
	"slices"
	"strings"
	"testing"
)

func keyTarget() Target {
	return Target{ID: "server", Named: "box.example", Address: "203.0.113.4", Port: 2222, User: "admin",
		HostKeyAlgorithm: "ssh-ed25519", Auth: AuthKey, Passphrase: true, Sudo: true}
}

func passwordTarget() Target {
	t := keyTarget()
	t.Auth, t.Passphrase = AuthPassword, false
	return t
}

// options reads argv's -o options, in order, failing on any other flag
// before the destination but -F /dev/null and the ones each builder adds.
func options(t *testing.T, argv []string, allowed ...string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return got
		case a == "-o":
			k, v, ok := strings.Cut(argv[i+1], "=")
			if !ok {
				t.Fatalf("option %q has no value", argv[i+1])
			}
			if _, dup := got[strings.ToLower(k)]; dup {
				t.Fatalf("option %s is given twice: ssh keeps the first value, so a second one is a lie", k)
			}
			got[strings.ToLower(k)] = v
			i++
		case a == "-F":
			if argv[i+1] != "/dev/null" {
				t.Fatalf("-F %s: no configuration file but /dev/null", argv[i+1])
			}
			i++
		case slices.Contains(allowed, a):
		default:
			t.Fatalf("argv has the flag %q before the destination: %q", a, argv)
		}
	}
	t.Fatalf("argv has no -- before the destination: %q", argv)
	return nil
}

// TestTheHostLinkForwardsNothing: no
// agent, X11, TCP, stream-local or tunnel forwarding, no other hop, no
// shared connection, no configuration of anyone's, the one checked key.
func TestTheHostLinkForwardsNothing(t *testing.T) {
	want := map[string]string{
		"forwardagent": "no", "forwardx11": "no", "forwardx11trusted": "no", "clearallforwardings": "yes",
		"gatewayports": "no", "tunnel": "no", "proxycommand": "none", "proxyjump": "none",
		"controlmaster": "no", "controlpath": "none", "identityagent": "none", "addkeystoagent": "no",
		"permitlocalcommand": "no", "localcommand": "none", "pkcs11provider": "none",
		"stricthostkeychecking": "yes", "globalknownhostsfile": "/dev/null", "knownhostscommand": "none",
		"updatehostkeys": "no", "hostkeyalias": HostKeyAlias, "hostkeyalgorithms": "ssh-ed25519",
		"userknownhostsfile": "/run/agentry/hosts/server/known_hosts", "identitiesonly": "yes",
		"batchmode": "no", "requesttty": "no", "escapechar": "none", "user": "admin", "port": "2222",
	}
	for name, argv := range map[string][]string{
		"ssh":   sshArgv(keyTarget(), "/run/agentry/hosts/server", "172.20.0.3", "uptime", false),
		"sftp":  sftpArgv(keyTarget(), "/run/agentry/hosts/server", "172.20.0.3"),
		"shell": shellArgv(keyTarget(), "/run/agentry/hosts/server", "172.20.0.3"),
	} {
		allowed := []string{"-T"}
		switch name {
		case "sftp":
			allowed = []string{"-b", "-"}
		case "shell":
			// The host's pseudo-terminal, asked for whatever this side
			// has, and no command, so the account's login shell.
			allowed = nil
			if argv[len(argv)-2] != "--" || argv[len(argv)-1] != "172.20.0.3" {
				t.Errorf("shell: argv ends %q", argv[len(argv)-2:])
			}
		}
		opts := options(t, argv, allowed...)
		for k, v := range want {
			if k == "requesttty" && name == "shell" {
				v = "force"
			}
			if opts[k] != v {
				t.Errorf("%s: %s=%q, want %q", name, k, opts[k], v)
			}
		}
		for _, a := range argv {
			for _, bad := range []string{"-A", "-X", "-Y", "-L", "-R", "-D", "-W", "-J", "-M", "-S", "-N", "-f", "-g"} {
				if a == bad {
					t.Errorf("%s: argv carries %s: %q", name, bad, argv)
				}
			}
		}
	}
}

func TestTheDestinationIsTheRelayAndTheCommandOneWord(t *testing.T) {
	argv := sshArgv(keyTarget(), "/d", "172.20.0.3", "ls -la; echo 'hi'", false)
	n := len(argv)
	if argv[n-3] != "--" || argv[n-2] != "172.20.0.3" || argv[n-1] != "ls -la; echo 'hi'" {
		t.Fatalf("argv ends %q", argv[n-3:])
	}
	sudo := sshArgv(keyTarget(), "/d", "172.20.0.3", "cat /etc/shadow | head -1", true)
	if got, want := sudo[len(sudo)-1], `sudo -S -p '' -- sh -c 'cat /etc/shadow | head -1'`; got != want {
		t.Errorf("sudo command %q, want %q", got, want)
	}
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// TestTheSignInIsOneWay: a key signs in by its key alone, a password by
// the password alone, so askpass is never asked for the wrong secret.
func TestTheSignInIsOneWay(t *testing.T) {
	k := options(t, sshArgv(keyTarget(), "/d", "r", "true", false), "-T")
	if k["identityfile"] != "/d/key" || k["pubkeyauthentication"] != "yes" || k["passwordauthentication"] != "no" ||
		k["kbdinteractiveauthentication"] != "no" || k["preferredauthentications"] != "publickey" {
		t.Errorf("key sign-in options: %v", k)
	}
	p := options(t, sshArgv(passwordTarget(), "/d", "r", "true", false), "-T")
	if p["identityfile"] != "none" || p["pubkeyauthentication"] != "no" || p["passwordauthentication"] != "yes" ||
		p["preferredauthentications"] != "keyboard-interactive,password" {
		t.Errorf("password sign-in options: %v", p)
	}
}

// TestSFTPKeepsAskpass: sftp -b sets BatchMode, which turns askpass off;
// the host link's BatchMode=no comes first, and ssh keeps the first.
func TestSFTPKeepsAskpass(t *testing.T) {
	argv := sftpArgv(passwordTarget(), "/d", "r")
	b, batch := slices.Index(argv, "BatchMode=no"), slices.Index(argv, "-b")
	if b < 0 || batch < 0 || b > batch {
		t.Fatalf("BatchMode=no at %d, -b at %d: %q", b, batch, argv)
	}
}

func TestSSHEnvHoldsTheAskpassAlone(t *testing.T) {
	env := sshEnv(passwordTarget(), "/run/agentry/hosts/server")
	for _, want := range []string{"SSH_ASKPASS=" + AskpassProgram, "SSH_ASKPASS_REQUIRE=force", AskpassFileEnv + "=/run/agentry/hosts/server/password"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
	k := keyTarget()
	if f := askpassFile(k, "/d"); f != "/d/passphrase" {
		t.Errorf("a locked key's askpass file %q", f)
	}
	k.Passphrase = false
	for _, e := range sshEnv(k, "/d") {
		if strings.HasPrefix(e, "SSH_ASKPASS") || strings.HasPrefix(e, AskpassFileEnv) {
			t.Errorf("a key with no passphrase still sets %s", e)
		}
	}
}

func TestSFTPQuoting(t *testing.T) {
	for in, want := range map[string]string{
		`/etc/nginx/nginx.conf`: `"/etc/nginx/nginx.conf"`,
		`a "b" c\d *?[x]`:       `"a \"b\" c\\d *?[x]"`,
	} {
		if got := sftpQuote(in); got != want {
			t.Errorf("sftpQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := remote("-rf"); got != `"./-rf"` {
		t.Errorf("a path that looks like a flag: %s", got)
	}
}
