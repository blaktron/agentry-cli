package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func defaults() options {
	return options{after: 5 * time.Minute, keepTurns: 2, keepBrowser: 2, browserPrefix: "browser_", top: 8}
}

func TestASessionsGrowthAndColdStartsAreMeasured(t *testing.T) {
	m, err := measureFile(defaults(), filepath.Join("testdata", "session", "receipts", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != "session" || m.Adapter != "docker-agent" || m.Mode != "persistent" {
		t.Fatalf("format %q adapter %q mode %q", m.Format, m.Adapter, m.Mode)
	}
	if m.Turns != 4 || m.Requests != 5 || m.FirstContext != 5000 || m.PeakContext != 11500 || m.ContextLimit != 200000 || m.Restarts != 0 {
		t.Fatalf("turns %d requests %d first %d peak %d limit %d restarts %d", m.Turns, m.Requests, m.FirstContext, m.PeakContext, m.ContextLimit, m.Restarts)
	}
	if m.SumContext != 5000+6000+9000+11000+11500 {
		t.Fatalf("summed context %d", m.SumContext)
	}
	// The second report of one call's result is not counted again.
	if m.ToolCalls != 7 || m.ToolResults != 7 || m.ResultBytes != 16100 || m.MaxTurnBytes != 9000 {
		t.Fatalf("calls %d results %d bytes %d largest turn %d", m.ToolCalls, m.ToolResults, m.ResultBytes, m.MaxTurnBytes)
	}
	if got := m.Tools[0]; got.Tool != "fs_read" || got.Calls != 4 || got.Results != 4 || got.Bytes != 12000 || got.MaxBytes != 5000 {
		t.Fatalf("top tool %+v", got)
	}
	if len(m.BrowserResults) != 3 {
		t.Fatalf("browser results %v", m.BrowserResults)
	}

	// A pause runs from the model's last activity: the note the runner
	// carries into a turn and its notices are not the model's.
	want := []coldStart{
		// Turns 1 and 2 are both kept, and the one browser result is among
		// the newest two.
		{Turn: 3, GapSeconds: 474, ContextTokens: 9000, ResultBytes: 13000},
		// Turn 1 is past the two kept turns: its read goes, and so does its
		// snapshot, which is no longer among the two newest browser results.
		{Turn: 4, GapSeconds: 1192, ContextTokens: 11000, ResultBytes: 16100, ClearableBytes: 4000, BrowserClearable: 3000},
	}
	if len(m.ColdStarts) != len(want) {
		t.Fatalf("cold starts %+v", m.ColdStarts)
	}
	for i := range want {
		if m.ColdStarts[i] != want[i] {
			t.Errorf("cold start %d = %+v, want %+v", i, m.ColdStarts[i], want[i])
		}
	}
}

func TestTheNewestBrowserResultsAreKeptWhereverTheyFall(t *testing.T) {
	tl := newTally(defaults(), "f", "docker-agent", "persistent", "session")
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		tl.turn(at)
		if i == 0 {
			tl.result("s", "browser_snapshot", 700)
		}
		tl.result(fmt.Sprintf("r%d", i), "fs_read", 100)
		tl.lastTime = at
		at = at.Add(time.Minute)
	}
	c := tl.cold(time.Hour)
	// Only turns 3 and 4 are kept, but the one browser result is the newest
	// of its kind and stays; the reads of turns 1 and 2 go.
	if c.ClearableBytes != 200 || c.BrowserClearable != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestAConversationThatShrankHoldsNoEarlierResults(t *testing.T) {
	tl := newTally(defaults(), "f", "docker-agent", "persistent", "session")
	tl.turn(time.Time{})
	tl.usage(50000, 200000, false)
	tl.result("a", "fs_read", 9000)
	tl.usage(52000, 200000, false)
	tl.usage(8000, 200000, false) // started over with a handoff
	tl.result("b", "fs_read", 100)
	if tl.m.Restarts != 1 || len(tl.results) != 1 {
		t.Fatalf("restarts %d results %+v", tl.m.Restarts, tl.results)
	}
	tl.usage(9000, 200000, true) // the harness's own compaction
	if tl.m.Restarts != 2 || tl.m.Compactions != 1 || len(tl.results) != 0 {
		t.Fatalf("restarts %d compactions %d results %+v", tl.m.Restarts, tl.m.Compactions, tl.results)
	}
}

func TestOnlyToolNamesAndSizesArePrinted(t *testing.T) {
	for _, args := range [][]string{{"testdata"}, {"-json", "testdata"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		for _, leak := range []string{"sk-live", "Authorization", "Bearer", "xxxx"} {
			if strings.Contains(out.String()+errOut.String(), leak) {
				t.Fatalf("%v printed %q:\n%s", args, leak, out.String())
			}
		}
	}
	for name, want := range map[string]string{
		"browser_snapshot":                  "browser_snapshot",
		"mcp:server/tool-1.2":               "mcp:server/tool-1.2",
		"Read with header Authorization: x": "(unnamed)",
		"":                                  "(unnamed)",
		strings.Repeat("a", 65):             "(unnamed)",
	} {
		if got := toolName(name); got != want {
			t.Errorf("toolName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAWarmTurnIsNoColdStart(t *testing.T) {
	opt := defaults()
	opt.after = time.Hour
	m, err := measureFile(opt, filepath.Join("testdata", "session", "receipts", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.ColdStarts) != 0 {
		t.Fatalf("cold starts under an hour's cache: %+v", m.ColdStarts)
	}
}

// Every adapter's recorded run stream is read through that adapter's own
// parser, so a run on any harness is measured the same way.
func TestRecordedRunsAreReadThroughTheirAdapters(t *testing.T) {
	conf := filepath.Join("..", "..", "pkg", "runner", "adapter", "testdata", "conformance")
	for _, name := range []string{"docker-agent", "claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			opt := defaults()
			opt.adapter = name
			m, err := measureFile(opt, filepath.Join(conf, name, "events.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			if m.Format != "run" || m.Turns != 1 {
				t.Fatalf("format %q turns %d", m.Format, m.Turns)
			}
			if m.ToolCalls == 0 || m.ToolResults != m.ToolCalls {
				t.Fatalf("calls %d results %d", m.ToolCalls, m.ToolResults)
			}
			if m.Requests == 0 || m.PeakContext == 0 {
				t.Fatalf("requests %d peak %d", m.Requests, m.PeakContext)
			}
			for _, s := range m.Tools {
				if s.Tool == "(unnamed)" {
					t.Fatalf("a result was not paired with its call: %+v", m.Tools)
				}
			}
		})
	}
}

func TestARunWithNoAdapterIsRefusedByName(t *testing.T) {
	var out, errOut bytes.Buffer
	conf := filepath.Join("..", "..", "pkg", "runner", "adapter", "testdata", "conformance", "docker-agent", "events.ndjson")
	if code := run([]string{conf}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "run-plan.json") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestAFolderIsSearchedAndJSONIsPrinted(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-json", "testdata"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var ms []measure
	if err := json.Unmarshal(out.Bytes(), &ms); err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].PeakContext != 11500 || len(ms[0].ColdStarts) != 2 {
		t.Fatalf("%+v", ms)
	}
}

func TestTheTableNamesTheColdStarts(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"testdata/session"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"docker-agent persistent (session)", "browser_snapshot", "cold start before turn 4 after 1192s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAConversationWithNoToolsListsNone(t *testing.T) {
	m := newTally(defaults(), "f", "docker-agent", "persistent", "session").finish()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tools":[]`) {
		t.Fatalf("%s", b)
	}
}
