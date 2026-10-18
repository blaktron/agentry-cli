package destination

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hostedSecret = "whsec_HOSTEDsigningSECRET0123456789abcdef"
	hostedAuth   = "Bearer HOSTEDauthorizationVALUE0123456789abcd"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeDestFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "destinations.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromFileReadsDestinationsAndTheirCredentialsFromTheEnvironment(t *testing.T) {
	p := writeDestFile(t, `{"destinations":[
		{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/in/abc?k=1","secretEnv":"AGENTRY_DEST_SECRET_0","authorizationEnv":"AGENTRY_DEST_AUTH_0"},
		{"name":"crm","kind":"webhook","url":"https://crm.example.com/hook","secretEnv":"AGENTRY_DEST_SECRET_1"}]}`)
	got, names, err := FromFile(p, envOf(map[string]string{
		"AGENTRY_DEST_SECRET_0": hostedSecret, "AGENTRY_DEST_AUTH_0": hostedAuth, "AGENTRY_DEST_SECRET_1": "second-secret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "n8n" || got[0].Secret != hostedSecret || got[0].Authorization != hostedAuth || got[1].Name != "crm" || got[1].Secret != "second-secret" || got[1].Authorization != "" {
		t.Errorf("destinations = %+v", got)
	}
	for _, d := range got {
		if !d.Hosted || d.OnMyNetwork {
			t.Errorf("%s: hosted %v, on my network %v", d.Name, d.Hosted, d.OnMyNetwork)
		}
	}
	if strings.Join(names, ",") != "AGENTRY_DEST_AUTH_0,AGENTRY_DEST_SECRET_0,AGENTRY_DEST_SECRET_1" {
		t.Errorf("variables read = %v", names)
	}
}

// TestFromFileRefusesWhatAHostMustNotDeliverTo: a private, loopback or
// link-local address, plain http, another port, a name that breaks the rule, a
// field that would widen the rule, and a file that is not the shape.
func TestFromFileRefusesWhatAHostMustNotDeliverTo(t *testing.T) {
	env := envOf(map[string]string{"AGENTRY_DEST_SECRET_0": hostedSecret})
	entry := func(url string) string {
		return `{"destinations":[{"name":"n8n","kind":"webhook","url":"` + url + `","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`
	}
	for name, body := range map[string]string{
		"private address":                   entry("https://10.0.0.5/hook"),
		"loopback":                          entry("https://127.0.0.1/hook"),
		"link local":                        entry("https://169.254.169.254/latest"),
		"metadata name":                     entry("https://localhost/hook"),
		"plain http":                        entry("http://hooks.example.com/hook"),
		"another port":                      entry("https://hooks.example.com:8443/hook"),
		"userinfo":                          entry("https://u:p@hooks.example.com/hook"),
		"a number host":                     entry("https://2130706433/hook"),
		"on my network":                     `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","onMyNetwork":true,"secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"another kind":                      `{"destinations":[{"name":"n8n","kind":"s3","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"bad name":                          `{"destinations":[{"name":"N8N!","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"twice":                             `{"destinations":[{"name":"a","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0"},{"name":"a","kind":"webhook","url":"https://hooks.example.com/y","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"none":                              `{"destinations":[]}`,
		"unknown field":                     `{"destinations":[],"extra":1}`,
		"two documents":                     entry("https://hooks.example.com/x") + entry("https://hooks.example.com/x"),
		"not json":                          `destinations: []`,
		"a variable that is not ours":       `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"HOME"}]}`,
		"an auth variable that is not ours": `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0","authorizationEnv":"PATH"}]}`,
	} {
		if _, _, err := FromFile(writeDestFile(t, body), env); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
	var many []string
	for i := range MaxPerRun + 1 {
		many = append(many, `{"name":"d`+string(rune('a'+i))+`","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0"}`)
	}
	if _, _, err := FromFile(writeDestFile(t, `{"destinations":[`+strings.Join(many, ",")+`]}`), env); err == nil {
		t.Error("more than the most a run delivers to was accepted")
	}
}

// TestFromFileNeverDeliversUnsigned: a signing secret that is not in the
// environment, is empty or is not a plain line refuses the file, as does an
// Authorization variable that is named and empty; no message carries a value.
func TestFromFileNeverDeliversUnsigned(t *testing.T) {
	body := `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0","authorizationEnv":"AGENTRY_DEST_AUTH_0"}]}`
	for name, env := range map[string]map[string]string{
		"no secret":                  {"AGENTRY_DEST_AUTH_0": hostedAuth},
		"empty secret":               {"AGENTRY_DEST_SECRET_0": "", "AGENTRY_DEST_AUTH_0": hostedAuth},
		"a secret with a line break": {"AGENTRY_DEST_SECRET_0": "a\nb", "AGENTRY_DEST_AUTH_0": hostedAuth},
		"no authorization":           {"AGENTRY_DEST_SECRET_0": hostedSecret},
		"a bad authorization":        {"AGENTRY_DEST_SECRET_0": hostedSecret, "AGENTRY_DEST_AUTH_0": "Bearer a\r\nX: y"},
	} {
		_, _, err := FromFile(writeDestFile(t, body), envOf(env))
		if err == nil {
			t.Errorf("%s: the file was accepted", name)
			continue
		}
		for _, v := range env {
			if v != "" && strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error carries a credential: %v", name, err)
			}
		}
	}
}

func TestFromFileRefusesAFileThatIsNotAPlainOneOfTheSize(t *testing.T) {
	env := envOf(map[string]string{"AGENTRY_DEST_SECRET_0": hostedSecret})
	dir := t.TempDir()
	if _, _, err := FromFile(dir, env); err == nil {
		t.Error("a directory was read")
	}
	if _, _, err := FromFile(filepath.Join(dir, "missing.json"), env); err == nil {
		t.Error("a missing file was read")
	}
	big := writeDestFile(t, `{"destinations":[],"pad":"`+strings.Repeat("x", MaxFileBytes)+`"}`)
	if _, _, err := FromFile(big, env); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("a file past the cap: %v", err)
	}
}
