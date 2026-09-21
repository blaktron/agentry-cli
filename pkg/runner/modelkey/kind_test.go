package modelkey

import "testing"

func TestKindOfReadsTheAnthropicShapesAndNothingElse(t *testing.T) {
	cases := []struct {
		provider, key string
		want          Kind
	}{
		{"anthropic", "sk-ant-api03-abc", KindAPI},
		{"anthropic", "sk-ant-oat01-abc", KindSubscription},
		{"Anthropic", "  sk-ant-oat01-abc  ", KindSubscription},
		{"anthropic", "sk-ant-admin01-abc", KindUnknown},
		{"anthropic", "", KindUnknown},
		{"openai", "sk-proj-abc", KindAPI},
		{"openai", "sk-ant-oat01-abc", KindAPI}, // another provider's key is what it is
		{"custom", "anything", KindAPI},
		{"google", "AIza...", KindAPI},
	}
	for _, c := range cases {
		if got := KindOf(c.provider, c.key); got != c.want {
			t.Errorf("KindOf(%q, %q) = %q, want %q", c.provider, c.key, got, c.want)
		}
	}
}
