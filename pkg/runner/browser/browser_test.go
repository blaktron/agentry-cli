package browser

import (
	"slices"
	"strings"
	"testing"
)

func TestToolsIsAnAllowlistAndLosesEvaluateWithSecrets(t *testing.T) {
	all := Tools(false)
	if !slices.IsSorted(all) || !slices.Contains(all, ToolEvaluate) || !slices.Contains(all, "browser_pdf_save") || !slices.Contains(all, "browser_verify_text_visible") {
		t.Fatalf("Tools(false) = %v", all)
	}
	for _, never := range []string{"browser_run_code_unsafe", "browser_file_upload", "browser_install", "browser_route", "browser_cookie_get", "browser_local_storage_get"} {
		if slices.Contains(all, never) {
			t.Fatalf("the allowlist passes %s", never)
		}
	}
	with := Tools(true)
	if slices.Contains(with, ToolEvaluate) || len(with) != len(all)-1 {
		t.Fatalf("Tools(true) = %v", with)
	}
}

func TestArgs(t *testing.T) {
	a := strings.Join(Args("proxy-x", false), " ")
	for _, want := range []string{"--headless", "--isolated", "--browser chromium", "--caps testing,pdf", "--proxy-server http://proxy-x:3128", "--output-dir /out-browser", "--config " + ConfigMount} {
		if !strings.Contains(a, want) {
			t.Errorf("args %q lack %q", a, want)
		}
	}
	if strings.Contains(a, "--secrets") || strings.Contains(a, "no-sandbox") || strings.Contains(a, "unrestricted") {
		t.Errorf("args %q", a)
	}
	if !strings.HasSuffix(strings.Join(Args("p", true), " "), "--secrets "+SecretsMount) {
		t.Error("the secrets file is not passed")
	}
}

func TestParseSite(t *testing.T) {
	for _, c := range []struct{ in, entry, url string }{
		{"https://Shop.Example/", "shop.example", "https://Shop.Example/"},
		{" https://shop.example/login?next=/cart ", "shop.example", "https://shop.example/login?next=/cart"},
		{"https://shop.example:443/", "shop.example", "https://shop.example:443/"},
		{"http://staging.lan:8080", "staging.lan:8080", "http://staging.lan:8080"},
		{"http://192.168.1.20/", "192.168.1.20", "http://192.168.1.20/"},
		{"https://[2001:db8::1]:8443/", "[2001:db8::1]:8443", "https://[2001:db8::1]:8443/"},
		{"https://bücher.example/", "xn--bcher-kva.example", "https://b%C3%BCcher.example/"},
	} {
		s, err := ParseSite(c.in)
		if err != nil || s.ProxyEntry() != c.entry || s.URL != c.url {
			t.Errorf("ParseSite(%q) = %+v %v, entry %q", c.in, s, err, s.ProxyEntry())
		}
	}
	for _, bad := range []string{"", "shop.example", "ftp://shop.example/", "https://user:pw@shop.example/", "https:///path", "https://shop.example/#top", "https://shop.example:0/", "https://shop.example:99999/", "javascript:alert(1)", "file:///etc/passwd", "https://" + strings.Repeat("a", 2050) + ".example/"} {
		if s, err := ParseSite(bad); err == nil {
			t.Errorf("ParseSite(%q) = %+v, want an error", bad, s)
		}
	}
}
