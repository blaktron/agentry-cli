package adapter

import (
	"regexp"
	"testing"
)

// notExactly matches every tool name but the input tools, under
// docker-agent's anchoring.
func TestNotExactlyMatchesEveryNameButTheInputTools(t *testing.T) {
	re := regexp.MustCompile("^(?:" + notExactly(InputTools...) + ")$")
	for _, name := range InputTools {
		if re.MatchString(name) {
			t.Errorf("%q matched", name)
		}
	}
	for _, name := range []string{"", "r", "read_inpu", "read_inputs", "xread_input", "search_input", "search_inputss",
		"list_inputs", "write_report", "fetch", "run_command", "web_read", "knowledge_read", "github_get_file", "read_input\n"} {
		if !re.MatchString(name) {
			t.Errorf("%q did not match", name)
		}
	}
}
