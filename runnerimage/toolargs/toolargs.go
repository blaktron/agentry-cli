// Package toolargs decodes the arguments of a model's tool call.
//
// Models do not always send arguments in the types a tool's input schema
// names: a boolean arrives as "true", a number as "30", an array as
// "[\"enter\"]", or the whole arguments object as one JSON string; the
// other way round, a request body declared as a string arrives as the
// object it holds, or a header's value as a number. Decode takes each such
// string as the value it encodes, and a value sent for a string parameter
// (or for an element of a list or map of strings) as that value's JSON
// text, so the call the model meant is the call that runs. A string sent
// for a string parameter is never reinterpreted, and a string that does not
// encode the wanted type is refused naming the parameter, what arrived and
// what was wanted.
package toolargs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Decode decodes raw into v, a pointer to a struct. Absent arguments mean
// an empty object. With strict, a field v does not declare is refused;
// without it, it is ignored.
func Decode(raw json.RawMessage, v any, strict bool) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if raw[0] == '"' {
		// The whole arguments object arrived as one JSON string.
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return err
		}
		raw = []byte(inner)
	}
	err := unmarshal(raw, v, strict)
	var te *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &te) {
		return err
	}
	coerced, err := coerce(raw, v)
	if err != nil {
		return err
	}
	return unmarshal(coerced, v, strict)
}

func unmarshal(raw []byte, v any, strict bool) error {
	if !strict {
		return json.Unmarshal(raw, v)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// coerce rewrites, in the arguments object, each value whose JSON type is
// not its struct field's (coerceValue).
func coerce(raw []byte, v any) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return raw, nil
	}
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = f.Name
		}
		r, ok := obj[name]
		if !ok {
			continue
		}
		c, err := coerceValue(name, r, f.Type)
		if err != nil {
			return nil, err
		}
		obj[name] = c
	}
	return json.Marshal(obj)
}

// coerceValue takes a JSON string sent for a non-string type as the JSON
// it encodes, and a non-null value sent for a string, or as an element of
// a list or map of strings, as a string of its compact JSON text.
func coerceValue(name string, r json.RawMessage, t reflect.Type) (json.RawMessage, error) {
	if len(r) == 0 || isNull(r) {
		return r, nil
	}
	if t.Kind() == reflect.String {
		if r[0] == '"' {
			return r, nil
		}
		return asText(r)
	}
	if r[0] == '"' {
		var inner string
		if err := json.Unmarshal(r, &inner); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(inner)) {
			return nil, fmt.Errorf("%s: got the string %q, want %s", name, inner, t)
		}
		c, err := coerceElems(json.RawMessage(inner), t)
		if err != nil || json.Unmarshal(c, reflect.New(t).Interface()) != nil {
			return nil, fmt.Errorf("%s: got the string %q, want %s", name, inner, t)
		}
		return c, nil
	}
	return coerceElems(r, t)
}

// coerceElems turns each non-string element of a list or map of strings
// into a string of its JSON text; any other value is returned as it is.
func coerceElems(r json.RawMessage, t reflect.Type) (json.RawMessage, error) {
	if (t.Kind() != reflect.Slice && t.Kind() != reflect.Map) || t.Elem().Kind() != reflect.String {
		return r, nil
	}
	switch {
	case t.Kind() == reflect.Slice && r[0] == '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(r, &elems); err != nil {
			return r, nil
		}
		for i, e := range elems {
			if len(e) > 0 && e[0] != '"' && !isNull(e) {
				text, err := asText(e)
				if err != nil {
					return nil, err
				}
				elems[i] = text
			}
		}
		return json.Marshal(elems)
	case t.Kind() == reflect.Map && r[0] == '{':
		var elems map[string]json.RawMessage
		if err := json.Unmarshal(r, &elems); err != nil {
			return r, nil
		}
		for k, e := range elems {
			if len(e) > 0 && e[0] != '"' && !isNull(e) {
				text, err := asText(e)
				if err != nil {
					return nil, err
				}
				elems[k] = text
			}
		}
		return json.Marshal(elems)
	}
	return r, nil
}

// asText is the JSON string of a value's compact JSON text.
func asText(r json.RawMessage) (json.RawMessage, error) {
	var text bytes.Buffer
	if err := json.Compact(&text, r); err != nil {
		return nil, err
	}
	return json.Marshal(text.String())
}

func isNull(r json.RawMessage) bool { return bytes.Equal(r, []byte("null")) }
