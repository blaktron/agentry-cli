package spec

import (
	"reflect"
	"strings"
	"testing"
)

func TestDiffToolFilter(t *testing.T) {
	listed := []string{"get_current_time", "convert_time", "Echo", "ECHO_ALL"}
	for _, c := range []struct {
		name   string
		filter []string
		want   ToolFilterDiff
	}{
		{"exact", []string{"get_current_time", "convert_time"}, ToolFilterDiff{}},
		{"case differs", []string{"Get_Current_Time"}, ToolFilterDiff{CaseOnly: []CaseOnlyMatch{{"Get_Current_Time", "get_current_time"}}}},
		{"mixed", []string{"CONVERT_TIME", "get_current_time", "nothing_like_it"}, ToolFilterDiff{
			CaseOnly: []CaseOnlyMatch{{"CONVERT_TIME", "convert_time"}},
			Absent:   []string{"nothing_like_it"},
		}},
		{"an exact name wins over a case variant", []string{"Echo"}, ToolFilterDiff{}},
		{"the other case variant is a mismatch", []string{"echo_all"}, ToolFilterDiff{CaseOnly: []CaseOnlyMatch{{"echo_all", "ECHO_ALL"}}}},
		{"duplicates count once", []string{"Convert_Time", "Convert_Time", "convert_time"}, ToolFilterDiff{CaseOnly: []CaseOnlyMatch{{"Convert_Time", "convert_time"}}}},
		{"no filter", nil, ToolFilterDiff{}},
	} {
		got := DiffToolFilter(c.filter, listed)
		if !reflect.DeepEqual(got, c.want) && (!got.Empty() || !c.want.Empty()) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestCaseMessageNamesTheExactToolAndBoundsWhatItQuotes(t *testing.T) {
	d := DiffToolFilter([]string{"Get_Current_Time"}, []string{"get_current_time"})
	msg := d.CaseMessage()
	for _, want := range []string{`"Get_Current_Time"`, `"get_current_time"`, "only when case is ignored", "write"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	long := strings.Repeat("a", 500)
	if msg := DiffToolFilter([]string{strings.ToUpper(long)}, []string{long}).CaseMessage(); len(msg) > 700 {
		t.Errorf("a long name is quoted whole: %d bytes", len(msg))
	}
	if (ToolFilterDiff{}).CaseMessage() != "" {
		t.Error("an empty diff has a message")
	}
}
