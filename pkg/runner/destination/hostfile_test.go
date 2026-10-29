package destination

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hostedSecret = "whsec_HOSTEDsignSECRET"
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
		"another kind":                      `{"destinations":[{"name":"n8n","kind":"ftp","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"a bucket with an address":          `{"destinations":[{"name":"b","kind":"s3","url":"https://hooks.example.com/x","bucket":"my-reports","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"}]}`,
		"a webhook with a bucket":           `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","bucket":"my-reports","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"a webhook with a key id variable":  `{"destinations":[{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"}]}`,
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

// TestAHostHandsAWebhookNotABucket: a destinations file names webhooks alone; a
// bucket entry, with or without bucket fields, refuses the whole file.
func TestAHostHandsAWebhookNotABucket(t *testing.T) {
	dir := t.TempDir()
	for name, doc := range map[string]string{
		"kind s3":      `{"destinations":[{"name":"b","kind":"s3","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_B"}]}`,
		"kind s3 bare": `{"destinations":[{"name":"b","kind":"s3","secretEnv":"AGENTRY_DEST_B"}]}`,
	} {
		path := filepath.Join(dir, "d.json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := FromFile(path, func(string) string { return "s3cret" }); err == nil {
			t.Errorf("%s: a bucket was taken from a destinations file", name)
		}
	}
}

const (
	hostedKeyID   = "AKIAHOSTEDKEYID0123"
	hostedSecret2 = "hostedSECRETaccessKEY0123456789abcdefghij"
)

func bucketEntry(over string) string {
	return `{"destinations":[{"name":"archive","kind":"s3","bucket":"my-reports","prefix":"agentry","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"` + over + `}]}`
}

var bucketEnv = map[string]string{"AGENTRY_DEST_SECRET_0": hostedSecret2, "AGENTRY_DEST_KEY_0": hostedKeyID}

func TestFromFileReadsABucketAndItsKeyPairFromTheEnvironment(t *testing.T) {
	got, names, err := FromFile(writeDestFile(t, `{"destinations":[
		{"name":"archive","kind":"s3","bucket":"my-reports","prefix":"agentry","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"},
		{"name":"r2","kind":"s3","bucket":"reports","endpoint":"https://acct.r2.cloudflarestorage.com","region":"auto","secretEnv":"AGENTRY_DEST_SECRET_1","accessKeyIdEnv":"AGENTRY_DEST_KEY_1"},
		{"name":"n8n","kind":"webhook","url":"https://hooks.example.com/x","secretEnv":"AGENTRY_DEST_SECRET_2"}]}`),
		envOf(map[string]string{"AGENTRY_DEST_SECRET_0": hostedSecret2, "AGENTRY_DEST_KEY_0": hostedKeyID, "AGENTRY_DEST_SECRET_1": hostedSecret2 + "x", "AGENTRY_DEST_KEY_1": hostedKeyID + "X", "AGENTRY_DEST_SECRET_2": hostedSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != KindS3 || got[0].Bucket != "my-reports" || got[0].Prefix != "agentry" || got[0].Region != "eu-west-1" || got[0].Secret != hostedSecret2 || got[0].AccessKeyID != hostedKeyID || got[0].Authorization != "" ||
		got[1].Endpoint != "https://acct.r2.cloudflarestorage.com" || got[1].AccessKeyID != hostedKeyID+"X" || got[2].Kind != KindWebhook {
		t.Errorf("destinations = %+v", got)
	}
	for _, d := range got {
		if !d.Hosted || d.OnMyNetwork {
			t.Errorf("%s: hosted %v, on my network %v", d.Name, d.Hosted, d.OnMyNetwork)
		}
	}
	if strings.Join(names, ",") != "AGENTRY_DEST_KEY_0,AGENTRY_DEST_KEY_1,AGENTRY_DEST_SECRET_0,AGENTRY_DEST_SECRET_1,AGENTRY_DEST_SECRET_2" {
		t.Errorf("variables read = %v", names)
	}
	// An endpoint with no region takes the CLI's default.
	one, _, err := FromFile(writeDestFile(t, `{"destinations":[{"name":"minio","kind":"s3","bucket":"reports","endpoint":"https://minio.example.com","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"}]}`), envOf(bucketEnv))
	if err != nil || one[0].Region != "us-east-1" {
		t.Errorf("default region: %+v %v", one, err)
	}
}

// TestFromFileRefusesABucketAHostMustNotDeliverTo: a private, loopback, link-local
// or odd endpoint, plain http, another port, an endpoint with a path or a login,
// a bad bucket, region or prefix, and a field that would widen the rule.
func TestFromFileRefusesABucketAHostMustNotDeliverTo(t *testing.T) {
	for name, over := range map[string]string{
		"private endpoint":   `,"endpoint":"https://10.0.0.5"`,
		"loopback":           `,"endpoint":"https://127.0.0.1"`,
		"link local":         `,"endpoint":"https://169.254.169.254"`,
		"metadata name":      `,"endpoint":"https://localhost"`,
		"plain http":         `,"endpoint":"http://s3.example.com"`,
		"another port":       `,"endpoint":"https://s3.example.com:9000"`,
		"userinfo":           `,"endpoint":"https://u:p@s3.example.com"`,
		"a path":             `,"endpoint":"https://s3.example.com/base"`,
		"a number host":      `,"endpoint":"https://2130706433"`,
		"on my network":      `,"onMyNetwork":true`,
		"an Authorization":   `,"authorizationEnv":"AGENTRY_DEST_AUTH_0"`,
		"an address":         `,"url":"https://s3.example.com"`,
		"a climbing prefix":  `,"prefix":"../x"`,
		"capitals in bucket": `,"bucket":"My_Reports"`,
	} {
		body := bucketEntry(over)
		if strings.Contains(over, `"prefix"`) {
			body = strings.Replace(body, `"prefix":"agentry",`, "", 1)
		}
		if strings.Contains(over, `"bucket"`) {
			body = strings.Replace(body, `"bucket":"my-reports",`, "", 1)
		}
		if _, _, err := FromFile(writeDestFile(t, body), envOf(bucketEnv)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
	for name, body := range map[string]string{
		"one variable for both halves":       `{"destinations":[{"name":"b","kind":"s3","bucket":"my-reports","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"no region at Amazon":                `{"destinations":[{"name":"b","kind":"s3","bucket":"my-reports","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"}]}`,
		"no key id variable":                 `{"destinations":[{"name":"b","kind":"s3","bucket":"my-reports","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0"}]}`,
		"a key id variable that is not ours": `{"destinations":[{"name":"b","kind":"s3","bucket":"my-reports","region":"eu-west-1","secretEnv":"AGENTRY_DEST_SECRET_0","accessKeyIdEnv":"HOME"}]}`,
		"a secret variable that is not ours": `{"destinations":[{"name":"b","kind":"s3","bucket":"my-reports","region":"eu-west-1","secretEnv":"PATH","accessKeyIdEnv":"AGENTRY_DEST_KEY_0"}]}`,
	} {
		if _, _, err := FromFile(writeDestFile(t, body), envOf(bucketEnv)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
}

// TestFromFileNeverDeliversToABucketWithHalfAKeyPair: either half missing, empty
// or not a plain value refuses the file, and no message carries a value.
func TestFromFileNeverDeliversToABucketWithHalfAKeyPair(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no key id":                  {"AGENTRY_DEST_SECRET_0": hostedSecret2},
		"no secret":                  {"AGENTRY_DEST_KEY_0": hostedKeyID},
		"an empty key id":            {"AGENTRY_DEST_SECRET_0": hostedSecret2, "AGENTRY_DEST_KEY_0": ""},
		"a key id with a line break": {"AGENTRY_DEST_SECRET_0": hostedSecret2, "AGENTRY_DEST_KEY_0": "AKIA\nX"},
		"a secret with a line break": {"AGENTRY_DEST_SECRET_0": "a\nb", "AGENTRY_DEST_KEY_0": hostedKeyID},
	} {
		_, _, err := FromFile(writeDestFile(t, bucketEntry("")), envOf(env))
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
