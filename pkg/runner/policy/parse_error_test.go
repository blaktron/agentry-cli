package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadExplainsParseErrors: the sentence a person reads when the policy will
// not parse names the file and the line, an unknown key with both causes and
// the action for each, a value of the wrong kind with what was found and what
// belongs, and malformed YAML with the decoder's own detail — and never a Go
// type name. Strictness is unchanged: every one is refused.
func TestLoadExplainsParseErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, text string
		want       []string
	}{
		{"unknown top-level key", "harnes:\n  model: x\n", []string{
			`line 1: "harnes" is not a policy key here`, "misspelt", "newer Agentry", "update Agentry, or remove the key"}},
		{"unknown nested key", "exec:\n  packagse:\n    git: git\n", []string{
			`line 2: "packagse" is not a policy key here`}},
		{"wrong kind of value", "images:\n  runner: [a, b]\n", []string{
			"line 2: the value is a list where text belongs"}},
		{"wrong kind with the value quoted", "llm:\n  maxCostUsd: lots\n", nil},
		{"malformed YAML", "images:\n  runner: [\n", []string{"parse ", "did not find expected"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(strings.ReplaceAll(tc.name, " ", "-")+".yaml", tc.text)
			_, _, err := Load(path)
			if err == nil {
				t.Fatalf("%q was accepted", tc.text)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("message does not name the file: %s", msg)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message lacks %q:\n%s", w, msg)
				}
			}
			for _, leak := range []string{"policy.Policy", "policy.", "not found in type", "cannot unmarshal", "!!"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message leaks the decoder's or a Go type's words %q:\n%s", leak, msg)
				}
			}
		})
	}
}
