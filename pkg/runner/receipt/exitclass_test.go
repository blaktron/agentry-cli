package receipt

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The table is sorted by code, names each class once, and gives every row a
// meaning a person can read: one phrase, lower case, no trailing stop.
func TestTheExitClassTableIsWellFormed(t *testing.T) {
	names := map[string]bool{}
	for i, c := range ExitClasses {
		if i > 0 && ExitClasses[i-1].Code >= c.Code {
			t.Errorf("row %d (%d) is not after row %d (%d)", i, c.Code, i-1, ExitClasses[i-1].Code)
		}
		if c.Name == "" || names[c.Name] {
			t.Errorf("code %d: name %q is empty or repeated", c.Code, c.Name)
		}
		names[c.Name] = true
		if c.Meaning == "" || strings.HasSuffix(c.Meaning, ".") || strings.ToLower(c.Meaning[:1]) != c.Meaning[:1] {
			t.Errorf("code %d: meaning %q is not one lower-case phrase without a full stop", c.Code, c.Meaning)
		}
		if strings.Contains(c.Meaning, "\n") || len(c.Meaning) > 110 {
			t.Errorf("code %d: meaning %q is not one short phrase", c.Code, c.Meaning)
		}
	}
	if ok := ExitClassOf(0); ok.Name != "ok" || ok.RetryLikelyHelps {
		t.Errorf("code 0: %+v", ok)
	}
}

// A code the table does not hold still has a row, named exit-<code>, so a
// reader never shows a bare number.
func TestAnUnknownCodeHasARowOfItsOwn(t *testing.T) {
	for _, code := range []int{3, 9, 19, 99, 137} {
		c := ExitClassOf(code)
		if c.Code != code || c.Name != "exit-"+itoa(code) || c.Meaning == "" || c.RetryLikelyHelps {
			t.Errorf("code %d: %+v", code, c)
		}
		if ExitClassName(code) != c.Name {
			t.Errorf("ExitClassName(%d) = %q, want %q", code, ExitClassName(code), c.Name)
		}
	}
	if _, ok := ExitClassByName("exit-99"); ok {
		t.Error("exit-99 is not a row of the table")
	}
	if c, ok := ExitClassByName("blocked"); !ok || c.Code != 18 {
		t.Errorf("blocked: %+v, %v", c, ok)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// exit-classes.json is the table rendered by ExitClassesJSON; go generate
// rewrites it, and a table change without the file fails here, in CI.
func TestTheShippedExitClassFileIsTheTable(t *testing.T) {
	got, err := os.ReadFile("exit-classes.json")
	if err != nil {
		t.Fatal(err)
	}
	want := ExitClassesJSON()
	if !bytes.Equal(got, want) {
		t.Fatal("exit-classes.json differs from the table: run go generate ./pkg/runner/receipt")
	}
	var back struct {
		ExitClasses []ExitClass `json:"exitClasses"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.ExitClasses) != len(ExitClasses) || back.ExitClasses[len(back.ExitClasses)-1] != ExitClasses[len(ExitClasses)-1] {
		t.Errorf("the file reads back as %d rows, the table has %d", len(back.ExitClasses), len(ExitClasses))
	}
}

// The envelope carries the class's words beside its name, from the table.
func TestTheEnvelopeCarriesTheClassWords(t *testing.T) {
	env := &Envelope{Status: "error", ExitCode: 14}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["exitClass"] != "harness" || back["exitClassMeaning"] != ExitClassOf(14).Meaning || back["retryLikelyHelps"] != true {
		t.Errorf("envelope: class %v, meaning %v, retry %v", back["exitClass"], back["exitClassMeaning"], back["retryLikelyHelps"])
	}
	ok := &Envelope{Status: "ok", ExitCode: 0}
	b, _ = ok.Marshal()
	if !strings.Contains(string(b), `"retryLikelyHelps": false`) || !strings.Contains(string(b), `"exitClassMeaning": "the run finished`) {
		t.Errorf("ok envelope: %s", b)
	}

	// Every code a run exits with names its own class in the envelope, and the
	// table answers the same row by code and by name. A client acts on these
	// words, so each is written out here rather than read back from the table.
	classes := []struct {
		code  int
		name  string
		retry bool
	}{
		{0, "ok", false}, {1, "cli", false}, {2, "usage", false}, {10, "verify", false},
		{11, "policy", false}, {12, "compile", false}, {13, "gateway", true}, {14, "harness", true},
		{15, "timeout", false}, {16, "budget", false}, {17, "outputSchema", true}, {18, "blocked", false},
		{137, "exit-137", false},
	}
	if len(ExitClasses) != len(classes)-1 {
		t.Errorf("the table has %d rows, the contract has %d", len(ExitClasses), len(classes)-1)
	}
	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			row := ExitClassOf(c.code)
			if row.Code != c.code || row.Name != c.name || row.RetryLikelyHelps != c.retry || row.Meaning == "" {
				t.Fatalf("ExitClassOf(%d) = %+v, want %s with retry %v", c.code, row, c.name, c.retry)
			}
			if byName, found := ExitClassByName(c.name); c.code != 137 && (!found || byName != row) {
				t.Errorf("ExitClassByName(%q) = %+v, %v, want %+v", c.name, byName, found, row)
			}
			env := &Envelope{Status: "error", ExitCode: c.code}
			raw, err := env.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if doc["exitClass"] != c.name || doc["exitClassMeaning"] != row.Meaning || doc["retryLikelyHelps"] != c.retry {
				t.Errorf("envelope for exit %d: class %v, meaning %v, retry %v", c.code, doc["exitClass"], doc["exitClassMeaning"], doc["retryLikelyHelps"])
			}
		})
	}
}
