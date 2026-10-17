package toolargs

import (
	"encoding/json"
	"strings"
	"testing"
)

type args struct {
	Command string   `json:"command"`
	Keys    []string `json:"keys"`
	Timeout int      `json:"timeout"`
	Sudo    bool     `json:"sudo"`
}

func TestStringsAreTakenAsTheValueTheyEncode(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var v args
		err := Decode(json.RawMessage(`{"command":"whoami","keys":"[\"enter\"]","timeout":"30","sudo":"true"}`), &v, strict)
		if err != nil || v.Command != "whoami" || len(v.Keys) != 1 || v.Keys[0] != "enter" || v.Timeout != 30 || !v.Sudo {
			t.Fatalf("strict=%v: %+v %v", strict, v, err)
		}
		v = args{}
		if err := Decode(json.RawMessage(`"{\"command\":\"id\",\"sudo\":true}"`), &v, strict); err != nil || v.Command != "id" || !v.Sudo {
			t.Fatalf("strict=%v: the object as one string: %+v %v", strict, v, err)
		}
	}
}

func TestAStringParameterIsTakenAsWritten(t *testing.T) {
	var v args
	if err := Decode(json.RawMessage(`{"command":"true","timeout":5}`), &v, true); err != nil || v.Command != "true" || v.Timeout != 5 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestAStringThatIsNotTheTypeIsRefusedNamingIt(t *testing.T) {
	var v args
	err := Decode(json.RawMessage(`{"command":"id","sudo":"yes"}`), &v, true)
	if err == nil || !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), `"yes"`) || !strings.Contains(err.Error(), "bool") {
		t.Fatalf("%v", err)
	}
	err = Decode(json.RawMessage(`{"keys":"enter"}`), &v, false)
	if err == nil || !strings.Contains(err.Error(), "keys") || !strings.Contains(err.Error(), "[]string") {
		t.Fatalf("%v", err)
	}
}

func TestUnknownFieldsAreRefusedOnlyWhenStrict(t *testing.T) {
	var v args
	if err := Decode(json.RawMessage(`{"command":"id","become":true}`), &v, false); err != nil || v.Command != "id" {
		t.Fatalf("lenient: %+v %v", v, err)
	}
	if err := Decode(json.RawMessage(`{"command":"id","become":true}`), &v, true); err == nil || !strings.Contains(err.Error(), "become") {
		t.Fatalf("strict: %v", err)
	}
	if err := Decode(json.RawMessage(`{"sudo":"true","become":true}`), &v, true); err == nil || !strings.Contains(err.Error(), "become") {
		t.Fatalf("strict after coercion: %v", err)
	}
}

func TestAbsentArgumentsAreAnEmptyObject(t *testing.T) {
	var v args
	if err := Decode(nil, &v, true); err != nil || v.Command != "" || v.Keys != nil || v.Sudo {
		t.Fatalf("%+v %v", v, err)
	}
	if err := Decode(json.RawMessage(`{broken`), &v, true); err == nil {
		t.Fatal("malformed JSON decoded")
	}
}

// TestAValueForAStringParameterIsTakenAsItsJSONText: a request body sent as
// an object rather than as the string of that object is that string.
func TestAValueForAStringParameterIsTakenAsItsJSONText(t *testing.T) {
	type fetch struct {
		URL  string `json:"url"`
		Body string `json:"body"`
	}
	for _, strict := range []bool{false, true} {
		var v fetch
		err := Decode(json.RawMessage(`{"url":"https://example.com","body":{"name":"a", "tags":[1,2]}}`), &v, strict)
		if err != nil || v.URL != "https://example.com" || v.Body != `{"name":"a","tags":[1,2]}` {
			t.Fatalf("strict=%v: object body: %+v %v", strict, v, err)
		}
		for in, want := range map[string]string{`[1,2]`: `[1,2]`, `42`: `42`, `true`: `true`} {
			v = fetch{}
			if err := Decode(json.RawMessage(`{"body":`+in+`}`), &v, strict); err != nil || v.Body != want {
				t.Fatalf("strict=%v: body %s: %+v %v", strict, in, v, err)
			}
		}
	}
	var v fetch
	if err := Decode(json.RawMessage(`{"body":null}`), &v, true); err != nil || v.Body != "" {
		t.Fatalf("null body: %+v %v", v, err)
	}
}

// TestNonStringElementsOfAStringCollectionAreTakenAsTheirJSONText: a
// header's value or a command's argument sent as a number is that number's
// text.
func TestNonStringElementsOfAStringCollectionAreTakenAsTheirJSONText(t *testing.T) {
	type call struct {
		Args    []string          `json:"args"`
		Headers map[string]string `json:"headers"`
	}
	for _, strict := range []bool{false, true} {
		var v call
		err := Decode(json.RawMessage(`{"args":["-n",5,true],"headers":{"X-Count":5,"Accept":"text/html","X-Obj":{"a":1}}}`), &v, strict)
		if err != nil || len(v.Args) != 3 || v.Args[1] != "5" || v.Args[2] != "true" ||
			v.Headers["X-Count"] != "5" || v.Headers["Accept"] != "text/html" || v.Headers["X-Obj"] != `{"a":1}` {
			t.Fatalf("strict=%v: %+v %v", strict, v, err)
		}
		v = call{}
		if err := Decode(json.RawMessage(`{"args":"[\"-n\",5]"}`), &v, strict); err != nil || len(v.Args) != 2 || v.Args[1] != "5" {
			t.Fatalf("strict=%v: stringified array with a number: %+v %v", strict, v, err)
		}
	}
}
