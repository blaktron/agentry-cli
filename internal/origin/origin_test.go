package origin

import "testing"

func TestOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://reg.example/api/v1", "https://reg.example"},
		{"HTTPS://Reg.Example.:443/", "https://reg.example"},
		{"http://reg.example:80", "http://reg.example"},
		{"https://reg.example:8443", "https://reg.example:8443"},
		{"http://[::1]:8080/x", "http://[::1]:8080"},
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"/relative/path", ""},
		{"::bad url", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Of(tt.in); got != tt.want {
			t.Errorf("Of(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSame(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same origin, other path", "https://reg.example/att/1", "https://reg.example", true},
		{"default port spelled out", "https://reg.example:443/x", "https://reg.example", true},
		{"case and trailing dot", "https://REG.example./x", "https://reg.example", true},
		{"punycode and unicode spellings", "https://xn--bcher-kva.example/x", "https://bücher.example", true},
		{"http spelling of an https host", "http://reg.example/att/1", "https://reg.example", false},
		{"http on the https port", "http://reg.example:443/att/1", "https://reg.example", false},
		{"other port", "https://reg.example:8443/x", "https://reg.example", false},
		{"other host", "https://cdn.example/x", "https://reg.example", false},
		{"relative URL", "/att/1", "https://reg.example", false},
		{"both without a host", "/a", "/a", false},
		{"unparsable", "::bad url", "https://reg.example", false},
	}
	for _, tt := range tests {
		if got := Same(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Same(%q, %q) = %v, want %v", tt.name, tt.a, tt.b, got, tt.want)
		}
	}
}
