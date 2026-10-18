package destination

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
)

// fakeKeychain stands in for the system keychain.
func fakeKeychain(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	g, s, d := keyringGet, keyringSet, keyringDelete
	keyringGet = func(service, account string) (string, error) {
		if service != modelkey.Service {
			t.Errorf("service %q", service)
		}
		v, ok := m[account]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	keyringSet = func(_, account, value string) error { m[account] = value; return nil }
	keyringDelete = func(_, account string) error {
		if _, ok := m[account]; !ok {
			return keyring.ErrNotFound
		}
		delete(m, account)
		return nil
	}
	t.Cleanup(func() { keyringGet, keyringSet, keyringDelete = g, s, d })
	t.Setenv(modelkey.EnvNoKeyring, "")
	return m
}

func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configdir.EnvVar, dir)
	return dir
}

func TestADestinationIsSavedByNameAndItsSecretsGoToTheKeychain(t *testing.T) {
	dir := home(t)
	kc := fakeKeychain(t)
	s, err := Load()
	if err != nil || len(s.Destinations) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	if err := s.Upsert(Destination{Name: "n8n", Kind: KindWebhook, URL: "https://hooks.example.com/in/abc?token=URLTOKEN", OnMyNetwork: false}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSecrets("n8n", "whsec_SECRET", "Bearer AUTHVALUE"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "AUTHVALUE") {
		t.Errorf("the file holds a credential: %s", raw)
	}
	if fi, _ := os.Stat(filepath.Join(dir, File)); fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("the file's mode is %v", fi.Mode().Perm())
	}
	if kc[SecretAccount("n8n")] != "whsec_SECRET" || kc[AuthorizationAccount("n8n")] != "Bearer AUTHVALUE" {
		t.Errorf("keychain: %v", kc)
	}
	r, err := Resolve([]string{"n8n", "n8n"})
	if err != nil || len(r) != 1 || r[0].Secret != "whsec_SECRET" || r[0].Authorization != "Bearer AUTHVALUE" {
		t.Fatalf("%+v %v", r, err)
	}
	// An empty Authorization removes the saved one; an empty secret keeps it.
	if err := SaveSecrets("n8n", "", ""); err != nil {
		t.Fatal(err)
	}
	if kc[SecretAccount("n8n")] != "whsec_SECRET" || kc[AuthorizationAccount("n8n")] != "" {
		t.Errorf("keychain: %v", kc)
	}
	if got := (Destination{Name: "n", Kind: KindWebhook, URL: "https://hooks.example.com/in/abc?token=URLTOKEN"}).Where(); got != "https://hooks.example.com" {
		t.Errorf("Where: %q", got)
	}
}

func TestResolveRefusesAMissingNameAndAMissingSecret(t *testing.T) {
	home(t)
	fakeKeychain(t)
	s, _ := Load()
	_ = s.Upsert(Destination{Name: "a", Kind: KindWebhook, URL: "https://hooks.example.com/x"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "no destination is saved") {
		t.Errorf("a name that is not saved: %v", err)
	}
	if _, err := Resolve([]string{"a"}); err == nil || !strings.Contains(err.Error(), "no signing secret") {
		t.Errorf("a destination with no secret: %v", err)
	}
	if r, err := Resolve(nil); err != nil || r != nil {
		t.Errorf("no names is no destinations: %+v %v", r, err)
	}
}

func TestADestinationHeldToTheAddressRuleIsNotSavedOrLoaded(t *testing.T) {
	dir := home(t)
	for _, d := range []Destination{
		{Name: "Bad Name", Kind: KindWebhook, URL: "https://hooks.example.com/x"},
		{Name: "x", Kind: "s3", URL: "https://hooks.example.com/x"},
		{Name: "x", Kind: KindWebhook, URL: "http://hooks.example.com/x"},
		{Name: "x", Kind: KindWebhook, URL: "https://10.0.0.5/x"},
		{Name: "x", Kind: KindWebhook, URL: "https://127.0.0.1:8443/x", OnMyNetwork: true},
		{Name: "", Kind: KindWebhook, URL: "https://hooks.example.com/x"},
	} {
		var s Store
		if err := s.Upsert(d); err == nil {
			t.Errorf("%+v was taken", d)
		}
	}
	// A hand-edited file that breaks the rules is refused whole.
	if err := os.WriteFile(filepath.Join(dir, File), []byte("destinations:\n- name: x\n  kind: webhook\n  url: http://10.0.0.5/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("a file with an http address and no On my network was loaded")
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte("destinations: []\nextra: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("a file with an unknown key was loaded")
	}
}

func TestDefaultSetsFollowTheNamesAndTheQuickAgent(t *testing.T) {
	home(t)
	var s Store
	for _, n := range []string{"a", "b"} {
		if err := s.Upsert(Destination{Name: n, Kind: KindWebhook, URL: "https://hooks.example.com/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetDefault("acme/weekly", []string{"a", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if got := s.DefaultsFor("acme/weekly@1.2.3"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("defaults: %v", got)
	}
	if s.DefaultsFor("acme/other@1") != nil {
		t.Error("another QuickAgent has a default set")
	}
	if err := s.SetDefault("acme/weekly", []string{"missing"}); err == nil {
		t.Error("a default naming nothing was taken")
	}
	if err := s.SetDefault("not-an-agent", nil); err == nil {
		t.Error("a bad agent was taken")
	}
	s.Delete("a")
	if got := s.DefaultsFor("acme/weekly"); len(got) != 1 || got[0] != "b" {
		t.Errorf("after delete: %v", got)
	}
	s.Delete("b")
	if len(s.Defaults) != 0 {
		t.Errorf("defaults: %v", s.Defaults)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveSecretsRefusesWhatCouldSplitAHeader(t *testing.T) {
	home(t)
	fakeKeychain(t)
	if err := SaveSecrets("a", "ok", "Bearer x\r\nX-Evil: 1"); err == nil {
		t.Error("an Authorization value with a line break was saved")
	}
	t.Setenv(modelkey.EnvNoKeyring, "1")
	if err := SaveSecrets("a", "ok", ""); err == nil {
		t.Error("credentials were kept with the keychain off")
	}
}

// TestABucketIsSavedByNameAndItsKeyPairGoesToTheKeychain: the file holds the
// bucket's fields and no credential; the key id and the secret access key are
// two keychain accounts, and a run is given both.
func TestABucketIsSavedByNameAndItsKeyPairGoesToTheKeychain(t *testing.T) {
	dir := home(t)
	kc := fakeKeychain(t)
	s, _ := Load()
	d := Destination{Name: "archive", Kind: KindS3, Bucket: "my-reports", Prefix: "agentry/out", Region: "us-east-1", Endpoint: "http://192.168.1.20:9000", OnMyNetwork: true}
	if err := s.Upsert(d); err != nil {
		t.Fatal(err)
	}
	if err := SaveS3Credentials("archive", "AKIAPLANTED0123", "PLANTEDsecretKEY/0123"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, File))
	for _, v := range []string{"PLANTED"} {
		if strings.Contains(string(raw), v) {
			t.Errorf("the file holds a credential: %s", raw)
		}
	}
	if kc[AccessKeyAccount("archive")] != "AKIAPLANTED0123" || kc[SecretAccount("archive")] != "PLANTEDsecretKEY/0123" {
		t.Errorf("keychain: %v", kc)
	}
	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := back.Find("archive")
	if got != d {
		t.Errorf("round trip: %+v", got)
	}
	r, err := Resolve([]string{"archive"})
	if err != nil || len(r) != 1 || r[0].AccessKeyID != "AKIAPLANTED0123" || r[0].Secret != "PLANTEDsecretKEY/0123" || r[0].Authorization != "" {
		t.Fatalf("%+v %v", r, err)
	}
	if !HasAccessKey("archive") || !HasSecret("archive") || HasAuthorization("archive") {
		t.Error("Has*")
	}
	req, opt := back.SealAccounts("archive")
	if len(req) != 2 || req[0] != SecretAccount("archive") || req[1] != AccessKeyAccount("archive") || len(opt) != 0 {
		t.Errorf("seal accounts: %v %v", req, opt)
	}
	if w := d.Where(); w != "s3://my-reports/agentry/out at http://192.168.1.20:9000" {
		t.Errorf("where: %q", w)
	}
	// Without the key id a run is refused before it starts.
	delete(kc, AccessKeyAccount("archive"))
	if _, err := Resolve([]string{"archive"}); err == nil || !strings.Contains(err.Error(), "access key") {
		t.Errorf("a bucket without its key id: %v", err)
	}
	if err := DeleteSecrets("archive"); err != nil || len(kc) != 0 {
		t.Errorf("delete: %v %v", err, kc)
	}
}

// TestAKindIsHeldToItsOwnFields: a webhook takes no bucket field and a bucket no
// address; a bucket is held to the step's rule on load too.
func TestAKindIsHeldToItsOwnFields(t *testing.T) {
	good := Destination{Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", Endpoint: "https://acct.r2.cloudflarestorage.com"}
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]Destination{
		"webhook with bucket":  {Name: "w", Kind: KindWebhook, URL: "https://hooks.example.com/x", Bucket: "my-reports"},
		"webhook with region":  {Name: "w", Kind: KindWebhook, URL: "https://hooks.example.com/x", Region: "auto"},
		"bucket with url":      {Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", URL: "https://hooks.example.com/x"},
		"bucket no region":     {Name: "b", Kind: KindS3, Bucket: "my-reports"},
		"private endpoint":     {Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", Endpoint: "https://10.0.0.5"},
		"metadata endpoint":    {Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", Endpoint: "http://169.254.169.254", OnMyNetwork: true},
		"prefix dot dot":       {Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", Prefix: "../x"},
		"tick without service": {Name: "b", Kind: KindS3, Bucket: "my-reports", Region: "auto", OnMyNetwork: true},
		"unknown kind":         {Name: "b", Kind: "ftp"},
	} {
		if err := d.Check(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	dir := home(t)
	_ = os.WriteFile(filepath.Join(dir, File), []byte("destinations:\n- name: b\n  kind: s3\n  bucket: my-reports\n  region: auto\n  endpoint: https://10.0.0.5\n"), 0o600)
	if _, err := Load(); err == nil {
		t.Error("a file that names a private endpoint without the tick was read")
	}
}

// TestAWebhookSavedOverABucketKeepsNoKeyId: changing a destination's kind leaves
// no credential of the old one behind.
func TestAWebhookSavedOverABucketKeepsNoKeyId(t *testing.T) {
	home(t)
	kc := fakeKeychain(t)
	if err := SaveS3Credentials("x", "AKIAPLANTED0123", "secretKEY"); err != nil {
		t.Fatal(err)
	}
	if err := SaveSecrets("x", "whsec_NEW", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc[AccessKeyAccount("x")]; ok || kc[SecretAccount("x")] != "whsec_NEW" {
		t.Errorf("keychain: %v", kc)
	}
	if err := SaveSecrets("x", "", "Bearer AUTH"); err != nil {
		t.Fatal(err)
	}
	if err := SaveS3Credentials("x", "AKIAPLANTED0123", "secretKEY"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc[AuthorizationAccount("x")]; ok {
		t.Errorf("an Authorization value stayed on a bucket: %v", kc)
	}
	if err := SaveS3Credentials("x", "A/B", ""); err == nil {
		t.Error("a key id with a slash was saved")
	}
}
