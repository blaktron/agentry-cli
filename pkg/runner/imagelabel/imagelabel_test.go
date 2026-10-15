package imagelabel

import (
	"strings"
	"testing"
)

func TestCheckWantsTheLabelInEveryStageThatRuns(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		ok         bool
	}{
		{"single stage labelled", "FROM a\n" + Line(Decode) + "RUN x\n", true},
		{"single stage unlabelled", "FROM a\nRUN x\n", false},
		{"a FROM-only stage needs none", "ARG T=t\nFROM ${T} AS t\n\nFROM b\n" + Line(Decode) + "COPY --from=t / /\n", true},
		{"an earlier stage unlabelled", "FROM a AS build\nRUN x\nFROM b\n" + Line(Decode) + "COPY --from=build / /\n", false},
		{"the final stage unlabelled", "FROM a AS build\n" + Line(Decode) + "RUN x\nFROM b\nCOPY --from=build / /\n", false},
		{"another kind's label", "FROM a\n" + Line(Exec) + "RUN x\n", false},
		{"no FROM", "RUN x\n", false},
	} {
		err := Check(tc.file, Decode)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Check = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The key must never be one the run's sweep or the socket filter reads: a
// container inherits its image's labels.
func TestKeyIsNoneOfTheRunsOwn(t *testing.T) {
	for _, own := range []string{"agentry.run", "agentry.gateway.run", "agentry.owner.", "org.agentry.harness."} {
		if Key == own || strings.HasPrefix(Key, own) {
			t.Errorf("Key %q is in the run's own namespace %q", Key, own)
		}
	}
	if Filter != "label=org.agentry.image" {
		t.Errorf("Filter = %q", Filter)
	}
	for _, k := range []string{Runner, Exec, UnsafeExec, Decode, HostLink, ClaudeCode, Copilot} {
		if strings.ContainsAny(k, " \"=\n") {
			t.Errorf("kind %q needs quoting in a LABEL", k)
		}
	}
}
